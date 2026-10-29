package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func testLogger() (*slog.Logger, *syncBuffer) {
	logs := &syncBuffer{}
	return slog.New(logging.NewHandler(slog.NewJSONHandler(logs, nil))), logs
}

// logRecord returns the first log record with message msg.
func logRecord(logs, msg string) map[string]any {
	for _, line := range strings.Split(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			return record
		}
	}
	return nil
}

// S10, L9: without PEPICANVAS_TRACE_FILE export stays disabled: no tracer,
// no file, and stopping logs nothing.
func TestTracerFromEnvDisablesExportWithoutATraceFile(t *testing.T) {
	logger, logs := testLogger()
	tracer, stop, err := tracerFromEnv(envOf(nil), logger)
	if err != nil || tracer != nil {
		t.Fatalf("tracerFromEnv = %v, %v; want no tracer and no error", tracer, err)
	}
	stop()
	stop()
	if logs.String() != "" {
		t.Fatalf("logs = %s, want none", logs)
	}
}

// S9, L9: a trace file is created owner-only, receives every ended span, and
// stopping closes the tracer and logs its four counters.
func TestTracerFromEnvExportsToAnOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	logger, logs := testLogger()
	tracer, stop, err := tracerFromEnv(envOf(map[string]string{"PEPICANVAS_TRACE_FILE": path}), logger)
	if err != nil || tracer == nil {
		t.Fatalf("tracerFromEnv = %v, %v; want a tracer", tracer, err)
	}
	_, span := telemetry.Start(telemetry.WithTracer(context.Background(), tracer), telemetry.SpanHTTPRequest)
	span.End(nil)
	stop()
	stop()

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file = %v, %v; want mode 0600", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Count(data, []byte("\n")) != 1 || !bytes.Contains(data, []byte(`"name":"http.request"`)) {
		t.Fatalf("trace file = %q, %v; want one http.request span", data, err)
	}
	record := logRecord(logs.String(), "telemetry stopped")
	if record == nil {
		t.Fatalf("logs lack telemetry stopped:\n%s", logs)
	}
	for field, want := range map[string]float64{"exported": 1, "dropped": 0, "failed": 0, "discarded": 0} {
		if record[field] != want {
			t.Fatalf("telemetry stopped %s = %v, want %v\n%s", field, record[field], want, logs)
		}
	}
	if strings.Count(logs.String(), "telemetry stopped") != 1 {
		t.Fatalf("telemetry stopped logged more than once:\n%s", logs)
	}
}

// S11, L9: an unusable trace file is refused naming only the variable, and an
// existing loose file is left untouched.
func TestTracerFromEnvRefusesAnUnusableTraceFile(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose-decoy.jsonl")
	if err := os.WriteFile(loose, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ path, want string }{
		"group readable file": {loose, "PEPICANVAS_TRACE_FILE must name a regular owner-only file"},
		"directory":           {dir, "PEPICANVAS_TRACE_FILE cannot be opened"},
		"missing directory":   {filepath.Join(dir, "missing-decoy", "spans.jsonl"), "PEPICANVAS_TRACE_FILE cannot be opened"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			logger, logs := testLogger()
			tracer, stop, err := tracerFromEnv(envOf(map[string]string{"PEPICANVAS_TRACE_FILE": c.path}), logger)
			if err == nil || tracer != nil || stop != nil {
				t.Fatalf("tracerFromEnv = %v, %v; want a refusal", tracer, err)
			}
			if err.Error() != c.want {
				t.Fatalf("error = %q, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "decoy") || strings.Contains(err.Error(), dir) || logs.String() != "" {
				t.Fatalf("refusal echoes the path: %q, logs %s", err, logs)
			}
		})
	}
	if data, err := os.ReadFile(loose); err != nil || string(data) != "kept\n" {
		t.Fatalf("loose file = %q, %v; want it untouched", data, err)
	}
}

// L9: an unusable trace file stops run before PostgreSQL opens; without the
// variable run reaches PostgreSQL as before; a usable file is closed again
// when the start fails later.
func TestRunChecksTheTraceFileBeforeOpeningPostgres(t *testing.T) {
	base := map[string]string{
		"PEPICANVAS_DATABASE_URL":   "postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas",
		"PEPICANVAS_ALLOWED_ORIGIN": testOrigin,
	}
	with := func(path string) map[string]string {
		env := map[string]string{"PEPICANVAS_TRACE_FILE": path}
		for k, v := range base {
			env[k] = v
		}
		return env
	}
	dir := t.TempDir()

	logger, logs := testLogger()
	err := run(context.Background(), envOf(with(dir)), logger)
	if err == nil || err.Error() != "PEPICANVAS_TRACE_FILE cannot be opened" {
		t.Fatalf("run with a directory trace file = %v, want the trace file refusal", err)
	}
	if logs.String() != "" {
		t.Fatalf("logs = %s, want none before the refusal", logs)
	}

	logger, logs = testLogger()
	err = run(context.Background(), envOf(base), logger)
	if err == nil || strings.Contains(err.Error(), "PEPICANVAS_TRACE_FILE") || strings.Contains(err.Error(), "hunter2-decoy") {
		t.Fatalf("run without a trace file = %v, want the PostgreSQL failure", err)
	}
	if strings.Contains(logs.String(), "telemetry") {
		t.Fatalf("logs mention telemetry without a trace file:\n%s", logs)
	}

	path := filepath.Join(dir, "spans.jsonl")
	logger, logs = testLogger()
	err = run(context.Background(), envOf(with(path)), logger)
	if err == nil || strings.Contains(err.Error(), "PEPICANVAS_TRACE_FILE") {
		t.Fatalf("run with a usable trace file = %v, want the PostgreSQL failure", err)
	}
	if logRecord(logs.String(), "telemetry stopped") == nil {
		t.Fatalf("a failed start left the tracer open:\n%s", logs)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file = %v, %v; want mode 0600", info, err)
	}
}

// S1, S5, L9: with a trace file, one upload request is queryable by its
// correlation across HTTP, PostgreSQL, S3, the inspection subprocesses, and,
// past the outbox, the delivery to the realtime actor.
func TestServerExportsTheSpansOfARequestByCorrelation(t *testing.T) {
	db := pgtest.Start(t)
	bucket := s3test.Start(t)
	dsn := db.DSN(t)
	useRunner(t, &cannedRunner{})
	ffprobe, ffmpeg := fakeTools(t)
	traceFile := filepath.Join(t.TempDir(), "spans.jsonl")
	server := runInProcess(t, map[string]string{
		"PEPICANVAS_DATABASE_URL":      dsn,
		"PEPICANVAS_LISTEN_ADDR":       "127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN":    testOrigin,
		"PEPICANVAS_S3_ENDPOINT":       bucket.Endpoint,
		"PEPICANVAS_S3_BUCKET":         s3test.Bucket,
		"PEPICANVAS_S3_ACCESS_KEY":     s3test.AccessKey,
		"PEPICANVAS_S3_SECRET_KEY":     s3test.SecretKey,
		"PEPICANVAS_S3_SECURE":         "false",
		"PEPICANVAS_MEDIA_CGROUP_ROOT": "/sys/fs/cgroup/pepicanvas-media",
		"PEPICANVAS_FFPROBE_PATH":      ffprobe,
		"PEPICANVAS_FFMPEG_PATH":       ffmpeg,
		"PEPICANVAS_MEDIA_TEMP_DIR":    t.TempDir(),
		"PEPICANVAS_TRACE_FILE":        traceFile,
	})

	if res, body := server.do(t, http.MethodPost, "/api/accounts", "application/json",
		[]byte(`{"operation_id":"op-trace-alice-01","email":"alice@example.com","password":"correct horse battery"}`), nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d %s", res.StatusCode, body)
	}
	res, body := server.do(t, http.MethodPost, "/api/sessions", "application/json",
		[]byte(`{"email":"alice@example.com","password":"correct horse battery"}`), nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sign in = %d %s", res.StatusCode, body)
	}
	var signedIn struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &signedIn); err != nil {
		t.Fatal(err)
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == "__Host-pcs" {
			cookie = c.Name + "=" + c.Value
		}
	}
	execSQL(t, dsn,
		"INSERT INTO tenants (tenant_id, subscription_status) VALUES ('tenant-a', 'active')",
		"INSERT INTO canvases (tenant_id, canvas_id) VALUES ('tenant-a', 'main')",
		"INSERT INTO memberships (tenant_id, account_id, role) SELECT 'tenant-a', account_id, 'owner' FROM accounts WHERE email = 'alice@example.com'",
	)
	auth := map[string]string{"Cookie": cookie, "X-CSRF-Token": signedIn.CSRF}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 120)...)
	res, body = server.do(t, http.MethodPost, "/api/tenants/tenant-a/canvases/main/media/uploads?operation_id=op-trace-up-01&reference_id=ref-01",
		"application/octet-stream", png, auth)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d %s", res.StatusCode, body)
	}
	id, err := correlation.Parse(res.Header.Get(httpx.CorrelationHeader))
	if err != nil {
		t.Fatalf("upload correlation %q: %v", res.Header.Get(httpx.CorrelationHeader), err)
	}

	const records = "SELECT count(*) FROM canvas_outbox WHERE tenant_id = 'tenant-a' AND kind = 'media_usage' AND correlation_id = $1"
	deadline := time.Now().Add(5 * time.Second)
	for {
		total := queryInt(t, dsn, records, id.String())
		delivered := queryInt(t, dsn, records+" AND outcome = 'delivered'", id.String())
		if total >= 1 && delivered == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("media_usage records of the upload: %d, %d delivered\n%s", total, delivered, server.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := server.stop(); err != nil {
		t.Fatalf("run = %v\n%s", err, server.logs)
	}
	if logRecord(server.logs.String(), "telemetry stopped") == nil {
		t.Fatalf("logs lack telemetry stopped:\n%s", server.logs)
	}

	file, err := os.Open(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	spans, malformed, err := telemetry.Query(file, id)
	if err != nil || malformed != 0 {
		t.Fatalf("Query = %d spans, %d malformed, %v", len(spans), malformed, err)
	}
	names := map[string]int{}
	for _, span := range spans {
		names[span.Name]++
	}
	for _, name := range []telemetry.SpanName{
		telemetry.SpanHTTPRequest, telemetry.SpanPostgres, telemetry.SpanS3, telemetry.SpanMediaInspect,
		telemetry.SpanSubprocess, telemetry.SpanOutboxDeliver, telemetry.SpanRealtimeSubmit,
	} {
		if names[string(name)] == 0 {
			t.Errorf("no %s span in the upload's trace; got %v", name, names)
		}
	}
	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"S3 secret key": s3test.SecretKey, "session cookie": cookie, "CSRF token": signedIn.CSRF, "media temp dir": os.TempDir() + "/"} {
		if strings.Contains(string(data), s) {
			t.Errorf("trace file contains the %s", name)
		}
	}
}
