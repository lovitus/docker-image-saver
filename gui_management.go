package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

type guiCredentialMutation struct {
	ID         string  `json:"id,omitempty"`
	RegistryID string  `json:"registry_id"`
	Name       string  `json:"name"`
	Username   string  `json:"username"`
	Secret     *string `json:"secret,omitempty"`
}

type guiRemoteMutation struct {
	ID                 string   `json:"id,omitempty"`
	Name               string   `json:"name"`
	Address            string   `json:"address"`
	User               string   `json:"user"`
	Port               int      `json:"port,omitempty"`
	AuthMethod         string   `json:"auth_method"`
	KeyPath            string   `json:"key_path,omitempty"`
	HostKeyFingerprint string   `json:"host_key_fingerprint,omitempty"`
	Workspace          string   `json:"workspace"`
	DiaPath            string   `json:"dia_path,omitempty"`
	StorageRoots       []string `json:"storage_roots,omitempty"`
	Secret             *string  `json:"secret,omitempty"`
}

type guiImageListMutation struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

type guiRemoteTestRequest struct {
	Fingerprint string `json:"fingerprint,omitempty"`
}

type guiRemoteTestResponse struct {
	OK                bool             `json:"ok"`
	NeedsConfirmation bool             `json:"needs_confirmation,omitempty"`
	Fingerprint       string           `json:"fingerprint"`
	Info              *remoteAgentInfo `json:"info,omitempty"`
}

type guiHarborActionRequest struct {
	RegistryID   string              `json:"registry_id"`
	CredentialID string              `json:"credential_id,omitempty"`
	Execution    string              `json:"execution,omitempty"`
	RemoteID     string              `json:"remote_id,omitempty"`
	Action       harborActionRequest `json:"action"`
}

type guiSyncRequest struct {
	RemoteID           string `json:"remote_id,omitempty"`
	SourceRegistryID   string `json:"source_registry_id"`
	SourceCredentialID string `json:"source_credential_id,omitempty"`
	TargetType         string `json:"target_type"`
	TargetRegistryID   string `json:"target_registry_id,omitempty"`
	TargetCredentialID string `json:"target_credential_id,omitempty"`
	ImageListID        string `json:"image_list_id,omitempty"`
	ImagesContent      string `json:"images_content,omitempty"`
	Engine             string `json:"engine,omitempty"`
	OutputRoot         string `json:"output_root,omitempty"`
}

const anonymousCredentialID = "__anonymous__"

type guiRemoteClient interface {
	probeHostKey(context.Context) (string, error)
	test(context.Context) (remoteAgentInfo, error)
	probeStorage(context.Context, []string) ([]storageCandidate, error)
	listFiles(context.Context, remoteFileRequest) ([]remoteFileEntry, error)
	deleteFile(context.Context, remoteFileRequest) error
	downloadFile(context.Context, remoteFileRequest, io.Writer, func(remoteFileDownloadMeta) error) (remoteFileDownloadMeta, error)
	harborAction(context.Context, remoteHarborRequest) (json.RawMessage, error)
	runSync(context.Context, syncJobSpec, func(syncProgressEvent)) (syncJobResult, error)
}

func (s *guiServer) newRemoteClient(profile remoteProfile, secret string) (guiRemoteClient, error) {
	if s.remoteFactory == nil {
		return nil, fmt.Errorf("remote execution is not configured")
	}
	return s.remoteFactory(profile, secret, s.version)
}

func (s *guiServer) requireConfigStore() (*configStore, error) {
	if s.configErr != nil {
		return nil, s.configErr
	}
	if s.configStore == nil {
		return nil, fmt.Errorf("persistent GUI configuration is not initialized")
	}
	return s.configStore, nil
}

func (s *guiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/settings" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store, err := s.requireConfigStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, store.snapshot())
}

func (s *guiServer) handleSettingsResource(w http.ResponseWriter, r *http.Request) {
	store, err := s.requireConfigStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	pathParts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/settings/"), "/"), "/")
	if len(pathParts) == 0 || len(pathParts) > 2 || pathParts[0] == "" {
		http.NotFound(w, r)
		return
	}
	resource := pathParts[0]
	id := ""
	if len(pathParts) > 1 {
		id = pathParts[1]
	}
	if resource == "default-remote" {
		if len(pathParts) != 1 {
			http.NotFound(w, r)
			return
		}
	} else if (r.Method == http.MethodPost && len(pathParts) != 1) ||
		((r.Method == http.MethodPut || r.Method == http.MethodDelete) && len(pathParts) != 2) {
		http.NotFound(w, r)
		return
	}

	switch resource {
	case "registries":
		s.handleRegistryMutation(w, r, store, id)
	case "credentials":
		s.handleCredentialMutation(w, r, store, id)
	case "remotes":
		s.handleRemoteMutation(w, r, store, id)
	case "image-lists":
		s.handleImageListMutation(w, r, store, id)
	case "default-remote":
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := decodeGUIJSON(r, &payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := store.setDefaultRemote(strings.TrimSpace(payload.ID)); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, store.snapshot())
	default:
		http.NotFound(w, r)
	}
}

func (s *guiServer) handleRegistryMutation(w http.ResponseWriter, r *http.Request, store *configStore, id string) {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var profile registryProfile
		if err := decodeGUIJSON(r, &profile); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if r.Method == http.MethodPut {
			if id == "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("registry ID is required"))
				return
			}
			profile.ID = id
		} else {
			profile.ID = ""
		}
		result, err := store.upsertRegistry(profile)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("registry ID is required"))
			return
		}
		if err := store.deleteRegistry(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *guiServer) handleCredentialMutation(w http.ResponseWriter, r *http.Request, store *configStore, id string) {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var payload guiCredentialMutation
		if err := decodeGUIJSON(r, &payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if r.Method == http.MethodPut {
			if id == "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("credential ID is required"))
				return
			}
			payload.ID = id
		} else {
			payload.ID = ""
		}
		result, err := store.upsertCredential(credentialProfile{
			ID: payload.ID, RegistryID: payload.RegistryID, Name: payload.Name, Username: payload.Username,
		}, payload.Secret)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("credential ID is required"))
			return
		}
		if err := store.deleteCredential(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *guiServer) handleRemoteMutation(w http.ResponseWriter, r *http.Request, store *configStore, id string) {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var payload guiRemoteMutation
		if err := decodeGUIJSON(r, &payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if r.Method == http.MethodPut {
			if id == "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("remote ID is required"))
				return
			}
			payload.ID = id
		} else {
			payload.ID = ""
		}
		result, err := store.upsertRemote(remoteProfile{
			ID: payload.ID, Name: payload.Name, Address: payload.Address, User: payload.User,
			Port: payload.Port, AuthMethod: payload.AuthMethod, KeyPath: payload.KeyPath,
			HostKeyFingerprint: payload.HostKeyFingerprint, Workspace: payload.Workspace,
			DiaPath: payload.DiaPath, StorageRoots: payload.StorageRoots,
		}, payload.Secret)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("remote ID is required"))
			return
		}
		if err := store.deleteRemote(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *guiServer) handleImageListMutation(w http.ResponseWriter, r *http.Request, store *configStore, id string) {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var payload guiImageListMutation
		if err := decodeGUIJSON(r, &payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if r.Method == http.MethodPut {
			if id == "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Errorf("image list ID is required"))
				return
			}
			payload.ID = id
		} else {
			payload.ID = ""
		}
		result, err := store.upsertImageList(imageListProfile{ID: payload.ID, Name: payload.Name, Content: payload.Content})
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("image list ID is required"))
			return
		}
		if err := store.deleteImageList(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *guiServer) handleRemoteResource(w http.ResponseWriter, r *http.Request) {
	store, err := s.requireConfigStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/remotes/"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	remoteID := parts[0]
	action := parts[1]
	profile, secret, err := store.remote(remoteID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	client, err := s.newRemoteClient(profile, secret)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	switch action {
	case "test":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload guiRemoteTestRequest
		if r.ContentLength != 0 {
			if err := decodeGUIJSON(r, &payload); err != nil {
				writeJSONError(w, http.StatusBadRequest, err)
				return
			}
		}
		if profile.HostKeyFingerprint == "" {
			fingerprint, err := client.probeHostKey(r.Context())
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, err)
				return
			}
			if strings.TrimSpace(payload.Fingerprint) == "" {
				writeJSON(w, http.StatusConflict, guiRemoteTestResponse{NeedsConfirmation: true, Fingerprint: fingerprint})
				return
			}
			if !secureTokenEqual(strings.TrimSpace(payload.Fingerprint), fingerprint) {
				writeJSONError(w, http.StatusConflict, fmt.Errorf("confirmed SSH fingerprint does not match the server"))
				return
			}
			profile.HostKeyFingerprint = fingerprint
			if _, err := store.upsertRemote(profile, nil); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err)
				return
			}
			client, err = s.newRemoteClient(profile, secret)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, err)
				return
			}
		}
		info, err := client.test(r.Context())
		if err != nil {
			var mismatch *sshHostKeyMismatchError
			status := http.StatusBadGateway
			if errors.As(err, &mismatch) {
				status = http.StatusConflict
			}
			writeJSONError(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, guiRemoteTestResponse{OK: true, Fingerprint: profile.HostKeyFingerprint, Info: &info})
	case "storage":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := client.probeStorage(r.Context(), profile.StorageRoots)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case "files":
		s.handleRemoteFiles(w, r, client, profile)
	case "download":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleRemoteDownload(w, r, client, profile)
	default:
		http.NotFound(w, r)
	}
}

func (s *guiServer) handleRemoteFiles(w http.ResponseWriter, r *http.Request, client guiRemoteClient, profile remoteProfile) {
	request := remoteFileRequest{Path: r.URL.Query().Get("path"), StorageRoots: profile.StorageRoots}
	switch r.Method {
	case http.MethodGet:
		result, err := client.listFiles(r.Context(), request)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if err := decodeGUIJSON(r, &request); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		request.StorageRoots = profile.StorageRoots
		if err := client.deleteFile(r.Context(), request); err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *guiServer) handleRemoteDownload(w http.ResponseWriter, r *http.Request, client guiRemoteClient, profile remoteProfile) {
	request := remoteFileRequest{Path: r.URL.Query().Get("path"), StorageRoots: profile.StorageRoots}
	started := false
	_, err := client.downloadFile(r.Context(), request, w, func(meta remoteFileDownloadMeta) error {
		mediaType := "application/octet-stream"
		if strings.EqualFold(filepath.Ext(meta.Name), ".tar") {
			mediaType = "application/x-tar"
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Dia-SHA256", meta.SHA256)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.Name}))
		started = true
		return nil
	})
	if err != nil && !started {
		writeJSONError(w, http.StatusBadGateway, err)
	}
}

func (s *guiServer) handleHarborAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request guiHarborActionRequest
	if err := decodeGUIJSON(r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	store, err := s.requireConfigStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	profile, err := store.registry(strings.TrimSpace(request.RegistryID))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if profile.Kind != "harbor" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("selected profile is not a Harbor server"))
		return
	}
	access, err := s.registryAccess(store, profile, request.CredentialID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	execution := strings.ToLower(strings.TrimSpace(request.Execution))
	if execution == "" {
		// Requests from versions before direct Harbor support were always remote.
		execution = "remote"
	}
	if execution == "local" {
		if strings.TrimSpace(request.RemoteID) != "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("remote_id must be empty for local Harbor execution"))
			return
		}
		client, err := newHarborClient(access)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		result, err := executeHarborAction(r.Context(), client, request.Action)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if execution != "remote" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("Harbor execution must be local or remote"))
		return
	}
	remoteID := strings.TrimSpace(request.RemoteID)
	if remoteID == "" {
		remoteID = store.snapshot().DefaultRemoteID
	}
	if remoteID == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("an execution machine must be selected for Harbor operations"))
		return
	}
	remote, secret, err := store.remote(remoteID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	remoteClient, err := s.newRemoteClient(remote, secret)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	result, err := remoteClient.harborAction(r.Context(), remoteHarborRequest{Registry: access, Action: request.Action})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *guiServer) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request guiSyncRequest
	if err := decodeGUIJSON(r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	store, err := s.requireConfigStore()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	remoteID := strings.TrimSpace(request.RemoteID)
	if remoteID == "" {
		remoteID = store.snapshot().DefaultRemoteID
	}
	if remoteID == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("an execution machine must be selected"))
		return
	}
	remote, remoteSecret, err := store.remote(remoteID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	sourceProfile, err := store.registry(strings.TrimSpace(request.SourceRegistryID))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("source registry: %w", err))
		return
	}
	source, err := s.registryAccess(store, sourceProfile, request.SourceCredentialID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	targetType := strings.ToLower(strings.TrimSpace(request.TargetType))
	var target registryAccess
	if targetType == syncTargetRegistry {
		targetProfile, err := store.registry(strings.TrimSpace(request.TargetRegistryID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("target registry: %w", err))
			return
		}
		target, err = s.registryAccess(store, targetProfile, request.TargetCredentialID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
	} else if targetType != syncTargetLocalTar {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("target type must be registry or local_tar"))
		return
	}

	listContent := request.ImagesContent
	if strings.TrimSpace(listContent) == "" {
		list, err := store.imageList(strings.TrimSpace(request.ImageListID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		listContent = list.Content
	}
	images, err := parseImageList(listContent)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	remoteClient, err := s.newRemoteClient(remote, remoteSecret)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	task, err := s.taskStore.newRemoteTask(guiTaskInput{
		Kind:             "sync",
		RemoteID:         remoteID,
		SourceRegistryID: sourceProfile.ID,
		TargetRegistryID: request.TargetRegistryID,
		ImageListID:      request.ImageListID,
	}, remoteID)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, guiExportResponse{TaskID: task.snapshot().ID})

	ctx, cancel := context.WithCancel(context.Background())
	task.setCancel(cancel)
	spec := syncJobSpec{
		Source:       source,
		TargetType:   targetType,
		Target:       target,
		Images:       images,
		Engine:       request.Engine,
		Workspace:    remote.Workspace,
		StorageRoots: remote.StorageRoots,
		OutputRoot:   request.OutputRoot,
	}
	go func() {
		defer cancel()
		defer task.releaseOutput()
		result, runErr := remoteClient.runSync(ctx, spec, task.handleSyncProgress)
		if runErr != nil {
			task.failSync(result, runErr)
			return
		}
		task.completeSync(result)
	}()
}

func (s *guiServer) registryAccess(store *configStore, profile registryProfile, credentialID string) (registryAccess, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == anonymousCredentialID {
		credentialID = ""
		return registryAccess{
			Endpoint: profile.Endpoint, Namespace: profile.Namespace, Proxy: profile.Proxy, Insecure: profile.Insecure,
		}, nil
	}
	if credentialID == "" {
		credentialID = profile.DefaultCredentialID
	}
	access := registryAccess{
		Endpoint: profile.Endpoint, Namespace: profile.Namespace, Proxy: profile.Proxy, Insecure: profile.Insecure,
	}
	if credentialID == "" {
		return access, nil
	}
	credential, secret, err := store.credential(credentialID)
	if err != nil {
		return registryAccess{}, err
	}
	if credential.RegistryID != profile.ID {
		return registryAccess{}, fmt.Errorf("selected account does not belong to the registry")
	}
	access.Username = credential.Username
	access.Password = secret
	return access, nil
}

func decodeGUIJSON(r *http.Request, target any) error {
	reader := io.LimitReader(r.Body, 8<<20)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON value")
		}
		return err
	}
	return nil
}
