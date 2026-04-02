package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPerPlatformOutputsSingleSelection(t *testing.T) {
	platforms := []platformOption{{Platform: platform{OS: "linux", Architecture: "amd64"}}}
	selected := []int{0}
	outputs, err := perPlatformOutputs("/tmp/image.tar", platforms, selected)
	if err != nil {
		t.Fatalf("perPlatformOutputs returned error: %v", err)
	}
	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}
	if outputs[0] != "/tmp/image.tar" {
		t.Fatalf("unexpected output path: %s", outputs[0])
	}
}

func TestPerPlatformOutputsMultiSelection(t *testing.T) {
	platforms := []platformOption{
		{Platform: platform{OS: "linux", Architecture: "amd64"}},
		{Platform: platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}
	selected := []int{0, 1}
	outputs, err := perPlatformOutputs("/tmp/image.tar", platforms, selected)
	if err != nil {
		t.Fatalf("perPlatformOutputs returned error: %v", err)
	}
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(outputs))
	}
	if outputs[0] != "/tmp/image_linux_amd64.tar" {
		t.Fatalf("unexpected first output path: %s", outputs[0])
	}
	if outputs[1] != "/tmp/image_linux_arm64_v8.tar" {
		t.Fatalf("unexpected second output path: %s", outputs[1])
	}
}

func TestOpenLayerStreamFromRawZstd(t *testing.T) {
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("new zstd writer: %v", err)
	}
	original := []byte("hello from zstd layer")
	if _, err := encoder.Write(original); err != nil {
		t.Fatalf("write zstd payload: %v", err)
	}
	encoder.Close()

	stream, err := openLayerStreamFromRaw(compressed.Bytes(), mtOCILayerTarZstd, descriptor{
		Digest:    "sha256:test",
		MediaType: mtOCILayerTarZstd,
	})
	if err != nil {
		t.Fatalf("openLayerStreamFromRaw: %v", err)
	}
	defer stream.Reader.Close()

	if !stream.Decoded {
		t.Fatal("expected decoded zstd stream")
	}
	got, err := io.ReadAll(stream.Reader)
	if err != nil {
		t.Fatalf("read decoded zstd stream: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("decoded payload mismatch: got %q want %q", got, original)
	}
}

func TestExportSessionBlobCacheEvictsOldEntries(t *testing.T) {
	session := newExportSession()
	chunk := bytes.Repeat([]byte("a"), 48<<20)

	for i := 0; i < 8; i++ {
		session.storeCachedBlob(
			"sha256:test-"+string(rune('a'+i)),
			mtDockerLayerTar,
			append([]byte(nil), chunk...),
		)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.blobCacheSize > maxCachedBlobBytes {
		t.Fatalf("cache exceeded byte budget: %d > %d", session.blobCacheSize, maxCachedBlobBytes)
	}
	if len(session.blobCache) >= 8 {
		t.Fatalf("expected eviction to happen, still have %d entries", len(session.blobCache))
	}
	if _, ok := session.blobCache["sha256:test-a"]; ok {
		t.Fatal("expected oldest cached blob to be evicted")
	}
}

func TestReadBlobForCacheRejectsOversizePayload(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), maxCachedBlobSize+1024)
	raw, tooLarge, err := readBlobForCache(bytes.NewReader(payload), maxCachedBlobSize)
	if err != nil {
		t.Fatalf("readBlobForCache returned error: %v", err)
	}
	if !tooLarge {
		t.Fatal("expected oversize payload to be rejected for caching")
	}
	if raw != nil {
		t.Fatalf("expected no cached payload, got %d bytes", len(raw))
	}
}

func TestWriteLayerFromRawBlobRejectsDescriptorSizeMismatch(t *testing.T) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	err := writeLayerFromRawBlob(
		tw,
		[]byte("abc"),
		mtDockerLayerTar,
		platform{OS: "linux", Architecture: "amd64"},
		descriptor{Digest: "sha256:test", MediaType: mtDockerLayerTar, Size: 5},
		"",
		1,
		1,
		"layer.tar",
		nil,
	)
	_ = tw.Close()

	if err == nil {
		t.Fatal("expected cached raw blob size mismatch to fail")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("size mismatch")) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteLayerFromRawBlobRejectsEmptyBlobWhenDescriptorHasSize(t *testing.T) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	err := writeLayerFromRawBlob(
		tw,
		nil,
		mtDockerLayerTar,
		platform{OS: "linux", Architecture: "amd64"},
		descriptor{Digest: "sha256:empty", MediaType: mtDockerLayerTar, Size: 1},
		"",
		1,
		1,
		"layer.tar",
		nil,
	)
	_ = tw.Close()

	if err == nil {
		t.Fatal("expected empty cached raw blob with positive descriptor size to fail")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("size mismatch")) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteLayerFromRawBlobAcceptsSHA512DiffID(t *testing.T) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	raw := []byte("abc")
	diffIDSum := sha512.Sum512(raw)
	err := writeLayerFromRawBlob(
		tw,
		raw,
		mtDockerLayerTar,
		platform{OS: "linux", Architecture: "amd64"},
		descriptor{Digest: "sha256:test", MediaType: mtDockerLayerTar, Size: int64(len(raw))},
		"sha512:"+hex.EncodeToString(diffIDSum[:]),
		1,
		1,
		"layer.tar",
		nil,
	)
	if err != nil {
		t.Fatalf("writeLayerFromRawBlob returned error: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
}

func TestValidateDockerArchiveAcceptsValidArchive(t *testing.T) {
	tempDir := t.TempDir()
	archivePath := filepath.Join(tempDir, "valid.tar")

	configData := []byte(`{"rootfs":{"diff_ids":["sha256:test"]}}`)
	configSum := sha256.Sum256(configData)
	configName := hex.EncodeToString(configSum[:]) + ".json"
	layerID := "layer123"
	layerPath := layerID + "/layer.tar"

	var layerBuf bytes.Buffer
	layerTw := tar.NewWriter(&layerBuf)
	if err := writeTarBytes(layerTw, "hello.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("write inner layer tar: %v", err)
	}
	if err := layerTw.Close(); err != nil {
		t.Fatalf("close inner layer tar: %v", err)
	}

	manifestData, err := json.Marshal([]saveManifestEntry{
		{
			Config:   configName,
			RepoTags: []string{"example/app:latest"},
			Layers:   []string{layerPath},
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	repositoriesData, err := json.Marshal(map[string]map[string]string{
		"example/app": {"latest": layerID},
	})
	if err != nil {
		t.Fatalf("marshal repositories: %v", err)
	}

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	tw := tar.NewWriter(file)
	if err := writeTarDir(tw, layerID+"/"); err != nil {
		t.Fatalf("write layer dir: %v", err)
	}
	if err := writeTarBytes(tw, configName, configData, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := writeTarBytes(tw, layerID+"/VERSION", []byte("1.0\n"), 0o644); err != nil {
		t.Fatalf("write version: %v", err)
	}
	if err := writeTarBytes(tw, layerID+"/json", []byte(`{"id":"layer123"}`), 0o644); err != nil {
		t.Fatalf("write layer json: %v", err)
	}
	if err := writeTarBytes(tw, layerPath, layerBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write layer tar: %v", err)
	}
	if err := writeTarBytes(tw, "manifest.json", manifestData, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := writeTarBytes(tw, "repositories", repositoriesData, 0o644); err != nil {
		t.Fatalf("write repositories: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close archive tar: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive file: %v", err)
	}

	if err := validateDockerArchive(archivePath); err != nil {
		t.Fatalf("validateDockerArchive returned error: %v", err)
	}
}

func TestValidateDockerArchiveContentsAcceptsRepoWithPortAndSHA512ConfigDigest(t *testing.T) {
	configData := []byte(`{"rootfs":{"diff_ids":["sha256:test"]}}`)
	configSum := sha512.Sum512(configData)
	configDigest := "sha512:" + hex.EncodeToString(configSum[:])
	configName := digestHex(configDigest) + ".json"
	layerPath := "layer123/layer.tar"

	manifestData, err := json.Marshal([]saveManifestEntry{
		{
			Config:   configName,
			RepoTags: []string{"localhost:5000/myapp:latest"},
			Layers:   []string{layerPath},
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	repositoriesData, err := json.Marshal(map[string]map[string]string{
		"localhost:5000/myapp": {"latest": "layer123"},
	})
	if err != nil {
		t.Fatalf("marshal repositories: %v", err)
	}

	err = validateDockerArchiveContents(
		map[string][]byte{
			"manifest.json": manifestData,
			"repositories":  repositoriesData,
			configName:      configData,
		},
		map[string]struct{}{
			layerPath: {},
		},
		archiveValidationExpectation{
			ConfigDigests: map[string]string{
				configName: configDigest,
			},
		},
	)
	if err != nil {
		t.Fatalf("validateDockerArchiveContents returned error: %v", err)
	}
}
