package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	syncTargetRegistry = "registry"
	syncTargetLocalTar = "local_tar"
)

var (
	syncExecutableLookPath = exec.LookPath
	syncNativeCopy         = copyRegistryImage
	syncSkopeoCopy         = copyRegistryImageWithSkopeo
)

type syncJobSpec struct {
	Source       registryAccess  `json:"source"`
	TargetType   string          `json:"target_type"`
	Target       registryAccess  `json:"target,omitempty"`
	Images       []imageListItem `json:"images"`
	Engine       string          `json:"engine,omitempty"`
	Workspace    string          `json:"workspace"`
	StorageRoots []string        `json:"storage_roots,omitempty"`
	OutputRoot   string          `json:"output_root,omitempty"`
}

type syncJobItemResult struct {
	Source       string              `json:"source"`
	Target       string              `json:"target"`
	Status       string              `json:"status"`
	Error        string              `json:"error,omitempty"`
	RegistryCopy *registryCopyResult `json:"registry_copy,omitempty"`
	Export       *exportReport       `json:"export,omitempty"`
}

type syncJobResult struct {
	TargetType string              `json:"target_type"`
	Engine     string              `json:"engine"`
	Storage    *storageCandidate   `json:"storage,omitempty"`
	Items      []syncJobItemResult `json:"items"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at"`
	Succeeded  int                 `json:"succeeded"`
	Failed     int                 `json:"failed"`
}

func runSyncJob(ctx context.Context, spec syncJobSpec, hooks *syncHooks) (syncJobResult, error) {
	result := syncJobResult{
		TargetType: strings.ToLower(strings.TrimSpace(spec.TargetType)),
		StartedAt:  time.Now().UTC(),
		Items:      make([]syncJobItemResult, 0, len(spec.Images)),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(spec.Images) == 0 {
		return result, fmt.Errorf("image list contains no images")
	}
	if result.TargetType != syncTargetRegistry && result.TargetType != syncTargetLocalTar {
		return result, fmt.Errorf("target type must be registry or local_tar")
	}

	engine, err := selectSyncEngine(spec)
	if err != nil {
		return result, err
	}
	result.Engine = engine
	requestedEngine := strings.ToLower(strings.TrimSpace(spec.Engine))
	allowNativeFallback := requestedEngine == "" || requestedEngine == "auto"
	fallbackAttempted := false
	skopeoSucceeded := false

	var storage *storageCandidate
	if result.TargetType == syncTargetLocalTar {
		selected, err := selectSyncOutputRoot(spec)
		if err != nil {
			return result, err
		}
		storage = &selected
		result.Storage = storage
		if err := validateLocalTarOutputNames(selected.Path, spec.Images); err != nil {
			return result, err
		}
		hooks.emit(syncProgressEvent{
			Stage:       "storage",
			Message:     selected.Path,
			TotalImages: len(spec.Images),
		})
	}

	copySession := newRegistryCopySession(hooks)
	copySession.totalImages = len(spec.Images)
	var authFile string
	if result.TargetType == syncTargetRegistry && engine == "skopeo" {
		authFile, err = writeSkopeoAuthFile(spec.Workspace, spec.Source, spec.Target)
		if err != nil {
			return result, err
		}
		defer os.Remove(authFile)
	}

	for index, item := range spec.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		itemResult := syncJobItemResult{Source: item.Source, Target: item.Target, Status: "running"}
		hooks.emit(syncProgressEvent{
			Stage:        "image_start",
			Image:        item.Source,
			Message:      item.Target,
			CurrentImage: index + 1,
			TotalImages:  len(spec.Images),
		})

		var itemErr error
		switch result.TargetType {
		case syncTargetRegistry:
			if engine == "skopeo" {
				var copyResult registryCopyResult
				copyResult, itemErr = syncSkopeoCopy(ctx, spec.Source, spec.Target, item.Source, item.Target, authFile, index+1, len(spec.Images), hooks)
				if itemErr == nil {
					skopeoSucceeded = true
				} else if allowNativeFallback && ctx.Err() == nil {
					skopeoErr := itemErr
					fallbackAttempted = true
					hooks.emit(syncProgressEvent{
						Stage: "fallback", Image: item.Source, Message: "skopeo failed; retrying with native Registry V2",
						CurrentImage: index + 1, TotalImages: len(spec.Images),
					})
					copySession.currentImage = index + 1
					copyResult, itemErr = syncNativeCopy(ctx, spec.Source, spec.Target, item.Source, item.Target, copySession)
					if itemErr != nil {
						itemErr = fmt.Errorf("skopeo failed: %v; native fallback failed: %w", skopeoErr, itemErr)
					}
				}
				itemResult.RegistryCopy = &copyResult
			} else {
				copySession.currentImage = index + 1
				var copyResult registryCopyResult
				copyResult, itemErr = syncNativeCopy(ctx, spec.Source, spec.Target, item.Source, item.Target, copySession)
				itemResult.RegistryCopy = &copyResult
			}
		case syncTargetLocalTar:
			var report exportReport
			report, itemErr = exportImageToLocalTar(ctx, spec.Source, item.Source, item.Target, storage.Path, index+1, len(spec.Images), hooks)
			itemResult.Export = &report
			if report.OutputBase != "" {
				itemResult.Target = report.OutputBase
			}
		}
		if itemErr != nil {
			itemResult.Status = "failed"
			itemResult.Error = itemErr.Error()
			result.Failed++
			hooks.emit(syncProgressEvent{
				Stage:        "image_failed",
				Image:        item.Source,
				Message:      itemErr.Error(),
				CurrentImage: index + 1,
				TotalImages:  len(spec.Images),
			})
		} else {
			itemResult.Status = "done"
			result.Succeeded++
		}
		result.Items = append(result.Items, itemResult)
	}
	result.FinishedAt = time.Now().UTC()
	if fallbackAttempted {
		if skopeoSucceeded {
			result.Engine = "mixed"
		} else {
			result.Engine = "native-fallback"
		}
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("sync completed with %d failed and %d successful images", result.Failed, result.Succeeded)
	}
	hooks.emit(syncProgressEvent{
		Stage:        "complete",
		Message:      "all images completed",
		CurrentImage: len(spec.Images),
		TotalImages:  len(spec.Images),
	})
	return result, nil
}

func selectSyncEngine(spec syncJobSpec) (string, error) {
	if strings.EqualFold(strings.TrimSpace(spec.TargetType), syncTargetLocalTar) {
		return "native", nil
	}
	engine := strings.ToLower(strings.TrimSpace(spec.Engine))
	if engine == "" {
		engine = "auto"
	}
	switch engine {
	case "native":
		return "native", nil
	case "skopeo":
		if _, err := syncExecutableLookPath("skopeo"); err != nil {
			return "", fmt.Errorf("skopeo engine requested but skopeo is not installed on the execution machine")
		}
		if err := validateSkopeoAccessCompatibility(spec.Source, spec.Target); err != nil {
			return "", err
		}
		return "skopeo", nil
	case "auto":
		if _, err := syncExecutableLookPath("skopeo"); err == nil {
			if compatibilityErr := validateSkopeoAccessCompatibility(spec.Source, spec.Target); compatibilityErr == nil {
				return "skopeo", nil
			}
		}
		return "native", nil
	default:
		return "", fmt.Errorf("sync engine must be auto, native, or skopeo")
	}
}

func syncProxyCompatible(source, target string) bool {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	return source == target
}

func validateSkopeoAccessCompatibility(source, target registryAccess) error {
	if !syncProxyCompatible(source.Proxy, target.Proxy) {
		return fmt.Errorf("skopeo engine requires the same source and target proxy; use native")
	}
	sourceHost, sourceErr := registryEndpointIdentity(source.Endpoint)
	targetHost, targetErr := registryEndpointIdentity(target.Endpoint)
	if sourceErr != nil {
		return sourceErr
	}
	if targetErr != nil {
		return targetErr
	}
	if strings.EqualFold(sourceHost, targetHost) && (source.Username != target.Username || source.Password != target.Password) {
		return fmt.Errorf("skopeo cannot use two accounts for the same registry host; use native")
	}
	return nil
}

func selectSyncOutputRoot(spec syncJobSpec) (storageCandidate, error) {
	if strings.TrimSpace(spec.OutputRoot) == "" {
		return selectArchiveStorage(spec.Workspace, spec.StorageRoots)
	}
	root, err := expandUserPath(spec.OutputRoot)
	if err != nil {
		return storageCandidate{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return storageCandidate{}, fmt.Errorf("create output root: %w", err)
	}
	total, available, err := filesystemSpace(root)
	if err != nil {
		return storageCandidate{}, err
	}
	return storageCandidate{Path: root, MountPoint: root, AvailableBytes: available, TotalBytes: total, Recommended: true}, nil
}

func validateLocalTarOutputNames(root string, images []imageListItem) error {
	seen := make(map[string]imageListItem, len(images))
	for _, item := range images {
		if _, err := localTarTargetRef(item.Target); err != nil {
			return fmt.Errorf("line %d target image: %w", item.Line, err)
		}
		name, err := archiveOutputName(item.Target)
		if err != nil {
			return fmt.Errorf("line %d target image: %w", item.Line, err)
		}
		key, err := normalizeOutputReservationKey(filepath.Join(root, name))
		if err != nil {
			return err
		}
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("local_tar targets on lines %d and %d resolve to the same output %s", previous.Line, item.Line, filepath.Join(root, name))
		}
		seen[key] = item
	}
	return nil
}

func exportImageToLocalTar(
	ctx context.Context,
	source registryAccess,
	sourceImage string,
	targetImage string,
	outputRoot string,
	currentImage int,
	totalImages int,
	hooks *syncHooks,
) (exportReport, error) {
	if err := ctx.Err(); err != nil {
		return exportReport{}, err
	}
	ref, err := source.imageRef(sourceImage)
	if err != nil {
		return exportReport{}, err
	}
	client, err := source.client(ctx)
	if err != nil {
		return exportReport{}, err
	}
	platforms, singleManifest, err := resolvePlatforms(client, ref)
	if err != nil {
		return exportReport{}, err
	}
	selected, err := selectPlatforms("all", len(platforms))
	if err != nil {
		return exportReport{}, err
	}
	outputName, err := archiveOutputName(targetImage)
	if err != nil {
		return exportReport{}, err
	}
	outputPath := filepath.Join(outputRoot, outputName)
	archiveRef, err := localTarTargetRef(targetImage)
	if err != nil {
		return exportReport{}, fmt.Errorf("target archive image: %w", err)
	}
	exportHooks := &exportHooks{Progress: func(event progressEvent) {
		hooks.emit(syncProgressEvent{
			Stage:        "archive_" + event.Stage,
			Image:        sourceImage,
			Platform:     event.Platform,
			Message:      event.Message,
			CurrentImage: currentImage,
			TotalImages:  totalImages,
			CurrentBlob:  event.CurrentLayer,
			TotalBlobs:   event.TotalLayers,
			BytesDone:    event.BytesDone,
			BytesTotal:   event.BytesTotal,
			SpeedBPS:     event.SpeedBPS,
			ETASeconds:   event.ETASeconds,
		})
	}}
	return exportSelectedPlatformsAs(client, ref, archiveRef, singleManifest, platforms, selected, outputPath, exportHooks)
}

func localTarTargetRef(target string) (imageRef, error) {
	ref, err := parseImageRef(target)
	if err != nil {
		return imageRef{}, err
	}
	if ref.Digest != "" {
		return imageRef{}, fmt.Errorf("local_tar target must use a tag because docker-load RepoTags cannot represent digest references")
	}
	return ref, nil
}

func archiveOutputName(image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	repository := strings.TrimPrefix(ref.Repository, "library/")
	name := sanitizeComponent(strings.ReplaceAll(repository, "/", "_"))
	tag := ref.Tag
	if tag == "" {
		tag = sanitizeComponent(strings.ReplaceAll(ref.Digest, ":", "_"))
	}
	if tag == "" {
		tag = "latest"
	}
	return name + "_" + sanitizeComponent(tag) + ".tar", nil
}

func writeSkopeoAuthFile(workspace string, source, target registryAccess) (string, error) {
	workspace, err := expandUserPath(workspace)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return "", fmt.Errorf("create execution workspace: %w", err)
	}
	auths := make(map[string]map[string]string)
	for _, access := range []registryAccess{source, target} {
		endpoints, parseErr := skopeoAuthRegistryKeys(access.Endpoint)
		if parseErr != nil {
			return "", parseErr
		}
		if access.Username != "" {
			auth := map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte(access.Username + ":" + access.Password)),
			}
			for _, endpoint := range endpoints {
				auths[endpoint] = auth
			}
		}
	}
	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(workspace, ".dia-skopeo-auth-*.json")
	if err != nil {
		return "", fmt.Errorf("create temporary skopeo auth file: %w", err)
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func copyRegistryImageWithSkopeo(
	ctx context.Context,
	sourceAccess registryAccess,
	targetAccess registryAccess,
	sourceImage string,
	targetImage string,
	authFile string,
	currentImage int,
	totalImages int,
	hooks *syncHooks,
) (registryCopyResult, error) {
	sourceRef, err := sourceAccess.imageRef(sourceImage)
	if err != nil {
		return registryCopyResult{}, err
	}
	targetRef, err := targetAccess.imageRef(targetImage)
	if err != nil {
		return registryCopyResult{}, err
	}
	hooks.emit(syncProgressEvent{
		Stage:        "skopeo",
		Image:        sourceRef.DisplayTag(),
		Message:      "copying all manifests and blobs",
		CurrentImage: currentImage,
		TotalImages:  totalImages,
	})
	args := []string{"copy", "--all", "--preserve-digests", "--src-authfile", authFile, "--dest-authfile", authFile}
	if sourceAccess.Insecure {
		args = append(args, "--src-tls-verify=false")
	}
	if targetAccess.Insecure {
		args = append(args, "--dest-tls-verify=false")
	}
	args = append(args, "docker://"+sourceRef.DisplayTag(), "docker://"+targetRef.DisplayTag())
	cmd := exec.CommandContext(ctx, "skopeo", args...)
	proxy := strings.TrimSpace(sourceAccess.Proxy)
	if proxy == "" {
		proxy = strings.TrimSpace(targetAccess.Proxy)
	} else if targetProxy := strings.TrimSpace(targetAccess.Proxy); targetProxy != "" {
		proxy = targetProxy
	}
	cmd.Env = skopeoEnvironment(os.Environ(), proxy)
	output := &limitedBuffer{limit: 4096}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(output.String())
		return registryCopyResult{}, fmt.Errorf("skopeo copy failed: %w (%s)", err, message)
	}

	sourceClient, err := sourceAccess.client(ctx)
	if err != nil {
		return registryCopyResult{}, err
	}
	targetClient, err := targetAccess.client(ctx)
	if err != nil {
		return registryCopyResult{}, err
	}
	sourceManifest, _, sourceDigest, err := sourceClient.getManifestWithDigest(sourceRef, sourceRef.ManifestReference())
	if err != nil {
		return registryCopyResult{}, fmt.Errorf("verify source after skopeo copy: %w", err)
	}
	targetManifest, _, targetDigest, err := targetClient.getManifestWithDigest(targetRef, targetRef.ManifestReference())
	if err != nil {
		return registryCopyResult{}, fmt.Errorf("verify target after skopeo copy: %w", err)
	}
	if !bytes.Equal(sourceManifest, targetManifest) {
		return registryCopyResult{}, fmt.Errorf("skopeo target manifest does not match source manifest")
	}
	if !strings.EqualFold(sourceDigest, targetDigest) {
		return registryCopyResult{}, fmt.Errorf("skopeo target digest %s does not match source digest %s", targetDigest, sourceDigest)
	}
	return registryCopyResult{Source: sourceRef.DisplayTag(), Target: targetRef.DisplayTag(), Digest: sourceDigest}, nil
}

func skopeoEnvironment(base []string, proxy string) []string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return append([]string(nil), base...)
	}
	blocked := map[string]struct{}{
		"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {},
		"NO_PROXY": {},
	}
	env := make([]string, 0, len(base)+6)
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, remove := blocked[strings.ToUpper(name)]; remove {
				continue
			}
		}
		env = append(env, entry)
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env = append(env, name+"="+proxy)
	}
	return env
}

func urlHost(endpoint string) (string, error) {
	parsed, err := url.Parse(normalizeRegistryEndpoint(endpoint))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid registry endpoint %q", endpoint)
	}
	return parsed.Host, nil
}

func registryEndpointIdentity(endpoint string) (string, error) {
	parsed, err := url.Parse(normalizeRegistryEndpoint(endpoint))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid registry endpoint %q", endpoint)
	}
	return comparableRegistryHost(parsed.Host, parsed.Scheme), nil
}

func skopeoAuthRegistryKeys(endpoint string) ([]string, error) {
	parsed, err := url.Parse(normalizeRegistryEndpoint(endpoint))
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid registry endpoint %q", endpoint)
	}
	host := parsed.Host
	if comparableRegistryHost(host, parsed.Scheme) == dockerHubRegistryHost {
		return []string{
			dockerHubRegistryAlias,
			dockerHubRegistryHost,
			"index.docker.io",
			"https://index.docker.io/v1/",
		}, nil
	}
	return []string{host}, nil
}
