package main

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCopyRegistryImageStreamsBlobsWithoutDocker(t *testing.T) {
	configBlob := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layerBlob := []byte("registry-copy-layer-payload")
	configDigest := sha256Digest(configBlob)
	layerDigest := sha256Digest(layerBlob)
	manifest := imageManifest{
		SchemaVersion: 2,
		MediaType:     mtOCIManifestV1,
		Config: descriptor{
			MediaType: mtDockerConfigV1,
			Digest:    configDigest,
			Size:      int64(len(configBlob)),
		},
		Layers: []descriptor{{
			MediaType: mtOCILayerTar,
			Digest:    layerDigest,
			Size:      int64(len(layerBlob)),
		}},
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := sha512Digest(manifestBody)

	sourceHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIManifestV1)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = w.Write(manifestBody)
		case "/v2/team/app/blobs/" + configDigest:
			_, _ = w.Write(configBlob)
		case "/v2/team/app/blobs/" + layerDigest:
			_, _ = w.Write(layerBlob)
		default:
			http.NotFound(w, r)
		}
	})
	sourceURL, closeSource := startIPv4HTTPServer(t, sourceHandler)
	defer closeSource()

	uploaded := make(map[string][]byte)
	var targetManifest []byte
	var targetURL string
	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && len(r.URL.Path) > len("/v2/team/app/blobs/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/team/app/blobs/uploads/":
			w.Header().Set("Location", targetURL+"/upload/session")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Path == "/upload/session":
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read uploaded blob: %v", readErr)
			}
			digest := r.URL.Query().Get("digest")
			uploaded[digest] = body
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/team/app/manifests/v1":
			targetManifest, _ = io.ReadAll(r.Body)
			w.Header().Set("Docker-Content-Digest", sha512Digest(targetManifest))
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	})
	targetURL, closeTarget := startIPv4HTTPServer(t, targetHandler)
	defer closeTarget()

	events := make([]syncProgressEvent, 0)
	session := newRegistryCopySession(&syncHooks{Progress: func(event syncProgressEvent) {
		events = append(events, event)
	}})
	session.currentImage = 1
	session.totalImages = 1
	result, err := copyRegistryImage(context.Background(),
		registryAccess{Endpoint: sourceURL},
		registryAccess{Endpoint: targetURL},
		"team/app:v1",
		"team/app:v1",
		session,
	)
	if err != nil {
		t.Fatalf("copy registry image: %v", err)
	}
	if string(uploaded[configDigest]) != string(configBlob) || string(uploaded[layerDigest]) != string(layerBlob) {
		t.Fatalf("unexpected uploaded blobs: %+v", uploaded)
	}
	if string(targetManifest) != string(manifestBody) {
		t.Fatalf("target manifest changed:\n%s\nwant:\n%s", targetManifest, manifestBody)
	}
	if result.BlobsCopied != 2 || result.BytesCopied != int64(len(configBlob)+len(layerBlob)) {
		t.Fatalf("unexpected copy result: %+v", result)
	}
	if result.Digest != manifestDigest {
		t.Fatalf("manifest digest algorithm was not preserved: got %s want %s", result.Digest, manifestDigest)
	}
	if len(events) == 0 || events[len(events)-1].Stage != "image_done" {
		t.Fatalf("expected terminal progress event, got %+v", events)
	}
	for _, event := range events {
		if event.TotalBlobs > 2 {
			t.Fatalf("single-manifest blob total was counted twice: %+v", event)
		}
	}
}

func TestCopyRegistryImageCopiesMultiArchIndexAndDeduplicatesSharedBlob(t *testing.T) {
	sharedLayer := []byte("shared-multi-platform-layer")
	sharedDigest := sha256Digest(sharedLayer)
	configAMD64 := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configARM64 := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configAMD64Digest := sha256Digest(configAMD64)
	configARM64Digest := sha256Digest(configARM64)

	manifestFor := func(config []byte, configDigest string) []byte {
		body, err := json.Marshal(imageManifest{
			SchemaVersion: 2,
			MediaType:     mtOCIManifestV1,
			Config: descriptor{
				MediaType: mtDockerConfigV1, Digest: configDigest, Size: int64(len(config)),
			},
			Layers: []descriptor{{
				MediaType: mtOCILayerTar, Digest: sharedDigest, Size: int64(len(sharedLayer)),
			}},
		})
		if err != nil {
			t.Fatalf("marshal platform manifest: %v", err)
		}
		return body
	}
	manifestAMD64 := manifestFor(configAMD64, configAMD64Digest)
	manifestARM64 := manifestFor(configARM64, configARM64Digest)
	manifestAMD64Digest := sha256Digest(manifestAMD64)
	manifestARM64Digest := sha256Digest(manifestARM64)
	indexBody, err := json.Marshal(manifestList{
		SchemaVersion: 2,
		MediaType:     mtOCIImageIndexV1,
		Manifests: []descriptor{
			{MediaType: mtOCIManifestV1, Digest: manifestAMD64Digest, Size: int64(len(manifestAMD64)), Platform: platform{OS: "linux", Architecture: "amd64"}},
			{MediaType: mtOCIManifestV1, Digest: manifestARM64Digest, Size: int64(len(manifestARM64)), Platform: platform{OS: "linux", Architecture: "arm64"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal image index: %v", err)
	}
	indexDigest := sha256Digest(indexBody)

	sourceURL, closeSource := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIImageIndexV1)
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = w.Write(indexBody)
		case "/v2/team/app/manifests/" + manifestAMD64Digest:
			w.Header().Set("Content-Type", mtOCIManifestV1)
			_, _ = w.Write(manifestAMD64)
		case "/v2/team/app/manifests/" + manifestARM64Digest:
			w.Header().Set("Content-Type", mtOCIManifestV1)
			_, _ = w.Write(manifestARM64)
		case "/v2/team/app/blobs/" + configAMD64Digest:
			_, _ = w.Write(configAMD64)
		case "/v2/team/app/blobs/" + configARM64Digest:
			_, _ = w.Write(configARM64)
		case "/v2/team/app/blobs/" + sharedDigest:
			_, _ = w.Write(sharedLayer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeSource()

	var targetURL string
	var mu sync.Mutex
	uploaded := make(map[string][]byte)
	published := make(map[string][]byte)
	uploadSequence := 0
	targetURL, closeTarget := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/v2/archive/app/blobs/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/archive/app/blobs/uploads/":
			mu.Lock()
			uploadSequence++
			location := targetURL + "/upload/" + strconv.Itoa(uploadSequence)
			mu.Unlock()
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/"):
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read uploaded blob: %v", readErr)
			}
			digest := r.URL.Query().Get("digest")
			mu.Lock()
			uploaded[digest] = body
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v2/archive/app/manifests/"):
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read published manifest: %v", readErr)
			}
			reference := strings.TrimPrefix(r.URL.Path, "/v2/archive/app/manifests/")
			mu.Lock()
			published[reference] = body
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", sha256Digest(body))
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeTarget()

	result, err := copyRegistryImage(
		context.Background(),
		registryAccess{Endpoint: sourceURL},
		registryAccess{Endpoint: targetURL},
		"team/app:v1",
		"archive/app:release",
		nil,
	)
	if err != nil {
		t.Fatalf("copy multi-platform image: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(uploaded) != 3 || string(uploaded[sharedDigest]) != string(sharedLayer) {
		t.Fatalf("unexpected uploaded blobs: %+v", uploaded)
	}
	if string(published[manifestAMD64Digest]) != string(manifestAMD64) || string(published[manifestARM64Digest]) != string(manifestARM64) {
		t.Fatalf("platform manifests were not preserved: %+v", published)
	}
	if string(published["release"]) != string(indexBody) {
		t.Fatalf("top-level index was not published under target tag")
	}
	if result.Digest != indexDigest || result.BlobsCopied != 3 || result.BlobsSkipped != 1 {
		t.Fatalf("unexpected multi-platform result: %+v", result)
	}
}

func TestRegistryAccessAppliesNamespaceWithoutRewritingPaths(t *testing.T) {
	access := registryAccess{Endpoint: "https://registry.example.test", Namespace: "archive/team"}
	ref, err := access.imageRef("service/api:v1")
	if err != nil {
		t.Fatalf("resolve namespaced image: %v", err)
	}
	if ref.Registry != "registry.example.test" || ref.Repository != "archive/team/service/api" || ref.Tag != "v1" {
		t.Fatalf("unexpected namespaced reference: %+v", ref)
	}
}

func TestRegistryAccessRejectsExplicitDifferentRegistry(t *testing.T) {
	access := registryAccess{Endpoint: "https://registry.example.test"}
	_, err := access.imageRef("other.example.test/team/app:v1")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unexpected mismatched registry result: %v", err)
	}
	if _, err := access.imageRef("registry.example.test:443/team/app:v1"); err != nil {
		t.Fatalf("matching default HTTPS port was rejected: %v", err)
	}
}

func TestCopyRegistryImageRejectsCorruptSourceBlobEvenWhenTargetAcceptsIt(t *testing.T) {
	declaredBlob := []byte("good")
	servedBlob := []byte("evil")
	digest := sha256Digest(declaredBlob)
	manifestBody, err := json.Marshal(imageManifest{
		SchemaVersion: 2,
		MediaType:     mtOCIManifestV1,
		Config: descriptor{
			MediaType: mtDockerConfigV1,
			Digest:    digest,
			Size:      int64(len(declaredBlob)),
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	sourceURL, closeSource := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIManifestV1)
			_, _ = w.Write(manifestBody)
		case "/v2/team/app/blobs/" + digest:
			_, _ = w.Write(servedBlob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeSource()

	var targetURL string
	targetURL, closeTarget := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.Header().Set("Location", targetURL+"/upload/session")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Path == "/upload/session":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeTarget()

	_, err = copyRegistryImage(context.Background(), registryAccess{Endpoint: sourceURL}, registryAccess{Endpoint: targetURL}, "team/app:v1", "team/app:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected corrupt source digest rejection, got %v", err)
	}
}

func TestCopyRegistryImageRejectsSourceBlobLargerThanDescriptor(t *testing.T) {
	servedBlob := []byte("four")
	descriptorPayload := servedBlob[:3]
	digest := sha256Digest(descriptorPayload)
	manifestBody, err := json.Marshal(imageManifest{
		SchemaVersion: 2,
		MediaType:     mtOCIManifestV1,
		Config: descriptor{
			MediaType: mtDockerConfigV1,
			Digest:    digest,
			Size:      int64(len(descriptorPayload)),
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	sourceURL, closeSource := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", mtOCIManifestV1)
			_, _ = w.Write(manifestBody)
		case "/v2/team/app/blobs/" + digest:
			_, _ = w.Write(servedBlob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeSource()

	var targetURL string
	targetURL, closeTarget := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.Header().Set("Location", targetURL+"/upload/session")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Path == "/upload/session":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer closeTarget()

	_, err = copyRegistryImage(context.Background(), registryAccess{Endpoint: sourceURL}, registryAccess{Endpoint: targetURL}, "team/app:v1", "team/app:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected oversized source rejection, got %v", err)
	}
}

func TestRegistryAccessAppliesNamespaceAndSelectedEndpoint(t *testing.T) {
	access := registryAccess{Endpoint: "https://registry.example.test:5443", Namespace: "archive"}
	ref, err := access.imageRef("team/app:v2")
	if err != nil {
		t.Fatalf("build image ref: %v", err)
	}
	if ref.Registry != "registry.example.test:5443" || ref.Repository != "archive/team/app" || ref.Tag != "v2" {
		t.Fatalf("unexpected image ref: %+v", ref)
	}

	ref, err = access.imageRef("alpine:latest")
	if err != nil {
		t.Fatalf("build short image ref: %v", err)
	}
	if ref.Repository != "archive/alpine" {
		t.Fatalf("unexpected short repository: %q", ref.Repository)
	}
}

func TestGetManifestWithDigestPreservesUpstreamAlgorithm(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIManifestV1 + `","config":{"digest":"sha256:test","size":1},"layers":[]}`)
	sum := sha512.Sum512(body)
	wantDigest := "sha512:" + hex.EncodeToString(sum[:])
	serverURL, closeServer := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", mtOCIManifestV1)
		w.Header().Set("Docker-Content-Digest", wantDigest)
		_, _ = w.Write(body)
	}))
	defer closeServer()

	access := registryAccess{Endpoint: serverURL}
	ref, err := access.imageRef("team/app:v1")
	if err != nil {
		t.Fatalf("build image ref: %v", err)
	}
	client, err := access.client(context.Background())
	if err != nil {
		t.Fatalf("build registry client: %v", err)
	}
	_, _, gotDigest, err := client.getManifestWithDigest(ref, "v1")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("unexpected manifest digest: got %s want %s", gotDigest, wantDigest)
	}
}

func startIPv4HTTPServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on IPv4: %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	return "http://" + listener.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sha512Digest(data []byte) string {
	sum := sha512.Sum512(data)
	return "sha512:" + hex.EncodeToString(sum[:])
}
