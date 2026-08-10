package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const (
	remoteProtocolVersion = 1
	remoteFrameJSON       = byte(1)
	remoteFrameData       = byte(2)
	maxRemoteJSONFrame    = 16 << 20
	maxRemoteDataFrame    = 2 << 20
)

type remoteRequest struct {
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type remoteEvent struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type remoteFrameReader struct {
	reader io.Reader
}

type remoteFrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func newRemoteFrameReader(reader io.Reader) *remoteFrameReader {
	return &remoteFrameReader{reader: reader}
}

func newRemoteFrameWriter(writer io.Writer) *remoteFrameWriter {
	return &remoteFrameWriter{writer: writer}
}

func (r *remoteFrameReader) readFrame() (byte, []byte, error) {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r.reader, header); err != nil {
		return 0, nil, err
	}
	kind := header[0]
	size := binary.BigEndian.Uint64(header[1:])
	limit := uint64(maxRemoteJSONFrame)
	if kind == remoteFrameData {
		limit = maxRemoteDataFrame
	} else if kind != remoteFrameJSON {
		return 0, nil, fmt.Errorf("unknown remote frame type %d", kind)
	}
	if size > limit {
		return 0, nil, fmt.Errorf("remote frame size %d exceeds limit %d", size, limit)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r.reader, payload); err != nil {
		return 0, nil, err
	}
	return kind, payload, nil
}

func (r *remoteFrameReader) readJSON(target any) error {
	kind, payload, err := r.readFrame()
	if err != nil {
		return err
	}
	if kind != remoteFrameJSON {
		return fmt.Errorf("expected JSON frame, got type %d", kind)
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode remote JSON frame: %w", err)
	}
	return nil
}

func (w *remoteFrameWriter) writeFrame(kind byte, payload []byte) error {
	limit := maxRemoteJSONFrame
	if kind == remoteFrameData {
		limit = maxRemoteDataFrame
	} else if kind != remoteFrameJSON {
		return fmt.Errorf("unknown remote frame type %d", kind)
	}
	if len(payload) > limit {
		return fmt.Errorf("remote frame size %d exceeds limit %d", len(payload), limit)
	}
	header := make([]byte, 9)
	header[0] = kind
	binary.BigEndian.PutUint64(header[1:], uint64(len(payload)))
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.writer.Write(header); err != nil {
		return err
	}
	_, err := w.writer.Write(payload)
	return err
}

func (w *remoteFrameWriter) writeJSON(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return w.writeFrame(remoteFrameJSON, data)
}

func (w *remoteFrameWriter) writeEvent(id, eventType string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = data
	}
	return w.writeJSON(remoteEvent{
		Version: remoteProtocolVersion,
		ID:      id,
		Type:    eventType,
		Payload: raw,
	})
}

func (w *remoteFrameWriter) writeError(id string, err error) error {
	return w.writeJSON(remoteEvent{
		Version: remoteProtocolVersion,
		ID:      id,
		Type:    "error",
		Error:   err.Error(),
	})
}
