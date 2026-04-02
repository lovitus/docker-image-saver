package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var guiAssets embed.FS

type guiOptions struct {
	Image     string
	Output    string
	Proxy     string
	Username  string
	Password  string
	Insecure  bool
	NoBrowser bool
	Stdout    io.Writer
	Stderr    io.Writer
}

type guiInspectRequest struct {
	Image            string `json:"image"`
	Proxy            string `json:"proxy,omitempty"`
	Username         string `json:"username,omitempty"`
	Password         string `json:"password,omitempty"`
	UseSavedPassword bool   `json:"use_saved_password,omitempty"`
	Insecure         bool   `json:"insecure,omitempty"`
}

type guiPlatform struct {
	Index        int    `json:"index"`
	DisplayIndex int    `json:"display_index"`
	ManifestRef  string `json:"manifest_ref"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	Label        string `json:"label"`
}

type guiInspectResponse struct {
	Image         string        `json:"image"`
	DefaultOutput string        `json:"default_output"`
	Platforms     []guiPlatform `json:"platforms"`
}

type guiBootstrapData struct {
	Version          string `json:"version"`
	Image            string `json:"image"`
	Output           string `json:"output"`
	OutputExplicit   bool   `json:"output_explicit"`
	Proxy            string `json:"proxy"`
	Username         string `json:"username"`
	Insecure         bool   `json:"insecure"`
	HasSavedPassword bool   `json:"has_saved_password"`
}

type guiExportRequest struct {
	Image            string    `json:"image"`
	Output           string    `json:"output"`
	Proxy            string    `json:"proxy,omitempty"`
	Username         string    `json:"username,omitempty"`
	Password         string    `json:"password,omitempty"`
	UseSavedPassword bool      `json:"use_saved_password,omitempty"`
	Insecure         bool      `json:"insecure,omitempty"`
	Selected         *[]string `json:"selected,omitempty"`
}

type guiExportResponse struct {
	TaskID string `json:"task_id"`
}

type guiTaskInput struct {
	Image    string   `json:"image"`
	Output   string   `json:"output"`
	Proxy    string   `json:"proxy,omitempty"`
	Username string   `json:"username,omitempty"`
	Insecure bool     `json:"insecure,omitempty"`
	Selected []string `json:"selected"`
}

type guiTaskSnapshot struct {
	ID                 string       `json:"id"`
	Status             string       `json:"status"`
	Input              guiTaskInput `json:"input"`
	Stage              string       `json:"stage,omitempty"`
	Message            string       `json:"message,omitempty"`
	Platform           string       `json:"platform,omitempty"`
	CurrentLayer       int          `json:"current_layer,omitempty"`
	TotalLayers        int          `json:"total_layers,omitempty"`
	BytesDone          int64        `json:"bytes_done,omitempty"`
	BytesTotal         int64        `json:"bytes_total,omitempty"`
	SpeedBPS           float64      `json:"speed_bps,omitempty"`
	ETASeconds         int64        `json:"eta_seconds,omitempty"`
	OutputFiles        []string     `json:"output_files,omitempty"`
	PlatformIndexPath  string       `json:"platform_index_path,omitempty"`
	DockerLoadCommands []string     `json:"docker_load_commands,omitempty"`
	Error              string       `json:"error,omitempty"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

type guiServer struct {
	version   string
	options   guiOptions
	taskStore *guiTaskStore
	inspectFn func(guiInspectRequest) (guiInspectResponse, error)
	planFn    func(guiExportRequest) ([]string, error)
	exportFn  func(guiExportRequest, *exportHooks) (exportReport, error)
}

func runGUI(version string, opts guiOptions) error {
	server := newGUIServer(version, opts)
	httpServer := &http.Server{
		Handler:           server.routes(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for GUI: %w", err)
	}
	defer listener.Close()

	address := "http://" + listener.Addr().String()
	logf(opts.Stdout, "GUI: %s\n", address)
	logf(opts.Stdout, "Press Ctrl+C to stop.\n")

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	if !opts.NoBrowser {
		if err := openBrowser(address); err != nil {
			logf(opts.Stderr, "warning: unable to open browser automatically: %v\n", err)
			logf(opts.Stdout, "Open manually: %s\n", address)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newGUIServer(version string, opts guiOptions) *guiServer {
	server := &guiServer{
		version:   version,
		options:   opts,
		taskStore: newGUITaskStore(),
	}
	server.inspectFn = server.inspect
	server.planFn = server.planExportOutputs
	server.exportFn = server.export
	return server
}

func (s *guiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/inspect", s.handleInspect)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/tasks/", s.handleTask)
	return mux
}

func (s *guiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := guiAssets.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bootstrap := guiBootstrapData{
		Version:          s.version,
		Image:            strings.TrimSpace(s.options.Image),
		Output:           strings.TrimSpace(s.options.Output),
		OutputExplicit:   strings.TrimSpace(s.options.Output) != "",
		Proxy:            strings.TrimSpace(s.options.Proxy),
		Username:         strings.TrimSpace(s.options.Username),
		Insecure:         s.options.Insecure,
		HasSavedPassword: strings.TrimSpace(s.options.Password) != "",
	}
	if bootstrap.Image == "" {
		bootstrap.Image = "alpine:latest"
	}
	if bootstrap.Output == "" {
		defaultOut, derr := defaultOutputTar(bootstrap.Image)
		if derr == nil {
			bootstrap.Output = defaultOut
		}
	}
	bootstrapJSON, err := json.Marshal(bootstrap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.ReplaceAll(string(data), "__DIA_BOOTSTRAP_JSON__", template.JSEscapeString(string(bootstrapJSON)))
	_, _ = io.WriteString(w, page)
}

func (s *guiServer) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req guiInspectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req = s.applyInspectDefaults(req)
	resp, err := s.inspectFn(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *guiServer) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req guiExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req = s.applyExportDefaults(req)
	if strings.TrimSpace(req.Image) == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("image is required"))
		return
	}
	if strings.TrimSpace(req.Output) == "" {
		defaultOut, err := defaultOutputTar(req.Image)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		req.Output = defaultOut
	}
	if req.Selected != nil && len(*req.Selected) == 0 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("at least one platform must be selected"))
		return
	}
	reservationPaths, err := s.planFn(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	var selected []string
	if req.Selected != nil {
		selected = append([]string(nil), (*req.Selected)...)
	}

	task, err := s.taskStore.newTaskWithOutputs(guiTaskInput{
		Image:    req.Image,
		Output:   req.Output,
		Proxy:    req.Proxy,
		Username: req.Username,
		Insecure: req.Insecure,
		Selected: selected,
	}, reservationPaths)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, guiExportResponse{TaskID: task.snapshot().ID})

	go func() {
		defer task.releaseOutput()
		hooks := &exportHooks{
			Progress: task.handleProgress,
		}
		report, err := s.exportFn(req, hooks)
		if err != nil {
			task.fail(err)
			return
		}
		task.complete(report)
	}()
}

func (s *guiServer) applyInspectDefaults(req guiInspectRequest) guiInspectRequest {
	if req.UseSavedPassword && strings.TrimSpace(req.Password) == "" {
		req.Password = s.options.Password
	}
	return req
}

func (s *guiServer) applyExportDefaults(req guiExportRequest) guiExportRequest {
	if req.UseSavedPassword && strings.TrimSpace(req.Password) == "" {
		req.Password = s.options.Password
	}
	return req
}

func (s *guiServer) handleTask(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	task := s.taskStore.get(parts[0])
	if task == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, task.snapshot())
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		s.handleTaskEvents(w, r, task)
		return
	}
	http.NotFound(w, r)
}

func (s *guiServer) handleTaskEvents(w http.ResponseWriter, r *http.Request, task *guiTask) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub, cancel := task.subscribe()
	defer cancel()
	if err := sendSSE(w, task.snapshot()); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-sub.notify:
			if !ok {
				return
			}
			if err := sendSSE(w, task.snapshot()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *guiServer) inspect(req guiInspectRequest) (guiInspectResponse, error) {
	ref, err := parseImageRef(req.Image)
	if err != nil {
		return guiInspectResponse{}, err
	}
	client, err := newRegistryClient(req.Proxy, req.Username, req.Password, req.Insecure)
	if err != nil {
		return guiInspectResponse{}, err
	}
	platforms, _, err := resolvePlatforms(client, ref)
	if err != nil {
		return guiInspectResponse{}, err
	}
	defaultOut, err := defaultOutputTar(req.Image)
	if err != nil {
		return guiInspectResponse{}, err
	}
	response := guiInspectResponse{
		Image:         ref.DisplayTag(),
		DefaultOutput: defaultOut,
		Platforms:     make([]guiPlatform, 0, len(platforms)),
	}
	for i, platformOption := range platforms {
		response.Platforms = append(response.Platforms, guiPlatform{
			Index:        i,
			DisplayIndex: i + 1,
			ManifestRef:  platformOption.ManifestRef,
			OS:           platformOption.Platform.OS,
			Architecture: platformOption.Platform.Architecture,
			Variant:      platformOption.Platform.Variant,
			Label:        platformOption.Platform.String(),
		})
	}
	return response, nil
}

func (s *guiServer) export(req guiExportRequest, hooks *exportHooks) (exportReport, error) {
	ref, err := parseImageRef(req.Image)
	if err != nil {
		return exportReport{}, err
	}
	client, err := newRegistryClient(req.Proxy, req.Username, req.Password, req.Insecure)
	if err != nil {
		return exportReport{}, err
	}
	platforms, singleManifest, err := resolvePlatforms(client, ref)
	if err != nil {
		return exportReport{}, err
	}
	if req.Selected != nil && len(*req.Selected) == 0 {
		return exportReport{}, fmt.Errorf("at least one platform must be selected")
	}
	selected, err := resolveSelectedPlatformRefs(platforms, req.Selected)
	if err != nil {
		return exportReport{}, err
	}
	return exportSelectedPlatforms(client, ref, singleManifest, platforms, selected, req.Output, hooks)
}

func (s *guiServer) planExportOutputs(req guiExportRequest) ([]string, error) {
	ref, err := parseImageRef(req.Image)
	if err != nil {
		return nil, err
	}
	client, err := newRegistryClient(req.Proxy, req.Username, req.Password, req.Insecure)
	if err != nil {
		return nil, err
	}
	platforms, _, err := resolvePlatforms(client, ref)
	if err != nil {
		return nil, err
	}
	selected, err := resolveSelectedPlatformRefs(platforms, req.Selected)
	if err != nil {
		return nil, err
	}
	outputs, err := perPlatformOutputs(req.Output, platforms, selected)
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, platformIndexPath(req.Output))
	return normalizeOutputReservationKeys(outputs)
}

func resolveSelectedPlatformRefs(platforms []platformOption, selected *[]string) ([]int, error) {
	if selected == nil {
		return selectPlatforms("all", len(platforms))
	}
	indices := make([]int, 0, len(*selected))
	indexByRef := make(map[string]int, len(platforms))
	for i, platform := range platforms {
		indexByRef[platform.ManifestRef] = i
	}
	seen := make(map[int]struct{}, len(*selected))
	for _, manifestRef := range *selected {
		manifestRef = strings.TrimSpace(manifestRef)
		if manifestRef == "" {
			return nil, fmt.Errorf("selected platform identifier is empty")
		}
		idx, ok := indexByRef[manifestRef]
		if !ok {
			return nil, fmt.Errorf("selected platform %s is no longer available; re-inspect before exporting", manifestRef)
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		indices = append(indices, idx)
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("at least one platform must be selected")
	}
	return indices, nil
}

type guiTaskStore struct {
	mu            sync.RWMutex
	tasks         map[string]*guiTask
	activeOutputs map[string]string
	taskRetention time.Duration
}

func newGUITaskStore() *guiTaskStore {
	return &guiTaskStore{
		tasks:         make(map[string]*guiTask),
		activeOutputs: make(map[string]string),
		taskRetention: 10 * time.Minute,
	}
}

func (s *guiTaskStore) newTask(input guiTaskInput) (*guiTask, error) {
	return s.newTaskWithOutputs(input, []string{input.Output})
}

func (s *guiTaskStore) newTaskWithOutputs(input guiTaskInput, outputs []string) (*guiTask, error) {
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	outputKeys, err := normalizeOutputReservationKeys(outputs)
	if err != nil {
		return nil, err
	}
	task := &guiTask{
		snapshotValue: guiTaskSnapshot{
			ID:        id,
			Status:    "running",
			Input:     input,
			UpdatedAt: time.Now().UTC(),
		},
		subscribers: make(map[*guiSubscriber]struct{}),
		store:       s,
		outputKeys:  outputKeys,
	}
	s.mu.Lock()
	for _, outputKey := range outputKeys {
		if owner, exists := s.activeOutputs[outputKey]; exists {
			s.mu.Unlock()
			return nil, fmt.Errorf("output path is already in use by task %s: %s", owner, outputKey)
		}
	}
	for _, outputKey := range outputKeys {
		s.activeOutputs[outputKey] = id
	}
	s.tasks[id] = task
	s.mu.Unlock()
	task.publish()
	return task, nil
}

func normalizeOutputReservationKey(output string) (string, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return "", fmt.Errorf("output path is required")
	}
	absPath, err := filepath.Abs(filepath.Clean(output))
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	canonicalPath, err := canonicalizeReservationPath(absPath)
	if err != nil {
		return "", err
	}
	foldCase, err := shouldFoldReservationPathCase(canonicalPath)
	if err != nil {
		return "", err
	}
	if foldCase {
		canonicalPath = strings.ToLower(canonicalPath)
	}
	return canonicalPath, nil
}

func canonicalizeReservationPath(absPath string) (string, error) {
	return canonicalizeReservationPathWithSeen(absPath, make(map[string]struct{}))
}

func canonicalizeReservationPathWithSeen(absPath string, seen map[string]struct{}) (string, error) {
	if _, ok := seen[absPath]; ok {
		return "", fmt.Errorf("resolve output path symlinks: symlink cycle detected")
	}
	seen[absPath] = struct{}{}
	info, err := os.Lstat(absPath)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(absPath)
			if err != nil {
				return "", fmt.Errorf("read output path symlink: %w", err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(absPath), target)
			}
			target, err = filepath.Abs(filepath.Clean(target))
			if err != nil {
				return "", fmt.Errorf("resolve output path symlink target: %w", err)
			}
			return canonicalizeReservationPathWithSeen(target, seen)
		}
		resolved, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", fmt.Errorf("resolve output path symlinks: %w", err)
		}
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat output path: %w", err)
	}
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	canonicalDir, err := canonicalizeReservationDir(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(canonicalDir, base), nil
}

func shouldFoldReservationPathCase(path string) (bool, error) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return false, nil
	}
	dir, err := canonicalizeReservationDir(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	probeDir, err := nearestExistingDirectory(dir)
	if err != nil {
		return false, err
	}
	return probeDirectoryCaseInsensitive(probeDir)
}

func probeDirectoryCaseInsensitive(dir string) (bool, error) {
	probeFile, err := os.CreateTemp(dir, "diaCaseProbe-*.tmp")
	if err != nil {
		return false, fmt.Errorf("probe output directory case sensitivity: %w", err)
	}
	probePath := probeFile.Name()
	if err := probeFile.Close(); err != nil {
		_ = os.Remove(probePath)
		return false, fmt.Errorf("close output directory case probe: %w", err)
	}
	defer os.Remove(probePath)

	aliasPath := filepath.Join(dir, swapASCIIFileCase(filepath.Base(probePath)))
	if aliasPath == probePath {
		return false, nil
	}
	_, err = os.Stat(aliasPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat output directory case probe alias: %w", err)
}

func swapASCIIFileCase(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func nearestExistingDirectory(dir string) (string, error) {
	current := filepath.Clean(dir)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("output ancestor is not a directory: %s", current)
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat output ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil
		}
		current = parent
	}
}

func canonicalizeReservationDir(dir string) (string, error) {
	current := filepath.Clean(dir)
	missing := make([]string, 0, 4)
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("resolve output directory symlinks: %w", err)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat output directory: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			for i := len(missing) - 1; i >= 0; i-- {
				current = filepath.Join(current, missing[i])
			}
			return current, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func normalizeOutputReservationKeys(outputs []string) ([]string, error) {
	if len(outputs) == 0 {
		return nil, fmt.Errorf("output path is required")
	}
	keys := make([]string, 0, len(outputs))
	seen := make(map[string]struct{}, len(outputs))
	for _, output := range outputs {
		key, err := normalizeOutputReservationKey(output)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *guiTaskStore) get(id string) *guiTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id]
}

func (s *guiTaskStore) remove(id string, task *guiTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.tasks[id]; ok && current == task {
		delete(s.tasks, id)
	}
}

type guiTask struct {
	mu            sync.RWMutex
	snapshotValue guiTaskSnapshot
	subscribers   map[*guiSubscriber]struct{}
	store         *guiTaskStore
	outputKeys    []string
	released      bool
	cleanupSet    bool
}

type guiSubscriber struct {
	notify chan struct{}
}

func (t *guiTask) snapshot() guiTaskSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snapshotValue
}

func (t *guiTask) subscribe() (*guiSubscriber, func()) {
	sub := &guiSubscriber{notify: make(chan struct{}, 1)}
	t.mu.Lock()
	t.subscribers[sub] = struct{}{}
	t.mu.Unlock()
	return sub, func() {
		t.mu.Lock()
		if _, ok := t.subscribers[sub]; ok {
			delete(t.subscribers, sub)
		}
		t.mu.Unlock()
	}
}

func (t *guiTask) publish() {
	t.mu.RLock()
	subscribers := make([]*guiSubscriber, 0, len(t.subscribers))
	for sub := range t.subscribers {
		subscribers = append(subscribers, sub)
	}
	t.mu.RUnlock()
	for _, sub := range subscribers {
		select {
		case sub.notify <- struct{}{}:
		default:
		}
	}
}

func (t *guiTask) releaseOutput() {
	t.mu.Lock()
	if t.released {
		t.mu.Unlock()
		return
	}
	t.released = true
	store := t.store
	outputKeys := append([]string(nil), t.outputKeys...)
	taskID := t.snapshotValue.ID
	t.mu.Unlock()

	if store == nil || len(outputKeys) == 0 {
		return
	}
	store.mu.Lock()
	for _, outputKey := range outputKeys {
		if owner, ok := store.activeOutputs[outputKey]; ok && owner == taskID {
			delete(store.activeOutputs, outputKey)
		}
	}
	store.mu.Unlock()
}

func (t *guiTask) update(updateFn func(*guiTaskSnapshot)) {
	t.mu.Lock()
	updateFn(&t.snapshotValue)
	t.snapshotValue.UpdatedAt = time.Now().UTC()
	t.mu.Unlock()
	t.publish()
}

func (t *guiTask) scheduleCleanup() {
	t.mu.Lock()
	if t.cleanupSet {
		t.mu.Unlock()
		return
	}
	t.cleanupSet = true
	store := t.store
	taskID := t.snapshotValue.ID
	t.mu.Unlock()

	if store == nil || store.taskRetention <= 0 {
		return
	}
	time.AfterFunc(store.taskRetention, func() {
		store.remove(taskID, t)
	})
}

func (t *guiTask) handleProgress(event progressEvent) {
	t.update(func(snapshot *guiTaskSnapshot) {
		platformChanged := event.Platform != "" && snapshot.Platform != "" && event.Platform != snapshot.Platform
		if platformChanged {
			snapshot.CurrentLayer = 0
			snapshot.TotalLayers = 0
			snapshot.BytesDone = 0
			snapshot.BytesTotal = 0
			snapshot.SpeedBPS = 0
			snapshot.ETASeconds = 0
		}
		snapshot.Stage = event.Stage
		snapshot.Message = event.Message
		snapshot.Platform = event.Platform
		if event.CurrentLayer > 0 || event.TotalLayers > 0 {
			snapshot.CurrentLayer = event.CurrentLayer
			snapshot.TotalLayers = event.TotalLayers
		}
		if event.BytesDone > 0 || event.BytesTotal > 0 || event.CurrentLayer > 0 || event.TotalLayers > 0 {
			snapshot.BytesDone = event.BytesDone
			snapshot.BytesTotal = event.BytesTotal
		}
		if event.SpeedBPS > 0 {
			snapshot.SpeedBPS = event.SpeedBPS
		}
		if event.ETASeconds > 0 {
			snapshot.ETASeconds = event.ETASeconds
		}
		if event.Error != "" {
			snapshot.Error = event.Error
			snapshot.Status = "failed"
		}
	})
}

func (t *guiTask) complete(report exportReport) {
	outputs := make([]string, 0, len(report.Archives))
	commands := make([]string, 0, len(report.Archives))
	for _, archive := range report.Archives {
		outputs = append(outputs, archive.Result.AbsPath)
		commands = append(commands, "docker load -i "+shellQuotePath(archive.Result.AbsPath))
	}
	t.update(func(snapshot *guiTaskSnapshot) {
		snapshot.Status = "done"
		snapshot.Stage = "complete"
		snapshot.OutputFiles = outputs
		snapshot.PlatformIndexPath = report.IndexPath
		snapshot.DockerLoadCommands = commands
	})
	t.scheduleCleanup()
}

func (t *guiTask) fail(err error) {
	t.update(func(snapshot *guiTaskSnapshot) {
		snapshot.Status = "failed"
		snapshot.Stage = "error"
		snapshot.Error = err.Error()
	})
	t.scheduleCleanup()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func sendSSE(w io.Writer, payload any) error {
	data, _ := json.Marshal(payload)
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
