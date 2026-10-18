package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

const origin = "http://127.0.0.1:5173"

func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "server")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	return bin
}

type process struct {
	cmd  *exec.Cmd
	addr string
	logs *bytes.Buffer
	done chan error
}

// start runs the binary and waits for its "listening" log record.
func start(t *testing.T, bin string, env ...string) *process {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: cmd, logs: &bytes.Buffer{}, done: make(chan error, 1)}
	addr := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			p.logs.WriteString(line + "\n")
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "listening" {
				addr <- record["addr"].(string)
			}
		}
		p.done <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case a := <-addr:
		p.addr = a
	case err := <-p.done:
		t.Fatalf("server exited before listening: %v\n%s", err, p.logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("server did not start\n%s", p.logs)
	}
	return p
}

func (p *process) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("server exit after SIGTERM: %v\n%s", err, p.logs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after SIGTERM")
	}
}

func request(t *testing.T, p *process, method, path, body, cookie string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), method, "http://"+p.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestServerKeepsSessionsAcrossRestartAndServesTheEditor(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	static := t.TempDir()
	if err := os.MkdirAll(filepath.Join(static, "editor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "editor", "index.html"), []byte("<!doctype html><title>editor</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PEPICANVAS_DATABASE_URL=" + db.DSN(t),
		"PEPICANVAS_LISTEN_ADDR=127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN=" + origin,
		"PEPICANVAS_STATIC_DIR=" + static,
	}

	server := start(t, bin, env...)
	if res := request(t, server, http.MethodPost, "/api/accounts", `{"operation_id":"op-process-test-01","email":"alice@example.com","password":"correct horse battery"}`, ""); res.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", res.StatusCode)
	}
	res := request(t, server, http.MethodPost, "/api/sessions", `{"email":"alice@example.com","password":"correct horse battery"}`, "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sign in = %d", res.StatusCode)
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == "__Host-pcs" {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	server.stop(t)

	// A reconnection to a restarted server keeps the session.
	server = start(t, bin, env...)
	if res := request(t, server, http.MethodGet, "/api/session", "", cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("session after restart = %d", res.StatusCode)
	}

	res = request(t, server, http.MethodGet, "/editor/", "", "")
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>editor</title>") {
		t.Fatalf("GET /editor/ = %d %q", res.StatusCode, body)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("editor page Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if res := request(t, server, http.MethodGet, "/", "", ""); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/editor/" {
		t.Fatalf("GET / = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	server.stop(t)

	logs := server.logs.String()
	if strings.Contains(logs, "correct horse battery") || strings.Contains(logs, strings.TrimPrefix(cookie, "__Host-pcs=")) {
		t.Fatal("process logs contain a credential")
	}
}

func TestServerRefusesToStartWithInvalidConfiguration(t *testing.T) {
	bin := buildServer(t)
	cases := map[string][]string{
		"missing database": {"PEPICANVAS_ALLOWED_ORIGIN=" + origin},
		"missing origin":   {"PEPICANVAS_DATABASE_URL=postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas"},
		"wildcard origin":  {"PEPICANVAS_DATABASE_URL=postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas", "PEPICANVAS_ALLOWED_ORIGIN=*"},
		"unreachable db":   {"PEPICANVAS_DATABASE_URL=postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas", "PEPICANVAS_ALLOWED_ORIGIN=" + origin},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bin)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if err == nil || !errorsAs(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1\n%s", err, out)
			}
			if strings.Contains(string(out), "hunter2-decoy") {
				t.Fatalf("output echoes the database password:\n%s", out)
			}
		})
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
