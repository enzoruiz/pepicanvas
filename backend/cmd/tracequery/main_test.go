package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

const decoy = "pci_decoy-content-7f3a"

func span(trace correlation.ID, id string, start time.Time) telemetry.SpanData {
	return telemetry.SpanData{TraceID: trace.String(), SpanID: id, Name: "http.request", Start: start, End: start, Status: "ok"}
}

// writeSpans writes spans and the extra raw lines to a file with mode.
func writeSpans(t *testing.T, mode os.FileMode, spans []telemetry.SpanData, extra ...string) string {
	t.Helper()
	var buf bytes.Buffer
	for _, s := range spans {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	for _, line := range extra {
		buf.WriteString(line + "\n")
	}
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func runQuery(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestQueryPrintsOnlyTheRequestedTrace(t *testing.T) {
	a, _ := correlation.New()
	b, _ := correlation.New()
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	path := writeSpans(t, 0o600, []telemetry.SpanData{
		span(a, "00000000000000a2", t0.Add(time.Second)),
		span(b, "00000000000000b1", t0),
		span(a, "00000000000000a1", t0),
	}, "not json "+decoy)

	code, stdout, stderr := runQuery("-file", path, a.String())
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stderr != "tracequery: skipped malformed lines: 1\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	var got []string
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var s telemetry.SpanData
		if err := json.Unmarshal(scanner.Bytes(), &s); err != nil {
			t.Fatalf("stdout line %q: %v", scanner.Text(), err)
		}
		if s.TraceID != a.String() {
			t.Fatalf("printed a span of another trace: %+v", s)
		}
		got = append(got, s.SpanID)
	}
	if strings.Join(got, ",") != "00000000000000a1,00000000000000a2" {
		t.Fatalf("printed spans %v, want a1,a2 in start order", got)
	}

	code, stdout, _ = runQuery("-file", path, b.String())
	if code != 0 || strings.Count(stdout, "\n") != 1 || !strings.Contains(stdout, "00000000000000b1") {
		t.Fatalf("query b: exit %d, stdout %q", code, stdout)
	}
}

func TestQueryExitsOneWhenNoSpanMatches(t *testing.T) {
	a, _ := correlation.New()
	other, _ := correlation.New()
	path := writeSpans(t, 0o600, []telemetry.SpanData{span(a, "00000000000000a1", time.Now())})
	code, stdout, stderr := runQuery("-file", path, other.String())
	if code != 1 || stdout != "" || stderr != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestQueryUsageAndInputErrorsExitTwoWithoutEchoingContent(t *testing.T) {
	a, _ := correlation.New()
	secure := writeSpans(t, 0o600, []telemetry.SpanData{span(a, "00000000000000a1", time.Now())})
	exposed := writeSpans(t, 0o644, []telemetry.SpanData{span(a, "00000000000000a1", time.Now())}, decoy)
	groupReadable := writeSpans(t, 0o640, []telemetry.SpanData{span(a, "00000000000000a1", time.Now())}, decoy)
	missing := filepath.Join(t.TempDir(), "absent.jsonl")

	for name, tc := range map[string]struct {
		args   []string
		stderr string
	}{
		"no arguments":      {nil, usage + "\n"},
		"missing file flag": {[]string{a.String()}, usage + "\n"},
		"missing id":        {[]string{"-file", secure}, usage + "\n"},
		"two ids":           {[]string{"-file", secure, a.String(), a.String()}, usage + "\n"},
		"unknown flag":      {[]string{"-x", "-file", secure, a.String()}, usage + "\n"},
		"malformed id":      {[]string{"-file", secure, decoy}, "tracequery: invalid correlation ID\n"},
		"uppercase id":      {[]string{"-file", secure, strings.ToUpper(a.String())}, "tracequery: invalid correlation ID\n"},
		"other readable":    {[]string{"-file", exposed, a.String()}, "tracequery: " + exposed + ": group or other permission bits set\n"},
		"group readable":    {[]string{"-file", groupReadable, a.String()}, "tracequery: " + groupReadable + ": group or other permission bits set\n"},
		"missing file":      {[]string{"-file", missing, a.String()}, "tracequery: " + missing + ": cannot open\n"},
		"directory":         {[]string{"-file", filepath.Dir(secure), a.String()}, "tracequery: " + filepath.Dir(secure) + ": not a regular file\n"},
	} {
		code, stdout, stderr := runQuery(tc.args...)
		if code != 2 || stdout != "" || stderr != tc.stderr {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q, want 2 and %q", name, code, stdout, stderr, tc.stderr)
		}
		if strings.Contains(stderr, decoy) {
			t.Errorf("%s: stderr echoed content: %q", name, stderr)
		}
	}
}
