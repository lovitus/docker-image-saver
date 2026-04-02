package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

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

func TestVerifyPayloadDigestRejectsMismatch(t *testing.T) {
	err := verifyPayloadDigest([]byte("abc"), "sha256:deadbeef", "blob")
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("unexpected error: %v", err)
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
