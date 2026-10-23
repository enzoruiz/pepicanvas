package ingress_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	origin   = "https://pepicanvas.test"
	peerIP   = "192.0.2.10"
	sessionA = "pcs_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sessionB = "pcs_" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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

// generous limits keep rate limiting out of the tests that are not about it.
var generous = ingress.Limits{
	UnsafePerIP:         ratelimit.Rate{Limit: 1000, Window: time.Minute},
	MutationsPerSession: ratelimit.Rate{Limit: 1000, Window: time.Minute},
	UpgradesPerIP:       ratelimit.Rate{Limit: 1000, Window: time.Minute},
	UpgradesPerSession:  ratelimit.Rate{Limit: 1000, Window: time.Minute},
}

type env struct {
	t       *testing.T
	clock   *clock
	logs    *syncBuffer
	ingress *ingress.Ingress
	handler http.Handler
	calls   atomic.Int64
}

func newEnv(t *testing.T, limits ingress.Limits, mutate ...func(*ingress.Config)) *env {
	t.Helper()
	e := &env{t: t, clock: &clock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}, logs: &syncBuffer{}}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(e.logs, nil)))
	config := ingress.Config{AllowedOrigins: []string{origin}, Limits: limits, Logger: logger, Now: e.clock.Now}
	for _, m := range mutate {
		m(&config)
	}
	in, err := ingress.New(config)
	if err != nil {
		t.Fatal(err)
	}
	e.ingress = in
	count := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux := http.NewServeMux()
	mux.Handle("GET /api/things/{id}", count)
	mux.Handle("POST /api/open", in.Route(ingress.Anonymous, count))
	mux.Handle("POST /api/things/{id}", in.Route(ingress.Session, count))
	mux.Handle("DELETE /api/things/{id}", in.Route(ingress.Session, count))
	mux.Handle("POST /api/unclassified", count)
	mux.Handle("GET /api/socket", in.Upgrade(count))
	e.handler = httpx.Correlate(in.Protect(mux))
	return e
}

type option func(*http.Request)

func withOrigin(o string) option    { return func(r *http.Request) { r.Header.Set("Origin", o) } }
func withoutOrigin() option         { return func(r *http.Request) { r.Header.Del("Origin") } }
func fromIP(ip string) option       { return func(r *http.Request) { r.RemoteAddr = ip + ":40000" } }
func withCSRF(v string) option      { return func(r *http.Request) { r.Header.Set(ingress.CSRFHeader, v) } }
func withHeader(k, v string) option { return func(r *http.Request) { r.Header.Set(k, v) } }
func withSession(token string) option {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: ingress.SessionCookie, Value: token}) }
}

func upgrade() option {
	return func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
}

func csrfOf(token string) string {
	return access.CSRFToken(secret.New(secret.KindSessionToken, token)).Reveal()
}

func (e *env) do(method, path string, options ...option) *http.Response {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.RemoteAddr = peerIP + ":40000"
	r.Header.Set("Origin", origin)
	for _, o := range options {
		o(r)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	res := w.Result()
	raw, _ := io.ReadAll(res.Body)
	res.Body = io.NopCloser(bytes.NewReader(raw))
	for name := range res.Header {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			e.t.Fatalf("%s %s: CORS header %s written; CORS must stay closed", method, path, name)
		}
	}
	return res
}

func expectRejected(t *testing.T, res *http.Response, status int, code string) {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d", res.StatusCode, status)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != code || body["correlation_id"] != res.Header.Get(httpx.CorrelationHeader) || len(body) != 2 {
		t.Fatalf("body = %v, want only error %q and the correlation ID", body, code)
	}
}

func (e *env) expectCalls(want int64) {
	e.t.Helper()
	if got := e.calls.Load(); got != want {
		e.t.Fatalf("handler ran %d times, want %d", got, want)
	}
}

func count(snapshot []ingress.Count, surface ingress.Surface, reason ingress.Reason) uint64 {
	for _, c := range snapshot {
		if c.Surface == surface && c.Reason == reason {
			return c.Value
		}
	}
	return 0
}

func TestConfigurationIsRejectedWhenAControlWouldBeWeakened(t *testing.T) {
	valid := ingress.Config{AllowedOrigins: []string{origin}, Limits: generous}
	if _, err := ingress.New(valid); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for _, origins := range [][]string{
		nil, {}, {"*"}, {"null"}, {""}, {"https://*.pepicanvas.test"}, {"https://pepicanvas.test/"},
		{"https://pepicanvas.test/editor"}, {"https://user@pepicanvas.test"}, {"ftp://pepicanvas.test"},
		{"pepicanvas.test"}, {"https://pepicanvas.test?x=1"}, {origin, "*"},
		// Browsers send origins lower-cased, without a default port, and in
		// ASCII; any other spelling could never match and only hides a typo.
		{"https://PepiCanvas.test"}, {"https://pepicanvas.test:443"}, {"http://pepicanvas.test:80"},
		{"https://pepicanvas.test:"}, {"https://bücher.test"},
	} {
		config := valid
		config.AllowedOrigins = origins
		if _, err := ingress.New(config); err == nil {
			t.Errorf("origins %q accepted", origins)
		}
	}
	for name, mutate := range map[string]func(*ingress.Limits){
		"unsafe per IP":         func(l *ingress.Limits) { l.UnsafePerIP.Limit = 0 },
		"mutations per session": func(l *ingress.Limits) { l.MutationsPerSession.Window = 0 },
		"upgrades per IP":       func(l *ingress.Limits) { l.UpgradesPerIP.Limit = -1 },
		"upgrades per session":  func(l *ingress.Limits) { l.UpgradesPerSession = ratelimit.Rate{} },
	} {
		config := valid
		mutate(&config.Limits)
		if _, err := ingress.New(config); err == nil {
			t.Errorf("%s: a non-positive limit was accepted", name)
		}
	}
}

func TestDefaultLimits(t *testing.T) {
	want := ingress.Limits{
		UnsafePerIP:         ratelimit.Rate{Limit: 300, Window: time.Minute},
		MutationsPerSession: ratelimit.Rate{Limit: 120, Window: time.Minute},
		UpgradesPerIP:       ratelimit.Rate{Limit: 30, Window: time.Minute},
		UpgradesPerSession:  ratelimit.Rate{Limit: 10, Window: time.Minute},
	}
	if ingress.DefaultLimits != want {
		t.Fatalf("DefaultLimits = %+v, want %+v", ingress.DefaultLimits, want)
	}
}

func TestOriginIsExactForEveryUnsafeMethodBeforeTheHandler(t *testing.T) {
	e := newEnv(t, generous)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for name, o := range map[string]option{
			"absent":       withoutOrigin(),
			"hostile":      withOrigin("https://evil.test"),
			"other scheme": withOrigin("http://pepicanvas.test"),
			"suffix":       withOrigin("https://pepicanvas.test.evil.test"),
			"null":         withOrigin("null"),
			"trailing":     withOrigin(origin + "/"),
			"case":         withOrigin("https://PepiCanvas.test"),
			"port":         withOrigin("https://pepicanvas.test:443"),
		} {
			res := e.do(method, "/api/open", o)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s origin: status = %d, want 403", method, name, res.StatusCode)
			}
			expectRejected(t, res, http.StatusForbidden, "forbidden")
		}
	}
	e.expectCalls(0)
	if res := e.do(http.MethodPost, "/api/open"); res.StatusCode != http.StatusNoContent {
		t.Fatalf("allowed origin: status = %d, want 204", res.StatusCode)
	}
	e.expectCalls(1)
	// Safe methods carry no side effect and pass without an Origin.
	if res := e.do(http.MethodGet, "/api/things/1", withoutOrigin()); res.StatusCode != http.StatusNoContent {
		t.Fatalf("GET without origin: status = %d, want 204", res.StatusCode)
	}
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonOrigin); got != 32 {
		t.Fatalf("origin rejections counted = %d, want 32", got)
	}
}

func TestCSRFIsBoundToTheSessionAndCheckedBeforeTheHandler(t *testing.T) {
	e := newEnv(t, generous)
	for name, o := range map[string]option{
		"missing":       func(*http.Request) {},
		"invalid":       withCSRF("not-a-token"),
		"other session": withCSRF(csrfOf(sessionB)),
		"session token": withCSRF(sessionA),
		"empty":         withCSRF(""),
		"padded":        withCSRF(csrfOf(sessionA) + " "),
	} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			res := e.do(method, "/api/things/1", withSession(sessionA), o)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s CSRF %s: status = %d, want 403", method, name, res.StatusCode)
			}
			expectRejected(t, res, http.StatusForbidden, "forbidden")
		}
	}
	// A malformed cookie has no CSRF token at all.
	expectRejected(t, e.do(http.MethodPost, "/api/things/1", withSession("garbage"), withCSRF(csrfOf("garbage"))), http.StatusForbidden, "forbidden")
	e.expectCalls(0)
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonCSRF); got != 13 {
		t.Fatalf("CSRF rejections counted = %d, want 13", got)
	}

	if res := e.do(http.MethodPost, "/api/things/1", withSession(sessionA), withCSRF(csrfOf(sessionA))); res.StatusCode != http.StatusNoContent {
		t.Fatalf("valid CSRF: status = %d, want 204", res.StatusCode)
	}
	// Without a session cookie there is no ambient credential to forge; the
	// handler answers for the missing session.
	if res := e.do(http.MethodPost, "/api/things/1"); res.StatusCode != http.StatusNoContent {
		t.Fatalf("no session: status = %d, want 204 from the handler", res.StatusCode)
	}
	e.expectCalls(2)
	// Origin is still checked first, even with a valid CSRF token.
	expectRejected(t, e.do(http.MethodPost, "/api/things/1", withSession(sessionA), withCSRF(csrfOf(sessionA)), withOrigin("https://evil.test")), http.StatusForbidden, "forbidden")
	e.expectCalls(2)
}

func TestUnclassifiedUnsafeRoutesFailClosed(t *testing.T) {
	e := newEnv(t, generous)
	expectRejected(t, e.do(http.MethodPost, "/api/unclassified"), http.StatusForbidden, "forbidden")
	e.expectCalls(0)
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonUnclassified); got != 1 {
		t.Fatalf("unclassified rejections = %d, want 1", got)
	}
	// Unmatched routes keep the mux answers.
	if res := e.do(http.MethodPost, "/api/missing"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route: status = %d, want 404", res.StatusCode)
	}
	if res := e.do(http.MethodPut, "/api/open"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method: status = %d, want 405", res.StatusCode)
	}
	e.expectCalls(0)
}

func TestCORSStaysClosed(t *testing.T) {
	e := newEnv(t, generous)
	for _, o := range []option{withOrigin(origin), withOrigin("https://evil.test"), withoutOrigin()} {
		res := e.do(http.MethodOptions, "/api/things/1", o,
			withHeader("Access-Control-Request-Method", "POST"),
			withHeader("Access-Control-Request-Headers", "x-csrf-token"))
		expectRejected(t, res, http.StatusForbidden, "forbidden")
	}
	e.expectCalls(0)
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonPreflight); got != 3 {
		t.Fatalf("preflight rejections = %d, want 3", got)
	}
	// e.do already fails on any Access-Control-* header; exercise a success too.
	e.do(http.MethodPost, "/api/open")
}

func TestSecurityHeadersOnEveryAnswer(t *testing.T) {
	check := func(t *testing.T, res *http.Response, hsts bool) {
		t.Helper()
		want := map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"X-Frame-Options":              "DENY",
			"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Cross-Origin-Opener-Policy":   "same-origin",
		}
		for k, v := range want {
			if got := res.Header.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		got := res.Header.Get("Strict-Transport-Security")
		if hsts && got != "max-age=31536000" {
			t.Errorf("HSTS = %q, want max-age=31536000", got)
		}
		if !hsts && got != "" {
			t.Errorf("HSTS = %q on a plain-HTTP origin", got)
		}
	}
	e := newEnv(t, generous)
	check(t, e.do(http.MethodGet, "/api/things/1"), true)
	check(t, e.do(http.MethodPost, "/api/open", withoutOrigin()), true)
	check(t, e.do(http.MethodOptions, "/api/open"), true)

	local := newEnv(t, generous, func(c *ingress.Config) { c.AllowedOrigins = []string{"http://localhost:4176"} })
	check(t, local.do(http.MethodPost, "/api/open", withOrigin("http://localhost:4176")), false)
}

func TestStaticFilesGetHeadersAndRejectUnsafeMethods(t *testing.T) {
	e := newEnv(t, generous)
	var served atomic.Int64
	static := e.ingress.Static(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		static.ServeHTTP(w, httptest.NewRequest(method, "/editor/", nil))
		if w.Code != http.StatusOK || w.Header().Get("X-Frame-Options") != "DENY" ||
			w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" ||
			w.Header().Get("Strict-Transport-Security") != "max-age=31536000" ||
			w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s static: %d %v", method, w.Code, w.Header())
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/editor/", nil)
		r.Header.Set("Origin", origin)
		static.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s static: status %d Allow %q, want 405 GET, HEAD", method, w.Code, w.Header().Get("Allow"))
		}
	}
	if served.Load() != 2 {
		t.Fatalf("static handler ran %d times, want 2", served.Load())
	}
}

func TestUnsafeRequestsAreLimitedPerIPAndRecover(t *testing.T) {
	limits := generous
	limits.UnsafePerIP = ratelimit.Rate{Limit: 3, Window: time.Minute}
	e := newEnv(t, limits)
	for i := range 3 {
		// Forwarding headers never move a request to another budget.
		if res := e.do(http.MethodPost, "/api/open", withHeader("X-Forwarded-For", "198.51.100."+strconv.Itoa(i))); res.StatusCode != http.StatusNoContent {
			t.Fatalf("attempt %d: status = %d", i+1, res.StatusCode)
		}
	}
	e.clock.Advance(20 * time.Second)
	res := e.do(http.MethodPost, "/api/things/1", withHeader("X-Real-IP", "198.51.100.99"), withHeader("Forwarded", "for=198.51.100.98"))
	if got := res.Header.Get("Retry-After"); got != "40" {
		t.Fatalf("Retry-After = %q, want 40", got)
	}
	expectRejected(t, res, http.StatusTooManyRequests, "rate_limited")
	e.expectCalls(3)
	// Reads are not limited by the unsafe budget; another address has its own.
	if res := e.do(http.MethodGet, "/api/things/1"); res.StatusCode != http.StatusNoContent {
		t.Fatalf("GET while limited: %d", res.StatusCode)
	}
	if res := e.do(http.MethodPost, "/api/open", fromIP("192.0.2.11")); res.StatusCode != http.StatusNoContent {
		t.Fatalf("other address: %d", res.StatusCode)
	}
	// A rejected Origin does not consume the budget.
	e.clock.Advance(40 * time.Second)
	for range 5 {
		e.do(http.MethodPost, "/api/open", withOrigin("https://evil.test"))
	}
	for i := range 3 {
		if res := e.do(http.MethodPost, "/api/open"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("after the window, attempt %d: %d", i+1, res.StatusCode)
		}
	}
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonRateIP); got != 1 {
		t.Fatalf("IP limit rejections = %d, want 1", got)
	}
}

func TestMutationsAreLimitedPerSessionIndependently(t *testing.T) {
	limits := generous
	limits.MutationsPerSession = ratelimit.Rate{Limit: 2, Window: time.Minute}
	e := newEnv(t, limits)
	mutate := func(session, ip string) *http.Response {
		return e.do(http.MethodPost, "/api/things/1", withSession(session), withCSRF(csrfOf(session)), fromIP(ip))
	}
	for range 2 {
		if res := mutate(sessionA, "192.0.2.20"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("A within limit: %d", res.StatusCode)
		}
	}
	// The session budget follows the session across addresses.
	res := mutate(sessionA, "192.0.2.21")
	if res.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", res.Header.Get("Retry-After"))
	}
	expectRejected(t, res, http.StatusTooManyRequests, "rate_limited")
	// Streamer B from the same address keeps its own session budget.
	for range 2 {
		if res := mutate(sessionB, "192.0.2.20"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("B after A was limited: %d", res.StatusCode)
		}
	}
	// A bad CSRF token is rejected before it can spend the session budget.
	e.clock.Advance(time.Minute)
	e.do(http.MethodPost, "/api/things/1", withSession(sessionA), withCSRF("wrong"))
	for range 2 {
		if res := mutate(sessionA, "192.0.2.20"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("A after the window: %d", res.StatusCode)
		}
	}
	e.expectCalls(6)
	if got := count(e.ingress.Snapshot(), ingress.SurfaceHTTP, ingress.ReasonRateSession); got != 1 {
		t.Fatalf("session limit rejections = %d, want 1", got)
	}
}

func TestAllowRecordsTheDimensionAndAFullTableFailsClosed(t *testing.T) {
	e := newEnv(t, generous)
	limiter := ratelimit.New(1, time.Minute, 2, e.clock.Now)
	run := func(key string, reason ingress.Reason) *http.Response {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !e.ingress.Allow(w, r, limiter, key, reason) {
				return
			}
			e.calls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/open", nil)
		httpx.Correlate(h).ServeHTTP(w, r)
		return w.Result()
	}
	if res := run("alice", ingress.ReasonRateAccount); res.StatusCode != http.StatusNoContent {
		t.Fatalf("first: %d", res.StatusCode)
	}
	expectRejected(t, run("alice", ingress.ReasonRateAccount), http.StatusTooManyRequests, "rate_limited")
	run("bob", ingress.ReasonRateAccount)
	// The table holds two keys: a third key is denied until one expires.
	res := run("carol", ingress.ReasonRateAccount)
	if res.Header.Get("Retry-After") != "60" {
		t.Fatalf("full table Retry-After = %q", res.Header.Get("Retry-After"))
	}
	expectRejected(t, res, http.StatusTooManyRequests, "rate_limited")
	e.expectCalls(2)
	snapshot := e.ingress.Snapshot()
	if count(snapshot, ingress.SurfaceHTTP, ingress.ReasonRateAccount) != 1 || count(snapshot, ingress.SurfaceHTTP, ingress.ReasonLimiterFull) != 1 {
		t.Fatalf("snapshot = %+v, want one account and one full-table rejection", snapshot)
	}
	e.clock.Advance(time.Minute)
	if res := run("carol", ingress.ReasonRateAccount); res.StatusCode != http.StatusNoContent {
		t.Fatalf("after expiry: %d", res.StatusCode)
	}
}

func TestUpgradeChecksOriginAndLimitsBeforeTheHandler(t *testing.T) {
	limits := generous
	limits.UpgradesPerIP = ratelimit.Rate{Limit: 4, Window: time.Minute}
	limits.UpgradesPerSession = ratelimit.Rate{Limit: 2, Window: time.Minute}
	e := newEnv(t, limits)
	for name, o := range map[string]option{
		"absent":  withoutOrigin(),
		"hostile": withOrigin("https://evil.test"),
		"null":    withOrigin("null"),
		"scheme":  withOrigin("http://pepicanvas.test"),
	} {
		res := e.do(http.MethodGet, "/api/socket", upgrade(), withSession(sessionA), o)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s origin upgrade: %d", name, res.StatusCode)
		}
		expectRejected(t, res, http.StatusForbidden, "forbidden")
	}
	// A plain GET to an upgrade route is not a WebSocket handshake.
	expectRejected(t, e.do(http.MethodGet, "/api/socket"), http.StatusBadRequest, "bad_request")
	expectRejected(t, e.do(http.MethodGet, "/api/socket", withHeader("Upgrade", "websocket")), http.StatusBadRequest, "bad_request")
	e.expectCalls(0)

	for range 2 {
		if res := e.do(http.MethodGet, "/api/socket", upgrade(), withSession(sessionA)); res.StatusCode != http.StatusNoContent {
			t.Fatalf("A upgrade: %d", res.StatusCode)
		}
	}
	expectRejected(t, e.do(http.MethodGet, "/api/socket", upgrade(), withSession(sessionA)), http.StatusTooManyRequests, "rate_limited")
	// Streamer B reconnects from the same address with its own session budget;
	// a session-limited attempt does not spend the address budget, so B's two
	// upgrades fill it, and the next upgrade from the address is refused.
	for range 2 {
		if res := e.do(http.MethodGet, "/api/socket", upgrade(), withSession(sessionB)); res.StatusCode != http.StatusNoContent {
			t.Fatalf("B upgrade: %d", res.StatusCode)
		}
	}
	expectRejected(t, e.do(http.MethodGet, "/api/socket", upgrade()), http.StatusTooManyRequests, "rate_limited")
	e.expectCalls(4)
	snapshot := e.ingress.Snapshot()
	for reason, want := range map[ingress.Reason]uint64{
		ingress.ReasonOrigin: 4, ingress.ReasonNotWebSocket: 2, ingress.ReasonRateSession: 1, ingress.ReasonRateIP: 1,
	} {
		if got := count(snapshot, ingress.SurfaceWebSocket, reason); got != want {
			t.Errorf("websocket %s = %d, want %d", reason, got, want)
		}
	}
	e.clock.Advance(time.Minute)
	if res := e.do(http.MethodGet, "/api/socket", upgrade(), withSession(sessionA)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("A after the window: %d", res.StatusCode)
	}
}

func TestRejectedUpgradesDoNotFillTheSessionTable(t *testing.T) {
	limits := generous
	limits.UpgradesPerIP = ratelimit.Rate{Limit: 30, Window: time.Minute}
	limits.UpgradesPerSession = ratelimit.Rate{Limit: 10, Window: time.Minute}
	e := newEnv(t, limits)
	// One address cycles invented session cookies; once its address budget is
	// spent, its attempts must not take slots in the shared session table.
	for i := range 100_000 {
		e.do(http.MethodGet, "/api/socket", upgrade(), fromIP("198.51.100.1"), withSession(fmt.Sprintf("pcs_attacker%07d", i)))
	}
	if res := e.do(http.MethodGet, "/api/socket", upgrade(), fromIP("203.0.113.5"), withSession(sessionB)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("victim on another address refused with %d", res.StatusCode)
	}
	if got := count(e.ingress.Snapshot(), ingress.SurfaceWebSocket, ingress.ReasonLimiterFull); got != 0 {
		t.Fatalf("limiter_full = %d, want 0", got)
	}
}

func TestOriginAndCSRFHostileVariants(t *testing.T) {
	e := newEnv(t, generous)
	for name, opts := range map[string][]option{
		"dup good+bad":  {func(r *http.Request) { r.Header.Add("Origin", "https://evil.test") }},
		"dup good+good": {func(r *http.Request) { r.Header.Add("Origin", origin) }},
		"comma list":    {withOrigin(origin + ", https://evil.test")},
		"punycode":      {withOrigin("https://xn--pepicanvas-test.test")},
	} {
		if res := e.do(http.MethodPost, "/api/open", opts...); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	two := ingress.SessionCookie + "=" + sessionA + "; " + ingress.SessionCookie + "=" + sessionB
	if res := e.do(http.MethodPost, "/api/things/1", func(r *http.Request) { r.Header.Set("Cookie", two) }, withCSRF(csrfOf(sessionB))); res.StatusCode != http.StatusForbidden {
		t.Errorf("second cookie's CSRF accepted: %d", res.StatusCode)
	}
	for _, p := range []string{"/api/%75nclassified", "//api/unclassified", "/api/things/../unclassified"} {
		e.do(http.MethodPost, p)
	}
	e.expectCalls(0)
}

func TestRejectionsAreLoggedWithReasonRouteAndCorrelationOnly(t *testing.T) {
	e := newEnv(t, generous)
	csrf := csrfOf(sessionB)
	ids := map[string]bool{}
	for _, res := range []*http.Response{
		e.do(http.MethodPost, "/api/things/4f1c9a", withOrigin("https://evil.test")),
		e.do(http.MethodPost, "/api/things/77ab02", withSession(sessionA), withCSRF(csrf)),
		e.do(http.MethodGet, "/api/socket", upgrade(), withOrigin("https://evil.test")),
	} {
		ids[res.Header.Get(httpx.CorrelationHeader)] = true
	}
	logs := e.logs.String()
	for _, leaked := range []string{sessionA, sessionB, csrf, "evil.test", "4f1c9a", "77ab02"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("logs contain %q:\n%s", leaked, logs)
		}
	}
	type line struct {
		Msg           string `json:"msg"`
		CorrelationID string `json:"correlation_id"`
		Error         string `json:"error"`
	}
	var found []line
	for _, raw := range strings.Split(strings.TrimSpace(logs), "\n") {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if l.Msg == "request failed" {
			found = append(found, l)
		}
	}
	want := []string{
		"ingress rejected: surface=http reason=origin route=POST /api/things/{id}",
		"ingress rejected: surface=http reason=csrf route=POST /api/things/{id}",
		"ingress rejected: surface=websocket reason=origin route=GET /api/socket",
	}
	if len(found) != len(want) {
		t.Fatalf("rejection lines = %+v", found)
	}
	for i, l := range found {
		if l.Error != want[i] || !ids[l.CorrelationID] {
			t.Errorf("line %d = %+v, want error %q with a response correlation ID", i, l, want[i])
		}
	}
}

func TestMetricsStayBoundedUnderHostileInput(t *testing.T) {
	e := newEnv(t, generous)
	for i := range 200 {
		suffix := strconv.Itoa(i)
		e.do(http.MethodPost, "/api/things/"+suffix, withOrigin("https://evil"+suffix+".test"))
		e.do(http.MethodPost, "/api/things/"+suffix, withSession(sessionA), withCSRF("bad"+suffix))
		e.do(http.MethodPost, "/api/nowhere/"+suffix, withOrigin("https://x"+suffix+".test"))
	}
	snapshot := e.ingress.Snapshot()
	if len(snapshot) > len(ingress.Surfaces())*len(ingress.Reasons()) {
		t.Fatalf("snapshot has %d series", len(snapshot))
	}
	raw, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw), "evil") || strings.Contains(string(raw), "nowhere") {
		t.Fatalf("snapshot carries request data: %s", raw)
	}
	if count(snapshot, ingress.SurfaceHTTP, ingress.ReasonOrigin) != 400 || count(snapshot, ingress.SurfaceHTTP, ingress.ReasonCSRF) != 200 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestSessionLimiterKeysAreNotTheToken(t *testing.T) {
	// The limiter key is a digest, so the in-memory table never holds tokens.
	sum := sha256.Sum256([]byte(sessionA))
	if got := ingress.SessionKey(sessionA); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("SessionKey = %q", got)
	}
}
