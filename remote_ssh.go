package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const releaseRepository = "lovitus/docker-image-saver"

type unknownSSHHostKeyError struct {
	Fingerprint string
}

func (e *unknownSSHHostKeyError) Error() string {
	return "SSH host key is not trusted; confirm fingerprint " + e.Fingerprint
}

type sshHostKeyMismatchError struct {
	Expected string
	Actual   string
}

func (e *sshHostKeyMismatchError) Error() string {
	return fmt.Sprintf("SSH host key changed: expected %s, received %s", e.Expected, e.Actual)
}

type sshRemoteClient struct {
	profile remoteProfile
	secret  string
	version string
}

type sshConnection struct {
	client    *ssh.Client
	agentConn net.Conn
}

type remoteOperation struct {
	connection *sshConnection
	session    *ssh.Session
	reader     *remoteFrameReader
	writer     *remoteFrameWriter
	stderr     *limitedBuffer
	done       chan struct{}
	closeOnce  sync.Once
}

func newSSHRemoteClient(profile remoteProfile, secret, localVersion string) (*sshRemoteClient, error) {
	if profile.Port == 0 {
		profile.Port = 22
	}
	if profile.AuthMethod == "" {
		profile.AuthMethod = "agent"
	}
	if err := validateRemoteProfile(profile); err != nil {
		return nil, err
	}
	return &sshRemoteClient{profile: profile, secret: secret, version: localVersion}, nil
}

func (c *sshRemoteClient) probeHostKey(ctx context.Context) (string, error) {
	address := net.JoinHostPort(c.profile.Address, fmt.Sprintf("%d", c.profile.Port))
	dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("connect SSH %s: %w", address, err)
	}
	defer conn.Close()
	var fingerprint string
	config := &ssh.ClientConfig{
		User:    c.profile.User,
		Timeout: 15 * time.Second,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return &unknownSSHHostKeyError{Fingerprint: fingerprint}
		},
	}
	_, _, _, err = ssh.NewClientConn(conn, address, config)
	var unknown *unknownSSHHostKeyError
	if errors.As(err, &unknown) {
		return unknown.Fingerprint, nil
	}
	if err != nil {
		return "", fmt.Errorf("read SSH host key: %w", err)
	}
	if fingerprint == "" {
		return "", fmt.Errorf("SSH server did not provide a host key")
	}
	return fingerprint, nil
}

func (c *sshRemoteClient) connect(ctx context.Context) (*sshConnection, error) {
	expectedFingerprint := strings.TrimSpace(c.profile.HostKeyFingerprint)
	if expectedFingerprint == "" {
		fingerprint, err := c.probeHostKey(ctx)
		if err != nil {
			return nil, err
		}
		return nil, &unknownSSHHostKeyError{Fingerprint: fingerprint}
	}
	authMethods, agentConn, err := c.authMethods()
	if err != nil {
		return nil, err
	}
	if agentConn != nil {
		defer func() {
			if err != nil {
				_ = agentConn.Close()
			}
		}()
	}
	config := &ssh.ClientConfig{
		User:    c.profile.User,
		Auth:    authMethods,
		Timeout: 20 * time.Second,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			actual := ssh.FingerprintSHA256(key)
			if actual != expectedFingerprint {
				return &sshHostKeyMismatchError{Expected: expectedFingerprint, Actual: actual}
			}
			return nil
		},
	}
	address := net.JoinHostPort(c.profile.Address, fmt.Sprintf("%d", c.profile.Port))
	dialer := net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect SSH %s: %w", address, err)
	}
	connection, channels, requests, err := ssh.NewClientConn(netConn, address, config)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("authenticate SSH %s: %w", address, err)
	}
	return &sshConnection{
		client:    ssh.NewClient(connection, channels, requests),
		agentConn: agentConn,
	}, nil
}

func (c *sshRemoteClient) authMethods() ([]ssh.AuthMethod, net.Conn, error) {
	switch strings.ToLower(strings.TrimSpace(c.profile.AuthMethod)) {
	case "agent":
		socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
		if socket == "" {
			return nil, nil, fmt.Errorf("SSH_AUTH_SOCK is not set; choose key or password authentication")
		}
		conn, err := net.DialTimeout("unix", socket, 5*time.Second)
		if err != nil {
			return nil, nil, fmt.Errorf("connect SSH agent: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(conn).Signers)}, conn, nil
	case "key":
		keyPath, err := expandUserPath(c.profile.KeyPath)
		if err != nil {
			return nil, nil, err
		}
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read SSH private key: %w", err)
		}
		var signer ssh.Signer
		if c.secret != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(c.secret))
		} else {
			signer, err = ssh.ParsePrivateKey(data)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parse SSH private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil
	case "password":
		if c.secret == "" {
			return nil, nil, fmt.Errorf("SSH password is not saved for this execution machine")
		}
		return []ssh.AuthMethod{ssh.Password(c.secret)}, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported SSH authentication method %q", c.profile.AuthMethod)
	}
}

func (c *sshRemoteClient) test(ctx context.Context) (remoteAgentInfo, error) {
	connection, err := c.connect(ctx)
	if err != nil {
		return remoteAgentInfo{}, err
	}
	defer connection.close()
	if err := c.ensureRemoteAgent(ctx, connection.client); err != nil {
		return remoteAgentInfo{}, err
	}
	var info remoteAgentInfo
	if err := c.callWithConnection(ctx, connection, "info", nil, nil, &info); err != nil {
		return remoteAgentInfo{}, err
	}
	return info, nil
}

func (c *sshRemoteClient) call(ctx context.Context, operation string, payload any, onProgress func(json.RawMessage), target any) error {
	connection, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer connection.close()
	if err := c.ensureRemoteAgent(ctx, connection.client); err != nil {
		return err
	}
	return c.callWithConnection(ctx, connection, operation, payload, onProgress, target)
}

func (c *sshRemoteClient) callWithConnection(ctx context.Context, connection *sshConnection, operation string, payload any, onProgress func(json.RawMessage), target any) error {
	operationSession, err := c.startOperation(connection, ctx)
	if err != nil {
		return err
	}
	defer operationSession.close()
	requestID := newConfigID("req")
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if payload == nil {
		rawPayload = nil
	}
	if err := operationSession.writer.writeJSON(remoteRequest{
		Version:   remoteProtocolVersion,
		ID:        requestID,
		Operation: operation,
		Payload:   rawPayload,
	}); err != nil {
		return err
	}

	var result json.RawMessage
	var agentErr error
	for {
		kind, data, readErr := operationSession.reader.readFrame()
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read remote agent response: %w", readErr)
		}
		if kind != remoteFrameJSON {
			return fmt.Errorf("unexpected binary frame for operation %s", operation)
		}
		var event remoteEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("decode remote event: %w", err)
		}
		if event.ID != requestID || event.Version != remoteProtocolVersion {
			return fmt.Errorf("remote agent response does not match request")
		}
		switch event.Type {
		case "progress":
			if onProgress != nil {
				onProgress(event.Payload)
			}
		case "result":
			result = append(result[:0], event.Payload...)
		case "error":
			agentErr = errors.New(event.Error)
		default:
			return fmt.Errorf("unexpected remote event type %q", event.Type)
		}
	}
	waitErr := operationSession.wait()
	if target != nil && len(result) > 0 {
		if err := json.Unmarshal(result, target); err != nil {
			return fmt.Errorf("decode remote result: %w", err)
		}
	}
	if agentErr != nil {
		return agentErr
	}
	if waitErr != nil {
		return fmt.Errorf("remote agent exited: %w%s", waitErr, operationSession.stderrSuffix())
	}
	if len(result) == 0 {
		return fmt.Errorf("remote agent returned no result%s", operationSession.stderrSuffix())
	}
	return nil
}

func (c *sshRemoteClient) info(ctx context.Context) (remoteAgentInfo, error) {
	var result remoteAgentInfo
	err := c.call(ctx, "info", nil, nil, &result)
	return result, err
}

func (c *sshRemoteClient) inspectImage(ctx context.Context, request remoteImageRequest) (remoteImageInspection, error) {
	var result remoteImageInspection
	err := c.call(ctx, "image.inspect", request, nil, &result)
	return result, err
}

func (c *sshRemoteClient) harborAction(ctx context.Context, request remoteHarborRequest) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.call(ctx, "harbor.action", request, nil, &result)
	return result, err
}

func (c *sshRemoteClient) runSync(ctx context.Context, request syncJobSpec, progress func(syncProgressEvent)) (syncJobResult, error) {
	var result syncJobResult
	err := c.call(ctx, "sync.run", request, func(raw json.RawMessage) {
		if progress == nil {
			return
		}
		var event syncProgressEvent
		if json.Unmarshal(raw, &event) == nil {
			progress(event)
		}
	}, &result)
	return result, err
}

func (c *sshRemoteClient) probeStorage(ctx context.Context, roots []string) ([]storageCandidate, error) {
	var result []storageCandidate
	err := c.call(ctx, "storage.probe", remoteStorageRequest{StorageRoots: roots}, nil, &result)
	return result, err
}

func (c *sshRemoteClient) listFiles(ctx context.Context, request remoteFileRequest) ([]remoteFileEntry, error) {
	var result []remoteFileEntry
	err := c.call(ctx, "files.list", request, nil, &result)
	return result, err
}

func (c *sshRemoteClient) deleteFile(ctx context.Context, request remoteFileRequest) error {
	var result map[string]bool
	return c.call(ctx, "files.delete", request, nil, &result)
}

func (c *sshRemoteClient) downloadFile(ctx context.Context, request remoteFileRequest, destination io.Writer, onMeta func(remoteFileDownloadMeta) error) (remoteFileDownloadMeta, error) {
	connection, err := c.connect(ctx)
	if err != nil {
		return remoteFileDownloadMeta{}, err
	}
	defer connection.close()
	if err := c.ensureRemoteAgent(ctx, connection.client); err != nil {
		return remoteFileDownloadMeta{}, err
	}
	operationSession, err := c.startOperation(connection, ctx)
	if err != nil {
		return remoteFileDownloadMeta{}, err
	}
	defer operationSession.close()
	requestID := newConfigID("req")
	rawPayload, err := json.Marshal(request)
	if err != nil {
		return remoteFileDownloadMeta{}, err
	}
	if err := operationSession.writer.writeJSON(remoteRequest{
		Version: remoteProtocolVersion, ID: requestID, Operation: "files.download", Payload: rawPayload,
	}); err != nil {
		return remoteFileDownloadMeta{}, err
	}

	var meta remoteFileDownloadMeta
	var received int64
	var agentErr error
	resultReceived := false
	for {
		kind, data, readErr := operationSession.reader.readFrame()
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return meta, fmt.Errorf("read remote file frame: %w", readErr)
		}
		if kind == remoteFrameData {
			if meta.Path == "" {
				return meta, fmt.Errorf("remote agent sent file data before metadata")
			}
			if _, err := destination.Write(data); err != nil {
				return meta, err
			}
			received += int64(len(data))
			continue
		}
		var event remoteEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return meta, err
		}
		if event.ID != requestID || event.Version != remoteProtocolVersion {
			return meta, fmt.Errorf("remote file response does not match request")
		}
		switch event.Type {
		case "meta":
			if err := json.Unmarshal(event.Payload, &meta); err != nil {
				return meta, err
			}
			if onMeta != nil {
				if err := onMeta(meta); err != nil {
					return meta, err
				}
			}
		case "result":
			resultReceived = true
		case "error":
			agentErr = errors.New(event.Error)
		default:
			return meta, fmt.Errorf("unexpected remote file event %q", event.Type)
		}
	}
	waitErr := operationSession.wait()
	if agentErr != nil {
		return meta, agentErr
	}
	if waitErr != nil {
		return meta, fmt.Errorf("remote agent exited: %w%s", waitErr, operationSession.stderrSuffix())
	}
	if !resultReceived || meta.Path == "" {
		return meta, fmt.Errorf("remote download ended without completion metadata")
	}
	if received != meta.Size-meta.Offset {
		return meta, fmt.Errorf("remote download size mismatch: got %d want %d", received, meta.Size-meta.Offset)
	}
	return meta, nil
}

func (c *sshRemoteClient) downloadFileToLocal(ctx context.Context, request remoteFileRequest, destination string) (remoteFileDownloadMeta, error) {
	destination, err := expandLocalDownloadPath(destination)
	if err != nil {
		return remoteFileDownloadMeta{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return remoteFileDownloadMeta{}, err
	}
	partPath := destination + ".part"
	var lastMeta remoteFileDownloadMeta
	for attempt := 0; attempt < 2; attempt++ {
		part, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return lastMeta, err
		}
		info, err := part.Stat()
		if err != nil {
			_ = part.Close()
			return lastMeta, err
		}
		request.Offset = info.Size()
		if _, err := part.Seek(request.Offset, io.SeekStart); err != nil {
			_ = part.Close()
			return lastMeta, err
		}
		meta, downloadErr := c.downloadFile(ctx, request, part, nil)
		lastMeta = meta
		closeErr := part.Close()
		if downloadErr != nil {
			if attempt == 0 && request.Offset > 0 && strings.Contains(downloadErr.Error(), "offset is out of range") {
				if err := os.Truncate(partPath, 0); err != nil {
					return meta, err
				}
				continue
			}
			return meta, downloadErr
		}
		if closeErr != nil {
			return meta, closeErr
		}
		if err := verifyLocalFileSHA256(partPath, meta.Size, meta.SHA256); err != nil {
			if attempt == 0 && request.Offset > 0 {
				if truncateErr := os.Truncate(partPath, 0); truncateErr != nil {
					return meta, truncateErr
				}
				continue
			}
			return meta, err
		}
		if err := replaceFile(partPath, destination); err != nil {
			return meta, fmt.Errorf("finalize local download: %w", err)
		}
		return meta, nil
	}
	return lastMeta, fmt.Errorf("remote download retry exhausted")
}

func verifyLocalFileSHA256(path string, expectedSize int64, expectedDigest string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("downloaded file size mismatch: got %d want %d", info.Size(), expectedSize)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != strings.ToLower(strings.TrimSpace(expectedDigest)) {
		return fmt.Errorf("downloaded file checksum mismatch: got %s want %s", actual, expectedDigest)
	}
	return nil
}

func (c *sshRemoteClient) startOperation(connection *sshConnection, ctx context.Context) (*remoteOperation, error) {
	binaryPath, workspace, err := c.remotePaths(connection.client)
	if err != nil {
		return nil, err
	}
	session, err := connection.client.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stderr := &limitedBuffer{limit: 16 * 1024}
	go func() {
		_, _ = io.Copy(stderr, stderrPipe)
	}()
	command := posixShellQuote(binaryPath) + " remote-agent --stdio --workspace " + posixShellQuote(workspace)
	if err := session.Start(command); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("start remote agent: %w", err)
	}
	operation := &remoteOperation{
		connection: connection,
		session:    session,
		reader:     newRemoteFrameReader(stdout),
		writer:     newRemoteFrameWriter(stdin),
		stderr:     stderr,
		done:       make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			operation.close()
		case <-operation.done:
		}
	}()
	return operation, nil
}

func (c *sshRemoteClient) ensureRemoteAgent(ctx context.Context, client *ssh.Client) error {
	binaryPath, _, err := c.remotePaths(client)
	if err != nil {
		return err
	}
	remoteVersion, versionErr := runSSHCommand(client, posixShellQuote(binaryPath)+" remote-agent --version")
	remoteVersion = strings.TrimSpace(remoteVersion)
	if versionErr == nil && remoteVersion != "" && (c.version == "" || c.version == "dev" || remoteVersion == c.version) {
		return nil
	}
	remoteOS, remoteArch, err := detectRemotePlatform(client)
	if err != nil {
		return err
	}
	binary, expectedVersion, err := c.bootstrapBinary(ctx, remoteOS, remoteArch)
	if err != nil {
		if versionErr != nil {
			return fmt.Errorf("remote agent is unavailable and automatic bootstrap failed: %w", err)
		}
		return err
	}
	if err := uploadRemoteBinary(client, binaryPath, binary); err != nil {
		return err
	}
	installedVersion, err := runSSHCommand(client, posixShellQuote(binaryPath)+" remote-agent --version")
	if err != nil {
		return fmt.Errorf("verify installed remote agent: %w", err)
	}
	if strings.TrimSpace(installedVersion) != expectedVersion && expectedVersion != "dev" {
		return fmt.Errorf("installed remote agent version %q does not match %q", strings.TrimSpace(installedVersion), expectedVersion)
	}
	return nil
}

func (c *sshRemoteClient) remotePaths(client *ssh.Client) (string, string, error) {
	home, err := runSSHCommand(client, `printf '%s' "$HOME"`)
	if err != nil {
		return "", "", fmt.Errorf("resolve remote home directory: %w", err)
	}
	home = strings.TrimSpace(home)
	if home == "" || !strings.HasPrefix(home, "/") {
		return "", "", fmt.Errorf("remote home directory is invalid")
	}
	binaryPath := expandRemoteHomePath(c.profile.DiaPath, home)
	if binaryPath == "" {
		binaryPath = path.Join(home, ".local", "bin", "dia")
	}
	workspace := expandRemoteHomePath(c.profile.Workspace, home)
	if workspace == "" {
		workspace = path.Join(home, ".local", "share", "dia")
	}
	return path.Clean(binaryPath), path.Clean(workspace), nil
}

func expandRemoteHomePath(value, home string) string {
	value = strings.TrimSpace(value)
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return path.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return value
}

func detectRemotePlatform(client *ssh.Client) (string, string, error) {
	output, err := runSSHCommand(client, `uname -s; uname -m`)
	if err != nil {
		return "", "", fmt.Errorf("detect remote platform: %w", err)
	}
	lines := strings.Fields(output)
	if len(lines) < 2 {
		return "", "", fmt.Errorf("unexpected remote platform response %q", output)
	}
	var goos string
	switch strings.ToLower(lines[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported remote operating system %q", lines[0])
	}
	goarch := normalizeRemoteArchitecture(lines[1])
	if goarch == "" {
		return "", "", fmt.Errorf("unsupported remote architecture %q", lines[1])
	}
	return goos, goarch, nil
}

func normalizeRemoteArchitecture(value string) string {
	archMap := map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
		"i386": "386", "i486": "386", "i586": "386", "i686": "386", "x86": "386",
		"arm": "arm", "armv5l": "arm", "armv5tel": "arm", "armv6l": "arm", "armv7l": "arm", "armv8l": "arm",
		"loong64": "loong64", "loongarch64": "loong64",
		"mips": "mips", "mipsel": "mipsle", "mipsle": "mipsle",
		"mips64": "mips64", "mips64el": "mips64le", "mips64le": "mips64le",
		"ppc64": "ppc64", "ppc64le": "ppc64le",
		"riscv64": "riscv64", "s390x": "s390x",
	}
	return archMap[strings.ToLower(strings.TrimSpace(value))]
}

func (c *sshRemoteClient) bootstrapBinary(ctx context.Context, goos, goarch string) ([]byte, string, error) {
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		executable, err := os.Executable()
		if err == nil {
			binary, readErr := os.ReadFile(executable)
			if readErr == nil {
				return binary, c.version, nil
			}
		}
	}
	if c.version == "" || c.version == "dev" {
		return nil, "", fmt.Errorf("a development build cannot bootstrap a different platform; install a released dia binary on the execution machine")
	}
	binary, err := downloadReleaseBinary(ctx, c.version, goos, goarch)
	return binary, c.version, err
}

func downloadReleaseBinary(ctx context.Context, releaseVersion, goos, goarch string) ([]byte, error) {
	base := "https://github.com/" + releaseRepository + "/releases/download/" + releaseVersion + "/"
	client, err := newHTTPClient("", false)
	if err != nil {
		return nil, fmt.Errorf("configure release download transport: %w", err)
	}
	client.Timeout = 5 * time.Minute
	checksums, err := downloadLimited(ctx, client, base+"checksums.txt", 4<<20)
	if err != nil {
		return nil, fmt.Errorf("download release checksums: %w", err)
	}
	assetName := "dia_" + goos + "_" + goarch
	if goos == "windows" {
		assetName += ".exe"
	}
	expectedDigest := checksumForAsset(checksums, assetName)
	if expectedDigest == "" {
		return nil, fmt.Errorf("release checksums do not contain %s", assetName)
	}
	binary, err := downloadLimited(ctx, client, base+assetName, 256<<20)
	if err != nil {
		return nil, fmt.Errorf("download release asset %s: %w", assetName, err)
	}
	sum := sha256.Sum256(binary)
	actual := hex.EncodeToString(sum[:])
	if actual != expectedDigest {
		return nil, fmt.Errorf("release asset checksum mismatch: got %s want %s", actual, expectedDigest)
	}
	return binary, nil
}

func downloadLimited(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}
	reader := io.LimitReader(resp.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return data, nil
}

func checksumForAsset(data []byte, assetName string) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == assetName {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

func uploadRemoteBinary(client *ssh.Client, binaryPath string, binary []byte) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	temporaryPath := binaryPath + ".upload"
	command := "umask 077; mkdir -p " + posixShellQuote(path.Dir(binaryPath)) +
		" && cat > " + posixShellQuote(temporaryPath) +
		" && chmod 755 " + posixShellQuote(temporaryPath) +
		" && mv " + posixShellQuote(temporaryPath) + " " + posixShellQuote(binaryPath)
	session.Stdin = bytes.NewReader(binary)
	output, err := session.CombinedOutput(command)
	if err != nil {
		return fmt.Errorf("upload remote agent: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runSSHCommand(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	output, err := session.CombinedOutput(command)
	if err != nil {
		return "", fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (c *sshConnection) close() {
	if c == nil {
		return
	}
	if c.client != nil {
		_ = c.client.Close()
	}
	if c.agentConn != nil {
		_ = c.agentConn.Close()
	}
}

func (o *remoteOperation) close() {
	if o == nil {
		return
	}
	o.closeOnce.Do(func() {
		close(o.done)
		_ = o.session.Close()
	})
}

func (o *remoteOperation) wait() error {
	err := o.session.Wait()
	o.closeOnce.Do(func() { close(o.done) })
	return err
}

func (o *remoteOperation) stderrSuffix() string {
	message := strings.TrimSpace(o.stderr.String())
	if message == "" {
		return ""
	}
	return " (" + message + ")"
}

type limitedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	if b.limit <= 0 {
		b.data = nil
		return written, nil
	}
	if len(data) >= b.limit {
		b.data = append(b.data[:0], data[len(data)-b.limit:]...)
		return written, nil
	}
	b.data = append(b.data, data...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return written, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

func expandLocalDownloadPath(value string) (string, error) {
	return expandUserPath(filepath.Clean(value))
}
