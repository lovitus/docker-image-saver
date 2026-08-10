package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHarborClientListsProjectsAndUsesBasicAuth(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2.0/projects" {
			http.NotFound(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "robot" || password != "test-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "20" || r.URL.Query().Get("q") != "name=~team" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("X-Total-Count", "41")
		w.Header().Set("X-Request-Id", "req-1")
		_ = json.NewEncoder(w).Encode([]harborProject{{ProjectID: 7, Name: "team", RepoCount: 3}})
	})
	serverURL, closeServer := startIPv4HTTPServer(t, handler)
	defer closeServer()

	client, err := newHarborClient(registryAccess{Endpoint: serverURL, Username: "robot", Password: "test-secret"})
	if err != nil {
		t.Fatalf("new Harbor client: %v", err)
	}
	page, err := client.ListProjects(context.Background(), "name=~team", 2, 20)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if page.Total != 41 || page.NextPage != 3 || page.RequestID != "req-1" || len(page.Items) != 1 || page.Items[0].Name != "team" {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestHarborClientEscapesNestedRepositoryName(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "/repositories/team%2Fservice/artifacts") {
			t.Errorf("repository path was not escaped: %s", r.RequestURI)
		}
		_ = json.NewEncoder(w).Encode([]harborArtifact{{Digest: "sha256:test", Tags: []harborTag{{Name: "v1"}}}})
	})
	serverURL, closeServer := startIPv4HTTPServer(t, handler)
	defer closeServer()

	client, err := newHarborClient(registryAccess{Endpoint: serverURL})
	if err != nil {
		t.Fatalf("new Harbor client: %v", err)
	}
	page, err := client.ListArtifacts(context.Background(), "prod", "team/service", "", 1, 50)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Tags[0].Name != "v1" {
		t.Fatalf("unexpected artifacts: %+v", page.Items)
	}
}

func TestHarborClientReturnsRelativeRepositoryResourceName(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2.0/projects/prod/repositories" {
			t.Errorf("unexpected repository list path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]harborRepository{{
			ID: 9, ProjectID: 7, Name: "prod/team/service", ArtifactCount: 2,
		}})
	})
	serverURL, closeServer := startIPv4HTTPServer(t, handler)
	defer closeServer()

	client, err := newHarborClient(registryAccess{Endpoint: serverURL})
	if err != nil {
		t.Fatalf("new Harbor client: %v", err)
	}
	page, err := client.ListRepositories(context.Background(), "prod", "", 1, 100)
	if err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ResourceName != "team/service" {
		t.Fatalf("unexpected repository resource name: %+v", page.Items)
	}
}

func TestHarborRepositoryResourceNamePreservesRelativeName(t *testing.T) {
	if got := harborRepositoryResourceName("prod", "team/service"); got != "team/service" {
		t.Fatalf("relative repository name changed: %q", got)
	}
}

func TestHarborClientSurfacesStructuredErrors(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"FORBIDDEN","message":"not permitted"}]}`))
	})
	serverURL, closeServer := startIPv4HTTPServer(t, handler)
	defer closeServer()

	client, err := newHarborClient(registryAccess{Endpoint: serverURL})
	if err != nil {
		t.Fatalf("new Harbor client: %v", err)
	}
	_, err = client.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("unexpected Harbor error: %v", err)
	}
}

func TestHarborClientRejectsMissingPathValuesBeforeRequest(t *testing.T) {
	requests := 0
	serverURL, closeServer := startIPv4HTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer closeServer()

	client, err := newHarborClient(registryAccess{Endpoint: serverURL})
	if err != nil {
		t.Fatalf("new Harbor client: %v", err)
	}
	if err := client.DeleteArtifact(context.Background(), "", "repo", "sha256:test"); err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("unexpected missing project error: %v", err)
	}
	if _, err := client.ListArtifacts(context.Background(), "project", "", "", 1, 10); err == nil || !strings.Contains(err.Error(), "repository is required") {
		t.Fatalf("unexpected missing repository error: %v", err)
	}
	if requests != 0 {
		t.Fatalf("invalid Harbor requests reached the server: %d", requests)
	}
}

func TestNewHarborPageOffersNextPageWhenHeadersAreMissing(t *testing.T) {
	page := newHarborPage([]harborProject{{Name: "one"}, {Name: "two"}}, 3, 2, &http.Response{Header: http.Header{}})
	if page.NextPage != 4 {
		t.Fatalf("expected a next-page probe, got %+v", page)
	}
	shortPage := newHarborPage([]harborProject{{Name: "one"}}, 3, 2, &http.Response{Header: http.Header{}})
	if shortPage.NextPage != 0 {
		t.Fatalf("short final page unexpectedly offered another page: %+v", shortPage)
	}
}
