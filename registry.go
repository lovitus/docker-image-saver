package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	mtDockerManifestV2      = "application/vnd.docker.distribution.manifest.v2+json"
	mtDockerManifestListV2  = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifestV1         = "application/vnd.oci.image.manifest.v1+json"
	mtOCIImageIndexV1       = "application/vnd.oci.image.index.v1+json"
	mtDockerConfigV1        = "application/vnd.docker.container.image.v1+json"
	mtDockerLayerGzip       = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	mtDockerLayerTar        = "application/vnd.docker.image.rootfs.diff.tar"
	mtOCILayerTar           = "application/vnd.oci.image.layer.v1.tar"
	mtOCILayerTarGzip       = "application/vnd.oci.image.layer.v1.tar+gzip"
	mtOCILayerTarZstd       = "application/vnd.oci.image.layer.v1.tar+zstd"
	mtDockerForeignLayerGz  = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
	mtDockerForeignLayer    = "application/vnd.docker.image.rootfs.foreign.diff.tar"
	defaultHTTPTimeout      = 5 * time.Minute
	defaultTokenHTTPTimeout = 1 * time.Minute
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    platform          `json:"platform,omitempty"`
}

type manifestList struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
	OSVersion    string `json:"os.version,omitempty"`
}

func (p platform) String() string {
	if p.OS == "" && p.Architecture == "" {
		return "unknown"
	}
	base := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		base += "/" + p.Variant
	}
	if p.OSVersion != "" {
		base += " (" + p.OSVersion + ")"
	}
	return base
}

type registryClient struct {
	httpClient *http.Client
	username   string
	password   string
	tokensMu   sync.Mutex
	tokens     map[string]string
}

func newRegistryClient(proxyOpt, username, password string, insecure bool) (*registryClient, error) {
	httpClient, err := newHTTPClient(proxyOpt, insecure)
	if err != nil {
		return nil, err
	}
	return &registryClient{
		httpClient: httpClient,
		username:   username,
		password:   password,
		tokens:     make(map[string]string),
	}, nil
}

func newHTTPClient(proxyOpt string, insecure bool) (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	if insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	proxyAddr := strings.TrimSpace(proxyOpt)
	if proxyAddr == "" {
		proxyAddr = firstProxyEnv()
	}
	if proxyAddr != "" {
		u, err := url.Parse(proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", proxyAddr, err)
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			dialer, err := xproxy.FromURL(u, xproxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("unable to use socks proxy %q: %w", proxyAddr, err)
			}
			transport.Proxy = nil
			if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
				transport.DialContext = contextDialer.DialContext
			} else {
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q (supported: http, https, socks5)", u.Scheme)
		}
	}

	return &http.Client{Timeout: defaultHTTPTimeout, Transport: transport}, nil
}

func firstProxyEnv() string {
	keys := []string{
		"HTTPS_PROXY", "https_proxy",
		"HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy",
	}
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func (c *registryClient) getManifest(ref imageRef, manifestRef string) ([]byte, string, error) {
	if manifestRef == "" {
		manifestRef = ref.ManifestReference()
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.RegistryHost(), ref.Repository, manifestRef)
	accept := strings.Join([]string{mtDockerManifestListV2, mtOCIImageIndexV1, mtDockerManifestV2, mtOCIManifestV1}, ", ")
	body, contentType, _, err := c.fetch(ref, url, accept)
	if err != nil {
		return nil, "", err
	}
	return body, contentType, nil
}

func (c *registryClient) getBlob(ref imageRef, digest string) ([]byte, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.RegistryHost(), ref.Repository, digest)
	body, contentType, _, err := c.fetch(ref, url, "")
	if err != nil {
		return nil, "", err
	}
	return body, contentType, nil
}

func (c *registryClient) openBlob(ref imageRef, digest string) (io.ReadCloser, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.RegistryHost(), ref.Repository, digest)
	_, contentType, resp, err := c.fetch(ref, url, "")
	if err != nil {
		return nil, "", err
	}
	return resp.Body, contentType, nil
}

func (c *registryClient) fetch(ref imageRef, rawURL, accept string) ([]byte, string, *http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.doWithAuth(ref, req)
	if err != nil {
		return nil, "", nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, "", nil, fmt.Errorf("request %s failed: %s (%s)", rawURL, resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.Body == nil {
		return nil, "", nil, fmt.Errorf("empty response body for %s", rawURL)
	}

	if strings.Contains(rawURL, "/blobs/") {
		return nil, resp.Header.Get("Content-Type"), resp, nil
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", nil, err
	}
	return body, resp.Header.Get("Content-Type"), resp, nil
}

func (c *registryClient) doWithAuth(ref imageRef, req *http.Request) (*http.Response, error) {
	scope := fmt.Sprintf("repository:%s:pull", ref.Repository)
	if token := c.getToken(req.URL.Host, scope); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("Www-Authenticate")
	if challenge == "" {
		return resp, nil
	}

	scheme, params := parseAuthChallenge(challenge)
	if strings.ToLower(scheme) != "bearer" {
		return resp, nil
	}

	token, err := c.getOrFetchBearerToken(req.Context(), req.URL.Host, scope, params)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	retry := req.Clone(req.Context())
	retry.Header = req.Header.Clone()
	retry.Header.Set("Authorization", "Bearer "+token)
	if c.username != "" && c.password != "" {
		retry.SetBasicAuth(c.username, c.password)
	}
	return c.httpClient.Do(retry)
}

func (c *registryClient) getToken(host, scope string) string {
	key := host + "|" + scope
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	return c.tokens[key]
}

func (c *registryClient) storeToken(host, scope, token string) {
	key := host + "|" + scope
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	c.tokens[key] = token
}

func (c *registryClient) getOrFetchBearerToken(ctx context.Context, host, scope string, params map[string]string) (string, error) {
	if token := c.getToken(host, scope); token != "" {
		return token, nil
	}

	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry requested bearer auth without realm")
	}
	query := url.Values{}
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if challengeScope := params["scope"]; challengeScope != "" {
		query.Set("scope", challengeScope)
	} else {
		query.Set("scope", scope)
	}
	requestURL := realm
	if strings.Contains(realm, "?") {
		requestURL += "&" + query.Encode()
	} else {
		requestURL += "?" + query.Encode()
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTokenHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return "", fmt.Errorf("token request failed: %s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("invalid token response: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("token response missing token")
	}
	c.storeToken(host, scope, token)
	return token, nil
}

func parseAuthChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	split := strings.SplitN(header, " ", 2)
	scheme := strings.TrimSpace(split[0])
	params := make(map[string]string)
	if len(split) < 2 {
		return scheme, params
	}
	rest := split[1]
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		params[strings.ToLower(key)] = value
	}
	return scheme, params
}
