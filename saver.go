package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
}

type saveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func runNonInteractive(opts runOptions) error {
	ref, err := parseImageRef(opts.Image)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(opts.Output)), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
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
	logf(opts.Stdout, "Image: %s\n", ref.DisplayTag())
	for _, idx := range selected {
		logf(opts.Stdout, "Selected [%d] %s\n", idx+1, platforms[idx].Platform.String())
	}
	if err := writeDockerTar(client, ref, singleManifest, platforms, selected, opts.Output, opts.Stdout); err != nil {
		return err
	}
	result, err := summarizeSavedFile(opts.Output)
	if err != nil {
		return err
	}
	printSaveResult(opts.Stdout, result)
	return nil
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
	ref imageRef,
	singleManifest *imageManifest,
	platforms []platformOption,
	selected []int,
	outputPath string,
	logOut io.Writer,
) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output tar: %w", err)
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
			return fmt.Errorf("selected index %d out of range", idx+1)
		}
		p := platforms[idx]
		logf(logOut, "[%d/%d] Resolving manifest for %s\n", i+1, len(selected), p.Platform.String())

		var manifest imageManifest
		if singleManifest != nil {
			manifest = *singleManifest
		} else {
			data, _, err := client.getManifest(ref, p.ManifestRef)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return fmt.Errorf("parse platform manifest (%s): %w", p.ManifestRef, err)
			}
		}
		if len(manifest.Layers) == 0 {
			return fmt.Errorf("manifest for %s has no layers", p.Platform.String())
		}

		configDigest := manifest.Config.Digest
		if configDigest == "" {
			return fmt.Errorf("manifest for %s missing config digest", p.Platform.String())
		}
		configName := digestHex(configDigest) + ".json"
		configBytes, _, err := client.getBlob(ref, configDigest)
		if err != nil {
			return fmt.Errorf("download config %s: %w", configDigest, err)
		}
		if !writtenConfig[configName] {
			if err := writeTarBytes(tw, configName, configBytes, 0o644); err != nil {
				return err
			}
			writtenConfig[configName] = true
		}

		diffIDs := extractDiffIDs(configBytes)
		layerPaths := make([]string, 0, len(manifest.Layers))
		parentID := ""
		for li, layer := range manifest.Layers {
			layerID := layerIDFrom(layer, diffIDs, li)
			layerDir := layerID + "/"
			layerTarPath := layerDir + "layer.tar"
			if !writtenDirs[layerDir] {
				if err := writeTarDir(tw, layerDir); err != nil {
					return err
				}
				writtenDirs[layerDir] = true
			}
			if !writtenLayers[layerID] {
				if err := writeTarBytes(tw, layerDir+"VERSION", []byte("1.0\n"), 0o644); err != nil {
					return err
				}
				meta := map[string]string{"id": layerID}
				if parentID != "" {
					meta["parent"] = parentID
				}
				metaJSON, _ := json.Marshal(meta)
				if err := writeTarBytes(tw, layerDir+"json", metaJSON, 0o644); err != nil {
					return err
				}

				logf(logOut, "Downloading layer %d/%d for %s\n", li+1, len(manifest.Layers), p.Platform.String())
				if err := writeLayerToTar(tw, client, ref, layer, layerTarPath); err != nil {
					return err
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
		return fmt.Errorf("encode manifest.json: %w", err)
	}
	if err := writeTarBytes(tw, "manifest.json", manifestJSON, 0o644); err != nil {
		return err
	}
	repoJSON, err := json.Marshal(repos)
	if err != nil {
		return fmt.Errorf("encode repositories: %w", err)
	}
	if err := writeTarBytes(tw, "repositories", repoJSON, 0o644); err != nil {
		return err
	}

	return nil
}

func writeLayerToTar(tw *tar.Writer, client *registryClient, ref imageRef, layer descriptor, tarPath string) error {
	stream, err := openLayerStream(client, ref, layer)
	if err != nil {
		return fmt.Errorf("open layer %s: %w", layer.Digest, err)
	}

	if !stream.Decoded && layer.Size > 0 {
		err = writeTarStream(tw, tarPath, stream.Reader, layer.Size, 0o644)
		closeErr := stream.Reader.Close()
		if err != nil {
			return fmt.Errorf("stream layer %s: %w", layer.Digest, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
		}
		return nil
	}

	uncompressedSize, err := io.Copy(io.Discard, stream.Reader)
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
	err = writeTarStream(tw, tarPath, stream.Reader, uncompressedSize, 0o644)
	closeErr = stream.Reader.Close()
	if err != nil {
		return fmt.Errorf("stream layer %s: %w", layer.Digest, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close layer stream %s: %w", layer.Digest, closeErr)
	}
	return nil
}

type layerStream struct {
	Reader  io.ReadCloser
	Decoded bool
}

func openLayerStream(client *registryClient, ref imageRef, layer descriptor) (layerStream, error) {
	rc, contentType, err := client.openBlob(ref, layer.Digest)
	if err != nil {
		return layerStream{}, err
	}

	mediaType := normalizeMediaType(layer.MediaType)
	if mediaType == "" {
		mediaType = normalizeMediaType(contentType)
	}

	switch mediaType {
	case mtOCILayerTarZstd:
		_ = rc.Close()
		return layerStream{}, fmt.Errorf("zstd-compressed layers are not supported in this build")
	case mtDockerLayerGzip, mtOCILayerTarGzip, mtDockerForeignLayerGz:
		return openGzipLayerStream(rc)
	case mtDockerLayerTar, mtOCILayerTar, mtDockerForeignLayer:
		return layerStream{Reader: rc, Decoded: false}, nil
	}

	if strings.Contains(mediaType, "gzip") {
		return openGzipLayerStream(rc)
	}

	// Media type can be omitted on some registries. Detect gzip magic as fallback.
	buffered := bufio.NewReader(rc)
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

type stackedReadCloser struct {
	io.Reader
	closers []io.Closer
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

func printSaveResult(out io.Writer, result saveResult) {
	mb := float64(result.Size) / (1024.0 * 1024.0)
	logf(out, "Status: SUCCESS\n")
	logf(out, "Output: %s\n", result.AbsPath)
	logf(out, "Size: %d bytes (%.2f MiB)\n", result.Size, mb)
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
