package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
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
	const unreachable = "PEPICANVAS_DATABASE_URL=postgres://pepicanvas:hunter2-decoy@127.0.0.1:1/pepicanvas"
	cases := map[string][]string{
		"missing database": {"PEPICANVAS_ALLOWED_ORIGIN=" + origin},
		"missing origin":   {unreachable},
		"unreachable db":   {unreachable, "PEPICANVAS_ALLOWED_ORIGIN=" + origin},
	}
	// S8, S10: an origin that would weaken CTRL-WEB-01 stops the process
	// before the database is opened or the listener exists.
	invalidOrigins := map[string]string{
		"wildcard origin":       "*",
		"wildcard subdomain":    "https://*.pepicanvas.test",
		"null origin":           "null",
		"trailing slash origin": origin + "/",
		"origin with a path":    origin + "/editor",
		"origin with userinfo":  "http://user@127.0.0.1:5173",
		"origin without scheme": "127.0.0.1:5173",
	}
	for name, o := range invalidOrigins {
		cases[name] = []string{unreachable, "PEPICANVAS_ALLOWED_ORIGIN=" + o}
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
			if strings.Contains(string(out), `"msg":"listening"`) {
				t.Fatalf("the server listened with an invalid configuration:\n%s", out)
			}
			if _, invalid := invalidOrigins[name]; invalid &&
				(!strings.Contains(string(out), "allowed origin must be exactly scheme://host[:port]") || strings.Contains(string(out), "127.0.0.1:1")) {
				t.Fatalf("the origin was not rejected before the database was opened:\n%s", out)
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

// pendingOutbox counts the outbox records of tenant by state.
func pendingOutbox(t *testing.T, dsn, tenant string) (pending, delivered int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	err = conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE settled_at IS NULL),
		count(*) FILTER (WHERE outcome = 'delivered' AND settled_at IS NOT NULL)
		FROM canvas_outbox WHERE tenant_id = $1`, tenant).Scan(&pending, &delivered)
	if err != nil {
		t.Fatal(err)
	}
	return pending, delivered
}

// TestServerPublishesCommittedResizesAndStopsThePublisher covers US-007 S9
// and S15: the running server delivers the outbox record of a resize without
// a restart, and SIGTERM stops the publisher and hub within the budget.
func TestServerPublishesCommittedResizesAndStopsThePublisher(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	dsn := db.DSN(t)
	env := []string{
		"PEPICANVAS_DATABASE_URL=" + dsn,
		"PEPICANVAS_LISTEN_ADDR=127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN=" + origin,
	}

	server := start(t, bin, env...)
	registerAndSignIn(t, server, "op-publish-oper-01", "operator@example.com")
	alice := registerAndSignIn(t, server, "op-publish-alice-1", "alice@example.com")
	if code, out := grantOperator(t, bin, []string{"PEPICANVAS_DATABASE_URL=" + dsn}, "operator@example.com"); code != 0 {
		t.Fatalf("grant-operator = %d\n%s", code, out)
	}
	operator := signIn(t, server, "operator@example.com")
	_, a := activate(t, server, operator, "op-publish-act-a01", "alice@example.com")
	if a == nil {
		t.Fatal("activate alice failed")
	}
	tenant, canvasID := a["tenant_id"].(string), a["canvas_id"].(string)

	res := requestWith(t, server, http.MethodPut, "/api/tenants/"+tenant+"/canvases/"+canvasID+"/dimensions",
		`{"operation_id":"op-publish-size-01","width":1280,"height":720}`, alice.cookie, alice.csrf)
	if body := decodeBody(t, res); res.StatusCode != http.StatusOK || body["revision"] != float64(1) {
		t.Fatalf("resize = %d %v", res.StatusCode, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	pending, delivered := pendingOutbox(t, dsn, tenant)
	for (pending != 0 || delivered != 1) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		pending, delivered = pendingOutbox(t, dsn, tenant)
	}
	if pending != 0 || delivered != 1 {
		t.Fatalf("outbox of %s: %d pending, %d delivered; want 0 and 1\n%s", tenant, pending, delivered, server.logs)
	}

	began := time.Now()
	server.stop(t)
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v, over the 5s budget", elapsed)
	}
	logs := server.logs.String()
	for _, msg := range []string{`"msg":"outbox record delivered"`, `"msg":"stopped"`} {
		if !strings.Contains(logs, msg) {
			t.Errorf("process logs lack %s\n%s", msg, logs)
		}
	}
	for name, s := range map[string]string{
		"password":      "correct horse battery",
		"session token": strings.TrimPrefix(alice.cookie, "__Host-pcs="),
		"CSRF token":    alice.csrf,
		"database URL":  dsn,
	} {
		if strings.Contains(logs, s) {
			t.Errorf("process logs contain the %s", name)
		}
	}
}

// S1, S4, L2c: the built server answers API and static requests with the
// security headers, refuses preflights without CORS headers, and refuses
// unsafe methods on static files.
func TestServerGuardsAPIAndStaticAnswers(t *testing.T) {
	db := pgtest.Start(t)
	bin := buildServer(t)
	static := t.TempDir()
	if err := os.MkdirAll(filepath.Join(static, "editor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "editor", "index.html"), []byte("<!doctype html><title>editor</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := start(t, bin,
		"PEPICANVAS_DATABASE_URL="+db.DSN(t),
		"PEPICANVAS_LISTEN_ADDR=127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN="+origin,
		"PEPICANVAS_STATIC_DIR="+static,
	)
	frame := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	expectHeaders := func(name string, res *http.Response, csp string) {
		t.Helper()
		want := maps.Clone(frame)
		want["Content-Security-Policy"] = csp
		for k, v := range want {
			if got := res.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
		// The origin is plain HTTP, so HSTS would lock a local deployment out.
		if got := res.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s: HSTS = %q on a plain-HTTP origin", name, got)
		}
		for k := range res.Header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s written", name, k)
			}
		}
	}
	api := "default-src 'none'; frame-ancestors 'none'"

	res := request(t, server, http.MethodPost, "/api/accounts", `{"operation_id":"op-process-guard-1","email":"alice@example.com","password":"correct horse battery"}`, "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", res.StatusCode)
	}
	expectHeaders("register", res, api)
	res = request(t, server, http.MethodGet, "/api/session", "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/session = %d, want 401", res.StatusCode)
	}
	expectHeaders("unauthorized session", res, api)

	r, err := http.NewRequestWithContext(context.Background(), http.MethodOptions, "http://"+server.addr+"/api/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", origin)
	r.Header.Set("Access-Control-Request-Method", http.MethodDelete)
	r.Header.Set("Access-Control-Request-Headers", "x-csrf-token")
	res, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("OPTIONS /api/session = %d, want 403", res.StatusCode)
	}
	expectHeaders("preflight", res, api)

	res = request(t, server, http.MethodGet, "/editor/", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /editor/ = %d", res.StatusCode)
	}
	expectHeaders("editor page", res, "frame-ancestors 'none'")
	res = request(t, server, http.MethodPost, "/editor/", "{}", "")
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /editor/ = %d Allow %q, want 405 GET, HEAD", res.StatusCode, res.Header.Get("Allow"))
	}
	expectHeaders("static POST", res, "frame-ancestors 'none'")
	server.stop(t)
}

// US-004: an activated owner creates, lists, and revokes an invitation over
// real PostgreSQL, and a creation retried after a restart replays its answer.
func TestOwnerManagesInvitationsAcrossRestart(t *testing.T) {
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
	stranger := registerAndSignIn(t, server, "op-process-strng-1", "stranger@example.com")
	if code, out := grantOperator(t, bin, []string{"PEPICANVAS_DATABASE_URL=" + dsn}, "operator@example.com"); code != 0 {
		t.Fatalf("grant-operator = %d\n%s", code, out)
	}
	operator := signIn(t, server, "operator@example.com")
	if res, _ := activate(t, server, operator, "op-process-act-a01", "alice@example.com"); res.StatusCode != http.StatusOK {
		t.Fatalf("activate alice = %d", res.StatusCode)
	}

	const token = "pci_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	create := `{"operation_id":"op-process-inv-c01","token":"` + token + `"}`
	res := requestWith(t, server, http.MethodPost, "/api/invitations", create, alice.cookie, alice.csrf)
	created, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated || strings.Contains(string(created), token) {
		t.Fatalf("create = %d %s", res.StatusCode, created)
	}
	var invitation map[string]any
	if err := json.Unmarshal(created, &invitation); err != nil || len(invitation["invitation_id"].(string)) != 32 {
		t.Fatalf("create body = %s", created)
	}
	id := invitation["invitation_id"].(string)

	list := func(p *process) []any {
		t.Helper()
		res := request(t, p, http.MethodGet, "/api/invitations", "", alice.cookie)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("list = %d", res.StatusCode)
		}
		items, _ := decodeBody(t, res)["invitations"].([]any)
		return items
	}
	if items := list(server); len(items) != 1 || items[0].(map[string]any)["invitation_id"] != id {
		t.Fatalf("pending = %v", items)
	}

	// S2: a stranger can neither create, list, nor revoke.
	if res := requestWith(t, server, http.MethodPost, "/api/invitations", `{"operation_id":"op-process-inv-s01","token":"pci_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}`, stranger.cookie, stranger.csrf); res.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger create = %d, want 403", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, "/api/invitations", "", stranger.cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger list = %d, want 403", res.StatusCode)
	}
	if res := requestWith(t, server, http.MethodPost, "/api/invitations/"+id+"/revoke", `{"operation_id":"op-process-inv-s02"}`, stranger.cookie, stranger.csrf); res.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger revoke = %d, want 403", res.StatusCode)
	}

	var revoked []string
	for _, op := range []string{"op-process-inv-r01", "op-process-inv-r02"} {
		res := requestWith(t, server, http.MethodPost, "/api/invitations/"+id+"/revoke", `{"operation_id":"`+op+`"}`, alice.cookie, alice.csrf)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(body) != `{"invitation_id":"`+id+`","status":"revoked"}`+"\n" {
			t.Fatalf("revoke %s = %d %s", op, res.StatusCode, body)
		}
		revoked = append(revoked, string(body))
	}
	if revoked[0] != revoked[1] {
		t.Fatalf("revocations differ: %v", revoked)
	}
	if items := list(server); len(items) != 0 {
		t.Fatalf("pending after revoke = %v", items)
	}
	server.stop(t)
	logs := server.logs.String()

	// S9: the creation retried after a restart returns the identical body.
	server = start(t, bin, env...)
	res = requestWith(t, server, http.MethodPost, "/api/invitations", create, alice.cookie, alice.csrf)
	if again, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusCreated || string(again) != string(created) {
		t.Fatalf("replay after restart = %d %s, want %s", res.StatusCode, again, created)
	}
	server.stop(t)
	logs += server.logs.String()

	for _, msg := range []string{`"msg":"invitation created"`, `"msg":"invitation revoked"`} {
		if !strings.Contains(logs, msg) {
			t.Fatalf("no %s log", msg)
		}
	}
	for name, s := range map[string]string{
		"invitation token": "pci_",
		"session token":    strings.TrimPrefix(alice.cookie, "__Host-pcs="),
		"CSRF token":       alice.csrf,
		"owner email":      "alice@example.com",
	} {
		if strings.Contains(logs, s) {
			t.Errorf("process logs contain the %s", name)
		}
	}
}

// US-005: an account accepts an owner's link over real PostgreSQL, the
// acceptance retried after a restart replays its answer, and the new
// moderator lists the canvas and is refused owner routes.
func TestModeratorAcceptsAnInvitationAcrossRestart(t *testing.T) {
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
	res, activation := activate(t, server, operator, "op-process-act-a01", "alice@example.com")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("activate alice = %d", res.StatusCode)
	}
	tenantID, canvasID := activation["tenant_id"].(string), activation["canvas_id"].(string)
	canvasPath := "/api/tenants/" + tenantID + "/canvases/" + canvasID

	const token = "pci_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	if res := requestWith(t, server, http.MethodPost, "/api/invitations", `{"operation_id":"op-process-inv-c01","token":"`+token+`"}`, alice.cookie, alice.csrf); res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", res.StatusCode)
	}

	// S1: no session, no acceptance and no list.
	if res := request(t, server, http.MethodPost, "/api/invitations/accept", `{"operation_id":"op-process-acc-n01","token":"`+token+`"}`, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous accept = %d, want 401", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, "/api/memberships", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous memberships = %d, want 401", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, canvasPath, "", bob.cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("canvas before acceptance = %d, want 403", res.StatusCode)
	}

	accept := `{"operation_id":"op-process-acc-a01","token":"` + token + `"}`
	res = requestWith(t, server, http.MethodPost, "/api/invitations/accept", accept, bob.cookie, bob.csrf)
	accepted, _ := io.ReadAll(res.Body)
	want := `{"tenant_id":"` + tenantID + `","canvas_id":"` + canvasID + `"}` + "\n"
	if res.StatusCode != http.StatusOK || string(accepted) != want {
		t.Fatalf("accept = %d %s, want %s", res.StatusCode, accepted, want)
	}
	if n := count(t, dsn, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'moderator'`, tenantID); n != 1 {
		t.Fatalf("moderator memberships = %d, want 1", n)
	}
	server.stop(t)
	logs := server.logs.String()

	// S12: the acceptance retried after a restart returns the identical body.
	server = start(t, bin, env...)
	res = requestWith(t, server, http.MethodPost, "/api/invitations/accept", accept, bob.cookie, bob.csrf)
	if again, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(again) != string(accepted) {
		t.Fatalf("replay after restart = %d %s, want %s", res.StatusCode, again, accepted)
	}
	// The consumed link under another operation is unavailable.
	if res := requestWith(t, server, http.MethodPost, "/api/invitations/accept", `{"operation_id":"op-process-acc-a02","token":"`+token+`"}`, bob.cookie, bob.csrf); res.StatusCode != http.StatusNotFound {
		t.Fatalf("consumed link = %d, want 404", res.StatusCode)
	}
	if n := count(t, dsn, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'moderator'`, tenantID); n != 1 {
		t.Fatalf("moderator memberships after replay = %d, want 1", n)
	}

	// S17: the moderated canvas is listed and readable.
	res = request(t, server, http.MethodGet, "/api/memberships", "", bob.cookie)
	listed, _ := io.ReadAll(res.Body)
	if want := `{"memberships":[{"tenant_id":"` + tenantID + `","canvas_id":"` + canvasID + `","role":"moderator"}]}` + "\n"; res.StatusCode != http.StatusOK || string(listed) != want {
		t.Fatalf("memberships = %d %s, want %s", res.StatusCode, listed, want)
	}
	if res := request(t, server, http.MethodGet, canvasPath, "", bob.cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("moderated canvas = %d, want 200", res.StatusCode)
	}

	// S8: the moderator is refused the owner's dimensions and invitations.
	if res := requestWith(t, server, http.MethodPut, canvasPath+"/dimensions", `{"operation_id":"op-process-dim-b01","width":1280,"height":720}`, bob.cookie, bob.csrf); res.StatusCode != http.StatusForbidden {
		t.Fatalf("moderator resize = %d, want 403", res.StatusCode)
	}
	if res := requestWith(t, server, http.MethodPost, "/api/invitations", `{"operation_id":"op-process-inv-b01","token":"pci_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}`, bob.cookie, bob.csrf); res.StatusCode != http.StatusForbidden {
		t.Fatalf("moderator create = %d, want 403", res.StatusCode)
	}
	if res := request(t, server, http.MethodGet, "/api/invitations", "", bob.cookie); res.StatusCode != http.StatusForbidden {
		t.Fatalf("moderator list = %d, want 403", res.StatusCode)
	}
	server.stop(t)
	logs += server.logs.String()

	if !strings.Contains(logs, `"msg":"invitation accepted"`) {
		t.Fatal("no invitation accepted log")
	}
	for name, s := range map[string]string{
		"invitation token": "pci_",
		"session token":    strings.TrimPrefix(bob.cookie, "__Host-pcs="),
		"CSRF token":       bob.csrf,
		"moderator email":  "bob@example.com",
	} {
		if strings.Contains(logs, s) {
			t.Errorf("process logs contain the %s", name)
		}
	}
}
