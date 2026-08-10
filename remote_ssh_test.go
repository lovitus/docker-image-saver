package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHHostKeyProbeDoesNotSendPasswordBeforeConfirmation(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("create host signer: %v", err)
	}
	var passwordAttempts atomic.Int32
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			passwordAttempts.Add(1)
			if metadata.User() == "dia" && string(password) == "test-password" {
				return nil, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	serverConfig.AddHostKey(signer)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen SSH test server: %v", err)
	}
	defer listener.Close()
	go serveTestSSH(listener, serverConfig)

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	client, err := newSSHRemoteClient(remoteProfile{
		Name:       "test",
		Address:    host,
		Port:       port,
		User:       "dia",
		AuthMethod: "password",
		Workspace:  "/tmp/dia-test",
	}, "test-password", "dev")
	if err != nil {
		t.Fatalf("new SSH remote client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fingerprint, err := client.probeHostKey(ctx)
	if err != nil {
		t.Fatalf("probe host key: %v", err)
	}
	if fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("unexpected fingerprint: %q", fingerprint)
	}
	if passwordAttempts.Load() != 0 {
		t.Fatalf("host-key probe attempted password authentication %d time(s)", passwordAttempts.Load())
	}

	client.profile.HostKeyFingerprint = fingerprint
	connection, err := client.connect(ctx)
	if err != nil {
		t.Fatalf("connect after trusting host key: %v", err)
	}
	connection.close()
	if passwordAttempts.Load() != 1 {
		t.Fatalf("expected one password authentication after confirmation, got %d", passwordAttempts.Load())
	}
}

func TestNormalizeRemoteArchitectureCoversReleaseTargets(t *testing.T) {
	tests := map[string]string{
		"x86_64": "amd64", "i686": "386", "aarch64": "arm64", "armv7l": "arm",
		"loongarch64": "loong64", "mips": "mips", "mipsel": "mipsle",
		"mips64": "mips64", "mips64el": "mips64le", "ppc64": "ppc64",
		"ppc64le": "ppc64le", "riscv64": "riscv64", "s390x": "s390x",
	}
	for uname, want := range tests {
		if got := normalizeRemoteArchitecture(uname); got != want {
			t.Errorf("normalizeRemoteArchitecture(%q) = %q, want %q", uname, got, want)
		}
	}
	if got := normalizeRemoteArchitecture("sparc64"); got != "" {
		t.Fatalf("unsupported architecture mapped to %q", got)
	}
}

func TestChecksumForAsset(t *testing.T) {
	data := []byte("abc123  dia_linux_amd64\nfff999 *dia_windows_amd64.exe\n")
	if got := checksumForAsset(data, "dia_windows_amd64.exe"); got != "fff999" {
		t.Fatalf("unexpected checksum: %q", got)
	}
}

func TestPosixShellQuote(t *testing.T) {
	if got := posixShellQuote("/tmp/user's dia"); got != `'/tmp/user'"'"'s dia'` {
		t.Fatalf("unexpected shell quote: %s", got)
	}
}

func serveTestSSH(listener net.Listener, config *ssh.ServerConfig) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
			if err != nil {
				_ = conn.Close()
				return
			}
			go ssh.DiscardRequests(requests)
			for channel := range channels {
				_ = channel.Reject(ssh.UnknownChannelType, "test server does not accept sessions")
			}
			_ = serverConn.Close()
		}()
	}
}
