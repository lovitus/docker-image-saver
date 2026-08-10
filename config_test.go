package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigStorePersistsEncryptedCredentials(t *testing.T) {
	dir := t.TempDir()
	store, err := openConfigStore(dir)
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}
	registry, err := store.upsertRegistry(registryProfile{
		Name:     "Build Harbor",
		Kind:     "harbor",
		Endpoint: "harbor.example.test",
	})
	if err != nil {
		t.Fatalf("save registry: %v", err)
	}
	plainRegistry, err := store.upsertRegistry(registryProfile{
		Name: "Mirror Registry", Kind: "registry", Endpoint: "registry.example.test",
	})
	if err != nil {
		t.Fatalf("save second registry: %v", err)
	}
	secret := "test-only-registry-password"
	credential, err := store.upsertCredential(credentialProfile{
		RegistryID: registry.ID,
		Name:       "robot",
		Username:   "robot$build",
	}, &secret)
	if err != nil {
		t.Fatalf("save credential: %v", err)
	}
	if !credential.HasSecret {
		t.Fatal("expected saved credential to report a secret")
	}
	secondSecret := "test-only-second-registry-password"
	secondCredential, err := store.upsertCredential(credentialProfile{
		RegistryID: registry.ID,
		Name:       "developer",
		Username:   "harbor-developer",
	}, &secondSecret)
	if err != nil {
		t.Fatalf("save second credential: %v", err)
	}

	for _, file := range []string{"config.json", "secrets.enc", "master.key"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), secret) || strings.Contains(string(data), secondSecret) {
			t.Fatalf("%s contains plaintext secret", file)
		}
	}

	reloaded, err := openConfigStore(dir)
	if err != nil {
		t.Fatalf("reload config store: %v", err)
	}
	gotProfile, gotSecret, err := reloaded.credential(credential.ID)
	if err != nil {
		t.Fatalf("load credential: %v", err)
	}
	if !gotProfile.HasSecret || gotSecret != secret {
		t.Fatalf("unexpected reloaded credential: profile=%+v secret=%q", gotProfile, gotSecret)
	}
	secondProfile, gotSecondSecret, err := reloaded.credential(secondCredential.ID)
	if err != nil {
		t.Fatalf("load second credential: %v", err)
	}
	if secondProfile.Username != "harbor-developer" || gotSecondSecret != secondSecret {
		t.Fatalf("unexpected second credential: profile=%+v secret=%q", secondProfile, gotSecondSecret)
	}
	reloadedRegistry, err := reloaded.registry(plainRegistry.ID)
	if err != nil {
		t.Fatalf("load second registry: %v", err)
	}
	if reloadedRegistry.Kind != "registry" || reloadedRegistry.Endpoint != "https://registry.example.test" {
		t.Fatalf("unexpected second registry: %+v", reloadedRegistry)
	}
	if snapshot := reloaded.snapshot(); len(snapshot.Registries) != 2 || len(snapshot.Credentials) != 2 {
		t.Fatalf("unexpected persisted record counts: %+v", snapshot)
	}

	empty := ""
	if _, err := reloaded.upsertCredential(gotProfile, &empty); err != nil {
		t.Fatalf("clear credential secret: %v", err)
	}
	_, gotSecret, err = reloaded.credential(credential.ID)
	if err != nil {
		t.Fatalf("reload cleared credential: %v", err)
	}
	if gotSecret != "" {
		t.Fatalf("expected cleared secret, got %q", gotSecret)
	}
}

func TestConfigStorePersistsDefaultRemoteAndImageList(t *testing.T) {
	dir := t.TempDir()
	store, err := openConfigStore(dir)
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}
	remoteSecret := "test-only-ssh-password"
	remote, err := store.upsertRemote(remoteProfile{
		Name: "edge runner", Address: "runner.example.test", User: "dia",
		AuthMethod: "password", Workspace: "/srv/dia", DiaPath: "/srv/dia/bin/dia",
		StorageRoots: []string{"/data/archives"}, HostKeyFingerprint: "SHA256:test",
	}, &remoteSecret)
	if err != nil {
		t.Fatalf("save remote: %v", err)
	}
	if remote.Port != 22 || remote.AuthMethod != "password" || !remote.HasSecret {
		t.Fatalf("unexpected remote defaults: %+v", remote)
	}
	if err := store.setDefaultRemote(remote.ID); err != nil {
		t.Fatalf("set default remote: %v", err)
	}
	list, err := store.upsertImageList(imageListProfile{
		Name: "release set",
		Content: "# application images\n" +
			"team/api:v1\n" +
			"team/worker:v1 -> archive/worker:v1\n",
	})
	if err != nil {
		t.Fatalf("save image list: %v", err)
	}
	if list.ID == "" {
		t.Fatal("expected image list ID")
	}
	reloaded, err := openConfigStore(dir)
	if err != nil {
		t.Fatalf("reopen config store: %v", err)
	}
	snapshot := reloaded.snapshot()
	if snapshot.DefaultRemoteID != remote.ID || len(snapshot.ImageLists) != 1 {
		t.Fatalf("unexpected reloaded config snapshot: %+v", snapshot)
	}
	gotRemote, gotSecret, err := reloaded.remote(remote.ID)
	if err != nil {
		t.Fatalf("load reloaded remote: %v", err)
	}
	if gotSecret != remoteSecret || gotRemote.DiaPath != "/srv/dia/bin/dia" ||
		gotRemote.HostKeyFingerprint != "SHA256:test" || len(gotRemote.StorageRoots) != 1 || gotRemote.StorageRoots[0] != "/data/archives" {
		t.Fatalf("unexpected reloaded remote: profile=%+v secret=%q", gotRemote, gotSecret)
	}
	gotList, err := reloaded.imageList(list.ID)
	if err != nil {
		t.Fatalf("load reloaded image list: %v", err)
	}
	if gotList.Name != list.Name || gotList.Content != list.Content {
		t.Fatalf("unexpected reloaded image list: %+v", gotList)
	}
	for _, file := range []string{"config.json", "secrets.enc", "master.key"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), remoteSecret) {
			t.Fatalf("%s contains plaintext SSH secret", file)
		}
	}
}

func TestParseImageListPreservesMappings(t *testing.T) {
	items, err := parseImageList("\n# ignored\nteam/api:v1\nteam/old:v2 -> migrated/new:v2\n")
	if err != nil {
		t.Fatalf("parse image list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("unexpected item count: %d", len(items))
	}
	if items[0].Source != "team/api:v1" || items[0].Target != "team/api:v1" || items[0].Line != 3 {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	if items[1].Source != "team/old:v2" || items[1].Target != "migrated/new:v2" {
		t.Fatalf("unexpected mapped item: %+v", items[1])
	}
}

func TestParseImageListRejectsEmptyList(t *testing.T) {
	if _, err := parseImageList("# comments only\n\n"); err == nil {
		t.Fatal("expected empty image list error")
	}
}

func TestDefaultConfigDirHonorsEnvironmentOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom", "config")
	t.Setenv("DIA_CONFIG_DIR", dir)

	got, err := defaultConfigDir()
	if err != nil {
		t.Fatalf("defaultConfigDir: %v", err)
	}
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve expected path: %v", err)
	}
	if got != want {
		t.Fatalf("unexpected config directory: got %q want %q", got, want)
	}
}

func TestConfigSnapshotUsesEmptyArraysInsteadOfNull(t *testing.T) {
	store, err := openConfigStore(t.TempDir())
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}
	data, err := json.Marshal(store.snapshot())
	if err != nil {
		t.Fatalf("marshal config snapshot: %v", err)
	}
	for _, field := range []string{"registries", "credentials", "remotes", "image_lists"} {
		if strings.Contains(string(data), `"`+field+`":null`) {
			t.Fatalf("snapshot encoded %s as null: %s", field, data)
		}
	}
}

func TestConfigUpsertRejectsUnknownExplicitID(t *testing.T) {
	store, err := openConfigStore(t.TempDir())
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}
	_, err = store.upsertRegistry(registryProfile{ID: "reg_missing", Name: "missing", Endpoint: "https://registry.example.test"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected explicit ID result: %v", err)
	}
}

func TestRegistryProfileRejectsEndpointPath(t *testing.T) {
	err := validateRegistryProfile(registryProfile{Name: "bad", Endpoint: "https://registry.example.test/prefix"})
	if err == nil || !strings.Contains(err.Error(), "without a path") {
		t.Fatalf("unexpected endpoint validation result: %v", err)
	}
}

func TestRegistryProfileRejectsUnsafeNamespace(t *testing.T) {
	for _, namespace := range []string{"../prod", "prod//team", "Prod/team", "prod\\team"} {
		err := validateRegistryProfile(registryProfile{
			Name: "bad", Endpoint: "https://registry.example.test", Namespace: namespace,
		})
		if err == nil || !strings.Contains(err.Error(), "namespace") {
			t.Errorf("namespace %q was not rejected: %v", namespace, err)
		}
	}
}

func TestRegistryProfileRejectsPersistentProxyCredentials(t *testing.T) {
	err := validateRegistryProfile(registryProfile{
		Name: "private", Endpoint: "https://registry.example.test",
		Proxy: "http://proxy-user:proxy-password@127.0.0.1:7890",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be saved") {
		t.Fatalf("unexpected authenticated proxy validation result: %v", err)
	}
}
