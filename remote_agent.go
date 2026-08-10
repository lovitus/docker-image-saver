package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type remoteAgentOptions struct {
	Workspace string
	Version   string
	Stdin     io.Reader
	Stdout    io.Writer
}

type remoteAgentInfo struct {
	Version      string `json:"version"`
	Protocol     int    `json:"protocol"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Workspace    string `json:"workspace"`
	Skopeo       bool   `json:"skopeo"`
}

type remoteImageRequest struct {
	Registry registryAccess `json:"registry"`
	Image    string         `json:"image"`
	Output   string         `json:"output,omitempty"`
	Selected []string       `json:"selected,omitempty"`
}

type remotePlatform struct {
	ManifestRef  string `json:"manifest_ref"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	Label        string `json:"label"`
}

type remoteImageInspection struct {
	Image     string           `json:"image"`
	Platforms []remotePlatform `json:"platforms"`
}

type remoteHarborRequest struct {
	Registry registryAccess      `json:"registry"`
	Action   harborActionRequest `json:"action"`
}

type remoteStorageRequest struct {
	StorageRoots []string `json:"storage_roots,omitempty"`
}

type remoteFileRequest struct {
	Path         string   `json:"path,omitempty"`
	StorageRoots []string `json:"storage_roots,omitempty"`
	Offset       int64    `json:"offset,omitempty"`
}

type remoteFileEntry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
	Directory  bool      `json:"directory"`
	Symlink    bool      `json:"symlink"`
}

type remoteFileDownloadMeta struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	SHA256 string `json:"sha256"`
}

func runRemoteAgent(opts remoteAgentOptions) error {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	workspace, err := expandUserPath(opts.Workspace)
	if err != nil {
		return err
	}
	if workspace == "" {
		workspace = "."
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create remote workspace: %w", err)
	}
	opts.Workspace = workspace

	reader := newRemoteFrameReader(opts.Stdin)
	writer := newRemoteFrameWriter(opts.Stdout)
	var request remoteRequest
	if err := reader.readJSON(&request); err != nil {
		return err
	}
	if request.Version != remoteProtocolVersion {
		err := fmt.Errorf("unsupported remote protocol version %d", request.Version)
		_ = writer.writeError(request.ID, err)
		return err
	}
	if strings.TrimSpace(request.ID) == "" {
		return fmt.Errorf("remote request ID is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var trailing [1]byte
		_, _ = opts.Stdin.Read(trailing[:])
		cancel()
	}()
	if err := dispatchRemoteRequest(ctx, opts, request, writer); err != nil {
		_ = writer.writeError(request.ID, err)
		return err
	}
	return nil
}

func dispatchRemoteRequest(ctx context.Context, opts remoteAgentOptions, request remoteRequest, writer *remoteFrameWriter) error {
	switch strings.ToLower(strings.TrimSpace(request.Operation)) {
	case "info", "ping":
		_, skopeoErr := execLookPath("skopeo")
		return writer.writeEvent(request.ID, "result", remoteAgentInfo{
			Version:      opts.Version,
			Protocol:     remoteProtocolVersion,
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
			Workspace:    opts.Workspace,
			Skopeo:       skopeoErr == nil,
		})
	case "image.inspect":
		var payload remoteImageRequest
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		result, err := inspectRemoteImage(ctx, payload)
		if err != nil {
			return err
		}
		return writer.writeEvent(request.ID, "result", result)
	case "image.export":
		var payload remoteImageRequest
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		return executeRemoteImageExport(ctx, opts, request.ID, payload, writer)
	case "harbor.action":
		var payload remoteHarborRequest
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		client, err := newHarborClient(payload.Registry)
		if err != nil {
			return err
		}
		result, err := executeHarborAction(ctx, client, payload.Action)
		if err != nil {
			return err
		}
		return writer.writeEvent(request.ID, "result", result)
	case "sync.run":
		var payload syncJobSpec
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		payload.Workspace = opts.Workspace
		if payload.OutputRoot != "" {
			allowedRoots, err := remoteAllowedRoots(opts.Workspace, payload.StorageRoots)
			if err != nil {
				return err
			}
			secureOutput, err := secureRemotePath(payload.OutputRoot, allowedRoots, false)
			if err != nil {
				return err
			}
			payload.OutputRoot = secureOutput
		}
		result, runErr := runSyncJob(ctx, payload, &syncHooks{Progress: func(event syncProgressEvent) {
			_ = writer.writeEvent(request.ID, "progress", event)
		}})
		if err := writer.writeEvent(request.ID, "result", result); err != nil {
			return err
		}
		return runErr
	case "storage.probe":
		var payload remoteStorageRequest
		if len(request.Payload) > 0 {
			if err := decodeRemotePayload(request.Payload, &payload); err != nil {
				return err
			}
		}
		result, err := probeArchiveStorage(opts.Workspace, payload.StorageRoots)
		if err != nil {
			return err
		}
		return writer.writeEvent(request.ID, "result", result)
	case "files.list":
		var payload remoteFileRequest
		if len(request.Payload) > 0 {
			if err := decodeRemotePayload(request.Payload, &payload); err != nil {
				return err
			}
		}
		entries, err := listRemoteFiles(opts.Workspace, payload)
		if err != nil {
			return err
		}
		return writer.writeEvent(request.ID, "result", entries)
	case "files.download":
		var payload remoteFileRequest
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		return streamRemoteFile(opts.Workspace, request.ID, payload, writer)
	case "files.delete":
		var payload remoteFileRequest
		if err := decodeRemotePayload(request.Payload, &payload); err != nil {
			return err
		}
		if err := deleteRemoteFile(opts.Workspace, payload); err != nil {
			return err
		}
		return writer.writeEvent(request.ID, "result", map[string]bool{"ok": true})
	default:
		return fmt.Errorf("unsupported remote operation %q", request.Operation)
	}
}

var execLookPath = func(file string) (string, error) {
	return exec.LookPath(file)
}

func decodeRemotePayload(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		return fmt.Errorf("remote request payload is required")
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode remote request payload: %w", err)
	}
	return nil
}

func inspectRemoteImage(ctx context.Context, request remoteImageRequest) (remoteImageInspection, error) {
	ref, err := request.Registry.imageRef(request.Image)
	if err != nil {
		return remoteImageInspection{}, err
	}
	client, err := request.Registry.client(ctx)
	if err != nil {
		return remoteImageInspection{}, err
	}
	platforms, _, err := resolvePlatforms(client, ref)
	if err != nil {
		return remoteImageInspection{}, err
	}
	result := remoteImageInspection{Image: ref.DisplayTag(), Platforms: make([]remotePlatform, 0, len(platforms))}
	for _, item := range platforms {
		result.Platforms = append(result.Platforms, remotePlatform{
			ManifestRef:  item.ManifestRef,
			OS:           item.Platform.OS,
			Architecture: item.Platform.Architecture,
			Variant:      item.Platform.Variant,
			Label:        item.Platform.String(),
		})
	}
	return result, nil
}

func executeRemoteImageExport(ctx context.Context, opts remoteAgentOptions, requestID string, request remoteImageRequest, writer *remoteFrameWriter) error {
	ref, err := request.Registry.imageRef(request.Image)
	if err != nil {
		return err
	}
	allowedRoots, err := remoteAllowedRoots(opts.Workspace, nil)
	if err != nil {
		return err
	}
	output, err := secureRemotePath(request.Output, allowedRoots, false)
	if err != nil {
		return err
	}
	client, err := request.Registry.client(ctx)
	if err != nil {
		return err
	}
	platforms, singleManifest, err := resolvePlatforms(client, ref)
	if err != nil {
		return err
	}
	var selected *[]string
	if request.Selected != nil {
		selected = &request.Selected
	}
	indices, err := resolveSelectedPlatformRefs(platforms, selected)
	if err != nil {
		return err
	}
	report, err := exportSelectedPlatforms(client, ref, singleManifest, platforms, indices, output, &exportHooks{Progress: func(event progressEvent) {
		_ = writer.writeEvent(requestID, "progress", event)
	}})
	if err != nil {
		return err
	}
	return writer.writeEvent(requestID, "result", report)
}

func remoteAllowedRoots(workspace string, storageRoots []string) ([]string, error) {
	result := make([]string, 0, len(storageRoots)+4)
	workspace, err := expandUserPath(workspace)
	if err != nil {
		return nil, err
	}
	result = append(result, workspace)
	for _, root := range storageRoots {
		expanded, err := expandUserPath(root)
		if err != nil {
			return nil, err
		}
		if expanded != "" {
			result = append(result, expanded)
		}
	}
	if candidates, err := probeArchiveStorage(workspace, storageRoots); err == nil {
		for _, candidate := range candidates {
			result = append(result, candidate.Path)
		}
	}
	return cleanUniquePaths(result), nil
}

func secureRemotePath(requested string, allowedRoots []string, mustExist bool) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("remote path is required")
	}
	if !filepath.IsAbs(requested) {
		if len(allowedRoots) == 0 {
			return "", fmt.Errorf("remote workspace is not configured")
		}
		requested = filepath.Join(allowedRoots[0], requested)
	}
	absPath, err := filepath.Abs(filepath.Clean(requested))
	if err != nil {
		return "", err
	}
	canonical, err := canonicalizeReservationPath(absPath)
	if err != nil {
		return "", err
	}
	if mustExist {
		if _, err := os.Stat(canonical); err != nil {
			return "", err
		}
	}
	for _, root := range allowedRoots {
		rootCanonical, err := canonicalizeReservationPath(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootCanonical, canonical)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("remote path is outside the configured workspace and storage roots")
}

func listRemoteFiles(workspace string, request remoteFileRequest) ([]remoteFileEntry, error) {
	roots, err := remoteAllowedRoots(workspace, request.StorageRoots)
	if err != nil {
		return nil, err
	}
	target := request.Path
	if strings.TrimSpace(target) == "" {
		target = roots[0]
	}
	target, err = secureRemotePath(target, roots, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []remoteFileEntry{remoteFileInfo(target, info, false)}, nil
	}
	directoryEntries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	result := make([]remoteFileEntry, 0, len(directoryEntries))
	for _, entry := range directoryEntries {
		entryPath := filepath.Join(target, entry.Name())
		entryInfo, err := os.Lstat(entryPath)
		if err != nil {
			continue
		}
		result = append(result, remoteFileInfo(entryPath, entryInfo, entryInfo.Mode()&os.ModeSymlink != 0))
	}
	slices.SortFunc(result, func(a, b remoteFileEntry) int {
		if a.Directory != b.Directory {
			if a.Directory {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return result, nil
}

func remoteFileInfo(path string, info os.FileInfo, symlink bool) remoteFileEntry {
	return remoteFileEntry{
		Path:       path,
		Name:       info.Name(),
		Size:       info.Size(),
		Mode:       info.Mode().String(),
		ModifiedAt: info.ModTime().UTC(),
		Directory:  info.IsDir(),
		Symlink:    symlink,
	}
}

func streamRemoteFile(workspace, requestID string, request remoteFileRequest, writer *remoteFrameWriter) error {
	roots, err := remoteAllowedRoots(workspace, request.StorageRoots)
	if err != nil {
		return err
	}
	path, err := secureRemotePath(request.Path, roots, true)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("remote download path is not a regular file")
	}
	if request.Offset < 0 || request.Offset > info.Size() {
		return fmt.Errorf("remote download offset is out of range")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("hash remote file: %w", err)
	}
	meta := remoteFileDownloadMeta{
		Path:   path,
		Name:   filepath.Base(path),
		Size:   info.Size(),
		Offset: request.Offset,
		SHA256: hex.EncodeToString(hasher.Sum(nil)),
	}
	if err := writer.writeEvent(requestID, "meta", meta); err != nil {
		return err
	}
	if _, err := file.Seek(request.Offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, 1<<20)
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			if err := writer.writeFrame(remoteFrameData, buffer[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return writer.writeEvent(requestID, "result", meta)
}

func deleteRemoteFile(workspace string, request remoteFileRequest) error {
	roots, err := remoteAllowedRoots(workspace, request.StorageRoots)
	if err != nil {
		return err
	}
	target, err := secureRemoteEntryPath(request.Path, roots)
	if err != nil {
		return err
	}
	for _, root := range roots {
		root, _ = filepath.Abs(filepath.Clean(root))
		if target == root {
			return fmt.Errorf("refusing to delete a configured storage root")
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("directory deletion is not supported")
	}
	return os.Remove(target)
}

func secureRemoteEntryPath(requested string, allowedRoots []string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("remote path is required")
	}
	if !filepath.IsAbs(requested) {
		if len(allowedRoots) == 0 {
			return "", fmt.Errorf("remote workspace is not configured")
		}
		requested = filepath.Join(allowedRoots[0], requested)
	}
	absPath, err := filepath.Abs(filepath.Clean(requested))
	if err != nil {
		return "", err
	}
	parent, err := canonicalizeReservationPath(filepath.Dir(absPath))
	if err != nil {
		return "", err
	}
	entryPath := filepath.Join(parent, filepath.Base(absPath))
	if _, err := os.Lstat(entryPath); err != nil {
		return "", err
	}
	for _, root := range allowedRoots {
		rootCanonical, err := canonicalizeReservationPath(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootCanonical, entryPath)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return entryPath, nil
		}
	}
	return "", fmt.Errorf("remote path is outside the configured workspace and storage roots")
}
