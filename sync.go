package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type registryAccess struct {
	Endpoint  string `json:"endpoint"`
	Namespace string `json:"namespace,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	Insecure  bool   `json:"insecure,omitempty"`
}

type syncProgressEvent struct {
	Stage        string  `json:"stage"`
	Image        string  `json:"image,omitempty"`
	Platform     string  `json:"platform,omitempty"`
	Message      string  `json:"message,omitempty"`
	CurrentImage int     `json:"current_image,omitempty"`
	TotalImages  int     `json:"total_images,omitempty"`
	CurrentBlob  int     `json:"current_blob,omitempty"`
	TotalBlobs   int     `json:"total_blobs,omitempty"`
	BytesDone    int64   `json:"bytes_done,omitempty"`
	BytesTotal   int64   `json:"bytes_total,omitempty"`
	SpeedBPS     float64 `json:"speed_bps,omitempty"`
	ETASeconds   int64   `json:"eta_seconds,omitempty"`
}

type syncHooks struct {
	Progress func(syncProgressEvent)
}

func (h *syncHooks) emit(event syncProgressEvent) {
	if h != nil && h.Progress != nil {
		h.Progress(event)
	}
}

type registryCopyResult struct {
	Source       string `json:"source"`
	Target       string `json:"target"`
	Digest       string `json:"digest"`
	BlobsCopied  int    `json:"blobs_copied"`
	BlobsSkipped int    `json:"blobs_skipped"`
	BytesCopied  int64  `json:"bytes_copied"`
}

type registryCopySession struct {
	mu            sync.Mutex
	presentBlobs  map[string]struct{}
	copiedBlobs   int
	skippedBlobs  int
	bytesCopied   int64
	currentBlob   int
	totalBlobs    int
	currentImage  int
	totalImages   int
	displayImage  string
	progressHooks *syncHooks
}

func newRegistryCopySession(hooks *syncHooks) *registryCopySession {
	return &registryCopySession{
		presentBlobs:  make(map[string]struct{}),
		progressHooks: hooks,
	}
}

func (a registryAccess) client(ctx context.Context) (*registryClient, error) {
	endpoint := normalizeRegistryEndpoint(a.Endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid registry endpoint %q", a.Endpoint)
	}
	return newRegistryClientWithScheme(ctx, a.Proxy, a.Username, a.Password, a.Insecure, u.Scheme)
}

func (a registryAccess) imageRef(raw string) (imageRef, error) {
	endpoint := normalizeRegistryEndpoint(a.Endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return imageRef{}, fmt.Errorf("invalid registry endpoint %q", a.Endpoint)
	}
	parsed, err := parseImageRef(raw)
	if err != nil {
		return imageRef{}, err
	}
	explicitRegistry := imageHasExplicitRegistry(raw)
	if explicitRegistry && !strings.EqualFold(comparableRegistryHost(parsed.RegistryHost(), u.Scheme), comparableRegistryHost(u.Host, u.Scheme)) {
		return imageRef{}, fmt.Errorf("image reference registry %q does not match configured registry %q", parsed.Registry, u.Host)
	}
	repository := parsed.Repository
	if !explicitRegistry && parsed.Registry == dockerHubRegistryAlias && !strings.Contains(imageNameWithoutTag(raw), "/") {
		repository = strings.TrimPrefix(repository, "library/")
	}
	namespace := strings.Trim(strings.TrimSpace(a.Namespace), "/")
	if namespace != "" && repository != namespace && !strings.HasPrefix(repository, namespace+"/") {
		repository = namespace + "/" + repository
	}
	registry := u.Host
	if registry == dockerHubRegistryHost {
		registry = dockerHubRegistryAlias
	}
	parsed.Registry = registry
	parsed.Repository = strings.TrimPrefix(repository, "/")
	if err := validateRepositoryPath(parsed.Repository); err != nil {
		return imageRef{}, fmt.Errorf("invalid repository in reference %q: %w", raw, err)
	}
	return parsed, nil
}

func comparableRegistryHost(host, scheme string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == dockerHubRegistryAlias || host == dockerHubRegistryHost || host == "index.docker.io" {
		return dockerHubRegistryHost
	}
	name, port, err := net.SplitHostPort(host)
	if err == nil && ((strings.EqualFold(scheme, "https") && port == "443") || (strings.EqualFold(scheme, "http") && port == "80")) {
		return strings.Trim(strings.ToLower(name), "[]")
	}
	return host
}

func imageHasExplicitRegistry(raw string) bool {
	name := imageNameWithoutTag(raw)
	first, _, hasSlash := strings.Cut(name, "/")
	return hasSlash && isRegistryComponent(first)
}

func imageNameWithoutTag(raw string) string {
	raw = strings.TrimSpace(raw)
	if before, _, ok := strings.Cut(raw, "@"); ok {
		raw = before
	}
	lastSlash := strings.LastIndex(raw, "/")
	lastColon := strings.LastIndex(raw, ":")
	if lastColon > lastSlash {
		raw = raw[:lastColon]
	}
	return raw
}

func copyRegistryImage(
	ctx context.Context,
	sourceAccess registryAccess,
	targetAccess registryAccess,
	sourceImage string,
	targetImage string,
	session *registryCopySession,
) (registryCopyResult, error) {
	if session == nil {
		session = newRegistryCopySession(nil)
	}
	sourceRef, err := sourceAccess.imageRef(sourceImage)
	if err != nil {
		return registryCopyResult{}, fmt.Errorf("source image: %w", err)
	}
	targetRef, err := targetAccess.imageRef(targetImage)
	if err != nil {
		return registryCopyResult{}, fmt.Errorf("target image: %w", err)
	}
	sourceClient, err := sourceAccess.client(ctx)
	if err != nil {
		return registryCopyResult{}, err
	}
	targetClient, err := targetAccess.client(ctx)
	if err != nil {
		return registryCopyResult{}, err
	}

	session.displayImage = sourceRef.DisplayTag()
	session.currentBlob = 0
	session.totalBlobs = 0
	session.progressHooks.emit(syncProgressEvent{
		Stage:        "manifest",
		Image:        session.displayImage,
		Message:      "fetching source manifest",
		CurrentImage: session.currentImage,
		TotalImages:  session.totalImages,
	})
	body, mediaType, topDigest, err := sourceClient.getManifestWithDigest(sourceRef, sourceRef.ManifestReference())
	if err != nil {
		return registryCopyResult{}, fmt.Errorf("fetch source manifest: %w", err)
	}
	mediaType = manifestMediaType(body, mediaType)
	startCopied := session.copiedBlobs
	startSkipped := session.skippedBlobs
	startBytes := session.bytesCopied

	digest, err := copyManifestTree(ctx, sourceClient, targetClient, sourceRef, targetRef, body, mediaType, topDigest, session)
	if err != nil {
		return registryCopyResult{}, err
	}
	if err := putRegistryManifest(ctx, targetClient, targetRef, targetRef.ManifestReference(), mediaType, body, digest); err != nil {
		return registryCopyResult{}, fmt.Errorf("publish target manifest: %w", err)
	}
	session.progressHooks.emit(syncProgressEvent{
		Stage:        "image_done",
		Image:        session.displayImage,
		Message:      "registry copy complete",
		CurrentImage: session.currentImage,
		TotalImages:  session.totalImages,
		CurrentBlob:  session.currentBlob,
		TotalBlobs:   session.totalBlobs,
		BytesDone:    session.bytesCopied - startBytes,
	})
	return registryCopyResult{
		Source:       sourceRef.DisplayTag(),
		Target:       targetRef.DisplayTag(),
		Digest:       digest,
		BlobsCopied:  session.copiedBlobs - startCopied,
		BlobsSkipped: session.skippedBlobs - startSkipped,
		BytesCopied:  session.bytesCopied - startBytes,
	}, nil
}

func copyManifestTree(
	ctx context.Context,
	sourceClient *registryClient,
	targetClient *registryClient,
	sourceRef imageRef,
	targetRef imageRef,
	body []byte,
	mediaType string,
	expectedDigest string,
	session *registryCopySession,
) (string, error) {
	digest, err := payloadDigest(body, expectedDigest)
	if err != nil {
		return "", err
	}
	switch normalizeMediaType(mediaType) {
	case mtDockerManifestListV2, mtOCIImageIndexV1:
		var index manifestList
		if err := json.Unmarshal(body, &index); err != nil {
			return "", fmt.Errorf("parse image index: %w", err)
		}
		for _, child := range index.Manifests {
			childBody, childMediaType, err := sourceClient.getManifestDescriptor(sourceRef, child)
			if err != nil {
				return "", fmt.Errorf("fetch child manifest %s: %w", child.Digest, err)
			}
			childMediaType = manifestMediaType(childBody, childMediaType)
			if _, err := copyManifestTree(ctx, sourceClient, targetClient, sourceRef, targetRef, childBody, childMediaType, child.Digest, session); err != nil {
				return "", err
			}
			if err := putRegistryManifest(ctx, targetClient, targetRef, child.Digest, childMediaType, childBody, child.Digest); err != nil {
				return "", fmt.Errorf("publish child manifest %s: %w", child.Digest, err)
			}
		}
	case mtDockerManifestV2, mtOCIManifestV1, "":
		var manifest imageManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return "", fmt.Errorf("parse image manifest: %w", err)
		}
		descriptors := make([]descriptor, 0, len(manifest.Layers)+1)
		descriptors = append(descriptors, manifest.Config)
		descriptors = append(descriptors, manifest.Layers...)
		session.totalBlobs += len(descriptors)
		for _, blob := range descriptors {
			if strings.TrimSpace(blob.Digest) == "" {
				return "", fmt.Errorf("image manifest contains a blob without digest")
			}
			if err := copyRegistryBlob(ctx, sourceClient, targetClient, sourceRef, targetRef, blob, session); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("unsupported manifest media type %s", mediaType)
	}
	return digest, nil
}

func copyRegistryBlob(
	ctx context.Context,
	sourceClient *registryClient,
	targetClient *registryClient,
	sourceRef imageRef,
	targetRef imageRef,
	desc descriptor,
	session *registryCopySession,
) error {
	if _, _, err := newDigestHasher(desc.Digest); err != nil {
		return fmt.Errorf("invalid blob descriptor digest: %w", err)
	}
	session.currentBlob++
	cacheKey := targetRef.RegistryHost() + "/" + targetRef.Repository + "@" + desc.Digest
	session.mu.Lock()
	_, cached := session.presentBlobs[cacheKey]
	session.mu.Unlock()
	if cached {
		session.skippedBlobs++
		session.progressHooks.emit(syncProgressEvent{
			Stage: "blob_skip", Image: session.displayImage, Message: desc.Digest,
			CurrentImage: session.currentImage, TotalImages: session.totalImages,
			CurrentBlob: session.currentBlob, TotalBlobs: session.totalBlobs,
		})
		return nil
	}
	present, err := registryBlobExists(ctx, targetClient, targetRef, desc.Digest)
	if err != nil {
		return fmt.Errorf("check target blob %s: %w", desc.Digest, err)
	}
	if present {
		session.mu.Lock()
		session.presentBlobs[cacheKey] = struct{}{}
		session.mu.Unlock()
		session.skippedBlobs++
		session.progressHooks.emit(syncProgressEvent{
			Stage: "blob_skip", Image: session.displayImage, Message: desc.Digest,
			CurrentImage: session.currentImage, TotalImages: session.totalImages,
			CurrentBlob: session.currentBlob, TotalBlobs: session.totalBlobs,
		})
		return nil
	}

	session.progressHooks.emit(syncProgressEvent{
		Stage:        "blob",
		Image:        session.displayImage,
		Message:      desc.Digest,
		CurrentImage: session.currentImage,
		TotalImages:  session.totalImages,
		CurrentBlob:  session.currentBlob,
		TotalBlobs:   session.totalBlobs,
		BytesTotal:   desc.Size,
	})
	uploadURL, mounted, err := startRegistryBlobUpload(ctx, targetClient, sourceRef, targetRef, desc.Digest)
	if err != nil {
		return fmt.Errorf("start target upload for %s: %w", desc.Digest, err)
	}
	if mounted {
		session.mu.Lock()
		session.presentBlobs[cacheKey] = struct{}{}
		session.mu.Unlock()
		session.skippedBlobs++
		return nil
	}
	sourceBody, _, err := sourceClient.openBlobDescriptor(sourceRef, desc)
	if err != nil {
		return fmt.Errorf("open source blob %s: %w", desc.Digest, err)
	}
	defer sourceBody.Close()

	tracker := &syncCopyProgressReader{
		reader: sourceBody,
		total:  desc.Size,
		start:  time.Now(),
		event: syncProgressEvent{
			Stage:        "blob",
			Image:        session.displayImage,
			Message:      desc.Digest,
			CurrentImage: session.currentImage,
			TotalImages:  session.totalImages,
			CurrentBlob:  session.currentBlob,
			TotalBlobs:   session.totalBlobs,
			BytesTotal:   desc.Size,
		},
		hooks: session.progressHooks,
	}
	if err := finishRegistryBlobUpload(ctx, targetClient, targetRef, uploadURL, desc, tracker); err != nil {
		return fmt.Errorf("upload blob %s: %w", desc.Digest, err)
	}
	if tracker.done != desc.Size && desc.Size >= 0 {
		return fmt.Errorf("uploaded blob %s size mismatch: got %d want %d", desc.Digest, tracker.done, desc.Size)
	}
	// net/http may stop reading a request body after ContentLength bytes without
	// requesting EOF. Read once more so the source-side digest verifier runs and
	// so an upstream body larger than its descriptor cannot pass silently.
	var extra [1]byte
	n, verifyErr := tracker.Read(extra[:])
	if n > 0 {
		return fmt.Errorf("source blob %s size mismatch: got more than %d bytes", desc.Digest, desc.Size)
	}
	if verifyErr != io.EOF {
		if verifyErr == nil {
			return fmt.Errorf("source blob %s did not terminate at its declared size", desc.Digest)
		}
		return fmt.Errorf("verify source blob %s: %w", desc.Digest, verifyErr)
	}
	session.mu.Lock()
	session.presentBlobs[cacheKey] = struct{}{}
	session.mu.Unlock()
	session.copiedBlobs++
	session.bytesCopied += tracker.done
	return nil
}

func registryBlobExists(ctx context.Context, client *registryClient, ref imageRef, digest string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, client.endpointURL(ref, "blobs/"+digest), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.doWithAuthActions(ref, req, "pull,push")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}
}

func startRegistryBlobUpload(ctx context.Context, client *registryClient, sourceRef, targetRef imageRef, digest string) (string, bool, error) {
	u, err := url.Parse(client.endpointURL(targetRef, "blobs/uploads/"))
	if err != nil {
		return "", false, err
	}
	if sourceRef.RegistryHost() == targetRef.RegistryHost() && sourceRef.Repository != "" {
		query := u.Query()
		query.Set("mount", digest)
		query.Set("from", sourceRef.Repository)
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return "", false, err
	}
	resp, err := client.doWithAuthActions(targetRef, req, "pull,push")
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return "", true, nil
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return "", false, fmt.Errorf("registry upload response missing Location header")
	}
	resolved, err := resp.Request.URL.Parse(location)
	if err != nil {
		return "", false, fmt.Errorf("resolve upload URL: %w", err)
	}
	return resolved.String(), false, nil
}

func finishRegistryBlobUpload(ctx context.Context, client *registryClient, ref imageRef, uploadURL string, desc descriptor, body io.Reader) error {
	u, err := url.Parse(uploadURL)
	if err != nil {
		return err
	}
	query := u.Query()
	query.Set("digest", desc.Digest)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	if desc.Size >= 0 {
		req.ContentLength = desc.Size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	var resp *http.Response
	if strings.EqualFold(req.URL.Host, ref.RegistryHost()) {
		resp, err = client.doWithAuthActions(ref, req, "pull,push")
	} else {
		resp, err = client.httpClient.Do(req)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if returnedDigest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); returnedDigest != "" && returnedDigest != desc.Digest {
		return fmt.Errorf("target registry returned digest %s, expected %s", returnedDigest, desc.Digest)
	}
	return nil
}

func putRegistryManifest(ctx context.Context, client *registryClient, ref imageRef, reference, mediaType string, body []byte, expectedDigest string) error {
	if strings.TrimSpace(reference) == "" {
		return fmt.Errorf("manifest reference is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, client.endpointURL(ref, "manifests/"+reference), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", normalizeMediaType(mediaType))
	req.ContentLength = int64(len(body))
	resp, err := client.doWithAuthActions(ref, req, "pull,push")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	if returnedDigest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); returnedDigest != "" && expectedDigest != "" && returnedDigest != expectedDigest {
		return fmt.Errorf("target manifest digest mismatch: got %s want %s", returnedDigest, expectedDigest)
	}
	return nil
}

func manifestMediaType(body []byte, header string) string {
	mediaType := normalizeMediaType(header)
	if mediaType != "" && mediaType != "application/json" {
		return mediaType
	}
	var envelope struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
		Layers    json.RawMessage `json:"layers"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.MediaType != "" {
			return normalizeMediaType(envelope.MediaType)
		}
		if len(envelope.Manifests) > 0 {
			return mtOCIImageIndexV1
		}
		if len(envelope.Layers) > 0 {
			return mtOCIManifestV1
		}
	}
	return mediaType
}

func payloadDigest(body []byte, expected string) (string, error) {
	if expected != "" {
		if err := verifyPayloadDigest(body, expected, "manifest"); err != nil {
			return "", err
		}
		return expected, nil
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type syncCopyProgressReader struct {
	reader   io.Reader
	total    int64
	done     int64
	start    time.Time
	lastEmit time.Time
	event    syncProgressEvent
	hooks    *syncHooks
}

func (r *syncCopyProgressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.done += int64(n)
	now := time.Now()
	if err != nil || r.lastEmit.IsZero() || now.Sub(r.lastEmit) >= 250*time.Millisecond {
		elapsed := now.Sub(r.start).Seconds()
		speed := float64(0)
		if elapsed > 0 {
			speed = float64(r.done) / elapsed
		}
		eta := int64(0)
		if speed > 0 && r.total > r.done {
			eta = int64(float64(r.total-r.done) / speed)
		}
		event := r.event
		event.BytesDone = r.done
		event.BytesTotal = r.total
		event.SpeedBPS = speed
		event.ETASeconds = eta
		r.hooks.emit(event)
		r.lastEmit = now
	}
	return n, err
}
