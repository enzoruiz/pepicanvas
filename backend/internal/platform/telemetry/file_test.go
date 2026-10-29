package telemetry_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

func spanData(trace correlation.ID, span string, start time.Time) telemetry.SpanData {
	return telemetry.SpanData{
		TraceID:    trace.String(),
		SpanID:     span,
		Name:       "http.request",
		Start:      start,
		End:        start.Add(time.Millisecond),
		Status:     "ok",
		Attributes: map[string]string{"route": "GET /api/session"},
	}
}

func openFile(t *testing.T, path string) *telemetry.FileExporter {
	t.Helper()
	exporter, err := telemetry.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile = %v", err)
	}
	return exporter
}

func readLines(t *testing.T, path string) []telemetry.SpanData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spans []telemetry.SpanData
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var span telemetry.SpanData
		if err := json.Unmarshal(scanner.Bytes(), &span); err != nil {
			t.Fatalf("line %q is not one JSON span: %v", scanner.Text(), err)
		}
		spans = append(spans, span)
	}
	return spans
}

func TestFileExporterAppendsOneOwnerOnlyJSONLinePerSpan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	trace, _ := correlation.New()
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	exporter := openFile(t, path)
	if err := exporter.Export(context.Background(), []telemetry.SpanData{spanData(trace, "00f067aa0ba902b7", start), spanData(trace, "00f067aa0ba902b8", start)}); err != nil {
		t.Fatalf("Export = %v", err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	exporter = openFile(t, path)
	if err := exporter.Export(context.Background(), []telemetry.SpanData{spanData(trace, "00f067aa0ba902b9", start)}); err != nil {
		t.Fatalf("Export after reopening = %v", err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	spans := readLines(t, path)
	if len(spans) != 3 {
		t.Fatalf("file holds %d spans, want 3 appended", len(spans))
	}
	for i, id := range []string{"00f067aa0ba902b7", "00f067aa0ba902b8", "00f067aa0ba902b9"} {
		if spans[i].SpanID != id || spans[i].TraceID != trace.String() || !spans[i].Start.Equal(start) || spans[i].Attributes["route"] != "GET /api/session" {
			t.Errorf("line %d = %+v", i, spans[i])
		}
	}
}

func TestOpenFileRefusesAFileOthersCanAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o644} {
		path := filepath.Join(t.TempDir(), "spans.jsonl")
		if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := telemetry.OpenFile(path); !errors.Is(err, telemetry.ErrInsecureFile) {
			t.Errorf("mode %v: OpenFile = %v, want ErrInsecureFile", mode, err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != "kept\n" {
			t.Errorf("mode %v: file changed to %q", mode, raw)
		}
	}
	if _, err := telemetry.OpenFile(t.TempDir()); err == nil {
		t.Error("OpenFile accepted a directory")
	}
}

func TestFileExporterRejectsExportAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	exporter := openFile(t, path)
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
	trace, _ := correlation.New()
	if err := exporter.Export(context.Background(), []telemetry.SpanData{spanData(trace, "00f067aa0ba902b7", time.Now())}); err == nil {
		t.Fatal("Export after Close succeeded")
	}
	if err := exporter.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if spans := readLines(t, path); len(spans) != 0 {
		t.Fatalf("file holds %d spans after a rejected export", len(spans))
	}
}

func TestTracerSpansReachTheFileAndQueryFindsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	exporter := openFile(t, path)
	tracer := newTracer(t, exporter)
	a, _ := correlation.New()
	b, _ := correlation.New()
	for _, id := range []correlation.ID{a, b} {
		ctx := telemetry.WithTracer(correlation.WithID(context.Background(), id), tracer)
		ctx, root := telemetry.Start(ctx, telemetry.SpanHTTPRequest)
		_, child := telemetry.Start(ctx, telemetry.SpanPostgres)
		child.End(nil)
		root.End(nil)
	}
	closeTracer(t, tracer)
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	spans, malformed, err := telemetry.Query(f, a)
	if err != nil || malformed != 0 {
		t.Fatalf("Query = %v, %d malformed", err, malformed)
	}
	if len(spans) != 2 || spans[0].Name != "http.request" || spans[1].Name != "postgres" || spans[1].ParentSpanID != spans[0].SpanID {
		t.Fatalf("spans of %s = %+v", a, spans)
	}
	for _, span := range spans {
		if span.TraceID != a.String() {
			t.Fatalf("Query returned a span of another trace: %+v", span)
		}
	}
}
