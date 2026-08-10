package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGUISettingsPersistProfilesWithoutReturningSecrets(t *testing.T) {
	configDir := t.TempDir()
	server := newGUIServer("test", guiOptions{ConfigDir: configDir})
	registryResponse := serveGUIJSON(t, server, http.MethodPost, "/api/settings/registries", `{
		"name":"Private Harbor","kind":"harbor","endpoint":"https://harbor.example.test","namespace":"team"
	}`)
	if registryResponse.Code != http.StatusOK {
		t.Fatalf("save registry status=%d body=%s", registryResponse.Code, registryResponse.Body.String())
	}
	var registry registryProfile
	decodeRecorderJSON(t, registryResponse, &registry)

	credentialResponse := serveGUIJSON(t, server, http.MethodPost, "/api/settings/credentials", `{
		"registry_id":"`+registry.ID+`","name":"robot","username":"robot$sync","secret":"test-password"
	}`)
	if credentialResponse.Code != http.StatusOK {
		t.Fatalf("save credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	if strings.Contains(credentialResponse.Body.String(), "test-password") || strings.Contains(credentialResponse.Body.String(), `"secret"`) {
		t.Fatalf("credential response exposed secret: %s", credentialResponse.Body.String())
	}

	settingsRequest := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	settingsResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(settingsResponse, settingsRequest)
	if settingsResponse.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", settingsResponse.Code, settingsResponse.Body.String())
	}
	if strings.Contains(settingsResponse.Body.String(), "test-password") || strings.Contains(settingsResponse.Body.String(), `"secret"`) {
		t.Fatalf("settings response exposed secret: %s", settingsResponse.Body.String())
	}
	vaultData, err := os.ReadFile(filepath.Join(configDir, "secrets.enc"))
	if err != nil {
		t.Fatalf("read encrypted vault: %v", err)
	}
	if bytes.Contains(vaultData, []byte("test-password")) {
		t.Fatal("encrypted vault contains plaintext password")
	}
}

func TestGUIManagementRoutesRejectExtraPathSegments(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	response := serveGUIJSON(t, server, http.MethodPost, "/api/settings/registries/unexpected", `{"name":"bad","endpoint":"https://registry.example.test"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unexpected settings route status: %d", response.Code)
	}
	response = serveGUIJSON(t, server, http.MethodGet, "/api/remotes/host/files/extra", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unexpected remote route status: %d", response.Code)
	}
}

func TestGUISyncUsesRememberedExecutionMachine(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	store := server.configStore
	source, err := store.upsertRegistry(registryProfile{Name: "source", Kind: "registry", Endpoint: "https://source.example.test"})
	if err != nil {
		t.Fatalf("save source: %v", err)
	}
	target, err := store.upsertRegistry(registryProfile{Name: "target", Kind: "registry", Endpoint: "https://target.example.test", Namespace: "archive"})
	if err != nil {
		t.Fatalf("save target: %v", err)
	}
	remoteSecret := "ssh-test-secret"
	remote, err := store.upsertRemote(remoteProfile{
		Name: "runner", Address: "runner.example.test", User: "dia", AuthMethod: "password",
		Workspace: "/srv/dia", HostKeyFingerprint: "SHA256:test",
	}, &remoteSecret)
	if err != nil {
		t.Fatalf("save remote: %v", err)
	}
	if err := store.setDefaultRemote(remote.ID); err != nil {
		t.Fatalf("set default remote: %v", err)
	}
	list, err := store.upsertImageList(imageListProfile{Name: "release", Content: "team/api:v1\nteam/worker:v1\n"})
	if err != nil {
		t.Fatalf("save image list: %v", err)
	}

	mock := &mockGUIRemoteClient{}
	mock.runSyncFn = func(ctx context.Context, spec syncJobSpec, progress func(syncProgressEvent)) (syncJobResult, error) {
		if spec.Source.Endpoint != source.Endpoint || spec.Target.Endpoint != target.Endpoint || spec.Target.Namespace != "archive" {
			t.Fatalf("unexpected sync endpoints: %+v -> %+v", spec.Source, spec.Target)
		}
		if len(spec.Images) != 2 {
			t.Fatalf("unexpected image list: %+v", spec.Images)
		}
		progress(syncProgressEvent{Stage: "blob", Image: spec.Images[0].Source, CurrentImage: 1, TotalImages: 2, BytesDone: 5, BytesTotal: 10})
		return syncJobResult{TargetType: syncTargetRegistry, Engine: "native", Succeeded: 2, Items: []syncJobItemResult{
			{Source: spec.Images[0].Source, Target: spec.Images[0].Target, Status: "done"},
			{Source: spec.Images[1].Source, Target: spec.Images[1].Target, Status: "done"},
		}}, nil
	}
	server.remoteFactory = func(profile remoteProfile, secret, version string) (guiRemoteClient, error) {
		if profile.ID != remote.ID || secret != remoteSecret {
			t.Fatalf("unexpected remote credentials: profile=%+v secret=%q", profile, secret)
		}
		return mock, nil
	}

	response := serveGUIJSON(t, server, http.MethodPost, "/api/sync", `{
		"source_registry_id":"`+source.ID+`",
		"target_type":"registry",
		"target_registry_id":"`+target.ID+`",
		"image_list_id":"`+list.ID+`",
		"engine":"native"
	}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start sync status=%d body=%s", response.Code, response.Body.String())
	}
	var accepted guiExportResponse
	decodeRecorderJSON(t, response, &accepted)
	task := server.taskStore.get(accepted.TaskID)
	if task == nil {
		t.Fatal("sync task not found")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := task.snapshot()
		if snapshot.Status == "done" {
			if snapshot.RemoteID != remote.ID || snapshot.SyncResult == nil || snapshot.SyncResult.Succeeded != 2 {
				t.Fatalf("unexpected sync snapshot: %+v", snapshot)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sync task did not complete: %+v", task.snapshot())
}

func TestGUIHarborUsesRememberedExecutionMachine(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	store := server.configStore
	harbor, err := store.upsertRegistry(registryProfile{Name: "harbor", Kind: "harbor", Endpoint: "https://harbor.example.test"})
	if err != nil {
		t.Fatalf("save Harbor: %v", err)
	}
	remote, err := store.upsertRemote(remoteProfile{
		Name: "runner", Address: "runner.example.test", User: "dia", AuthMethod: "agent", Workspace: "/srv/dia",
	}, nil)
	if err != nil {
		t.Fatalf("save remote: %v", err)
	}
	if err := store.setDefaultRemote(remote.ID); err != nil {
		t.Fatalf("set default remote: %v", err)
	}
	mock := &mockGUIRemoteClient{}
	server.remoteFactory = func(profile remoteProfile, secret, version string) (guiRemoteClient, error) {
		if profile.ID != remote.ID {
			t.Fatalf("unexpected Harbor execution machine: %+v", profile)
		}
		return mock, nil
	}

	response := serveGUIJSON(t, server, http.MethodPost, "/api/harbor", `{
		"registry_id":"`+harbor.ID+`",
		"action":{"action":"health"}
	}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("Harbor action status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGUIHarborCanExecuteDirectlyWithSavedCredential(t *testing.T) {
	const (
		username = "robot$qa"
		password = "direct-harbor-secret"
	)
	requestObserved := make(chan bool, 1)
	serverURL, closeServer := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUsername, gotPassword, authenticated := r.BasicAuth()
		requestObserved <- r.URL.Path == "/api/v2.0/health" && authenticated && gotUsername == username && gotPassword == password
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"healthy"}`)
	}))
	defer closeServer()

	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	harbor, err := server.configStore.upsertRegistry(registryProfile{Name: "direct", Kind: "harbor", Endpoint: serverURL})
	if err != nil {
		t.Fatalf("save Harbor: %v", err)
	}
	secret := password
	credential, err := server.configStore.upsertCredential(credentialProfile{
		RegistryID: harbor.ID, Name: "robot", Username: username,
	}, &secret)
	if err != nil {
		t.Fatalf("save Harbor credential: %v", err)
	}
	server.remoteFactory = func(profile remoteProfile, secret, version string) (guiRemoteClient, error) {
		t.Fatal("direct Harbor action unexpectedly created a remote client")
		return nil, nil
	}

	response := serveGUIJSON(t, server, http.MethodPost, "/api/harbor", `{
		"registry_id":"`+harbor.ID+`",
		"credential_id":"`+credential.ID+`",
		"execution":"local",
		"action":{"action":"health"}
	}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"healthy"`) {
		t.Fatalf("direct Harbor action status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case valid := <-requestObserved:
		if !valid {
			t.Fatal("direct Harbor request path or credentials were incorrect")
		}
	case <-time.After(time.Second):
		t.Fatal("direct Harbor server did not receive a request")
	}
}

func TestGUIHarborRejectsUnknownExecutionMode(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	harbor, err := server.configStore.upsertRegistry(registryProfile{Name: "harbor", Kind: "harbor", Endpoint: "https://harbor.example.test"})
	if err != nil {
		t.Fatalf("save Harbor: %v", err)
	}
	response := serveGUIJSON(t, server, http.MethodPost, "/api/harbor", `{
		"registry_id":"`+harbor.ID+`",
		"execution":"browser",
		"action":{"action":"health"}
	}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "local or remote") {
		t.Fatalf("unexpected Harbor execution response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGUIHarborRejectsOperationWithoutExecutionMachine(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	harbor, err := server.configStore.upsertRegistry(registryProfile{Name: "harbor", Kind: "harbor", Endpoint: "https://harbor.example.test"})
	if err != nil {
		t.Fatalf("save Harbor: %v", err)
	}
	response := serveGUIJSON(t, server, http.MethodPost, "/api/harbor", `{
		"registry_id":"`+harbor.ID+`",
		"action":{"action":"health"}
	}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "execution machine") {
		t.Fatalf("unexpected Harbor action response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGUISessionRequiresTokenAndSetsHTTPOnlyCookie(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	server.sessionToken = "fixed-test-token"

	denied := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	denied.Host = "127.0.0.1:1234"
	deniedResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("unexpected unauthenticated status: %d", deniedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/?token=fixed-test-token", nil)
	request.Host = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("unexpected token exchange status: %d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected GUI session cookie: %+v", cookies)
	}
}

func TestGUISessionRejectsOriginFromAnotherLoopbackPort(t *testing.T) {
	server := newGUIServer("test", guiOptions{})
	server.sessionToken = "fixed-test-token"
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/settings", nil)
	request.Host = "127.0.0.1:1234"
	request.Header.Set("Origin", "http://127.0.0.1:9999")
	request.AddCookie(&http.Cookie{Name: "dia_session", Value: server.sessionToken})
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unexpected cross-port origin status: %d", response.Code)
	}
}

func TestGUIRegistryRequestUsesSavedProfileAndCredential(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	registry, err := server.configStore.upsertRegistry(registryProfile{
		Name: "build registry", Kind: "registry", Endpoint: "http://registry.example.test:5000", Namespace: "archive",
	})
	if err != nil {
		t.Fatalf("save registry: %v", err)
	}
	secret := "saved-test-secret"
	credential, err := server.configStore.upsertCredential(credentialProfile{
		RegistryID: registry.ID, Name: "robot", Username: "robot$build",
	}, &secret)
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}

	ref, client, err := server.resolveGUIRegistryRequest(
		context.Background(),
		"service:v1", registry.ID, credential.ID,
		"http://ignored-proxy.invalid", "ignored", "ignored", true,
	)
	if err != nil {
		t.Fatalf("resolve saved registry request: %v", err)
	}
	if ref.RegistryHost() != "registry.example.test:5000" || ref.Repository != "archive/service" || ref.Tag != "v1" {
		t.Fatalf("unexpected resolved reference: %+v", ref)
	}
	if client.scheme != "http" || client.username != "robot$build" || client.password != secret {
		t.Fatalf("saved access was not applied: scheme=%q username=%q password=%q", client.scheme, client.username, client.password)
	}
}

func TestGUIRegistryRequestRejectsCredentialFromAnotherRegistry(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	first, err := server.configStore.upsertRegistry(registryProfile{Name: "first", Endpoint: "https://first.example.test"})
	if err != nil {
		t.Fatalf("save first registry: %v", err)
	}
	second, err := server.configStore.upsertRegistry(registryProfile{Name: "second", Endpoint: "https://second.example.test"})
	if err != nil {
		t.Fatalf("save second registry: %v", err)
	}
	credential, err := server.configStore.upsertCredential(credentialProfile{RegistryID: second.ID, Name: "other", Username: "other"}, nil)
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}

	_, _, err = server.resolveGUIRegistryRequest(context.Background(), "service:v1", first.ID, credential.ID, "", "", "", false)
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("expected account ownership error, got %v", err)
	}
}

func TestGUIRegistryRequestCanOverrideDefaultCredentialWithAnonymous(t *testing.T) {
	server := newGUIServer("test", guiOptions{ConfigDir: t.TempDir()})
	registry, err := server.configStore.upsertRegistry(registryProfile{
		Name: "default-auth", Endpoint: "https://registry.example.test",
	})
	if err != nil {
		t.Fatalf("save registry: %v", err)
	}
	secret := "test-secret"
	credential, err := server.configStore.upsertCredential(credentialProfile{
		RegistryID: registry.ID, Name: "default", Username: "robot",
	}, &secret)
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}
	registry.DefaultCredentialID = credential.ID
	registry, err = server.configStore.upsertRegistry(registry)
	if err != nil {
		t.Fatalf("set default credential: %v", err)
	}

	access, err := server.registryAccess(server.configStore, registry, anonymousCredentialID)
	if err != nil {
		t.Fatalf("resolve anonymous access: %v", err)
	}
	if access.Username != "" || access.Password != "" {
		t.Fatalf("default credential leaked into anonymous access: %+v", access)
	}
	defaultAccess, err := server.registryAccess(server.configStore, registry, "")
	if err != nil {
		t.Fatalf("resolve default access: %v", err)
	}
	if defaultAccess.Username != "robot" || defaultAccess.Password != secret {
		t.Fatalf("default credential was not applied: %+v", defaultAccess)
	}
}

type mockGUIRemoteClient struct {
	runSyncFn func(context.Context, syncJobSpec, func(syncProgressEvent)) (syncJobResult, error)
}

func (m *mockGUIRemoteClient) probeHostKey(context.Context) (string, error) {
	return "SHA256:test", nil
}
func (m *mockGUIRemoteClient) test(context.Context) (remoteAgentInfo, error) {
	return remoteAgentInfo{Version: "test", Protocol: remoteProtocolVersion, OS: "linux", Architecture: "amd64"}, nil
}
func (m *mockGUIRemoteClient) probeStorage(context.Context, []string) ([]storageCandidate, error) {
	return nil, nil
}
func (m *mockGUIRemoteClient) listFiles(context.Context, remoteFileRequest) ([]remoteFileEntry, error) {
	return nil, nil
}
func (m *mockGUIRemoteClient) deleteFile(context.Context, remoteFileRequest) error { return nil }
func (m *mockGUIRemoteClient) downloadFile(context.Context, remoteFileRequest, io.Writer, func(remoteFileDownloadMeta) error) (remoteFileDownloadMeta, error) {
	return remoteFileDownloadMeta{}, nil
}
func (m *mockGUIRemoteClient) harborAction(context.Context, remoteHarborRequest) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}
func (m *mockGUIRemoteClient) runSync(ctx context.Context, spec syncJobSpec, progress func(syncProgressEvent)) (syncJobResult, error) {
	return m.runSyncFn(ctx, spec, progress)
}

func serveGUIJSON(t *testing.T, server *guiServer, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	return response
}

func decodeRecorderJSON(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response JSON: %v (body=%s)", err, response.Body.String())
	}
}
