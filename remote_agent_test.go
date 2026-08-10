package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteAgentInfoRoundTrip(t *testing.T) {
	input := encodeRemoteRequestForTest(t, remoteRequest{
		Version:   remoteProtocolVersion,
		ID:        "request-1",
		Operation: "info",
	})
	var output bytes.Buffer
	if err := runRemoteAgent(remoteAgentOptions{
		Workspace: t.TempDir(),
		Version:   "v-test",
		Stdin:     bytes.NewReader(input),
		Stdout:    &output,
	}); err != nil {
		t.Fatalf("run remote agent: %v", err)
	}
	reader := newRemoteFrameReader(&output)
	var event remoteEvent
	if err := reader.readJSON(&event); err != nil {
		t.Fatalf("read remote event: %v", err)
	}
	if event.Type != "result" || event.ID != "request-1" {
		t.Fatalf("unexpected event: %+v", event)
	}
	var info remoteAgentInfo
	if err := json.Unmarshal(event.Payload, &info); err != nil {
		t.Fatalf("decode remote info: %v", err)
	}
	if info.Version != "v-test" || info.Protocol != remoteProtocolVersion || info.OS == "" || info.Architecture == "" {
		t.Fatalf("unexpected remote info: %+v", info)
	}
}

func TestRemoteAgentStreamsFileDataAfterExplicitDownload(t *testing.T) {
	workspace := t.TempDir()
	content := bytes.Repeat([]byte("remote-file-data\n"), 1024)
	filePath := filepath.Join(workspace, "archive.tar")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("write remote file: %v", err)
	}
	payload, _ := json.Marshal(remoteFileRequest{Path: filePath, Offset: 7})
	input := encodeRemoteRequestForTest(t, remoteRequest{
		Version:   remoteProtocolVersion,
		ID:        "download-1",
		Operation: "files.download",
		Payload:   payload,
	})
	var output bytes.Buffer
	if err := runRemoteAgent(remoteAgentOptions{
		Workspace: workspace,
		Version:   "test",
		Stdin:     bytes.NewReader(input),
		Stdout:    &output,
	}); err != nil {
		t.Fatalf("run remote download: %v", err)
	}

	reader := newRemoteFrameReader(&output)
	var metaEvent remoteEvent
	if err := reader.readJSON(&metaEvent); err != nil {
		t.Fatalf("read download metadata: %v", err)
	}
	if metaEvent.Type != "meta" {
		t.Fatalf("unexpected first event: %+v", metaEvent)
	}
	var downloaded bytes.Buffer
	for {
		kind, data, err := reader.readFrame()
		if err != nil {
			t.Fatalf("read download frame: %v", err)
		}
		if kind == remoteFrameData {
			_, _ = downloaded.Write(data)
			continue
		}
		var event remoteEvent
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatalf("decode terminal event: %v", err)
		}
		if event.Type != "result" {
			t.Fatalf("unexpected terminal event: %+v", event)
		}
		break
	}
	if !bytes.Equal(downloaded.Bytes(), content[7:]) {
		t.Fatalf("downloaded data mismatch: got %d want %d", downloaded.Len(), len(content)-7)
	}
}

func TestSecureRemotePathRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.tar")
	if err := os.WriteFile(outsideFile, []byte("not accessible"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(workspace, "link.tar")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	_, err := secureRemotePath(link, []string{workspace}, true)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected symlink escape rejection, got %v", err)
	}
}

func TestDeleteRemoteFileRemovesSymlinkWithoutDeletingTarget(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.tar")
	if err := os.WriteFile(target, []byte("archive"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(workspace, "link.tar")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := deleteRemoteFile(workspace, remoteFileRequest{Path: link}); err != nil {
		t.Fatalf("delete symlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("symlink still exists: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "archive" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestDeleteRemoteFileCanRemoveDanglingSymlink(t *testing.T) {
	workspace := t.TempDir()
	link := filepath.Join(workspace, "dangling.tar")
	if err := os.Symlink(filepath.Join(workspace, "missing.tar"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := deleteRemoteFile(workspace, remoteFileRequest{Path: link}); err != nil {
		t.Fatalf("delete dangling symlink: %v", err)
	}
}

func TestRemoteFrameReaderRejectsOversizedFrame(t *testing.T) {
	header := make([]byte, 9)
	header[0] = remoteFrameJSON
	header[1] = 1
	_, _, err := newRemoteFrameReader(bytes.NewReader(header)).readFrame()
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("unexpected oversized frame error: %v", err)
	}
}

func encodeRemoteRequestForTest(t *testing.T, request remoteRequest) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := newRemoteFrameWriter(&buffer).writeJSON(request); err != nil {
		t.Fatalf("encode remote request: %v", err)
	}
	return buffer.Bytes()
}
