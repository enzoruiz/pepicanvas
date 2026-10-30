//go:build linux

package telemetry_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

const (
	tornFileEnv  = "TELEMETRY_TORN_FILE"
	tornTraceEnv = "TELEMETRY_TORN_TRACE"
)

// TestAFailedWriteLeavesNoTornLine makes the file size limit cut a batch in
// the middle of a line. The helper runs in a child process because the limit
// applies to every file the process writes.
func TestAFailedWriteLeavesNoTornLine(t *testing.T) {
	if path := os.Getenv(tornFileEnv); path != "" {
		writeAcrossTheSizeLimit(t, path, os.Getenv(tornTraceEnv))
		return
	}
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	trace, _ := correlation.New()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAFailedWriteLeavesNoTornLine$", "-test.count=1")
	cmd.Env = append(os.Environ(), tornFileEnv+"="+path, tornTraceEnv+"="+trace.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}

	spans := readLines(t, path)
	if len(spans) != 2 || spans[0].SpanID != "00000000000000a1" || spans[1].SpanID != "00000000000000a4" {
		t.Fatalf("file lines = %+v, want the spans a1 and a4 only", spans)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, malformed, err := telemetry.Query(f, trace)
	if err != nil || malformed != 0 || len(got) != 2 {
		t.Fatalf("Query = %d spans, %d malformed, %v; want 2, 0, nil", len(got), malformed, err)
	}
}

func writeAcrossTheSizeLimit(t *testing.T, path, traceHex string) {
	trace, err := correlation.Parse(traceHex)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	exporter := openFile(t, path)
	ctx := context.Background()
	if err := exporter.Export(ctx, []telemetry.SpanData{spanData(trace, "00000000000000a1", start)}); err != nil {
		t.Fatalf("first Export = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	cut := limit
	cut.Cur = uint64(info.Size()) + 40 // inside the next span's line
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cut); err != nil {
		t.Fatal(err)
	}
	err = exporter.Export(ctx, []telemetry.SpanData{spanData(trace, "00000000000000a2", start), spanData(trace, "00000000000000a3", start)})
	if restore := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); restore != nil {
		t.Fatal(restore)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "telemetry: write span file") {
		t.Fatalf("Export past the size limit = %v, want a write error", err)
	}
	if err := exporter.Export(ctx, []telemetry.SpanData{spanData(trace, "00000000000000a4", start)}); err != nil {
		t.Fatalf("Export after the failed write = %v", err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}
}
