package main

import (
	"archive/tar"
	"bytes"
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
	logf(opts.Stdout, "Saved: %s\n", opts.Output)
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
				layerBytes, err := fetchLayerTarBytes(client, ref, layer)
				if err != nil {
					return fmt.Errorf("download layer %s: %w", layer.Digest, err)
				}
				if err := writeTarBytes(tw, layerTarPath, layerBytes, 0o644); err != nil {
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

func fetchLayerTarBytes(client *registryClient, ref imageRef, layer descriptor) ([]byte, error) {
	rc, contentType, err := client.openBlob(ref, layer.Digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	mediaType := layer.MediaType
	if mediaType == "" {
		mediaType = normalizeMediaType(contentType)
	}
	mediaType = normalizeMediaType(mediaType)

	var reader io.Reader = rc
	switch mediaType {
	case mtDockerLayerGzip, mtOCILayerTarGzip, mtDockerForeignLayerGz:
		gz, err := gzip.NewReader(rc)
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	case mtDockerLayerTar, mtOCILayerTar, mtDockerForeignLayer, "":
		reader = rc
	case mtOCILayerTarZstd:
		return nil, fmt.Errorf("zstd-compressed layers are not supported in this build")
	default:
		if strings.Contains(mediaType, "gzip") {
			gz, err := gzip.NewReader(rc)
			if err != nil {
				return nil, fmt.Errorf("create gzip reader: %w", err)
			}
			defer gz.Close()
			reader = gz
		}
	}

	buf := bytes.NewBuffer(nil)
	if _, err := io.Copy(buf, reader); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
