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

	"github.com/jackc/pgx/v5"

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
	return requestWith(t, p, method, path, body, cookie, "")
}

// requestWith sends a request from the allowed origin with an optional
// session cookie and CSRF token.
func requestWith(t *testing.T, p *process, method, path, body, cookie, csrf string) *http.Response {
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
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
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
	if err := os.MkdirAll(filepath.Join(static, "editor", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "editor", "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
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
	if res := request(t, server, http.MethodGet, "/editor/assets/app.js", "", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("GET asset = %d", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, "/editor/assets/", "", ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("directory listing = %d, want 404", res.StatusCode)
	}

	// Canvas requests read the real membership facts (US-002 writes them).
	canvasPath := "/api/tenants/tenant-a/canvases/main"
	if res := request(t, server, http.MethodGet, canvasPath, "", cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("canvas without membership = %d, want 403", res.StatusCode)
	}
	seed(t, db.DSN(t),
		"INSERT INTO tenants (tenant_id, subscription_status) VALUES ('tenant-a', 'active'), ('tenant-b', 'active')",
		"INSERT INTO canvases (tenant_id, canvas_id) VALUES ('tenant-a', 'main'), ('tenant-b', 'main')",
		"INSERT INTO memberships (tenant_id, account_id, role) SELECT 'tenant-a', account_id, 'owner' FROM accounts WHERE email = 'alice@example.com'",
	)
	res = request(t, server, http.MethodGet, canvasPath, "", cookie)
	body, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"width":1920`) {
		t.Fatalf("owner canvas = %d %s", res.StatusCode, body)
	}
	if res := request(t, server, http.MethodGet, "/api/tenants/tenant-b/canvases/main", "", cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant canvas = %d, want 403", res.StatusCode)
	}
	seed(t, db.DSN(t), "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = 'tenant-a'")
	if res := request(t, server, http.MethodGet, canvasPath, "", cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("canvas of an inactive tenant = %d, want 403", res.StatusCode)
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

func seed(t *testing.T, dsn string, statements ...string) {
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

func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

type session struct {
	cookie   string
	csrf     string
	operator bool
}

func decodeBody(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func registerAndSignIn(t *testing.T, p *process, op, email string) session {
	t.Helper()
	if res := request(t, p, http.MethodPost, "/api/accounts", `{"operation_id":"`+op+`","email":"`+email+`","password":"correct horse battery"}`, ""); res.StatusCode != http.StatusCreated {
		t.Fatalf("register %s = %d", email, res.StatusCode)
	}
	return signIn(t, p, email)
}

func signIn(t *testing.T, p *process, email string) session {
	t.Helper()
	res := request(t, p, http.MethodPost, "/api/sessions", `{"email":"`+email+`","password":"correct horse battery"}`, "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sign in %s = %d", email, res.StatusCode)
	}
	s := session{}
	for _, c := range res.Cookies() {
		if c.Name == "__Host-pcs" {
			s.cookie = c.Name + "=" + c.Value
		}
	}
	body := decodeBody(t, res)
	s.csrf, _ = body["csrf_token"].(string)
	s.operator, _ = body["operator"].(bool)
	if s.cookie == "" || s.csrf == "" {
		t.Fatalf("sign in %s: no session", email)
	}
	return s
}

// grantOperator runs the grant-operator subcommand and returns its exit code
// and combined output.
func grantOperator(t *testing.T, bin string, env []string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"grant-operator"}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("grant-operator %v did not exit\n%s", args, out)
	}
	var exit *exec.ExitError
	if err != nil && !errorsAs(err, &exit) {
		t.Fatalf("grant-operator %v: %v", args, err)
	}
	if exit != nil {
		return exit.ExitCode(), string(out)
	}
	return 0, string(out)
}

func activate(t *testing.T, p *process, s session, op, email string) (*http.Response, map[string]any) {
	t.Helper()
	res := requestWith(t, p, http.MethodPost, "/api/activations", `{"operation_id":"`+op+`","email":"`+email+`"}`, s.cookie, s.csrf)
	if res.StatusCode != http.StatusOK {
		return res, nil
	}
	return res, decodeBody(t, res)
}

func TestOperatorActivatesStreamersAcrossRestart(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	dsn := db.DSN(t)
	env := []string{
		"PEPICANVAS_DATABASE_URL=" + dsn,
		"PEPICANVAS_LISTEN_ADDR=127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN=" + origin,
	}

	server := start(t, bin, env...)
	operator := registerAndSignIn(t, server, "op-process-oper-01", "operator@example.com")
	alice := registerAndSignIn(t, server, "op-process-alice-1", "alice@example.com")
	bob := registerAndSignIn(t, server, "op-process-bob-001", "bob@example.com")
	if operator.operator || alice.operator {
		t.Fatal("an account is an operator before the grant")
	}

	// S13: the host grants the operator role.
	code, out := grantOperator(t, bin, []string{"PEPICANVAS_DATABASE_URL=" + dsn}, "Operator@Example.com")
	if code != 0 || !strings.Contains(out, "operator granted: ") {
		t.Fatalf("grant-operator = %d\n%s", code, out)
	}
	if code, again := grantOperator(t, bin, []string{"PEPICANVAS_DATABASE_URL=" + dsn}, "operator@example.com"); code != 0 || again != out {
		t.Fatalf("repeated grant-operator = %d %q, want 0 %q", code, again, out)
	}
	operator = signIn(t, server, "operator@example.com")
	if !operator.operator {
		t.Fatal("sign-in does not report the operator role")
	}
	res := request(t, server, http.MethodGet, "/api/session", "", operator.cookie)
	if body := decodeBody(t, res); body["operator"] != true {
		t.Fatalf("session body = %v", body)
	}
	if body := decodeBody(t, request(t, server, http.MethodGet, "/api/session", "", alice.cookie)); body["operator"] != false {
		t.Fatalf("streamer session body = %v", body)
	}

	// S4: a non-operator cannot activate.
	if res, _ := activate(t, server, alice, "op-process-deny-01", "alice@example.com"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-operator activation = %d, want 403", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, "/api/canvas", "", alice.cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("canvas before activation = %d, want 403", res.StatusCode)
	}

	res, a := activate(t, server, operator, "op-process-act-a01", "alice@example.com")
	if res.StatusCode != http.StatusOK || a["created"] != true {
		t.Fatalf("activate alice = %d %v", res.StatusCode, a)
	}
	_, b := activate(t, server, operator, "op-process-act-b01", "bob@example.com")
	if b == nil || b["tenant_id"] == a["tenant_id"] || b["canvas_id"] == a["canvas_id"] {
		t.Fatalf("activate bob = %v, alice %v", b, a)
	}
	// A repeated activation, by replay or by a new operation, keeps the canvas.
	for _, op := range []string{"op-process-act-a01", "op-process-act-a02"} {
		if _, again := activate(t, server, operator, op, "alice@example.com"); again == nil || again["tenant_id"] != a["tenant_id"] || again["canvas_id"] != a["canvas_id"] {
			t.Fatalf("repeat %s = %v, want %v", op, again, a)
		}
	}

	ownCanvas := func(p *process, s session, want map[string]any) {
		t.Helper()
		res := request(t, p, http.MethodGet, "/api/canvas", "", s.cookie)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/canvas = %d", res.StatusCode)
		}
		got := decodeBody(t, res)
		if got["tenant_id"] != want["tenant_id"] || got["canvas_id"] != want["canvas_id"] || got["width"] != float64(1920) || got["height"] != float64(1080) {
			t.Fatalf("canvas = %v, want %v", got, want)
		}
	}
	ownCanvas(server, alice, a)
	ownCanvas(server, bob, b)
	alicePath := "/api/tenants/" + a["tenant_id"].(string) + "/canvases/" + a["canvas_id"].(string)
	if res := request(t, server, http.MethodGet, alicePath, "", bob.cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bob reads alice's canvas = %d, want 403", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, alicePath, "", alice.cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("alice reads her canvas by tenant = %d", res.StatusCode)
	}
	server.stop(t)
	logs := server.logs.String()

	server = start(t, bin, env...)
	ownCanvas(server, alice, a)
	server.stop(t)
	logs += server.logs.String()

	if !strings.Contains(logs, `"msg":"activation confirmed"`) {
		t.Fatal("no activation log")
	}
	for name, s := range map[string]string{
		"password":       "correct horse battery",
		"operator email": "operator@example.com",
		"streamer email": "alice@example.com",
		"other email":    "bob@example.com",
		"session token":  strings.TrimPrefix(operator.cookie, "__Host-pcs="),
		"CSRF token":     operator.csrf,
	} {
		if strings.Contains(logs, s) {
			t.Errorf("process logs contain the %s", name)
		}
	}
}

func TestGrantOperatorRejectsBadInput(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	dsn := db.DSN(t)
	withDB := []string{"PEPICANVAS_DATABASE_URL=" + dsn}

	code, out := grantOperator(t, bin, withDB)
	if code != 2 || !strings.Contains(out, "usage: server grant-operator <email>") {
		t.Fatalf("missing argument = %d\n%s", code, out)
	}
	if code, out := grantOperator(t, bin, withDB, "a@example.com", "b@example.com"); code != 2 || !strings.Contains(out, "usage:") {
		t.Fatalf("extra argument = %d\n%s", code, out)
	}
	for _, email := range []string{"nobody@example.com", "not-an-email"} {
		code, out := grantOperator(t, bin, withDB, email)
		if code != 1 || !strings.Contains(out, "no account has that email") || strings.Contains(out, "operator granted") {
			t.Fatalf("grant-operator %q = %d\n%s", email, code, out)
		}
		if strings.Contains(out, dsn) {
			t.Fatalf("output echoes the DSN:\n%s", out)
		}
	}
	for name, env := range map[string][]string{
		"missing database": nil,
		"unreachable db":   {"PEPICANVAS_DATABASE_URL=postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas"},
	} {
		code, out := grantOperator(t, bin, env, "a@example.com")
		if code != 1 || strings.Contains(out, "hunter2-decoy") {
			t.Fatalf("%s = %d\n%s", name, code, out)
		}
	}
}

func deactivate(t *testing.T, p *process, s session, op, email string) (*http.Response, string) {
	t.Helper()
	res := requestWith(t, p, http.MethodPost, "/api/deactivations", `{"operation_id":"`+op+`","email":"`+email+`"}`, s.cookie, s.csrf)
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

// count runs a single-value query.
func count(t *testing.T, dsn, query string, args ...any) int64 {
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

// logLine returns the first log record with msg whose correlation_id is id.
func logLine(logs, msg, id string) map[string]any {
	for _, line := range strings.Split(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg && record["correlation_id"] == id {
			return record
		}
	}
	return nil
}

// US-003 S1, S4, S6, S13, S15: an operator deactivates a streamer over the
// real server; the cut holds across a restart, the relay applies it, and
// reactivation returns the same canvas.
func TestOperatorDeactivatesAndReactivatesAcrossRestart(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	dsn := db.DSN(t)
	env := []string{
		"PEPICANVAS_DATABASE_URL=" + dsn,
		"PEPICANVAS_LISTEN_ADDR=127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN=" + origin,
	}

	server := start(t, bin, env...)
	registerAndSignIn(t, server, "op-process-oper-01", "operator@example.com")
	alice := registerAndSignIn(t, server, "op-process-alice-1", "alice@example.com")
	bob := registerAndSignIn(t, server, "op-process-bob-001", "bob@example.com")
	if code, out := grantOperator(t, bin, []string{"PEPICANVAS_DATABASE_URL=" + dsn}, "operator@example.com"); code != 0 {
		t.Fatalf("grant-operator = %d\n%s", code, out)
	}
	operator := signIn(t, server, "operator@example.com")
	_, a := activate(t, server, operator, "op-process-act-a01", "alice@example.com")
	_, b := activate(t, server, operator, "op-process-act-b01", "bob@example.com")
	if a == nil || b == nil {
		t.Fatal("activation failed")
	}
	// S5: the canvas keeps its objects through deactivation.
	seed(t, dsn,
		"INSERT INTO canvas_objects (tenant_id, canvas_id, object_id, kind, content, x, y, width, height, rotation, z) VALUES ('"+
			a["tenant_id"].(string)+"', '"+a["canvas_id"].(string)+"', 'obj-1', 'text', 'kept', 5, 6, 100, 40, 0, 1)",
		"UPDATE canvases SET revision = 1, next_z = 2 WHERE tenant_id = '"+a["tenant_id"].(string)+"'",
	)
	alicePath := "/api/tenants/" + a["tenant_id"].(string) + "/canvases/" + a["canvas_id"].(string)
	bobPath := "/api/tenants/" + b["tenant_id"].(string) + "/canvases/" + b["canvas_id"].(string)
	read := func(p *process, s session, path string) string {
		t.Helper()
		res := request(t, p, http.MethodGet, path, "", s.cookie)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, res.StatusCode, body)
		}
		return string(body)
	}
	denied := func(p *process, s session, path, code string) {
		t.Helper()
		res := request(t, p, http.MethodGet, path, "", s.cookie)
		body := decodeBody(t, res)
		if res.StatusCode != http.StatusForbidden || body["error"] != code || len(body) != 2 {
			t.Fatalf("GET %s = %d %v, want 403 %s", path, res.StatusCode, body, code)
		}
		if res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q", path, res.Header.Get("Cache-Control"))
		}
	}
	own := read(server, alice, "/api/canvas")
	if !strings.Contains(own, `"content":"kept"`) || !strings.Contains(own, `"width":1920`) {
		t.Fatalf("own canvas = %s", own)
	}
	byTenant := read(server, alice, alicePath)
	bobOwn := read(server, bob, "/api/canvas")

	// S6: a non-operator, and an operator without the CSRF token, change nothing.
	unchanged := func() {
		t.Helper()
		if n := count(t, dsn, "SELECT count(*) FROM tenants WHERE subscription_status = 'active'"); n != 2 {
			t.Fatalf("active tenants = %d, want 2", n)
		}
		if n := count(t, dsn, "SELECT count(*) FROM subscription_deactivations"); n != 0 {
			t.Fatalf("deactivation records = %d, want 0", n)
		}
		if n := count(t, dsn, "SELECT count(*) FROM access_cuts"); n != 0 {
			t.Fatalf("access cuts = %d, want 0", n)
		}
	}
	if res, body := deactivate(t, server, alice, "op-process-deny-01", "bob@example.com"); res.StatusCode != http.StatusForbidden || !strings.Contains(body, `"error":"forbidden"`) {
		t.Fatalf("non-operator deactivation = %d %s", res.StatusCode, body)
	}
	if res, body := deactivate(t, server, session{cookie: operator.cookie}, "op-process-deny-02", "alice@example.com"); res.StatusCode != http.StatusForbidden || !strings.Contains(body, `"error":"forbidden"`) {
		t.Fatalf("deactivation without CSRF = %d %s", res.StatusCode, body)
	}
	unchanged()

	res, first := deactivate(t, server, operator, "op-process-deact-01", "alice@example.com")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deactivate = %d %s", res.StatusCode, first)
	}
	correlationID := res.Header.Get("X-Correlation-ID")
	var result map[string]any
	if err := json.Unmarshal([]byte(first), &result); err != nil {
		t.Fatal(err)
	}
	if result["account_id"] != a["account_id"] || result["tenant_id"] != a["tenant_id"] || result["canvas_id"] != a["canvas_id"] || result["deactivated"] != true || len(result) != 4 {
		t.Fatalf("deactivation = %v, activation %v", result, a)
	}
	// S11: the replay is byte-identical; a new operation changes nothing.
	if res, again := deactivate(t, server, operator, "op-process-deact-01", "alice@example.com"); res.StatusCode != http.StatusOK || again != first {
		t.Fatalf("replay = %d %s, want %s", res.StatusCode, again, first)
	}
	if res, again := deactivate(t, server, operator, "op-process-deact-02", "alice@example.com"); res.StatusCode != http.StatusOK || !strings.Contains(again, `"deactivated":false`) {
		t.Fatalf("second deactivation = %d %s", res.StatusCode, again)
	}

	// S1: the cut is effective at the answer.
	denied(server, alice, "/api/canvas", "subscription_inactive")
	denied(server, alice, alicePath, "forbidden")
	if got := read(server, bob, "/api/canvas"); got != bobOwn {
		t.Fatalf("bob canvas = %s, want %s", got, bobOwn)
	}
	read(server, bob, bobPath)
	// S13: the stored record and cut keep the request correlation, and the
	// relay applies the cut.
	if n := count(t, dsn, "SELECT count(*) FROM subscription_deactivations WHERE operation_id = 'op-process-deact-01' AND correlation_id = $1", correlationID); n != 1 {
		t.Fatalf("deactivation records with the correlation = %d", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for count(t, dsn, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND correlation_id = $2 AND applied_at IS NOT NULL", a["tenant_id"], correlationID) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the access cut was not applied\n%s", server.logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	server.stop(t)
	logs := server.logs.String()

	// S4: a restart does not restore access.
	server = start(t, bin, env...)
	if n := count(t, dsn, "SELECT count(*) FROM tenants WHERE tenant_id = $1 AND subscription_status = 'inactive'", a["tenant_id"]); n != 1 {
		t.Fatal("tenant A is not inactive after the restart")
	}
	if n := count(t, dsn, "SELECT count(*) FROM access_cuts WHERE applied_at IS NOT NULL"); n != 1 {
		t.Fatalf("applied cuts after the restart = %d, want 1", n)
	}
	if n := count(t, dsn, "SELECT count(*) FROM access_cuts"); n != 1 {
		t.Fatalf("cuts after the restart = %d, want 1", n)
	}
	denied(server, alice, "/api/canvas", "subscription_inactive")
	denied(server, alice, alicePath, "forbidden")
	read(server, bob, bobPath)

	// S5: reactivation is the activation and returns the same canvas.
	_, again := activate(t, server, operator, "op-process-react-1", "alice@example.com")
	if again == nil || again["tenant_id"] != a["tenant_id"] || again["canvas_id"] != a["canvas_id"] || again["created"] != false {
		t.Fatalf("reactivation = %v, activation %v", again, a)
	}
	if got := read(server, alice, "/api/canvas"); got != own {
		t.Fatalf("own canvas after reactivation = %s, want %s", got, own)
	}
	if got := read(server, alice, alicePath); got != byTenant {
		t.Fatalf("tenant canvas after reactivation = %s, want %s", got, byTenant)
	}
	server.stop(t)
	logs += server.logs.String()

	confirmed := logLine(logs, "deactivation confirmed", correlationID)
	if confirmed == nil || confirmed["tenant_id"] != a["tenant_id"] || confirmed["account_id"] != a["account_id"] || confirmed["canvas_id"] != a["canvas_id"] {
		t.Fatalf("deactivation log = %v\n%s", confirmed, logs)
	}
	for _, msg := range []string{"access cut confirmed", "access cut applied"} {
		if record := logLine(logs, msg, correlationID); record == nil || record["tenant_id"] != a["tenant_id"] {
			t.Fatalf("%s log = %v\n%s", msg, record, logs)
		}
	}
	for name, s := range map[string]string{
		"password":       "correct horse battery",
		"operator email": "operator@example.com",
		"streamer email": "alice@example.com",
		"other email":    "bob@example.com",
		"session token":  strings.TrimPrefix(operator.cookie, "__Host-pcs="),
		"CSRF token":     operator.csrf,
		"streamer token": strings.TrimPrefix(alice.cookie, "__Host-pcs="),
		"streamer CSRF":  alice.csrf,
	} {
		if strings.Contains(logs, s) {
			t.Errorf("process logs contain the %s", name)
		}
	}
}
