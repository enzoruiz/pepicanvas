package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrInsecureFile reports a span file that is not a regular file or that
// grants group or other permissions: only its owner may read evidence.
var ErrInsecureFile = errors.New("telemetry: span file must be a regular owner-only file")

// FileExporter appends one JSON object per span per line to a local file.
// Attributes were redacted and bounded when the span was recorded; the
// exporter writes nothing else. It is safe for concurrent use.
type FileExporter struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

// OpenFile creates path with mode 0600 or opens it for appending. An existing
// file with group or other permission bits is refused and left untouched.
func OpenFile(path string) (*FileExporter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open span file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("telemetry: stat span file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return nil, ErrInsecureFile
	}
	return &FileExporter{file: f}, nil
}

// Export writes spans, one JSON line each, in a single write and syncs the
// file before returning.
func (e *FileExporter) Export(ctx context.Context, spans []SpanData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, span := range spans {
		if err := enc.Encode(span); err != nil {
			return fmt.Errorf("telemetry: encode span: %w", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return os.ErrClosed
	}
	if _, err := e.file.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("telemetry: write span file: %w", err)
	}
	if err := e.file.Sync(); err != nil {
		return fmt.Errorf("telemetry: sync span file: %w", err)
	}
	return nil
}

// Close closes the file; later exports fail. Close is idempotent.
func (e *FileExporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.file.Close()
}
