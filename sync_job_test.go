package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSkopeoEnvironmentOverridesInheritedProxy(t *testing.T) {
	env := skopeoEnvironment([]string{
		"PATH=/test/bin",
		"HTTP_PROXY=http://old.example.test:8080",
		"https_proxy=socks5://old.example.test:1080",
		"NO_PROXY=registry.example.test",
	}, "socks5h://127.0.0.1:7897")
	if !slices.Contains(env, "PATH=/test/bin") {
		t.Fatal("unrelated environment entry was removed")
	}
	for _, entry := range env {
		if strings.Contains(entry, "old.example.test") || strings.HasPrefix(strings.ToUpper(entry), "NO_PROXY=") {
			t.Fatalf("inherited proxy routing leaked into explicit skopeo environment: %q", entry)
		}
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if !slices.Contains(env, name+"=socks5h://127.0.0.1:7897") {
			t.Fatalf("missing explicit proxy variable %s", name)
		}
	}
}

func TestSkopeoEnvironmentPreservesInheritedProxyWithoutOverride(t *testing.T) {
	base := []string{"HTTP_PROXY=http://inherited.example.test:8080", "PATH=/test/bin"}
	got := skopeoEnvironment(base, "")
	if !slices.Equal(got, base) {
		t.Fatalf("unexpected inherited environment: got %v want %v", got, base)
	}
}

func TestLimitedBufferRetainsOnlyOutputTail(t *testing.T) {
	buffer := &limitedBuffer{limit: 5}
	if _, err := buffer.Write([]byte("abcdef")); err != nil {
		t.Fatalf("write first output: %v", err)
	}
	if _, err := buffer.Write([]byte("gh")); err != nil {
		t.Fatalf("write second output: %v", err)
	}
	if got := buffer.String(); got != "defgh" {
		t.Fatalf("unexpected retained output: %q", got)
	}
}

func TestSyncProxyCompatibilityRequiresSameRouting(t *testing.T) {
	if !syncProxyCompatible("socks5h://127.0.0.1:1080", "socks5h://127.0.0.1:1080") {
		t.Fatal("matching proxies should be compatible")
	}
	if syncProxyCompatible("", "socks5h://127.0.0.1:1080") {
		t.Fatal("direct and proxied access must not share one skopeo process")
	}
}

func TestSkopeoRejectsDifferentAccountsForSameHost(t *testing.T) {
	err := validateSkopeoAccessCompatibility(
		registryAccess{Endpoint: "https://registry.example.test", Username: "reader", Password: "one"},
		registryAccess{Endpoint: "https://registry.example.test", Username: "writer", Password: "two"},
	)
	if err == nil || !strings.Contains(err.Error(), "two accounts") {
		t.Fatalf("unexpected compatibility result: %v", err)
	}
}

func TestSkopeoRejectsDifferentAccountsAcrossDockerHubAliases(t *testing.T) {
	err := validateSkopeoAccessCompatibility(
		registryAccess{Endpoint: "https://registry-1.docker.io", Username: "reader", Password: "one"},
		registryAccess{Endpoint: "https://docker.io", Username: "writer", Password: "two"},
	)
	if err == nil || !strings.Contains(err.Error(), "two accounts") {
		t.Fatalf("unexpected Docker Hub alias compatibility result: %v", err)
	}
}

func TestWriteSkopeoAuthFileIncludesDockerHubAliases(t *testing.T) {
	path, err := writeSkopeoAuthFile(t.TempDir(), registryAccess{
		Endpoint: "https://registry-1.docker.io", Username: "robot", Password: "secret",
	}, registryAccess{Endpoint: "https://target.example.test"})
	if err != nil {
		t.Fatalf("write skopeo auth file: %v", err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skopeo auth file: %v", err)
	}
	var payload struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode skopeo auth file: %v", err)
	}
	for _, key := range []string{"docker.io", "registry-1.docker.io", "index.docker.io", "https://index.docker.io/v1/"} {
		if payload.Auths[key]["auth"] == "" {
			t.Errorf("Docker Hub auth alias %q is missing", key)
		}
	}
}

func TestValidateLocalTarOutputNamesRejectsCollisions(t *testing.T) {
	root := t.TempDir()
	err := validateLocalTarOutputNames(root, []imageListItem{
		{Line: 1, Source: "team/app:v1", Target: "team/app:v1"},
		{Line: 2, Source: "mirror/app:v1", Target: "team/app:v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "lines 1 and 2") {
		t.Fatalf("unexpected collision result: %v", err)
	}
	if err := validateLocalTarOutputNames(root, []imageListItem{
		{Line: 1, Source: "team/app:v1", Target: "team/app:v1"},
		{Line: 2, Source: "team/worker:v1", Target: "team/worker:v1"},
	}); err != nil {
		t.Fatalf("distinct outputs rejected: %v", err)
	}
	name, _ := archiveOutputName("team/app:v1")
	if filepath.Dir(filepath.Join(root, name)) != root {
		t.Fatal("test output escaped root")
	}
}

func TestValidateLocalTarOutputNamesRejectsDigestTarget(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	err := validateLocalTarOutputNames(t.TempDir(), []imageListItem{{
		Line: 1, Source: "team/app@" + digest, Target: "archive/app@" + digest,
	}})
	if err == nil || !strings.Contains(err.Error(), "target must use a tag") {
		t.Fatalf("unexpected digest target validation result: %v", err)
	}
}

func TestExportImageToLocalTarUsesTargetRepoTag(t *testing.T) {
	var layer bytes.Buffer
	layerWriter := tar.NewWriter(&layer)
	if err := layerWriter.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: 5}); err != nil {
		t.Fatalf("write layer header: %v", err)
	}
	if _, err := layerWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("write layer contents: %v", err)
	}
	if err := layerWriter.Close(); err != nil {
		t.Fatalf("close layer: %v", err)
	}
	layerBytes := layer.Bytes()
	layerDigest := sha256Digest(layerBytes)

	configBytes, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{layerDigest},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDigest := sha256Digest(configBytes)

	manifestBytes, err := json.Marshal(imageManifest{
		SchemaVersion: 2,
		MediaType:     mtOCIManifestV1,
		Config: descriptor{
			MediaType: mtDockerConfigV1,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: []descriptor{{
			MediaType: mtOCILayerTar,
			Digest:    layerDigest,
			Size:      int64(len(layerBytes)),
		}},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := sha256Digest(manifestBytes)

	serverURL, closeServer := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/source/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIManifestV1)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestBytes)
		case "/v2/source/app/blobs/" + configDigest:
			w.Header().Set("Content-Type", mtDockerConfigV1)
			_, _ = w.Write(configBytes)
		case "/v2/source/app/blobs/" + layerDigest:
			w.Header().Set("Content-Type", mtOCILayerTar)
			_, _ = w.Write(layerBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeServer()

	report, err := exportImageToLocalTar(
		context.Background(),
		registryAccess{Endpoint: serverURL},
		"source/app:v1",
		"archive/renamed:v2",
		t.TempDir(),
		1,
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("export local tar: %v", err)
	}
	if len(report.Archives) != 1 {
		t.Fatalf("unexpected archive count: %d", len(report.Archives))
	}
	if got := report.Archives[0].Platform.String(); got != "linux/amd64" {
		t.Fatalf("single-manifest platform was not read from config: %q", got)
	}

	archiveFile, err := os.Open(report.Archives[0].Result.AbsPath)
	if err != nil {
		t.Fatalf("open output archive: %v", err)
	}
	defer archiveFile.Close()
	targetTag := ""
	tarReader := tar.NewReader(archiveFile)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read output archive: %v", err)
		}
		if header.Name != "manifest.json" {
			continue
		}
		var entries []saveManifestEntry
		if err := json.NewDecoder(tarReader).Decode(&entries); err != nil {
			t.Fatalf("decode manifest.json: %v", err)
		}
		if len(entries) != 1 || len(entries[0].RepoTags) != 1 {
			t.Fatalf("unexpected manifest entries: %+v", entries)
		}
		targetTag = entries[0].RepoTags[0]
		break
	}
	if targetTag != "archive/renamed:v2" {
		t.Fatalf("unexpected RepoTags entry: got %q", targetTag)
	}
}

func TestExportImageToLocalTarUsesPlatformSpecificRepoTags(t *testing.T) {
	var layer bytes.Buffer
	layerWriter := tar.NewWriter(&layer)
	if err := layerWriter.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: 5}); err != nil {
		t.Fatalf("write layer header: %v", err)
	}
	if _, err := layerWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("write layer contents: %v", err)
	}
	if err := layerWriter.Close(); err != nil {
		t.Fatalf("close layer: %v", err)
	}
	layerBytes := layer.Bytes()
	layerDigest := sha256Digest(layerBytes)

	platforms := []platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "linux", Architecture: "arm64", Variant: "v8"},
	}
	configs := make(map[string][]byte, len(platforms))
	manifests := make(map[string][]byte, len(platforms))
	manifestDescriptors := make([]descriptor, 0, len(platforms))
	for _, item := range platforms {
		configBytes, err := json.Marshal(map[string]any{
			"architecture": item.Architecture,
			"os":           item.OS,
			"variant":      item.Variant,
			"rootfs": map[string]any{
				"type":     "layers",
				"diff_ids": []string{layerDigest},
			},
		})
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		configDigest := sha256Digest(configBytes)
		configs[configDigest] = configBytes

		manifestBytes, err := json.Marshal(imageManifest{
			SchemaVersion: 2,
			MediaType:     mtOCIManifestV1,
			Config: descriptor{
				MediaType: mtDockerConfigV1,
				Digest:    configDigest,
				Size:      int64(len(configBytes)),
			},
			Layers: []descriptor{{
				MediaType: mtOCILayerTar,
				Digest:    layerDigest,
				Size:      int64(len(layerBytes)),
			}},
		})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		manifestDigest := sha256Digest(manifestBytes)
		manifests[manifestDigest] = manifestBytes
		manifestDescriptors = append(manifestDescriptors, descriptor{
			MediaType: mtOCIManifestV1,
			Digest:    manifestDigest,
			Size:      int64(len(manifestBytes)),
			Platform:  item,
		})
	}
	indexBytes, err := json.Marshal(manifestList{
		SchemaVersion: 2,
		MediaType:     mtOCIImageIndexV1,
		Manifests:     manifestDescriptors,
	})
	if err != nil {
		t.Fatalf("marshal image index: %v", err)
	}

	serverURL, closeServer := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const manifestPrefix = "/v2/source/app/manifests/"
		const blobPrefix = "/v2/source/app/blobs/"
		switch {
		case r.URL.Path == manifestPrefix+"v1":
			w.Header().Set("Content-Type", mtOCIImageIndexV1)
			_, _ = w.Write(indexBytes)
		case strings.HasPrefix(r.URL.Path, manifestPrefix):
			data, ok := manifests[strings.TrimPrefix(r.URL.Path, manifestPrefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", mtOCIManifestV1)
			_, _ = w.Write(data)
		case r.URL.Path == blobPrefix+layerDigest:
			w.Header().Set("Content-Type", mtOCILayerTar)
			_, _ = w.Write(layerBytes)
		case strings.HasPrefix(r.URL.Path, blobPrefix):
			data, ok := configs[strings.TrimPrefix(r.URL.Path, blobPrefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", mtDockerConfigV1)
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeServer()

	report, err := exportImageToLocalTar(
		context.Background(),
		registryAccess{Endpoint: serverURL},
		"source/app:v1",
		"archive/renamed:v2",
		t.TempDir(),
		1,
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("export multi-platform local tar: %v", err)
	}
	if len(report.Archives) != 2 {
		t.Fatalf("unexpected archive count: %d", len(report.Archives))
	}
	wantTags := map[string]string{
		"linux/amd64":    "archive/renamed:v2-linux-amd64",
		"linux/arm64/v8": "archive/renamed:v2-linux-arm64-v8",
	}
	for _, archive := range report.Archives {
		got := readArchiveRepoTag(t, archive.Result.AbsPath)
		want := wantTags[archive.Platform.String()]
		if got != want {
			t.Errorf("unexpected RepoTags entry for %s: got %q want %q", archive.Platform.String(), got, want)
		}
	}
}

func readArchiveRepoTag(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer file.Close()

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Name != "manifest.json" {
			continue
		}
		var entries []saveManifestEntry
		if err := json.NewDecoder(reader).Decode(&entries); err != nil {
			t.Fatalf("decode manifest.json: %v", err)
		}
		if len(entries) != 1 || len(entries[0].RepoTags) != 1 {
			t.Fatalf("unexpected manifest entries: %+v", entries)
		}
		return entries[0].RepoTags[0]
	}
	t.Fatal("archive did not contain manifest.json")
	return ""
}

func TestAutoEngineFallsBackToNativeWhenSkopeoFails(t *testing.T) {
	originalLookPath := syncExecutableLookPath
	originalSkopeoCopy := syncSkopeoCopy
	originalNativeCopy := syncNativeCopy
	t.Cleanup(func() {
		syncExecutableLookPath = originalLookPath
		syncSkopeoCopy = originalSkopeoCopy
		syncNativeCopy = originalNativeCopy
	})
	syncExecutableLookPath = func(string) (string, error) { return "/test/skopeo", nil }
	syncSkopeoCopy = func(context.Context, registryAccess, registryAccess, string, string, string, int, int, *syncHooks) (registryCopyResult, error) {
		return registryCopyResult{}, errors.New("synthetic skopeo failure")
	}
	nativeCalls := 0
	syncNativeCopy = func(_ context.Context, _, _ registryAccess, source, target string, _ *registryCopySession) (registryCopyResult, error) {
		nativeCalls++
		return registryCopyResult{Source: source, Target: target, Digest: "sha256:test"}, nil
	}

	result, err := runSyncJob(context.Background(), syncJobSpec{
		Source:     registryAccess{Endpoint: "https://source.example.test"},
		Target:     registryAccess{Endpoint: "https://target.example.test"},
		TargetType: syncTargetRegistry,
		Engine:     "auto",
		Workspace:  t.TempDir(),
		Images:     []imageListItem{{Line: 1, Source: "team/app:v1", Target: "archive/app:v1"}},
	}, nil)
	if err != nil {
		t.Fatalf("run sync job: %v", err)
	}
	if nativeCalls != 1 || result.Engine != "native-fallback" || result.Succeeded != 1 {
		t.Fatalf("unexpected fallback result: calls=%d result=%+v", nativeCalls, result)
	}
}
