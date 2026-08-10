package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxHarborResponseSize = 32 << 20

type harborClient struct {
	baseURL    *url.URL
	httpClient *http.Client
	username   string
	password   string
}

type harborHealth struct {
	Status     string                  `json:"status"`
	Components []harborHealthComponent `json:"components,omitempty"`
}

type harborHealthComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type harborProject struct {
	ProjectID          int64             `json:"project_id"`
	OwnerID            int64             `json:"owner_id,omitempty"`
	Name               string            `json:"name"`
	OwnerName          string            `json:"owner_name,omitempty"`
	RepoCount          int64             `json:"repo_count"`
	CreationTime       time.Time         `json:"creation_time,omitempty"`
	UpdateTime         time.Time         `json:"update_time,omitempty"`
	CurrentUserRoleIDs []int             `json:"current_user_role_ids,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type harborRepository struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"project_id"`
	Name          string    `json:"name"`
	ResourceName  string    `json:"resource_name"`
	Description   string    `json:"description,omitempty"`
	ArtifactCount int64     `json:"artifact_count"`
	PullCount     int64     `json:"pull_count"`
	CreationTime  time.Time `json:"creation_time,omitempty"`
	UpdateTime    time.Time `json:"update_time,omitempty"`
}

type harborArtifact struct {
	ID                int64             `json:"id"`
	Type              string            `json:"type,omitempty"`
	MediaType         string            `json:"media_type,omitempty"`
	ManifestMediaType string            `json:"manifest_media_type,omitempty"`
	RepositoryName    string            `json:"repository_name,omitempty"`
	Digest            string            `json:"digest"`
	Size              int64             `json:"size"`
	PushTime          time.Time         `json:"push_time,omitempty"`
	PullTime          time.Time         `json:"pull_time,omitempty"`
	Tags              []harborTag       `json:"tags,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

type harborTag struct {
	ID           int64     `json:"id"`
	RepositoryID int64     `json:"repository_id,omitempty"`
	ArtifactID   int64     `json:"artifact_id,omitempty"`
	Name         string    `json:"name"`
	PushTime     time.Time `json:"push_time,omitempty"`
	PullTime     time.Time `json:"pull_time,omitempty"`
	Immutable    bool      `json:"immutable"`
}

type harborPage[T any] struct {
	Items     []T    `json:"items"`
	Total     int    `json:"total"`
	Page      int    `json:"page"`
	PageSize  int    `json:"page_size"`
	NextPage  int    `json:"next_page,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type harborActionRequest struct {
	Action     string `json:"action"`
	Project    string `json:"project,omitempty"`
	Repository string `json:"repository,omitempty"`
	Reference  string `json:"reference,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Query      string `json:"query,omitempty"`
	Page       int    `json:"page,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
	Public     bool   `json:"public,omitempty"`
}

func newHarborClient(access registryAccess) (*harborClient, error) {
	endpoint := normalizeRegistryEndpoint(access.Endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid Harbor endpoint %q", access.Endpoint)
	}
	httpClient, err := newHTTPClient(access.Proxy, access.Insecure)
	if err != nil {
		return nil, err
	}
	return &harborClient{
		baseURL:    u,
		httpClient: httpClient,
		username:   access.Username,
		password:   access.Password,
	}, nil
}

func (c *harborClient) Health(ctx context.Context) (harborHealth, error) {
	var result harborHealth
	err := c.do(ctx, http.MethodGet, []string{"health"}, nil, nil, []int{http.StatusOK}, &result)
	return result, err
}

func (c *harborClient) ListProjects(ctx context.Context, query string, page, pageSize int) (harborPage[harborProject], error) {
	page, pageSize = normalizeHarborPage(page, pageSize)
	values := harborPageQuery(query, page, pageSize)
	values.Set("with_detail", "true")
	var items []harborProject
	response, err := c.doResponse(ctx, http.MethodGet, []string{"projects"}, values, nil, []int{http.StatusOK}, &items)
	return newHarborPage(items, page, pageSize, response), err
}

func (c *harborClient) CreateProject(ctx context.Context, name string, public bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("project name is required")
	}
	payload := map[string]any{
		"project_name": name,
		"metadata":     map[string]string{"public": strconv.FormatBool(public)},
	}
	return c.do(ctx, http.MethodPost, []string{"projects"}, nil, payload, []int{http.StatusCreated}, nil)
}

func (c *harborClient) DeleteProject(ctx context.Context, project string) error {
	project, err := requireHarborValue("project", project)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, []string{"projects", project}, nil, nil, []int{http.StatusOK}, nil)
}

func (c *harborClient) ListRepositories(ctx context.Context, project, query string, page, pageSize int) (harborPage[harborRepository], error) {
	project, err := requireHarborValue("project", project)
	if err != nil {
		return harborPage[harborRepository]{}, err
	}
	page, pageSize = normalizeHarborPage(page, pageSize)
	values := harborPageQuery(query, page, pageSize)
	var items []harborRepository
	response, err := c.doResponse(ctx, http.MethodGet, []string{"projects", project, "repositories"}, values, nil, []int{http.StatusOK}, &items)
	if err == nil {
		for index := range items {
			items[index].ResourceName = harborRepositoryResourceName(project, items[index].Name)
		}
	}
	return newHarborPage(items, page, pageSize, response), err
}

func harborRepositoryResourceName(project, repository string) string {
	project = strings.Trim(strings.TrimSpace(project), "/")
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if project != "" {
		repository = strings.TrimPrefix(repository, project+"/")
	}
	return repository
}

func (c *harborClient) DeleteRepository(ctx context.Context, project, repository string) error {
	project, err := requireHarborValue("project", project)
	if err != nil {
		return err
	}
	repository, err = requireHarborValue("repository", repository)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, []string{
		"projects", project, "repositories", repository,
	}, nil, nil, []int{http.StatusOK}, nil)
}

func (c *harborClient) ListArtifacts(ctx context.Context, project, repository, query string, page, pageSize int) (harborPage[harborArtifact], error) {
	project, err := requireHarborValue("project", project)
	if err != nil {
		return harborPage[harborArtifact]{}, err
	}
	repository, err = requireHarborValue("repository", repository)
	if err != nil {
		return harborPage[harborArtifact]{}, err
	}
	page, pageSize = normalizeHarborPage(page, pageSize)
	values := harborPageQuery(query, page, pageSize)
	values.Set("with_tag", "true")
	values.Set("with_immutable_status", "true")
	var items []harborArtifact
	response, err := c.doResponse(ctx, http.MethodGet, []string{
		"projects", project, "repositories", repository, "artifacts",
	}, values, nil, []int{http.StatusOK}, &items)
	return newHarborPage(items, page, pageSize, response), err
}

func (c *harborClient) DeleteArtifact(ctx context.Context, project, repository, reference string) error {
	values, err := requireHarborValues(
		"project", project,
		"repository", repository,
		"reference", reference,
	)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, []string{
		"projects", values[0], "repositories", values[1], "artifacts", values[2],
	}, nil, nil, []int{http.StatusOK}, nil)
}

func (c *harborClient) CreateTag(ctx context.Context, project, repository, reference, tag string) error {
	values, err := requireHarborValues(
		"project", project,
		"repository", repository,
		"reference", reference,
		"tag", tag,
	)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, []string{
		"projects", values[0], "repositories", values[1], "artifacts", values[2], "tags",
	}, nil, map[string]string{"name": values[3]}, []int{http.StatusCreated}, nil)
}

func (c *harborClient) DeleteTag(ctx context.Context, project, repository, reference, tag string) error {
	values, err := requireHarborValues(
		"project", project,
		"repository", repository,
		"reference", reference,
		"tag", tag,
	)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, []string{
		"projects", values[0], "repositories", values[1],
		"artifacts", values[2], "tags", values[3],
	}, nil, nil, []int{http.StatusOK}, nil)
}

func executeHarborAction(ctx context.Context, client *harborClient, action harborActionRequest) (any, error) {
	switch strings.ToLower(strings.TrimSpace(action.Action)) {
	case "health":
		return client.Health(ctx)
	case "list_projects":
		return client.ListProjects(ctx, action.Query, action.Page, action.PageSize)
	case "create_project":
		return map[string]bool{"ok": true}, client.CreateProject(ctx, action.Project, action.Public)
	case "delete_project":
		return map[string]bool{"ok": true}, client.DeleteProject(ctx, action.Project)
	case "list_repositories":
		return client.ListRepositories(ctx, action.Project, action.Query, action.Page, action.PageSize)
	case "delete_repository":
		return map[string]bool{"ok": true}, client.DeleteRepository(ctx, action.Project, action.Repository)
	case "list_artifacts":
		return client.ListArtifacts(ctx, action.Project, action.Repository, action.Query, action.Page, action.PageSize)
	case "delete_artifact":
		return map[string]bool{"ok": true}, client.DeleteArtifact(ctx, action.Project, action.Repository, action.Reference)
	case "create_tag":
		return map[string]bool{"ok": true}, client.CreateTag(ctx, action.Project, action.Repository, action.Reference, action.Tag)
	case "delete_tag":
		return map[string]bool{"ok": true}, client.DeleteTag(ctx, action.Project, action.Repository, action.Reference, action.Tag)
	default:
		return nil, fmt.Errorf("unsupported Harbor action %q", action.Action)
	}
}

func (c *harborClient) do(
	ctx context.Context,
	method string,
	segments []string,
	query url.Values,
	payload any,
	expectedStatuses []int,
	out any,
) error {
	_, err := c.doResponse(ctx, method, segments, query, payload, expectedStatuses, out)
	return err
}

func (c *harborClient) doResponse(
	ctx context.Context,
	method string,
	segments []string,
	query url.Values,
	payload any,
	expectedStatuses []int,
	out any,
) (*http.Response, error) {
	requestURL := c.harborURL(segments, query)
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode Harbor request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Is-Resource-Name", "true")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if !containsStatus(expectedStatuses, resp.StatusCode) {
		defer resp.Body.Close()
		message := decodeHarborError(resp.Body)
		if message == "" {
			message = resp.Status
		}
		return resp, fmt.Errorf("Harbor API %s %s failed: %s", method, req.URL.Path, message)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp, nil
	}
	defer resp.Body.Close()
	data, err := readAllLimited(resp.Body, maxHarborResponseSize, "Harbor response")
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp, fmt.Errorf("decode Harbor response: %w", err)
	}
	return resp, nil
}

func (c *harborClient) harborURL(segments []string, query url.Values) string {
	basePath := strings.TrimRight(c.baseURL.EscapedPath(), "/") + "/api/v2.0"
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}
	rawPath := basePath + "/" + strings.Join(escaped, "/")
	result := c.baseURL.Scheme + "://" + c.baseURL.Host + rawPath
	if len(query) > 0 {
		result += "?" + query.Encode()
	}
	return result
}

func requireHarborValue(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return value, nil
}

func requireHarborValues(labelValuePairs ...string) ([]string, error) {
	if len(labelValuePairs)%2 != 0 {
		return nil, fmt.Errorf("invalid Harbor value validation")
	}
	values := make([]string, 0, len(labelValuePairs)/2)
	for index := 0; index < len(labelValuePairs); index += 2 {
		value, err := requireHarborValue(labelValuePairs[index], labelValuePairs[index+1])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func normalizeHarborPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

func harborPageQuery(query string, page, pageSize int) url.Values {
	values := url.Values{}
	values.Set("page", strconv.Itoa(page))
	values.Set("page_size", strconv.Itoa(pageSize))
	if strings.TrimSpace(query) != "" {
		values.Set("q", strings.TrimSpace(query))
	}
	return values
}

func newHarborPage[T any](items []T, page, pageSize int, response *http.Response) harborPage[T] {
	result := harborPage[T]{Items: items, Page: page, PageSize: pageSize}
	if response == nil {
		return result
	}
	result.Total, _ = strconv.Atoi(response.Header.Get("X-Total-Count"))
	result.RequestID = response.Header.Get("X-Request-Id")
	if page*pageSize < result.Total {
		result.NextPage = page + 1
	} else if result.Total == 0 && len(items) == pageSize {
		// Some reverse proxies omit Harbor's pagination headers. A full page is
		// enough to offer a safe next-page probe without hiding later entries.
		result.NextPage = page + 1
	}
	return result
}

func containsStatus(statuses []int, status int) bool {
	for _, candidate := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func decodeHarborError(reader io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(reader, 64*1024))
	var payload struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &payload) == nil && len(payload.Errors) > 0 {
		parts := make([]string, 0, len(payload.Errors))
		for _, item := range payload.Errors {
			message := strings.TrimSpace(item.Message)
			if message == "" {
				message = strings.TrimSpace(item.Code)
			}
			if message != "" {
				parts = append(parts, message)
			}
		}
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(string(data))
}
