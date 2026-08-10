package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
)

func TestEnvironmentProxyRoutingUsesSchemeFallbackAndNoProxy(t *testing.T) {
	for _, key := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HTTP_PROXY", "http://http-proxy.example.test:8080")
	t.Setenv("HTTPS_PROXY", "http://https-proxy.example.test:8443")
	t.Setenv("ALL_PROXY", "socks5h://fallback.example.test:1080")
	t.Setenv("NO_PROXY", "direct.example.test")

	proxy := environmentProxyFunc()
	tests := []struct {
		rawURL string
		want   string
	}{
		{"http://registry.example.test/v2/", "http://http-proxy.example.test:8080"},
		{"https://registry.example.test/v2/", "http://https-proxy.example.test:8443"},
		{"https://direct.example.test/v2/", ""},
	}
	for _, test := range tests {
		parsed, err := url.Parse(test.rawURL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := proxy(parsed)
		if err != nil {
			t.Fatalf("proxy %s: %v", test.rawURL, err)
		}
		gotString := ""
		if got != nil {
			gotString = got.String()
		}
		if gotString != test.want {
			t.Fatalf("proxy %s: got %q want %q", test.rawURL, gotString, test.want)
		}
	}

	t.Setenv("HTTPS_PROXY", "")
	proxy = environmentProxyFunc()
	parsed, _ := url.Parse("https://registry.example.test/v2/")
	got, err := proxy(parsed)
	if err != nil {
		t.Fatalf("ALL_PROXY fallback: %v", err)
	}
	if got == nil || got.String() != "socks5h://fallback.example.test:1080" {
		t.Fatalf("unexpected ALL_PROXY fallback: %v", got)
	}
}

func TestSocksDialContextLocalDNS(t *testing.T) {
	var gotAddr string
	dial := socksDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
		gotAddr = addr
		return nil, errors.New("dial blocked")
	}, "socks5://127.0.0.1:1080", false)

	_, err := dial(context.Background(), "tcp", "localhost:443")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "local DNS") {
		t.Fatalf("expected local DNS error context, got %v", err)
	}

	host, port, splitErr := net.SplitHostPort(gotAddr)
	if splitErr != nil {
		t.Fatalf("split host/port: %v", splitErr)
	}
	if port != "443" {
		t.Fatalf("unexpected port: %s", port)
	}
	if ip := net.ParseIP(host); ip == nil {
		t.Fatalf("expected resolved IP, got %q", host)
	}
}

func TestSocksDialContextRemoteDNS(t *testing.T) {
	var gotAddr string
	dial := socksDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
		gotAddr = addr
		return nil, errors.New("dial blocked")
	}, "socks5h://127.0.0.1:1080", true)

	_, err := dial(context.Background(), "tcp", "registry-1.docker.io:443")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "remote DNS") {
		t.Fatalf("expected remote DNS error context, got %v", err)
	}
	if gotAddr != "registry-1.docker.io:443" {
		t.Fatalf("expected unresolved remote host, got %q", gotAddr)
	}
}

func TestNewHTTPClientRejectsUnsupportedProxy(t *testing.T) {
	_, err := newHTTPClient("ftp://127.0.0.1:21", false)
	if err == nil {
		t.Fatal("expected proxy scheme error")
	}
	if !strings.Contains(err.Error(), "unsupported proxy scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExplicitProxyURLRequiresHostAndNoPath(t *testing.T) {
	for _, proxy := range []string{"socks5://", "http:///proxy", "http://127.0.0.1:7890/path", "https://proxy.example.test?mode=1"} {
		if _, err := newHTTPClient(proxy, false); err == nil {
			t.Errorf("proxy %q unexpectedly succeeded", proxy)
		}
	}
	for _, proxy := range []string{"socks5://127.0.0.1:7897", "socks5h://proxy.example.test:1080", "http://user:pass@127.0.0.1:7890"} {
		if _, err := newHTTPClient(proxy, false); err != nil {
			t.Errorf("proxy %q was rejected: %v", proxy, err)
		}
	}
}

func TestProxyURLDisplayRedactsCredentials(t *testing.T) {
	const secret = "proxy-password"
	for _, raw := range []string{
		"http://proxy-user:" + secret + "@127.0.0.1:7890",
		"http://proxy-user:" + secret + "@%zz",
	} {
		display := proxyURLDisplay(raw)
		if strings.Contains(display, "proxy-user") || strings.Contains(display, secret) {
			t.Fatalf("proxy credentials leaked from %q as %q", raw, display)
		}
	}
}

func TestVerifyPayloadDigestRejectsMismatch(t *testing.T) {
	err := verifyPayloadDigest([]byte("abc"), "sha256:"+strings.Repeat("0", 64), "blob")
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewDigestHasherRejectsMalformedReference(t *testing.T) {
	for _, digest := range []string{
		"sha256:../../bad",
		"sha256:" + strings.Repeat("g", 64),
		"sha1:" + strings.Repeat("0", 40),
	} {
		if _, _, err := newDigestHasher(digest); err == nil {
			t.Errorf("malformed digest %q was accepted", digest)
		}
	}
}

func TestGetManifestDescriptorRejectsMalformedDigestBeforeRequest(t *testing.T) {
	client := &registryClient{ctx: context.Background(), scheme: "https"}
	_, _, err := client.getManifestDescriptor(
		imageRef{Registry: "registry.example.test", Repository: "team/app", Tag: "latest"},
		descriptor{Digest: "sha256:../../bad"},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid manifest descriptor digest") {
		t.Fatalf("unexpected descriptor validation result: %v", err)
	}
}

func TestReadAllLimitedRejectsOversizedResponse(t *testing.T) {
	_, err := readAllLimited(strings.NewReader("1234"), 3, "test response")
	if err == nil || !strings.Contains(err.Error(), "exceeds 3 bytes") {
		t.Fatalf("unexpected bounded read result: %v", err)
	}
}

func TestVerifyingReadCloserRejectsSizeMismatch(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	rc, err := newVerifyingReadCloser(io.NopCloser(strings.NewReader("abc")), descriptor{
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size:   4,
	}, "blob")
	if err != nil {
		t.Fatalf("newVerifyingReadCloser: %v", err)
	}
	defer rc.Close()

	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected size mismatch")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyingReadCloserRejectsNonEmptyZeroSizeDescriptor(t *testing.T) {
	rc, err := newVerifyingReadCloser(io.NopCloser(strings.NewReader("x")), descriptor{Size: 0}, "blob")
	if err != nil {
		t.Fatalf("newVerifyingReadCloser: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected zero-size descriptor mismatch, got %v", err)
	}
}

func TestVerifyingReadCloserValidatesUnreadRemainderOnClose(t *testing.T) {
	content := []byte("compressed-layer-data")
	wrongSum := sha256.Sum256([]byte("different-layer-data"))
	rc, err := newVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), descriptor{
		Digest: "sha256:" + hex.EncodeToString(wrongSum[:]),
		Size:   int64(len(content)),
	}, "blob")
	if err != nil {
		t.Fatalf("newVerifyingReadCloser: %v", err)
	}

	prefix := make([]byte, 4)
	if _, err := io.ReadFull(rc, prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	if err := rc.Close(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected close-time digest mismatch, got %v", err)
	}
}
