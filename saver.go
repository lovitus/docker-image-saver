package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

type runOptions struct {
	Image    string
	Output   string
	Arch     string
	Proxy    string
	Username string
	Password string
	Insecure bool
	Stdout   io.Writer
	Stderr   io.Writer
}

type platformOption struct {
	ManifestRef string
	Platform    platform
	MediaType   string
	Size        int64
}

type saveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

const (
	maxCachedBlobSize  = 64 << 20
	maxCachedBlobBytes = 256 << 20
)

type progressEvent struct {
	Stage        string    `json:"stage"`
	Message      string    `json:"message,omitempty"`
	Platform     string    `json:"platform,omitempty"`
	CurrentLayer int       `json:"current_layer,omitempty"`
	TotalLayers  int       `json:"total_layers,omitempty"`
	BytesDone    int64     `json:"bytes_done,omitempty"`
	BytesTotal   int64     `json:"bytes_total,omitempty"`
	SpeedBPS     float64   `json:"speed_bps,omitempty"`
	ETASeconds   int64     `json:"eta_seconds,omitempty"`
	Done         bool      `json:"done,omitempty"`
	Error        string    `json:"error,omitempty"`
	Time         time.Time `json:"time"`
}

type exportHooks struct {
	Log      io.Writer
	Progress func(progressEvent)
}

func (h *exportHooks) logf(format string, args ...any) {
	if h == nil || h.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(h.Log, format, args...)
}

func (h *exportHooks) emit(event progressEvent) {
	if h == nil || h.Progress == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	h.Progress(event)
}

type exportSession struct {
	mu            sync.Mutex
	configCache   map[string][]byte
	blobCache     map[string]*cachedBlob
	blobCacheList *list.List
	blobCacheSize int64
}

type cachedBlob struct {
	Digest    string
	MediaType string
	Data      []byte
	element   *list.Element
}

func newExportSession() *exportSession {
	return &exportSession{
		configCache:   make(map[string][]byte),
		blobCache:     make(map[string]*cachedBlob),
		blobCacheList: list.New(),
	}
}

func runNonInteractive(opts runOptions) error {
	ref, err := parseImageRef(opts.Image)
	if err != nil {
		return err
	}

	client, err := newRegistryClient(opts.Proxy, opts.Username, opts.Password, opts.Insecure)
	if err != nil {
		return err
	}
	platforms, singleManifest, err := resolvePlatforms(client, ref)
	if err != nil {
		return err
	}

	selected, err := selectPlatforms(opts.Arch, len(platforms))
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no architectures selected")
	}
	hooks := &exportHooks{
		Log:      opts.Stdout,
		Progress: newTextProgressSink(opts.Stdout),
	}
	hooks.logf("Image: %s\n", ref.DisplayTag())
	for _, idx := range selected {
		hooks.logf("Selected [%d] %s\n", idx+1, platforms[idx].Platform.String())
	}
	report, err := exportSelectedPlatforms(client, ref, singleManifest, platforms, selected, opts.Output, hooks)
	if err != nil {
		return err
	}
	printExportReport(opts.Stdout, report)
	return nil
}

type exportedArchive struct {
	Platform platform
	Result   saveResult
}

type exportReport struct {
	Image      string
	Archives   []exportedArchive
	IndexPath  string
	OutputBase string
}

type archiveValidationExpectation struct {
	ConfigDigests map[string]string
}

type platformIndexFile struct {
	Image       string                  `json:"image"`
	GeneratedAt string                  `json:"generated_at"`
	Archives    []platformIndexFileItem `json:"archives"`
}

type platformIndexFileItem struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	TarPath      string `json:"tar_path"`
	SizeBytes    int64  `json:"size_bytes"`
}

func exportSelectedPlatforms(
	client *registryClient,
	ref imageRef,
	singleManifest *imageManifest,
	platforms []platformOption,
	selected []int,
	outputPath string,
	hooks *exportHooks,
) (exportReport, error) {
	report := exportReport{
		Image:      ref.DisplayTag(),
		OutputBase: filepath.Clean(outputPath),
	}
	if len(selected) == 0 {
		return report, fmt.Errorf("no architectures selected")
	}

	outputs, err := perPlatformOutputs(outputPath, platforms, selected)
	if err != nil {
		return report, err
	}
	session := newExportSession()

	report.Archives = make([]exportedArchive, 0, len(selected))
	for i, idx := range selected {
		if idx < 0 || idx >= len(platforms) {
			return report, fmt.Errorf("selected index %d out of range", idx+1)
		}
		outPath := outputs[i]
		if err := os.MkdirAll(filepath.Dir(filepath.Clean(outPath)), 0o755); err != nil {
			return report, fmt.Errorf("create output directory: %w", err)
		}
		if len(selected) > 1 {
			hooks.logf("\nSaving archive for %s -> %s\n", platforms[idx].Platform.String(), outPath)
		}
		hooks.emit(progressEvent{
			Stage:    "archive_start",
			Platform: platforms[idx].Platform.String(),
			Message:  outPath,
		})
		expectation, err := writeDockerTar(client, session, ref, singleManifest, platforms, []int{idx}, outPath, hooks)
		if err != nil {
			return report, err
		}
		hooks.emit(progressEvent{
			Stage:    "validate_archive",
			Platform: platforms[idx].Platform.String(),
			Message:  "validating archive",
		})
		if err := validateDockerArchiveWithExpectation(outPath, expectation); err != nil {
			return report, fmt.Errorf("validate archive %s: %w", outPath, err)
		}
		result, err := summarizeSavedFile(outPath)
		if err != nil {
			return report, err
		}
		report.Archives = append(report.Archives, exportedArchive{
			Platform: platforms[idx].Platform,
			Result:   result,
		})
		hooks.emit(progressEvent{
			Stage:    "archive_done",
			Platform: platforms[idx].Platform.String(),
			Message:  result.AbsPath,
			Done:     true,
		})
	}

	indexPath, err := writePlatformIndexFile(report.OutputBase, report.Image, report.Archives)
	if err != nil {
		return report, err
	}
	report.IndexPath = indexPath
	return report, nil
}

func resolvePlatforms(client *registryClient, ref imageRef) ([]platformOption, *imageManifest, error) {
	manifestBody, contentType, err := client.getManifest(ref, "")
	if err != nil {
		return nil, nil, err
	}
	mediaType := normalizeMediaType(contentType)
	switch mediaType {
	case mtDockerManifestListV2, mtOCIImageIndexV1:
		var idx manifestList
		if err := json.Unmarshal(manifestBody, &idx); err != nil {
			return nil, nil, fmt.Errorf("parse image index: %w", err)
		}
		if len(idx.Manifests) == 0 {
			return nil, nil, fmt.Errorf("manifest list contains no manifests")
		}
		platforms := make([]platformOption, 0, len(idx.Manifests))
		filteredUnknown := 0
		for _, desc := range idx.Manifests {
			if desc.Digest == "" {
				continue
			}
			if isUnknownPlatform(desc.Platform) {
				filteredUnknown++
				continue
			}
			platforms = append(platforms, platformOption{
				ManifestRef: desc.Digest,
				Platform:    desc.Platform,
				MediaType:   desc.MediaType,
				Size:        desc.Size,
			})
		}
		if len(platforms) == 0 && filteredUnknown > 0 {
			for _, desc := range idx.Manifests {
				if desc.Digest == "" {
					continue
				}
				platforms = append(platforms, platformOption{
					ManifestRef: desc.Digest,
					Platform:    desc.Platform,
					MediaType:   desc.MediaType,
					Size:        desc.Size,
				})
			}
		}
		if len(platforms) == 0 {
			return nil, nil, fmt.Errorf("manifest list contained no downloadable manifests")
		}
		return platforms, nil, nil
	case mtDockerManifestV2, mtOCIManifestV1, "":
		var m imageManifest
		if err := json.Unmarshal(manifestBody, &m); err != nil {
			return nil, nil, fmt.Errorf("parse image manifest: %w", err)
		}
		return []platformOption{{ManifestRef: ref.ManifestReference(), Platform: platform{OS: "linux", Architecture: "unknown"}}}, &m, nil
	default:
		return nil, nil, fmt.Errorf("unsupported top-level media type: %s", mediaType)
	}
}

func writeDockerTar(
	client *registryClient,
	session *exportSession,
	ref imageRef,
	singleManifest *imageManifest,
	platforms []platformOption,
	selected []int,
	outputPath string,
	hooks *exportHooks,
) (archiveValidationExpectation, error) {
	expectation := archiveValidationExpectation{ConfigDigests: make(map[string]string)}
	outFile, err := os.Create(outputPath)
	if err != nil {
		return expectation, fmt.Errorf("create output tar: %w", err)
	}
	defer outFile.Close()

	tw := tar.NewWriter(outFile)
	defer tw.Close()

	entries := make([]saveManifestEntry, 0, len(selected))
	repos := make(map[string]map[string]string)
	writtenDirs := make(map[string]bool)
	writtenConfig := make(map[string]bool)
	writtenLayers := make(map[string]bool)

	multipleSelection := len(selected) > 1
	for i, idx := range selected {
		if idx < 0 || idx >= len(platforms) {
			return expectation, fmt.Errorf("selected index %d out of range", idx+1)
		}
		p := platforms[idx]
		hooks.logf("[%d/%d] Resolving manifest for %s\n", i+1, len(selected), p.Platform.String())
		hooks.emit(progressEvent{
			Stage:    "manifest",
			Platform: p.Platform.String(),
			Message:  "resolving manifest",
		})

		var manifest imageManifest
		if singleManifest != nil {
			manifest = *singleManifest
		} else {
			data, _, err := client.getManifestDescriptor(ref, descriptor{
				Digest: p.ManifestRef,
				Size:   p.Size,
			})
			if err != nil {
				return expectation, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return expectation, fmt.Errorf("parse platform manifest (%s): %w", p.ManifestRef, err)
			}
		}
		if len(manifest.Layers) == 0 {
			return expectation, fmt.Errorf("manifest for %s has no layers", p.Platform.String())
		}

		configDigest := manifest.Config.Digest
		if configDigest == "" {
			return expectation, fmt.Errorf("manifest for %s missing config digest", p.Platform.String())
		}
		configName := digestHex(configDigest) + ".json"
		configBytes, _, err := session.getConfigDescriptor(client, ref, manifest.Config)
		if err != nil {
			return expectation, fmt.Errorf("download config %s: %w", configDigest, err)
		}
		expectation.ConfigDigests[configName] = configDigest
		if !writtenConfig[configName] {
			if err := writeTarBytes(tw, configName, configBytes, 0o644); err != nil {
				return expectation, err
			}
			writtenConfig[configName] = true
		}

		diffIDs := extractDiffIDs(configBytes)
		if len(diffIDs) > 0 && len(diffIDs) != len(manifest.Layers) {
			return expectation, fmt.Errorf("config diff_ids count mismatch for %s: got %d want %d", p.Platform.String(), len(diffIDs), len(manifest.Layers))
		}
		layerPaths := make([]string, 0, len(manifest.Layers))
		parentID := ""
		for li, layer := range manifest.Layers {
			layerID := layerIDFrom(layer, diffIDs, li)
			layerDir := layerID + "/"
			layerTarPath := layerDir + "layer.tar"
			if !writtenDirs[layerDir] {
				if err := writeTarDir(tw, layerDir); err != nil {
					return expectation, err
				}
				writtenDirs[layerDir] = true
			}
			if !writtenLayers[layerID] {
				if err := writeTarBytes(tw, layerDir+"VERSION", []byte("1.0\n"), 0o644); err != nil {
					return expectation, err
				}
				meta := map[string]string{"id": layerID}
				if parentID != "" {
					meta["parent"] = parentID
				}
				metaJSON, _ := json.Marshal(meta)
				if err := writeTarBytes(tw, layerDir+"json", metaJSON, 0o644); err != nil {
					return expectation, err
				}

				hooks.logf("Downloading layer %d/%d for %s\n", li+1, len(manifest.Layers), p.Platform.String())
				expectedDiffID := ""
				if li < len(diffIDs) {
					expectedDiffID = diffIDs[li]
				}
				if err := writeLayerToTar(tw, client, session, ref, p.Platform, layer, expectedDiffID, li+1, len(manifest.Layers), layerTarPath, hooks); err != nil {
					return expectation, err
				}
				writtenLayers[layerID] = true
			}
			layerPaths = append(layerPaths, layerTarPath)
			parentID = layerID
		}

		repoName, tag := outputRepoAndTag(ref, p.Platform, multipleSelection)
		repoTag := repoName + ":" + tag
		entries = append(entries, saveManifestEntry{
			Config:   configName,
			RepoTags: []string{repoTag},
			Layers:   layerPaths,
		})
		if repos[repoName] == nil {
			repos[repoName] = make(map[string]string)
		}
		repos[repoName][tag] = parentID
	}

	manifestJSON, err := json.Marshal(entries)
	if err != nil {
		return expectation, fmt.Errorf("encode manifest.json: %w", err)
	}
	if err := writeTarBytes(tw, "manifest.json", manifestJSON, 0o644); err != nil {
		return expectation, err
	}
	repoJSON, err := json.Marshal(repos)
	if err != nil {
		return expectation, fmt.Errorf("encode repositories: %w", err)
	}
	if err := writeTarBytes(tw, "repositories", repoJSON, 0o644); err != nil {
		return expectation, err
	}

	return expectation, nil
}

func (s *exportSession) getConfig(client *registryClient, ref imageRef, digest string) ([]byte, string, error) {
	return s.getConfigDescriptor(client, ref, descriptor{Digest: digest})
}

func (s *exportSession) getConfigDescriptor(client *registryClient, ref imageRef, desc descriptor) ([]byte, string, error) {
	s.mu.Lock()
	if data, ok := s.configCache[desc.Digest]; ok {
		s.mu.Unlock()
		return data, mtDockerConfigV1, nil
	}
	s.mu.Unlock()

	data, contentType, err := client.getBlobDescriptor(ref, desc)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.configCache[desc.Digest] = data
	s.mu.Unlock()
	return data, contentType, nil
}

func (s *exportSession) getCachedBlob(digest string) (cachedBlob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.blobCache[digest]
	if !ok {
		return cachedBlob{}, false
	}
	if blob.element != nil {
		s.blobCacheList.MoveToBack(blob.element)
	}
	return cachedBlob{Digest: blob.Digest, MediaType: blob.MediaType, Data: blob.Data}, true
}

func (s *exportSession) storeCachedBlob(digest, mediaType string, data []byte) {
	if int64(len(data)) > maxCachedBlobBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.blobCache[digest]; ok {
		s.blobCacheSize -= int64(len(existing.Data))
		if existing.element != nil {
			s.blobCacheList.Remove(existing.element)
		}
		delete(s.blobCache, digest)
	}
	for s.blobCacheSize+int64(len(data)) > maxCachedBlobBytes && s.blobCacheList.Len() > 0 {
		front := s.blobCacheList.Front()
		evicted := front.Value.(*cachedBlob)
		s.blobCacheSize -= int64(len(evicted.Data))
		delete(s.blobCache, evicted.Digest)
		s.blobCacheList.Remove(front)
	}
	blob := &cachedBlob{Digest: digest, MediaType: mediaType, Data: data}
	blob.element = s.blobCacheList.PushBack(blob)
	s.blobCache[digest] = blob
	s.blobCacheSize += int64(len(data))
}

func writeLayerToTar(
	tw *tar.Writer,
	client *registryClient,
	session *exportSession,
	ref imageRef,
	p platform,
	layer descriptor,
	expectedDiffID string,
	layerIndex int,
	totalLayers int,
	tarPath string,
	hooks *exportHooks,
) error {
	if rawBlob, ok := session.getCachedBlob(layer.Digest); ok {
		return writeLayerFromRawBlob(tw, rawBlob.Data, rawBlob.MediaType, p, layer, expectedDiffID, layerIndex, totalLayers, tarPath, hooks)
	}

	if layer.Size > 0 && layer.Size <= maxCachedBlobSize {
		rc, contentType, err := client.openBlobDescriptor(ref, layer)
		if err != nil {
			return fmt.Errorf("open layer %s: %w", layer.Digest, err)
		}
		raw, tooLarge, err := readBlobForCache(newProgressReader(rc, progressEvent{
			Stage:        "download_layer",
			Platform:     p.String(),
			CurrentLayer: layerIndex,
			TotalLayers:  totalLayers,
			BytesTotal:   layer.Size,
			Message:      "downloading layer",
		}, hooks), maxCachedBlobSize)
		closeErr := rc.Close()
		if err != nil {
			return fmt.Errorf("download layer %s: %w", layer.Digest, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
		}
		if tooLarge {
			return writeLayerStreamedToTar(tw, client, ref, p, layer, expectedDiffID, layerIndex, totalLayers, tarPath, hooks)
		}
		mediaType := normalizeMediaType(layer.MediaType)
		if mediaType == "" {
			mediaType = normalizeMediaType(contentType)
		}
		session.storeCachedBlob(layer.Digest, mediaType, raw)
		return writeLayerFromRawBlob(tw, raw, mediaType, p, layer, expectedDiffID, layerIndex, totalLayers, tarPath, hooks)
	}

	return writeLayerStreamedToTar(tw, client, ref, p, layer, expectedDiffID, layerIndex, totalLayers, tarPath, hooks)
}

func readBlobForCache(r io.Reader, limit int64) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		return nil, true, nil
	}
	return raw, false, nil
}

func writeLayerStreamedToTar(
	tw *tar.Writer,
	client *registryClient,
	ref imageRef,
	p platform,
	layer descriptor,
	expectedDiffID string,
	layerIndex int,
	totalLayers int,
	tarPath string,
	hooks *exportHooks,
) error {
	stream, err := openLayerStream(client, ref, layer)
	if err != nil {
		return fmt.Errorf("open layer %s: %w", layer.Digest, err)
	}

	if !stream.Decoded && layer.Size > 0 {
		reader, hasher, err := newDiffIDReader(stream.Reader, expectedDiffID)
		if err != nil {
			_ = stream.Reader.Close()
			return err
		}
		err = writeTarStream(tw, tarPath, newProgressReader(reader, progressEvent{
			Stage:        "write_layer",
			Platform:     p.String(),
			CurrentLayer: layerIndex,
			TotalLayers:  totalLayers,
			BytesTotal:   layer.Size,
			Message:      "writing layer",
		}, hooks), layer.Size, 0o644)
		closeErr := stream.Reader.Close()
		if err != nil {
			return fmt.Errorf("stream layer %s: %w", layer.Digest, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
		}
		if err := verifyDiffIDDigest(expectedDiffID, hasher.Sum(nil), layer.Digest); err != nil {
			return err
		}
		return nil
	}

	tracker := newRateTracker(0)
	uncompressedSize, err := io.Copy(io.Discard, newProgressReaderWithTracker(stream.Reader, progressEvent{
		Stage:        "measure_layer",
		Platform:     p.String(),
		CurrentLayer: layerIndex,
		TotalLayers:  totalLayers,
		Message:      "measuring layer",
	}, hooks, tracker))
	closeErr := stream.Reader.Close()
	if err != nil {
		return fmt.Errorf("measure layer %s: %w", layer.Digest, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
	}

	stream, err = openLayerStream(client, ref, layer)
	if err != nil {
		return fmt.Errorf("re-open layer %s: %w", layer.Digest, err)
	}
	reader, hasher, err := newDiffIDReader(stream.Reader, expectedDiffID)
	if err != nil {
		_ = stream.Reader.Close()
		return err
	}
	err = writeTarStream(tw, tarPath, newProgressReader(reader, progressEvent{
		Stage:        "write_layer",
		Platform:     p.String(),
		CurrentLayer: layerIndex,
		TotalLayers:  totalLayers,
		BytesTotal:   uncompressedSize,
		Message:      "writing layer",
	}, hooks), uncompressedSize, 0o644)
	closeErr = stream.Reader.Close()
	if err != nil {
		return fmt.Errorf("stream layer %s: %w", layer.Digest, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
	}
	if err := verifyDiffIDDigest(expectedDiffID, hasher.Sum(nil), layer.Digest); err != nil {
		return err
	}
	return nil
}

func writeLayerFromRawBlob(
	tw *tar.Writer,
	raw []byte,
	mediaType string,
	p platform,
	layer descriptor,
	expectedDiffID string,
	layerIndex int,
	totalLayers int,
	tarPath string,
	hooks *exportHooks,
) error {
	stream, err := openLayerStreamFromRaw(raw, mediaType, layer)
	if err != nil {
		return fmt.Errorf("open cached layer %s: %w", layer.Digest, err)
	}

	if !stream.Decoded {
		if layer.Size > 0 && int64(len(raw)) != layer.Size {
			_ = stream.Reader.Close()
			return fmt.Errorf("cached layer %s size mismatch: got %d want %d", layer.Digest, len(raw), layer.Size)
		}
		reader, hasher, err := newDiffIDReader(stream.Reader, expectedDiffID)
		if err != nil {
			_ = stream.Reader.Close()
			return err
		}
		err = writeTarStream(tw, tarPath, newProgressReader(reader, progressEvent{
			Stage:        "write_layer",
			Platform:     p.String(),
			CurrentLayer: layerIndex,
			TotalLayers:  totalLayers,
			BytesTotal:   int64(len(raw)),
			Message:      "writing cached layer",
		}, hooks), int64(len(raw)), 0o644)
		closeErr := stream.Reader.Close()
		if err != nil {
			return err
		}
		if err := verifyDiffIDDigest(expectedDiffID, hasher.Sum(nil), layer.Digest); err != nil {
			return err
		}
		return closeErr
	}

	uncompressedSize, err := io.Copy(io.Discard, newProgressReader(stream.Reader, progressEvent{
		Stage:        "measure_layer",
		Platform:     p.String(),
		CurrentLayer: layerIndex,
		TotalLayers:  totalLayers,
		Message:      "measuring cached layer",
	}, hooks))
	closeErr := stream.Reader.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	stream, err = openLayerStreamFromRaw(raw, mediaType, layer)
	if err != nil {
		return err
	}
	reader, hasher, err := newDiffIDReader(stream.Reader, expectedDiffID)
	if err != nil {
		_ = stream.Reader.Close()
		return err
	}
	err = writeTarStream(tw, tarPath, newProgressReader(reader, progressEvent{
		Stage:        "write_layer",
		Platform:     p.String(),
		CurrentLayer: layerIndex,
		TotalLayers:  totalLayers,
		BytesTotal:   uncompressedSize,
		Message:      "writing cached layer",
	}, hooks), uncompressedSize, 0o644)
	closeErr = stream.Reader.Close()
	if err != nil {
		return err
	}
	if err := verifyDiffIDDigest(expectedDiffID, hasher.Sum(nil), layer.Digest); err != nil {
		return err
	}
	return closeErr
}

type layerStream struct {
	Reader  io.ReadCloser
	Decoded bool
}

func openLayerStream(client *registryClient, ref imageRef, layer descriptor) (layerStream, error) {
	rc, contentType, err := client.openBlobDescriptor(ref, layer)
	if err != nil {
		return layerStream{}, err
	}
	return openLayerStreamFromReader(rc, normalizeMediaType(contentType), layer)
}

func openLayerStreamFromRaw(raw []byte, mediaType string, layer descriptor) (layerStream, error) {
	return openLayerStreamFromReader(io.NopCloser(bytes.NewReader(raw)), mediaType, layer)
}

func openLayerStreamFromReader(rc io.ReadCloser, contentType string, layer descriptor) (layerStream, error) {
	mediaType := normalizeMediaType(layer.MediaType)
	if mediaType == "" {
		mediaType = normalizeMediaType(contentType)
	}

	switch mediaType {
	case mtOCILayerTarZstd:
		return openZstdLayerStream(rc)
	case mtDockerLayerGzip, mtOCILayerTarGzip, mtDockerForeignLayerGz:
		return openGzipLayerStream(rc)
	case mtDockerLayerTar, mtOCILayerTar, mtDockerForeignLayer:
		return layerStream{Reader: rc, Decoded: false}, nil
	}

	if strings.Contains(mediaType, "gzip") {
		return openGzipLayerStream(rc)
	}

	// Media type can be omitted on some registries. Detect compression magic as fallback.
	buffered := bufio.NewReader(rc)
	if magic, err := buffered.Peek(4); err == nil && len(magic) == 4 &&
		magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd {
		decoder, err := zstd.NewReader(buffered)
		if err != nil {
			_ = rc.Close()
			return layerStream{}, fmt.Errorf("create zstd reader: %w", err)
		}
		return layerStream{Reader: &stackedReadCloser{Reader: decoder, closers: []io.Closer{zstdCloser{decoder}, rc}}, Decoded: true}, nil
	}
	if magic, err := buffered.Peek(2); err == nil && len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			_ = rc.Close()
			return layerStream{}, fmt.Errorf("create gzip reader: %w", err)
		}
		return layerStream{Reader: &stackedReadCloser{Reader: gz, closers: []io.Closer{gz, rc}}, Decoded: true}, nil
	}

	return layerStream{Reader: &readerWithCloser{Reader: buffered, Closer: rc}, Decoded: false}, nil
}

func openGzipLayerStream(rc io.ReadCloser) (layerStream, error) {
	gz, err := gzip.NewReader(rc)
	if err != nil {
		_ = rc.Close()
		return layerStream{}, fmt.Errorf("create gzip reader: %w", err)
	}
	return layerStream{Reader: &stackedReadCloser{Reader: gz, closers: []io.Closer{gz, rc}}, Decoded: true}, nil
}

func openZstdLayerStream(rc io.ReadCloser) (layerStream, error) {
	decoder, err := zstd.NewReader(rc)
	if err != nil {
		_ = rc.Close()
		return layerStream{}, fmt.Errorf("create zstd reader: %w", err)
	}
	return layerStream{Reader: &stackedReadCloser{Reader: decoder, closers: []io.Closer{zstdCloser{decoder}, rc}}, Decoded: true}, nil
}

type stackedReadCloser struct {
	io.Reader
	closers []io.Closer
}

type zstdCloser struct {
	decoder *zstd.Decoder
}

func (z zstdCloser) Close() error {
	z.decoder.Close()
	return nil
}

func (s *stackedReadCloser) Close() error {
	var firstErr error
	for i := len(s.closers) - 1; i >= 0; i-- {
		if err := s.closers[i].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type readerWithCloser struct {
	io.Reader
	io.Closer
}

func outputRepoAndTag(ref imageRef, p platform, addPlatform bool) (string, string) {
	repoName := ref.DisplayRepository()
	tag := ref.Tag
	if tag == "" {
		tag = "latest"
	}
	if addPlatform {
		suffix := sanitizeTag(p.OS + "-" + p.Architecture)
		if p.Variant != "" {
			suffix += "-" + sanitizeTag(p.Variant)
		}
		tag = sanitizeTag(tag + "-" + suffix)
	}
	return repoName, sanitizeTag(tag)
}

func extractDiffIDs(configJSON []byte) []string {
	var cfg struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil
	}
	return cfg.RootFS.DiffIDs
}

func layerIDFrom(layer descriptor, diffIDs []string, layerIndex int) string {
	if layerIndex < len(diffIDs) {
		if hex := digestHex(diffIDs[layerIndex]); hex != "" {
			return hex
		}
	}
	hex := digestHex(layer.Digest)
	if hex == "" {
		hex = fmt.Sprintf("layer-%d", layerIndex)
	}
	return hex
}

func digestHex(digest string) string {
	if digest == "" {
		return ""
	}
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(parts[0])
}

func verifyDiffIDDigest(expected string, gotSum []byte, layerDigest string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	parts := strings.SplitN(expected, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("layer %s diffID is invalid: %s", layerDigest, expected)
	}
	got := strings.ToLower(parts[0]) + ":" + hex.EncodeToString(gotSum)
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("layer %s diffID mismatch: got %s want %s", layerDigest, got, expected)
	}
	return nil
}

func newDiffIDReader(reader io.Reader, expected string) (io.Reader, hash.Hash, error) {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return reader, nil, nil
	}
	hasher, _, err := newDigestHasher(expected)
	if err != nil {
		return nil, nil, fmt.Errorf("create diffID hasher for %s: %w", expected, err)
	}
	return io.TeeReader(reader, hasher), hasher, nil
}

func sanitizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.ReplaceAll(tag, "/", "-")
	tag = strings.ReplaceAll(tag, ":", "-")
	tag = strings.ReplaceAll(tag, " ", "-")
	if tag == "" {
		return "latest"
	}
	return tag
}

func writeTarDir(tw *tar.Writer, dir string) error {
	dir = strings.TrimPrefix(dir, "/")
	hdr := &tar.Header{
		Name:     dir,
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now().UTC(),
	}
	return tw.WriteHeader(hdr)
}

func writeTarBytes(tw *tar.Writer, name string, data []byte, mode int64) error {
	name = strings.TrimPrefix(name, "/")
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    int64(len(data)),
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	return nil
}

func writeTarStream(tw *tar.Writer, name string, r io.Reader, size int64, mode int64) error {
	name = strings.TrimPrefix(name, "/")
	if size < 0 {
		return fmt.Errorf("invalid size %d for tar entry %s", size, name)
	}
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    size,
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	written, err := io.Copy(tw, r)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("tar entry %s size mismatch: header=%d written=%d", name, size, written)
	}
	return nil
}

func normalizeMediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	if idx := strings.Index(contentType, ";"); idx > -1 {
		contentType = contentType[:idx]
	}
	return strings.TrimSpace(contentType)
}

func isUnknownPlatform(p platform) bool {
	osName := strings.ToLower(strings.TrimSpace(p.OS))
	arch := strings.ToLower(strings.TrimSpace(p.Architecture))
	return osName == "" || arch == "" || osName == "unknown" || arch == "unknown"
}

type saveResult struct {
	AbsPath string
	Size    int64
}

func summarizeSavedFile(path string) (saveResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return saveResult{}, fmt.Errorf("resolve absolute output path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return saveResult{}, fmt.Errorf("stat output file: %w", err)
	}
	return saveResult{AbsPath: abs, Size: info.Size()}, nil
}

func validateDockerArchive(path string) error {
	return validateDockerArchiveWithExpectation(path, archiveValidationExpectation{})
}

func validateDockerArchiveWithExpectation(path string, expectation archiveValidationExpectation) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive for validation: %w", err)
	}
	defer file.Close()

	tr := tar.NewReader(file)
	regularFiles := make(map[string][]byte)
	layerFiles := make(map[string]struct{})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return fmt.Errorf("skip archive entry %s: %w", name, err)
			}
			continue
		}

		switch {
		case name == "manifest.json" || name == "repositories" || isTopLevelConfigJSON(name):
			data, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read archive entry %s: %w", name, err)
			}
			regularFiles[name] = data
		case strings.HasSuffix(name, "/layer.tar"):
			if err := validateLayerTarPayload(name, tr); err != nil {
				return err
			}
			layerFiles[name] = struct{}{}
		default:
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return fmt.Errorf("drain archive entry %s: %w", name, err)
			}
		}
	}

	return validateDockerArchiveContents(regularFiles, layerFiles, expectation)
}

func validateDockerArchiveContents(regularFiles map[string][]byte, layerFiles map[string]struct{}, expectation archiveValidationExpectation) error {
	manifestData, ok := regularFiles["manifest.json"]
	if !ok {
		return fmt.Errorf("archive missing manifest.json")
	}
	repositoriesData, ok := regularFiles["repositories"]
	if !ok {
		return fmt.Errorf("archive missing repositories")
	}
	var manifestEntries []saveManifestEntry
	if err := json.Unmarshal(manifestData, &manifestEntries); err != nil {
		return fmt.Errorf("parse manifest.json: %w", err)
	}
	if len(manifestEntries) == 0 {
		return fmt.Errorf("manifest.json contains no entries")
	}
	var repositories map[string]map[string]string
	if err := json.Unmarshal(repositoriesData, &repositories); err != nil {
		return fmt.Errorf("parse repositories: %w", err)
	}
	for _, entry := range manifestEntries {
		if entry.Config == "" {
			return fmt.Errorf("manifest entry missing config path")
		}
		configData, ok := regularFiles[entry.Config]
		if !ok {
			return fmt.Errorf("manifest references missing config %s", entry.Config)
		}
		if !json.Valid(configData) {
			return fmt.Errorf("config %s is not valid JSON", entry.Config)
		}
		if err := validateConfigFilename(entry.Config, configData, expectation.ConfigDigests[entry.Config]); err != nil {
			return err
		}
		if len(entry.RepoTags) == 0 {
			return fmt.Errorf("manifest entry for %s has no repo tags", entry.Config)
		}
		if len(entry.Layers) == 0 {
			return fmt.Errorf("manifest entry for %s has no layers", entry.Config)
		}
		for _, layerPath := range entry.Layers {
			if _, ok := layerFiles[layerPath]; !ok {
				return fmt.Errorf("manifest references missing layer %s", layerPath)
			}
		}
		lastLayerID := layerDirectoryID(entry.Layers[len(entry.Layers)-1])
		for _, repoTag := range entry.RepoTags {
			repoName, tag, ok := splitRepoTag(repoTag)
			if !ok || repoName == "" || tag == "" {
				return fmt.Errorf("invalid repo tag %q", repoTag)
			}
			if repositories[repoName] == nil {
				return fmt.Errorf("repositories missing repo %s", repoName)
			}
			if repositories[repoName][tag] != lastLayerID {
				return fmt.Errorf("repositories entry mismatch for %s:%s", repoName, tag)
			}
		}
	}
	return nil
}

func validateLayerTarPayload(name string, r io.Reader) error {
	layerTar := tar.NewReader(r)
	for {
		_, err := layerTar.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid layer tar %s: %w", name, err)
		}
	}
}

func validateConfigFilename(name string, configData []byte, expectedDigest string) error {
	expectedDigest = strings.TrimSpace(expectedDigest)
	if expectedDigest != "" {
		expectedName := digestHex(expectedDigest) + ".json"
		if name != expectedName {
			return fmt.Errorf("config filename mismatch: got %s want %s", name, expectedName)
		}
		if err := verifyPayloadDigest(configData, expectedDigest, "config"); err != nil {
			return err
		}
		return nil
	}

	configSum := sha256.Sum256(configData)
	expectedName := hex.EncodeToString(configSum[:]) + ".json"
	if name != expectedName {
		return fmt.Errorf("config filename mismatch: got %s want %s", name, expectedName)
	}
	return nil
}

func splitRepoTag(repoTag string) (string, string, bool) {
	idx := strings.LastIndex(repoTag, ":")
	if idx <= 0 || idx == len(repoTag)-1 {
		return "", "", false
	}
	return repoTag[:idx], repoTag[idx+1:], true
}

func isTopLevelConfigJSON(name string) bool {
	return !strings.Contains(name, "/") && strings.HasSuffix(name, ".json") && name != "manifest.json"
}

func layerDirectoryID(layerPath string) string {
	layerPath = strings.TrimSuffix(strings.TrimSpace(layerPath), "/")
	return filepath.Base(filepath.Dir(layerPath))
}

func sizeMiB(size int64) float64 {
	return float64(size) / (1024.0 * 1024.0)
}

func printExportReport(out io.Writer, report exportReport) {
	logf(out, "Status: SUCCESS\n")
	if len(report.Archives) == 1 {
		item := report.Archives[0]
		logf(out, "Output: %s\n", item.Result.AbsPath)
		logf(out, "Size: %d bytes (%.2f MiB)\n", item.Result.Size, sizeMiB(item.Result.Size))
		logf(out, "Platform: %s\n", item.Platform.String())
	} else {
		logf(out, "Archives (%d):\n", len(report.Archives))
		for _, item := range report.Archives {
			logf(out, "- %s -> %s (%d bytes, %.2f MiB)\n",
				item.Platform.String(), item.Result.AbsPath, item.Result.Size, sizeMiB(item.Result.Size))
		}
	}
	if report.IndexPath != "" {
		logf(out, "Platform index: %s\n", report.IndexPath)
	}
	logf(out, "\nManual Docker load:\n")
	for _, item := range report.Archives {
		logf(out, "docker load -i %s\n", shellQuotePath(item.Result.AbsPath))
	}
	logf(out, "docker image ls\n")
}

func newTextProgressSink(out io.Writer) func(progressEvent) {
	if out == nil {
		return nil
	}
	var (
		mu        sync.Mutex
		lastStage string
		lastLayer string
		lastPrint time.Time
	)
	return func(event progressEvent) {
		if event.Stage == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()

		key := fmt.Sprintf("%s|%s|%d/%d", event.Stage, event.Platform, event.CurrentLayer, event.TotalLayers)
		now := time.Now()
		if key == lastLayer && now.Sub(lastPrint) < 900*time.Millisecond && !event.Done && event.Error == "" {
			return
		}
		lastLayer = key
		lastPrint = now

		if event.Stage != lastStage {
			lastStage = event.Stage
		}
		if event.BytesTotal > 0 {
			logf(out, "Progress [%s %s %d/%d]: %s / %s", event.Stage, event.Platform, event.CurrentLayer, event.TotalLayers, formatBytes(event.BytesDone), formatBytes(event.BytesTotal))
		} else {
			logf(out, "Progress [%s %s %d/%d]: %s", event.Stage, event.Platform, event.CurrentLayer, event.TotalLayers, formatBytes(event.BytesDone))
		}
		if event.SpeedBPS > 0 {
			logf(out, " at %s/s", formatBytes(int64(event.SpeedBPS)))
		}
		if event.ETASeconds > 0 {
			logf(out, ", ETA %ds", event.ETASeconds)
		}
		if event.Message != "" {
			logf(out, " (%s)", event.Message)
		}
		logf(out, "\n")
	}
}

type rateTracker struct {
	start time.Time
	total int64
}

func newRateTracker(total int64) rateTracker {
	return rateTracker{start: time.Now(), total: total}
}

func (r rateTracker) snapshot(done int64) (float64, int64) {
	elapsed := time.Since(r.start).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	speed := float64(done) / elapsed
	if speed <= 0 || r.total <= 0 || done <= 0 || done >= r.total {
		return speed, 0
	}
	return speed, int64(float64(r.total-done) / speed)
}

type progressReader struct {
	reader    io.Reader
	hooks     *exportHooks
	template  progressEvent
	tracker   rateTracker
	lastEmit  time.Time
	bytesDone int64
	emitFinal bool
}

func newProgressReader(reader io.Reader, template progressEvent, hooks *exportHooks) *progressReader {
	return newProgressReaderWithTracker(reader, template, hooks, newRateTracker(template.BytesTotal))
}

func newProgressReaderWithTracker(reader io.Reader, template progressEvent, hooks *exportHooks, tracker rateTracker) *progressReader {
	return &progressReader{
		reader:    reader,
		hooks:     hooks,
		template:  template,
		tracker:   tracker,
		emitFinal: true,
	}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	if n > 0 {
		p.bytesDone += int64(n)
	}
	now := time.Now()
	if n > 0 && (p.lastEmit.IsZero() || now.Sub(p.lastEmit) >= 250*time.Millisecond) {
		p.emit(false)
		p.lastEmit = now
	}
	if err == io.EOF && p.emitFinal {
		p.emit(true)
		p.emitFinal = false
	}
	return n, err
}

func (p *progressReader) emit(done bool) {
	event := p.template
	event.BytesDone = p.bytesDone
	event.Done = done
	speed, eta := p.tracker.snapshot(p.bytesDone)
	event.SpeedBPS = speed
	event.ETASeconds = eta
	p.hooks.emit(event)
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func shellQuotePath(path string) string {
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func perPlatformOutputs(outputBase string, platforms []platformOption, selected []int) ([]string, error) {
	if len(selected) == 1 {
		return []string{filepath.Clean(outputBase)}, nil
	}
	used := make(map[string]int)
	outputs := make([]string, 0, len(selected))
	for _, idx := range selected {
		if idx < 0 || idx >= len(platforms) {
			return nil, fmt.Errorf("selected index %d out of range", idx+1)
		}
		candidate := derivePlatformOutputPath(outputBase, platforms[idx].Platform)
		used[candidate]++
		if used[candidate] > 1 {
			candidate = addNumericSuffix(candidate, used[candidate]-1)
		}
		outputs = append(outputs, candidate)
	}
	return outputs, nil
}

func derivePlatformOutputPath(outputBase string, p platform) string {
	clean := filepath.Clean(outputBase)
	ext := filepath.Ext(clean)
	if ext == "" {
		ext = ".tar"
		clean = clean + ext
	}
	prefix := strings.TrimSuffix(clean, ext)
	return prefix + "_" + platformSuffix(p) + ext
}

func platformSuffix(p platform) string {
	osName := sanitizeComponent(strings.TrimSpace(p.OS))
	arch := sanitizeComponent(strings.TrimSpace(p.Architecture))
	variant := sanitizeComponent(strings.TrimSpace(p.Variant))
	if osName == "" {
		osName = "unknownos"
	}
	if arch == "" {
		arch = "unknownarch"
	}
	suffix := osName + "_" + arch
	if variant != "" {
		suffix += "_" + variant
	}
	return suffix
}

func sanitizeComponent(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, ":", "-")
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ToLower(value)
	return value
}

func addNumericSuffix(path string, n int) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s_%d%s", base, n, ext)
}

func platformIndexPath(outputBase string) string {
	clean := filepath.Clean(outputBase)
	ext := filepath.Ext(clean)
	if ext == "" {
		return clean + "_platforms.json"
	}
	return strings.TrimSuffix(clean, ext) + "_platforms.json"
}

func writePlatformIndexFile(outputBase, image string, archives []exportedArchive) (string, error) {
	indexPath := platformIndexPath(outputBase)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return "", fmt.Errorf("create platform index directory: %w", err)
	}
	records := make([]platformIndexFileItem, 0, len(archives))
	for _, archive := range archives {
		records = append(records, platformIndexFileItem{
			OS:           archive.Platform.OS,
			Architecture: archive.Platform.Architecture,
			Variant:      archive.Platform.Variant,
			TarPath:      archive.Result.AbsPath,
			SizeBytes:    archive.Result.Size,
		})
	}
	payload := platformIndexFile{
		Image:       image,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Archives:    records,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode platform index: %w", err)
	}
	if err := os.WriteFile(indexPath, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write platform index file: %w", err)
	}
	abs, err := filepath.Abs(indexPath)
	if err != nil {
		return "", fmt.Errorf("resolve platform index absolute path: %w", err)
	}
	return abs, nil
}

func selectPlatforms(selection string, total int) ([]int, error) {
	if total <= 0 {
		return nil, fmt.Errorf("no architectures available")
	}
	selection = strings.TrimSpace(selection)
	if selection == "" || strings.EqualFold(selection, "all") {
		all := make([]int, total)
		for i := 0; i < total; i++ {
			all[i] = i
		}
		return all, nil
	}

	selected := make(map[int]bool)
	for _, token := range strings.Split(selection, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if strings.Contains(token, "-") {
			parts := strings.SplitN(token, "-", 2)
			start := 1
			end := total
			if strings.TrimSpace(parts[0]) != "" {
				v, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err != nil {
					return nil, fmt.Errorf("invalid range start %q", token)
				}
				start = v
			}
			if strings.TrimSpace(parts[1]) != "" {
				v, err := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil {
					return nil, fmt.Errorf("invalid range end %q", token)
				}
				end = v
			}
			if start < 1 || end < 1 || start > total {
				return nil, fmt.Errorf("range %q out of bounds", token)
			}
			if end > total {
				end = total
			}
			if start > end {
				return nil, fmt.Errorf("invalid descending range %q", token)
			}
			for i := start; i <= end; i++ {
				selected[i-1] = true
			}
			continue
		}
		v, err := strconv.Atoi(token)
		if err != nil {
			return nil, fmt.Errorf("invalid architecture index %q", token)
		}
		if v < 1 || v > total {
			return nil, fmt.Errorf("architecture index %d out of bounds (1-%d)", v, total)
		}
		selected[v-1] = true
	}

	indexes := make([]int, 0, len(selected))
	for idx := range selected {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	return indexes, nil
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format, args...)
}
