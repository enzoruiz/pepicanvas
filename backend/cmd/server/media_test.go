package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/mediaexec"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
)

const testOrigin = "http://127.0.0.1:5173"

// cannedRunner stands in for the cgroup runner, which needs a delegated
// cgroup-v2 subtree the test host does not have. Its executions run no
// process: ffprobe answers the metadata of a small PNG and every other
// command succeeds.
type cannedRunner struct {
	mu     sync.Mutex
	begins int
}

func (r *cannedRunner) Begin(context.Context, media.Limits) (mediaexec.Execution, mediaexec.Result) {
	r.mu.Lock()
	r.begins++
	r.mu.Unlock()
	return cannedExecution{}, mediaexec.SuccessfulResult()
}

func (r *cannedRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.begins
}

type cannedExecution struct{}

func (cannedExecution) Run(command mediaexec.Command, stdout, _ io.Writer) mediaexec.Result {
	if filepath.Base(command.Path) == "ffprobe" {
		_, _ = io.WriteString(stdout, `{"format":{"format_name":"png_pipe"},"streams":[{"codec_type":"video","codec_name":"png","width":10,"height":20}]}`)
	}
	return mediaexec.SuccessfulResult()
}

func (cannedExecution) Close() mediaexec.Result { return mediaexec.SuccessfulResult() }

// useRunner makes run build its inspector over runner until the test ends,
// and records the cgroup root it was given.
func useRunner(t *testing.T, runner mediaexec.Runner) *string {
	t.Helper()
	root := new(string)
	previous := newMediaRunner
	newMediaRunner = func(r string) (mediaexec.Runner, mediaexec.Result) {
		*root = r
		return runner, mediaexec.SuccessfulResult()
	}
	t.Cleanup(func() { newMediaRunner = previous })
	return root
}

// fakeTools writes executable ffprobe and ffmpeg files the canned runner
// never starts.
func fakeTools(t *testing.T) (ffprobe, ffmpeg string) {
	t.Helper()
	dir := t.TempDir()
	ffprobe, ffmpeg = filepath.Join(dir, "ffprobe"), filepath.Join(dir, "ffmpeg")
	for _, path := range []string{ffprobe, ffmpeg} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return ffprobe, ffmpeg
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// inProcess is run serving in this test process.
type inProcess struct {
	addr string
	logs *syncBuffer
	stop func() error
}

func runInProcess(t *testing.T, env map[string]string) *inProcess {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(key string) string { return env[key] }, logger) }()
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(10 * time.Second):
				stopErr = errors.New("run did not return after cancel")
			}
		})
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(logs.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "listening" {
				return &inProcess{addr: record["addr"].(string), logs: logs, stop: stop}
			}
		}
		select {
		case err := <-done:
			t.Fatalf("run returned before listening: %v\n%s", err, logs)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("run did not listen\n%s", logs)
	return nil
}

func (p *inProcess) do(t *testing.T, method, path, contentType string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), method, "http://"+p.addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", testOrigin)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, data
}

func queryInt(t *testing.T, dsn, query string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int64
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func execSQL(t *testing.T, dsn string, statements ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// US-016 S17, S11, S12: with object storage and inspection configured, an
// owner's upload is inspected through the configured runner, stored in the
// bucket, and every usage change it commits reaches the outbox publisher;
// the reconciler starts and the server still stops within the budget.
func TestServerWiresObjectStorageInspectionAndReconciliation(t *testing.T) {
	db := pgtest.Start(t)
	bucket := s3test.Start(t)
	dsn := db.DSN(t)
	runner := &cannedRunner{}
	root := useRunner(t, runner)
	ffprobe, ffmpeg := fakeTools(t)
	const cgroupRoot = "/sys/fs/cgroup/pepicanvas-media"
	server := runInProcess(t, map[string]string{
		"PEPICANVAS_DATABASE_URL":      dsn,
		"PEPICANVAS_LISTEN_ADDR":       "127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN":    testOrigin,
		"PEPICANVAS_S3_ENDPOINT":       bucket.Endpoint,
		"PEPICANVAS_S3_BUCKET":         s3test.Bucket,
		"PEPICANVAS_S3_ACCESS_KEY":     s3test.AccessKey,
		"PEPICANVAS_S3_SECRET_KEY":     s3test.SecretKey,
		"PEPICANVAS_S3_SECURE":         "false",
		"PEPICANVAS_MEDIA_CGROUP_ROOT": cgroupRoot,
		"PEPICANVAS_FFPROBE_PATH":      ffprobe,
		"PEPICANVAS_FFMPEG_PATH":       ffmpeg,
		"PEPICANVAS_MEDIA_TEMP_DIR":    t.TempDir(),
	})
	if *root != cgroupRoot {
		t.Fatalf("runner root = %q, want %q", *root, cgroupRoot)
	}

	if res, body := server.do(t, http.MethodPost, "/api/accounts", "application/json",
		[]byte(`{"operation_id":"op-media-alice-01","email":"alice@example.com","password":"correct horse battery"}`), nil); res.StatusCode != http.StatusCreated {
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
	mediaPath := "/api/tenants/tenant-a/canvases/main/media"

	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 120)...)
	res, body = server.do(t, http.MethodPost, mediaPath+"/uploads?operation_id=op-media-up-01&reference_id=ref-01", "application/octet-stream", png, auth)
	var uploaded struct {
		AssetID string `json:"asset_id"`
		Kind    string `json:"kind"`
		Usage   struct {
			UsedBytes     int64 `json:"used_bytes"`
			ReservedBytes int64 `json:"reserved_bytes"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &uploaded); err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d %s", res.StatusCode, body)
	}
	if uploaded.Kind != "image" || uploaded.Usage.UsedBytes != int64(len(png)) || uploaded.Usage.ReservedBytes != 0 {
		t.Fatalf("upload = %s", body)
	}
	if n := runner.count(); n != 1 {
		t.Fatalf("inspections through the configured runner = %d, want 1", n)
	}
	if keys := bucket.Keys(t); len(keys) != 1 {
		t.Fatalf("stored objects = %v, want one", keys)
	}
	res, body = server.do(t, http.MethodGet, mediaPath+"/usage", "", nil, auth)
	if want := `"used_bytes":128,"reserved_bytes":0`; res.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
		t.Fatalf("usage = %d %s, want %s", res.StatusCode, body, want)
	}

	// CTRL-CONS-01: every committed usage change wakes the publisher, which
	// delivers it without a restart.
	const records = "SELECT count(*) FROM canvas_outbox WHERE tenant_id = 'tenant-a' AND kind = 'media_usage'"
	deadline := time.Now().Add(5 * time.Second)
	for {
		total := queryInt(t, dsn, records)
		delivered := queryInt(t, dsn, records+" AND outcome = 'delivered' AND settled_at IS NOT NULL")
		if total >= 2 && delivered == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("media_usage records: %d, %d delivered; want at least 2, all delivered\n%s", total, delivered, server.logs)
		}
		time.Sleep(20 * time.Millisecond)
	}

	began := time.Now()
	if err := server.stop(); err != nil {
		t.Fatalf("run = %v\n%s", err, server.logs)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v, over the 5s budget", elapsed)
	}
	logs := server.logs.String()
	if !strings.Contains(logs, `"msg":"stopped"`) || strings.Contains(logs, "media storage disabled") {
		t.Fatalf("logs:\n%s", logs)
	}
	for name, s := range map[string]string{"S3 secret key": s3test.SecretKey, "database URL": dsn} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

// S17: the media variables take their documented defaults, and an inspector
// the adapter rejects stops the process before it listens.
func TestMediaConfigurationDefaultsAndRefusals(t *testing.T) {
	full := map[string]string{
		"PEPICANVAS_S3_ENDPOINT":       "s3.example.test",
		"PEPICANVAS_S3_BUCKET":         "media",
		"PEPICANVAS_S3_ACCESS_KEY":     "access-decoy",
		"PEPICANVAS_S3_SECRET_KEY":     "secret-decoy",
		"PEPICANVAS_MEDIA_CGROUP_ROOT": "/sys/fs/cgroup/pepicanvas-media",
	}
	getenv := func(env map[string]string) func(string) string {
		return func(key string) string { return env[key] }
	}

	if config, err := mediaFromEnv(getenv(nil)); config != nil || err != nil {
		t.Fatalf("no media variables = %v, %v; want disabled", config, err)
	}
	config, err := mediaFromEnv(getenv(full))
	if err != nil || config == nil {
		t.Fatalf("full media configuration = %v, %v", config, err)
	}
	if !config.storage.Secure || config.inspection.FFprobePath != "/usr/bin/ffprobe" || config.inspection.FFmpegPath != "/usr/bin/ffmpeg" ||
		config.inspection.TempRoot != os.TempDir() || config.inspection.Limits != media.DefaultLimits() || config.cgroupRoot != full["PEPICANVAS_MEDIA_CGROUP_ROOT"] {
		t.Fatalf("defaults = %+v", config)
	}
	if config.storage.AccessKey.Reveal() != "access-decoy" || config.storage.SecretKey.Reveal() != "secret-decoy" {
		t.Fatal("credentials are not carried as secrets")
	}
	if s := strings.Join([]string{config.storage.AccessKey.String(), config.storage.SecretKey.String()}, " "); strings.Contains(s, "decoy") {
		t.Fatalf("credentials format as %q", s)
	}

	useRunner(t, &cannedRunner{})
	relative := map[string]string{"PEPICANVAS_FFPROBE_PATH": "ffprobe"}
	for k, v := range full {
		relative[k] = v
	}
	config, err = mediaFromEnv(getenv(relative))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.inspector(); err == nil || !strings.Contains(err.Error(), "media inspection configuration is invalid") || strings.Contains(err.Error(), "decoy") {
		t.Fatalf("inspector with a relative ffprobe = %v", err)
	}
}
