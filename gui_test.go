package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGUIIndexBootstrapsCLIFlags(t *testing.T) {
	server := newGUIServer("vtest", guiOptions{
		Image:    "private.example/app:v1",
		Output:   "/tmp/private_app.tar",
		Proxy:    "socks5h://127.0.0.1:7897",
		Username: "alice",
		Password: "secret",
		Insecure: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	html := rec.Body.String()
	if strings.Contains(html, "__DIA_BOOTSTRAP_JSON__") {
		t.Fatal("bootstrap placeholder was not replaced")
	}
	for _, want := range []string{
		"private.example/app:v1",
		"/tmp/private_app.tar",
		"socks5h://127.0.0.1:7897",
		"alice",
		"JSON.parse(",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("bootstrap payload missing %q", want)
		}
	}
	if strings.Contains(html, "secret") {
		t.Fatal("password leaked into GUI bootstrap")
	}
}

func TestGUIInspectHandler(t *testing.T) {
	server := newGUIServer("test", guiOptions{Password: "secret"})
	server.inspectFn = func(req guiInspectRequest) (guiInspectResponse, error) {
		if req.Image != "alpine:latest" {
			t.Fatalf("unexpected image: %q", req.Image)
		}
		if req.Proxy != "socks5h://127.0.0.1:7897" {
			t.Fatalf("unexpected proxy: %q", req.Proxy)
		}
		if !req.Insecure {
			t.Fatal("expected insecure=true")
		}
		if req.Password != "secret" {
			t.Fatalf("expected password fallback, got %q", req.Password)
		}
		return guiInspectResponse{
			Image:         req.Image,
			DefaultOutput: "alpine.tar",
			Platforms: []guiPlatform{
				{Index: 0, DisplayIndex: 1, ManifestRef: "sha256:amd64", OS: "linux", Architecture: "amd64", Label: "linux/amd64"},
			},
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(`{"image":"alpine:latest","proxy":"socks5h://127.0.0.1:7897","use_saved_password":true,"insecure":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	var payload guiInspectResponse
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.DefaultOutput != "alpine.tar" {
		t.Fatalf("unexpected default output: %q", payload.DefaultOutput)
	}
	if len(payload.Platforms) != 1 || payload.Platforms[0].Label != "linux/amd64" {
		t.Fatalf("unexpected platforms: %+v", payload.Platforms)
	}
	if payload.Platforms[0].ManifestRef != "sha256:amd64" {
		t.Fatalf("unexpected manifest ref: %q", payload.Platforms[0].ManifestRef)
	}
}

func TestGUIExportTaskLifecycle(t *testing.T) {
	server := newGUIServer("test", guiOptions{Password: "secret"})
	server.planFn = func(req guiExportRequest) ([]string, error) {
		return []string{req.Output, platformIndexPath(req.Output)}, nil
	}
	server.exportFn = func(req guiExportRequest, hooks *exportHooks) (exportReport, error) {
		if req.Password != "secret" {
			t.Fatalf("expected password fallback, got %q", req.Password)
		}
		hooks.emit(progressEvent{
			Stage:        "manifest",
			Platform:     "linux/amd64",
			Message:      "resolving manifest",
			BytesDone:    0,
			BytesTotal:   0,
			CurrentLayer: 0,
			TotalLayers:  1,
		})
		time.Sleep(30 * time.Millisecond)
		hooks.emit(progressEvent{
			Stage:        "write_layer",
			Platform:     "linux/amd64",
			Message:      "writing layer",
			CurrentLayer: 1,
			TotalLayers:  1,
			BytesDone:    128,
			BytesTotal:   256,
			SpeedBPS:     64,
			ETASeconds:   2,
		})
		time.Sleep(30 * time.Millisecond)
		return exportReport{
			Archives: []exportedArchive{
				{
					Platform: platform{OS: "linux", Architecture: "amd64"},
					Result:   saveResult{AbsPath: "/tmp/alpine_linux_amd64.tar", Size: 256},
				},
			},
			IndexPath: "/tmp/alpine_platforms.json",
		}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"alpine.tar","use_saved_password":true,"selected":["sha256:amd64"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	var accepted guiExportResponse
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode accepted response: %v", err)
	}
	if accepted.TaskID == "" {
		t.Fatal("expected task id")
	}

	task := server.taskStore.get(accepted.TaskID)
	if task == nil {
		t.Fatal("task not found in store")
	}

	sub, cancel := task.subscribe()
	defer cancel()

	var sawWriteLayer bool
	var done guiTaskSnapshot
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-sub.notify:
			snapshot := task.snapshot()
			if snapshot.Stage == "write_layer" {
				sawWriteLayer = true
			}
			if snapshot.Status == "done" {
				done = snapshot
				goto verify
			}
		case <-timeout:
			t.Fatal("timed out waiting for task completion")
		}
	}

verify:
	if !sawWriteLayer {
		t.Fatal("did not observe write_layer progress update")
	}
	if done.PlatformIndexPath != "/tmp/alpine_platforms.json" {
		t.Fatalf("unexpected platform index path: %q", done.PlatformIndexPath)
	}
	if len(done.OutputFiles) != 1 || done.OutputFiles[0] != "/tmp/alpine_linux_amd64.tar" {
		t.Fatalf("unexpected output files: %+v", done.OutputFiles)
	}
	if len(done.DockerLoadCommands) != 1 || !strings.Contains(done.DockerLoadCommands[0], "docker load -i") {
		t.Fatalf("unexpected docker load commands: %+v", done.DockerLoadCommands)
	}
}

func TestGUITaskSubscriberSeesTerminalStateAfterLag(t *testing.T) {
	task := &guiTask{
		snapshotValue: guiTaskSnapshot{ID: "t1", Status: "running"},
		subscribers:   make(map[*guiSubscriber]struct{}),
		store:         newGUITaskStore(),
	}
	sub, cancel := task.subscribe()
	defer cancel()

	for i := 0; i < 32; i++ {
		task.handleProgress(progressEvent{
			Stage:        "write_layer",
			Platform:     "linux/amd64",
			CurrentLayer: i + 1,
			TotalLayers:  32,
			BytesDone:    int64(i + 1),
		})
	}
	task.complete(exportReport{
		Archives: []exportedArchive{
			{Platform: platform{OS: "linux", Architecture: "amd64"}, Result: saveResult{AbsPath: "/tmp/out.tar"}},
		},
	})

	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sub.notify:
			if !ok {
				t.Fatal("subscriber unexpectedly closed")
			}
			snapshot := task.snapshot()
			if snapshot.Status == "done" {
				if len(snapshot.DockerLoadCommands) != 1 {
					t.Fatalf("expected docker load command in terminal snapshot, got %+v", snapshot.DockerLoadCommands)
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for terminal state")
		}
	}
}

func TestGUITaskCancelDoesNotCloseNotifyChannel(t *testing.T) {
	task := &guiTask{
		snapshotValue: guiTaskSnapshot{ID: "t2", Status: "running"},
		subscribers:   make(map[*guiSubscriber]struct{}),
		store:         newGUITaskStore(),
	}
	sub, cancel := task.subscribe()
	cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("publish panicked after cancel: %v", r)
		}
	}()

	task.publish()

	select {
	case <-sub.notify:
		t.Fatal("canceled subscriber should not receive further notifications")
	default:
	}
}

func TestGUIExportRejectsExplicitEmptySelection(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	req := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"alpine.tar","selected":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(payload["error"], "at least one platform") {
		t.Fatalf("unexpected error payload: %+v", payload)
	}
}

func TestResolveSelectedPlatformRefsRejectsStaleSelection(t *testing.T) {
	_, err := resolveSelectedPlatformRefs([]platformOption{
		{ManifestRef: "sha256:current", Platform: platform{OS: "linux", Architecture: "amd64"}},
	}, &[]string{"sha256:stale"})
	if err == nil {
		t.Fatal("expected stale selection to fail")
	}
	if !strings.Contains(err.Error(), "re-inspect before exporting") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveSelectedPlatformRefsPreservesExplicitOrder(t *testing.T) {
	selected, err := resolveSelectedPlatformRefs([]platformOption{
		{ManifestRef: "sha256:one", Platform: platform{OS: "linux", Architecture: "amd64"}},
		{ManifestRef: "sha256:two", Platform: platform{OS: "linux", Architecture: "arm64"}},
	}, &[]string{"sha256:two", "sha256:one"})
	if err != nil {
		t.Fatalf("resolveSelectedPlatformRefs: %v", err)
	}
	if len(selected) != 2 || selected[0] != 1 || selected[1] != 0 {
		t.Fatalf("unexpected selected order: %+v", selected)
	}
}

func TestGUIInspectCanClearInheritedPassword(t *testing.T) {
	server := newGUIServer("test", guiOptions{Password: "secret"})
	server.inspectFn = func(req guiInspectRequest) (guiInspectResponse, error) {
		if req.Password != "" {
			t.Fatalf("expected cleared password, got %q", req.Password)
		}
		return guiInspectResponse{Image: req.Image, DefaultOutput: "alpine.tar"}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(`{"image":"alpine:latest","password":"","use_saved_password":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
}

func TestGUITaskHandleProgressPreservesCountersOnStageOnlyUpdate(t *testing.T) {
	task := &guiTask{
		snapshotValue: guiTaskSnapshot{ID: "t3", Status: "running"},
		subscribers:   make(map[*guiSubscriber]struct{}),
		store:         newGUITaskStore(),
	}

	task.handleProgress(progressEvent{
		Stage:        "write_layer",
		Platform:     "linux/amd64",
		CurrentLayer: 2,
		TotalLayers:  5,
		BytesDone:    128,
		BytesTotal:   256,
		SpeedBPS:     64,
		ETASeconds:   2,
	})
	task.handleProgress(progressEvent{
		Stage:    "archive_done",
		Platform: "linux/amd64",
		Message:  "/tmp/out.tar",
		Done:     true,
	})

	snapshot := task.snapshot()
	if snapshot.BytesDone != 128 || snapshot.BytesTotal != 256 {
		t.Fatalf("expected byte counters preserved, got done=%d total=%d", snapshot.BytesDone, snapshot.BytesTotal)
	}
	if snapshot.CurrentLayer != 2 || snapshot.TotalLayers != 5 {
		t.Fatalf("expected layer counters preserved, got %d/%d", snapshot.CurrentLayer, snapshot.TotalLayers)
	}
	if snapshot.SpeedBPS != 64 || snapshot.ETASeconds != 2 {
		t.Fatalf("expected speed preserved, got speed=%f eta=%d", snapshot.SpeedBPS, snapshot.ETASeconds)
	}
}

func TestGUITaskHandleProgressResetsCountersOnPlatformSwitch(t *testing.T) {
	task := &guiTask{
		snapshotValue: guiTaskSnapshot{ID: "t4", Status: "running"},
		subscribers:   make(map[*guiSubscriber]struct{}),
		store:         newGUITaskStore(),
	}

	task.handleProgress(progressEvent{
		Stage:        "write_layer",
		Platform:     "linux/amd64",
		CurrentLayer: 3,
		TotalLayers:  5,
		BytesDone:    256,
		BytesTotal:   512,
		SpeedBPS:     96,
		ETASeconds:   1,
	})
	task.handleProgress(progressEvent{
		Stage:    "manifest",
		Platform: "linux/arm64",
		Message:  "resolving manifest",
	})

	snapshot := task.snapshot()
	if snapshot.Platform != "linux/arm64" {
		t.Fatalf("unexpected platform: %q", snapshot.Platform)
	}
	if snapshot.BytesDone != 0 || snapshot.BytesTotal != 0 {
		t.Fatalf("expected byte counters reset, got done=%d total=%d", snapshot.BytesDone, snapshot.BytesTotal)
	}
	if snapshot.CurrentLayer != 0 || snapshot.TotalLayers != 0 {
		t.Fatalf("expected layer counters reset, got %d/%d", snapshot.CurrentLayer, snapshot.TotalLayers)
	}
	if snapshot.SpeedBPS != 0 || snapshot.ETASeconds != 0 {
		t.Fatalf("expected speed reset, got speed=%f eta=%d", snapshot.SpeedBPS, snapshot.ETASeconds)
	}
}

func TestGUITaskEventsStream(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	task, err := server.taskStore.newTask(guiTaskInput{Image: "alpine:latest", Output: "alpine.tar"})
	if err != nil {
		t.Fatalf("new task: %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	httpServer := &http.Server{Handler: server.routes()}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	req, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/api/tasks/"+task.snapshot().ID+"/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET task events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("unexpected content type: %q", ct)
	}

	lineCh := make(chan string, 4)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				if readErr != io.EOF {
					lineCh <- "ERR:" + readErr.Error()
				}
				close(lineCh)
				return
			}
			lineCh <- line
		}
	}()

	task.handleProgress(progressEvent{Stage: "manifest", Platform: "linux/amd64", Message: "resolving manifest"})

	timeout := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				t.Fatal("event stream closed before manifest event")
			}
			if strings.HasPrefix(line, "ERR:") {
				t.Fatal(strings.TrimPrefix(line, "ERR:"))
			}
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"stage":"manifest"`) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for SSE payload")
		}
	}
}

func TestGUIExportRejectsConcurrentOutputReuse(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	server.planFn = func(req guiExportRequest) ([]string, error) {
		return []string{req.Output, platformIndexPath(req.Output)}, nil
	}
	block := make(chan struct{})
	release := make(chan struct{})
	server.exportFn = func(req guiExportRequest, hooks *exportHooks) (exportReport, error) {
		close(block)
		<-release
		return exportReport{
			Archives: []exportedArchive{
				{
					Platform: platform{OS: "linux", Architecture: "amd64"},
					Result:   saveResult{AbsPath: req.Output, Size: 1},
				},
			},
			IndexPath: req.Output + "_platforms.json",
		}, nil
	}

	req1 := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"same.tar","selected":["sha256:amd64"]}`))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("unexpected first status: %d", rec1.Code)
	}

	select {
	case <-block:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first export to start")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"same.tar","selected":["sha256:amd64"]}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("unexpected second status: %d", rec2.Code)
	}

	var payload map[string]string
	if err := json.NewDecoder(rec2.Body).Decode(&payload); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if !strings.Contains(payload["error"], "already in use") {
		t.Fatalf("unexpected conflict payload: %+v", payload)
	}

	close(release)

	var accepted guiExportResponse
	if err := json.NewDecoder(rec1.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode accepted response: %v", err)
	}
	task := server.taskStore.get(accepted.TaskID)
	if task == nil {
		t.Fatal("task not found after first export")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot := task.snapshot(); snapshot.Status == "done" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for first export to complete")
}

func TestGUITaskStoreEvictsCompletedTaskAfterRetention(t *testing.T) {
	store := newGUITaskStore()
	store.taskRetention = 20 * time.Millisecond

	task, err := store.newTask(guiTaskInput{Image: "alpine:latest", Output: "done.tar"})
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	task.complete(exportReport{
		Archives: []exportedArchive{
			{Platform: platform{OS: "linux", Architecture: "amd64"}, Result: saveResult{AbsPath: "/tmp/done.tar"}},
		},
	})

	if store.get(task.snapshot().ID) == nil {
		t.Fatal("task should remain available immediately after completion")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.get(task.snapshot().ID) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for completed task eviction")
}

func TestGUIExportRejectsDerivedOutputConflict(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	server.planFn = func(req guiExportRequest) ([]string, error) {
		switch req.Output {
		case "foo.tar":
			return []string{"foo_linux_amd64.tar", "foo_linux_arm64.tar", "foo_platforms.json"}, nil
		case "foo_linux_amd64.tar":
			return []string{"foo_linux_amd64.tar", "foo_linux_amd64_platforms.json"}, nil
		default:
			return []string{req.Output, platformIndexPath(req.Output)}, nil
		}
	}
	block := make(chan struct{})
	release := make(chan struct{})
	server.exportFn = func(req guiExportRequest, hooks *exportHooks) (exportReport, error) {
		close(block)
		<-release
		return exportReport{
			Archives: []exportedArchive{
				{
					Platform: platform{OS: "linux", Architecture: "amd64"},
					Result:   saveResult{AbsPath: req.Output, Size: 1},
				},
			},
			IndexPath: platformIndexPath(req.Output),
		}, nil
	}

	req1 := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"foo.tar","selected":["sha256:amd64","sha256:arm64"]}`))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("unexpected first status: %d", rec1.Code)
	}

	select {
	case <-block:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first export to start")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/export", strings.NewReader(`{"image":"alpine:latest","output":"foo_linux_amd64.tar","selected":["sha256:amd64"]}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("unexpected second status: %d", rec2.Code)
	}

	var payload map[string]string
	if err := json.NewDecoder(rec2.Body).Decode(&payload); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if !strings.Contains(payload["error"], "foo_linux_amd64.tar") {
		t.Fatalf("unexpected conflict payload: %+v", payload)
	}

	close(release)
}

func TestNormalizeOutputReservationKeyResolvesSymlinkAlias(t *testing.T) {
	baseDir := t.TempDir()
	realDir := filepath.Join(baseDir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}
	linkDir := filepath.Join(baseDir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink real dir: %v", err)
	}

	realKey, err := normalizeOutputReservationKey(filepath.Join(realDir, "out.tar"))
	if err != nil {
		t.Fatalf("normalize real path: %v", err)
	}
	linkKey, err := normalizeOutputReservationKey(filepath.Join(linkDir, "out.tar"))
	if err != nil {
		t.Fatalf("normalize symlink path: %v", err)
	}
	if realKey != linkKey {
		t.Fatalf("expected symlink aliases to normalize identically: %q != %q", realKey, linkKey)
	}
}

func TestNormalizeOutputReservationKeyResolvesSymlinkFileAlias(t *testing.T) {
	baseDir := t.TempDir()
	realPath := filepath.Join(baseDir, "real.tar")
	if err := os.WriteFile(realPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	linkPath := filepath.Join(baseDir, "link.tar")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink real file: %v", err)
	}

	realKey, err := normalizeOutputReservationKey(realPath)
	if err != nil {
		t.Fatalf("normalize real path: %v", err)
	}
	linkKey, err := normalizeOutputReservationKey(linkPath)
	if err != nil {
		t.Fatalf("normalize symlink file path: %v", err)
	}
	if realKey != linkKey {
		t.Fatalf("expected symlink file aliases to normalize identically: %q != %q", realKey, linkKey)
	}
}

func TestNormalizeOutputReservationKeyResolvesDanglingSymlinkFileAlias(t *testing.T) {
	baseDir := t.TempDir()
	realPath := filepath.Join(baseDir, "nested", "real.tar")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	linkPath := filepath.Join(baseDir, "link.tar")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink dangling file: %v", err)
	}

	realKey, err := normalizeOutputReservationKey(realPath)
	if err != nil {
		t.Fatalf("normalize real target path: %v", err)
	}
	linkKey, err := normalizeOutputReservationKey(linkPath)
	if err != nil {
		t.Fatalf("normalize dangling symlink path: %v", err)
	}
	if realKey != linkKey {
		t.Fatalf("expected dangling symlink aliases to normalize identically: %q != %q", realKey, linkKey)
	}
}

func TestNormalizeOutputReservationKeyFoldsCaseOnlyOnCaseInsensitiveFilesystem(t *testing.T) {
	baseDir := t.TempDir()
	caseInsensitive, err := probeDirectoryCaseInsensitive(baseDir)
	if err != nil {
		t.Fatalf("probe case sensitivity: %v", err)
	}
	upperPath := filepath.Join(baseDir, "Foo.tar")
	lowerPath := filepath.Join(baseDir, "foo.tar")
	upperKey, err := normalizeOutputReservationKey(upperPath)
	if err != nil {
		t.Fatalf("normalize upper path: %v", err)
	}
	lowerKey, err := normalizeOutputReservationKey(lowerPath)
	if err != nil {
		t.Fatalf("normalize lower path: %v", err)
	}
	if caseInsensitive && upperKey != lowerKey {
		t.Fatalf("expected case-folded reservation keys to match: %q != %q", upperKey, lowerKey)
	}
	if !caseInsensitive && upperKey == lowerKey {
		t.Fatalf("expected case-sensitive reservation keys to differ: %q == %q", upperKey, lowerKey)
	}
}

func TestNormalizeOutputReservationKeyAllowsMissingParentDirectories(t *testing.T) {
	baseDir := t.TempDir()
	outputPath := filepath.Join(baseDir, "missing", "nested", "Foo.tar")

	key, err := normalizeOutputReservationKey(outputPath)
	if err != nil {
		t.Fatalf("normalize path with missing parents: %v", err)
	}
	if key == "" {
		t.Fatal("expected non-empty reservation key")
	}

	caseInsensitive, err := probeDirectoryCaseInsensitive(baseDir)
	if err != nil {
		t.Fatalf("probe case sensitivity: %v", err)
	}
	aliasKey, err := normalizeOutputReservationKey(filepath.Join(baseDir, "missing", "nested", "foo.tar"))
	if err != nil {
		t.Fatalf("normalize alias path with missing parents: %v", err)
	}
	if caseInsensitive && key != aliasKey {
		t.Fatalf("expected alias keys to match on case-insensitive filesystem: %q != %q", key, aliasKey)
	}
	if !caseInsensitive && key == aliasKey {
		t.Fatalf("expected alias keys to differ on case-sensitive filesystem: %q == %q", key, aliasKey)
	}
}
