package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/access/accesstest"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription/subscriptiontest"
)

const (
	origin   = "https://pepicanvas.test"
	password = "decoy-password-5d1e7b"
	clientIP = "192.0.2.10"
)

// canvases serves seeded canvases and the empty canvases activations created,
// and applies dimension changes over them, retaining operations as the
// PostgreSQL canvas store does.
type canvases struct {
	mu        sync.Mutex
	byID      map[canvas.Key]canvas.Canvas
	activated *subscriptiontest.MemoryStore
	// applied holds the canvases changed by Apply; Load reads it first.
	applied    map[canvas.Key]canvas.Canvas
	operations map[string]appliedOperation
	effects    int
	// failNext, when set, fails the next Apply without an effect.
	failNext error
}

type appliedOperation struct {
	fingerprint   [32]byte
	result        canvas.Result
	correlationID string
}

func (c *canvases) Load(_ context.Context, key canvas.Key) (canvas.Canvas, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked(key)
}

func (c *canvases) Apply(_ context.Context, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (canvas.Result, error) {
	if err := canvas.Validate(key, operation, mutation); err != nil {
		return canvas.Result{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.failNext; err != nil {
		c.failNext = nil
		return canvas.Result{}, err
	}
	fingerprint := canvas.Fingerprint(mutation)
	if retained, ok := c.operations[operation.ID]; ok {
		if retained.fingerprint != fingerprint {
			return canvas.Result{}, canvas.ErrOperationConflict
		}
		return retained.result, nil
	}
	current, err := c.loadLocked(key)
	if err != nil {
		return canvas.Result{}, err
	}
	resize, ok := mutation.(canvas.SetDimensions)
	if !ok {
		return canvas.Result{}, fmt.Errorf("fake canvases: unsupported mutation %T", mutation)
	}
	current.Dimensions = resize.Dimensions
	current.Revision++
	if c.applied == nil {
		c.applied = map[canvas.Key]canvas.Canvas{}
		c.operations = map[string]appliedOperation{}
	}
	c.applied[key] = current
	result := canvas.Result{Revision: current.Revision}
	c.operations[operation.ID] = appliedOperation{fingerprint: fingerprint, result: result, correlationID: operation.CorrelationID}
	c.effects++
	return result, nil
}

// Applies counts the mutations that took effect.
func (c *canvases) Applies() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effects
}

// Correlation returns the correlation ID retained with an operation.
func (c *canvases) Correlation(operationID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operations[operationID].correlationID
}

// FailNext makes the next Apply fail with err.
func (c *canvases) FailNext(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext = err
}

func (c *canvases) loadLocked(key canvas.Key) (canvas.Canvas, error) {
	if changed, ok := c.applied[key]; ok {
		return changed, nil
	}
	if dimensions, ok := c.activated.Canvases()[key]; ok {
		return canvas.Canvas{Key: key, Dimensions: dimensions}, nil
	}
	found, ok := c.byID[key]
	if !ok {
		return canvas.Canvas{}, canvas.ErrNotFound
	}
	return found, nil
}

// memberships reads the owner memberships activations created, as the
// PostgreSQL memberships and tenants tables do, then the seeded ones.
type memberships struct {
	activated *subscriptiontest.MemoryStore
	seeded    access.MembershipStore
}

func (m memberships) MembershipOf(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, bool, error) {
	if role, ok := m.activated.Membership(tenantID, account); ok {
		for _, tenant := range m.activated.Tenants() {
			if tenant.ID == tenantID {
				return access.Membership{TenantID: tenantID, Role: role, Subscription: tenant.Status}, true, nil
			}
		}
	}
	return m.seeded.MembershipOf(ctx, account, tenantID)
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

type env struct {
	t        *testing.T
	faults   *faults
	store    *accesstest.MemoryStore
	hasher   *accesstest.PlainHasher
	clock    *accesstest.Clock
	canvases *canvases
	subs     *subscriptiontest.MemoryStore
	logs     *syncBuffer
	handler  http.Handler
	bodies   []string
}

func newEnv(t *testing.T, limits httpapi.Limits) *env {
	t.Helper()
	e := &env{
		t:      t,
		store:  accesstest.NewMemoryStore(),
		hasher: &accesstest.PlainHasher{},
		clock:  accesstest.NewClock(),
		faults: &faults{},
		subs:   subscriptiontest.NewMemoryStore(),
		logs:   &syncBuffer{},
	}
	e.canvases = &canvases{byID: map[canvas.Key]canvas.Canvas{}, activated: e.subs}
	svc, err := access.NewService(e.store, e.store, e.hasher, access.WithClock(e.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(httpapi.Config{
		Accounts:      faultyAccounts{Accounts: svc, faults: e.faults},
		Memberships:   memberships{activated: e.subs, seeded: e.store},
		Canvases:      e.canvases,
		Subscriptions: faultySubscriptions{Subscriptions: subscription.NewService(e.subs, subscription.WithClock(e.clock.Now)), faults: e.faults},
		AllowedOrigin: origin,
		Logger:        slog.New(logging.NewHandler(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		Now:           e.clock.Now,
		Limits:        limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.handler = handler
	return e
}

type option func(*http.Request)

func withOrigin(o string) option { return func(r *http.Request) { r.Header.Set("Origin", o) } }
func withoutOrigin() option      { return func(r *http.Request) { r.Header.Del("Origin") } }
func fromIP(ip string) option    { return func(r *http.Request) { r.RemoteAddr = ip + ":40000" } }
func withCSRF(v string) option   { return func(r *http.Request) { r.Header.Set(httpapi.CSRFHeader, v) } }
func withBody(contentType, body string) option {
	return func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", contentType)
	}
}
func withSession(token string) option {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: token}) }
}

func (e *env) do(method, path string, body any, options ...option) *http.Response {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, reader)
	r.RemoteAddr = clientIP + ":40000"
	r.Header.Set("Origin", origin)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range options {
		o(r)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	res := w.Result()
	raw, _ := io.ReadAll(res.Body)
	res.Body = io.NopCloser(bytes.NewReader(raw))
	e.bodies = append(e.bodies, string(raw))
	if res.Header.Get(httpx.CorrelationHeader) == "" {
		e.t.Fatalf("%s %s: missing correlation header", method, path)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		e.t.Fatalf("%s %s: Cache-Control = %q, want no-store", method, path, got)
	}
	return res
}

func decode(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	res.Body = io.NopCloser(strings.NewReader(""))
	return body
}

func expectError(t *testing.T, res *http.Response, status int, code string) {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d", res.StatusCode, status)
	}
	body := decode(t, res)
	if body["error"] != code {
		t.Fatalf("error = %v, want %q", body["error"], code)
	}
	if body["correlation_id"] != res.Header.Get(httpx.CorrelationHeader) {
		t.Fatalf("correlation_id %v does not match the header", body["correlation_id"])
	}
	if len(body) != 2 {
		t.Fatalf("error body has extra fields: %v", body)
	}
}

func registration(op, email, pass string) map[string]string {
	return map[string]string{"operation_id": op, "email": email, "password": pass}
}

func credentials(email, pass string) map[string]string {
	return map[string]string{"email": email, "password": pass}
}

func (e *env) register(op, email string) access.AccountID {
	e.t.Helper()
	res := e.do(http.MethodPost, "/api/accounts", registration(op, email, password))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("register %s: status %d", email, res.StatusCode)
	}
	id := access.AccountID(decode(e.t, res)["account_id"].(string))
	// Both stores read one accounts table in PostgreSQL.
	normalized, _ := access.NormalizeEmail(email)
	e.subs.AddAccount(id, normalized)
	return id
}

type signedIn struct {
	token string
	csrf  string
}

func (e *env) signIn(email string, options ...option) signedIn {
	e.t.Helper()
	res := e.do(http.MethodPost, "/api/sessions", credentials(email, password), options...)
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("sign in %s: status %d", email, res.StatusCode)
	}
	var token string
	for _, c := range res.Cookies() {
		if c.Name == httpapi.SessionCookie {
			token = c.Value
		}
	}
	body := decode(e.t, res)
	return signedIn{token: token, csrf: body["csrf_token"].(string)}
}

func sessionCookie(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == httpapi.SessionCookie {
			return c
		}
	}
	return nil
}

func TestRegisterThenSignIn(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	id := e.register("op-http-register-01", "Alice@Example.com")

	res := e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sign in status = %d", res.StatusCode)
	}
	cookie := sessionCookie(res)
	if cookie == nil || !strings.HasPrefix(cookie.Value, "pcs_") {
		t.Fatal("sign in did not set the session cookie")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("cookie attributes = %+v", cookie)
	}
	if cookie.MaxAge != int(access.SessionAbsoluteLifetime/time.Second) {
		t.Fatalf("cookie Max-Age = %d", cookie.MaxAge)
	}
	body := decode(t, res)
	if body["account_id"] != string(id) || body["email"] != "alice@example.com" || body["csrf_token"] == "" {
		t.Fatalf("sign in body = %v", body)
	}
	if strings.Contains(e.bodies[len(e.bodies)-1], cookie.Value) {
		t.Fatal("the session token appears in the response body")
	}

	res = e.do(http.MethodGet, "/api/session", nil, withSession(cookie.Value), withoutOrigin())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/session = %d", res.StatusCode)
	}
	if got := decode(t, res); got["account_id"] != string(id) || got["csrf_token"] != body["csrf_token"] {
		t.Fatalf("session body = %v", got)
	}
}

func TestRegistrationResponses(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	res := e.do(http.MethodPost, "/api/accounts", registration("op-http-replay-0001", "alice@example.com", password))
	first := decode(t, res)
	if res.StatusCode != http.StatusCreated || first["account_id"] == "" || len(first) != 1 {
		t.Fatalf("register = %d %v", res.StatusCode, first)
	}

	// CTRL-IDEM-01: a retry after a timeout returns the same answer.
	res = e.do(http.MethodPost, "/api/accounts", registration("op-http-replay-0001", "alice@example.com", password))
	if again := decode(t, res); res.StatusCode != http.StatusCreated || again["account_id"] != first["account_id"] {
		t.Fatalf("retry = %d %v, want 201 %v", res.StatusCode, again, first)
	}
	expectError(t, e.do(http.MethodPost, "/api/accounts", registration("op-http-replay-0001", "bob@example.com", password)), http.StatusConflict, "conflict")
	expectError(t, e.do(http.MethodPost, "/api/accounts", registration("op-http-replay-0002", "ALICE@example.com", password)), http.StatusConflict, "identity_taken")
	if accounts, _ := e.store.Counts(); accounts != 1 {
		t.Fatalf("accounts = %d, want 1", accounts)
	}
}

func TestMalformedRequestsAreRejectedWithoutEffect(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	cases := map[string]func() *http.Response{
		"invalid email": func() *http.Response {
			return e.do(http.MethodPost, "/api/accounts", registration("op-http-bad-000001", "not-an-email", password))
		},
		"short password": func() *http.Response {
			return e.do(http.MethodPost, "/api/accounts", registration("op-http-bad-000002", "a@example.com", "short"))
		},
		"unknown field": func() *http.Response {
			return e.do(http.MethodPost, "/api/accounts", map[string]string{"operation_id": "op-http-bad-000003", "email": "a@example.com", "password": password, "role": "owner"})
		},
		"not JSON": func() *http.Response {
			r := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader("email=a@example.com"))
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.RemoteAddr = clientIP + ":1"
			w := httptest.NewRecorder()
			e.handler.ServeHTTP(w, r)
			return w.Result()
		},
		"oversized body": func() *http.Response {
			return e.do(http.MethodPost, "/api/accounts", registration("op-http-bad-000004", "a@example.com", strings.Repeat("x", 8192)))
		},
		"trailing data": func() *http.Response {
			r := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"operation_id":"op-http-bad-000005","email":"a@example.com","password":"`+password+`"} {}`))
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/json")
			r.RemoteAddr = clientIP + ":1"
			w := httptest.NewRecorder()
			e.handler.ServeHTTP(w, r)
			return w.Result()
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			expectError(t, run(), http.StatusBadRequest, "bad_request")
			if accounts, _ := e.store.Counts(); accounts != 0 {
				t.Fatalf("accounts = %d, want 0", accounts)
			}
		})
	}
}

func TestInvalidCredentialsLookTheSameAndCreateNoSession(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-invalid-01", "alice@example.com")

	wrong := e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong password!!"))
	unknown := e.do(http.MethodPost, "/api/sessions", credentials("nobody@example.com", password))
	for name, res := range map[string]*http.Response{"wrong password": wrong, "unknown email": unknown} {
		if sessionCookie(res) != nil {
			t.Fatalf("%s set a session cookie", name)
		}
		expectError(t, res, http.StatusUnauthorized, "unauthorized")
	}
	if _, sessions := e.store.Counts(); sessions != 0 {
		t.Fatalf("sessions = %d, want 0", sessions)
	}
}

func TestSessionLifecycle(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-lifecycle1", "alice@example.com")
	s := e.signIn("alice@example.com")

	expectError(t, e.do(http.MethodGet, "/api/session", nil), http.StatusUnauthorized, "unauthorized")

	// Expired: the server answers 401 and clears the cookie.
	e.clock.Advance(access.SessionIdleTimeout)
	res := e.do(http.MethodGet, "/api/session", nil, withSession(s.token))
	if c := sessionCookie(res); c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("expired session cookie not cleared: %+v", c)
	}
	expectError(t, res, http.StatusUnauthorized, "unauthorized")
}

func TestSignOutRequiresTheSessionCSRFToken(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-signout-01", "alice@example.com")
	alice := e.signIn("alice@example.com")
	other := e.signIn("alice@example.com")

	for name, opts := range map[string][]option{
		"missing CSRF":       {withSession(alice.token)},
		"invalid CSRF":       {withSession(alice.token), withCSRF("not-the-token")},
		"other session CSRF": {withSession(alice.token), withCSRF(other.csrf)},
	} {
		expectError(t, e.do(http.MethodDelete, "/api/session", nil, opts...), http.StatusForbidden, "forbidden")
		if res := e.do(http.MethodGet, "/api/session", nil, withSession(alice.token)); res.StatusCode != http.StatusOK {
			t.Fatalf("%s ended the session", name)
		}
	}

	res := e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token), withCSRF(alice.csrf))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out = %d", res.StatusCode)
	}
	if c := sessionCookie(res); c == nil || c.MaxAge >= 0 {
		t.Fatal("sign out did not clear the cookie")
	}
	expectError(t, e.do(http.MethodGet, "/api/session", nil, withSession(alice.token)), http.StatusUnauthorized, "unauthorized")
	// Repeating the sign-out after the session ended succeeds.
	if res := e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("repeated sign out = %d", res.StatusCode)
	}
	if res := e.do(http.MethodGet, "/api/session", nil, withSession(other.token)); res.StatusCode != http.StatusOK {
		t.Fatal("signing out one session ended another")
	}
}

func TestSignInReplacesThePresentedSession(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-replace-01", "alice@example.com")
	e.register("op-http-replace-02", "bob@example.com")
	first := e.signIn("alice@example.com")

	second := e.signIn("alice@example.com", withSession(first.token))
	expectError(t, e.do(http.MethodGet, "/api/session", nil, withSession(first.token)), http.StatusUnauthorized, "unauthorized")
	if res := e.do(http.MethodGet, "/api/session", nil, withSession(second.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("new session = %d", res.StatusCode)
	}
	// Signing in as another account also ends the presented session.
	e.signIn("bob@example.com", withSession(second.token))
	expectError(t, e.do(http.MethodGet, "/api/session", nil, withSession(second.token)), http.StatusUnauthorized, "unauthorized")
	// A failed sign-in keeps the presented session.
	third := e.signIn("alice@example.com")
	expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong password!!"), withSession(third.token)), http.StatusUnauthorized, "unauthorized")
	if res := e.do(http.MethodGet, "/api/session", nil, withSession(third.token)); res.StatusCode != http.StatusOK {
		t.Fatal("a failed sign-in ended the presented session")
	}
}

func TestOriginIsCheckedBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-origin-001", "alice@example.com")
	alice := e.signIn("alice@example.com")

	for name, o := range map[string]option{
		"absent":         withoutOrigin(),
		"hostile":        withOrigin("https://evil.test"),
		"other scheme":   withOrigin("http://pepicanvas.test"),
		"suffix":         withOrigin(origin + ".evil.test"),
		"null":           withOrigin("null"),
		"trailing slash": withOrigin(origin + "/"),
	} {
		t.Run(name, func(t *testing.T) {
			verifications := e.hasher.Verifications()
			accounts, sessions := e.store.Counts()
			expectError(t, e.do(http.MethodPost, "/api/accounts", registration("op-http-origin-x01", "mallory@example.com", password), o), http.StatusForbidden, "forbidden")
			expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password), o), http.StatusForbidden, "forbidden")
			expectError(t, e.do(http.MethodDelete, "/api/session", nil, o, withSession(alice.token), withCSRF(alice.csrf)), http.StatusForbidden, "forbidden")
			if e.hasher.Verifications() != verifications {
				t.Fatal("a rejected origin reached password verification")
			}
			if a, s := e.store.Counts(); a != accounts || s != sessions {
				t.Fatalf("a rejected origin changed storage: accounts %d->%d sessions %d->%d", accounts, a, sessions, s)
			}
		})
	}
	if res := e.do(http.MethodGet, "/api/session", nil, withSession(alice.token)); res.StatusCode != http.StatusOK {
		t.Fatal("a rejected sign-out ended the session")
	}
}

func TestCanvasRequestsRequireAMembership(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	owner := e.register("op-http-canvas-001", "owner@example.com")
	moderator := e.register("op-http-canvas-002", "moderator@example.com")
	inactive := e.register("op-http-canvas-003", "inactive@example.com")
	e.register("op-http-canvas-004", "nobody@example.com")
	ownerB := e.register("op-http-canvas-005", "owner-b@example.com")
	active := access.SubscriptionActive
	e.store.SetMembership(owner, access.Membership{TenantID: "tenant-a", Role: access.RoleOwner, Subscription: active})
	e.store.SetMembership(moderator, access.Membership{TenantID: "tenant-a", Role: access.RoleModerator, Subscription: active})
	e.store.SetMembership(inactive, access.Membership{TenantID: "tenant-c", Role: access.RoleOwner, Subscription: access.SubscriptionInactive})
	e.store.SetMembership(ownerB, access.Membership{TenantID: "tenant-b", Role: access.RoleOwner, Subscription: active})
	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-c"} {
		key := canvas.Key{TenantID: tenant, CanvasID: "main"}
		e.canvases.byID[key] = canvas.Canvas{Key: key, Dimensions: canvas.DefaultDimensions, Revision: 3, Objects: []canvas.Object{
			{ID: "obj-" + tenant, Kind: canvas.KindText, Content: "content of " + tenant, Transform: canvas.Transform{Width: 10, Height: 10}, Z: 1},
		}}
	}

	get := func(email, path string) *http.Response {
		s := e.signIn(email)
		return e.do(http.MethodGet, path, nil, withSession(s.token))
	}

	for _, email := range []string{"owner@example.com", "moderator@example.com"} {
		res := get(email, "/api/tenants/tenant-a/canvases/main")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", email, res.StatusCode)
		}
		body := e.bodies[len(e.bodies)-1]
		if !strings.Contains(body, "content of tenant-a") || strings.Contains(body, "tenant-b") {
			t.Fatalf("%s: canvas body %s", email, body)
		}
		got := decode(t, res)
		if got["width"] != float64(1920) || got["height"] != float64(1080) || got["revision"] != float64(3) {
			t.Fatalf("canvas body = %v", got)
		}
	}

	// Denials are identical whether the canvas exists elsewhere or nowhere.
	denied := map[string]*http.Response{
		"no membership":        get("nobody@example.com", "/api/tenants/tenant-a/canvases/main"),
		"inactive tenant":      get("inactive@example.com", "/api/tenants/tenant-c/canvases/main"),
		"other tenant":         get("owner@example.com", "/api/tenants/tenant-b/canvases/main"),
		"missing tenant":       get("owner@example.com", "/api/tenants/tenant-zz/canvases/main"),
		"missing canvas":       get("owner@example.com", "/api/tenants/tenant-a/canvases/other"),
		"tenant b reads a":     get("owner-b@example.com", "/api/tenants/tenant-a/canvases/main"),
		"no membership in b":   get("nobody@example.com", "/api/tenants/tenant-b/canvases/main"),
		"oversized identifier": get("owner@example.com", "/api/tenants/"+strings.Repeat("t", 200)+"/canvases/main"),
	}
	for name, res := range denied {
		t.Run(name, func(t *testing.T) {
			expectError(t, res, http.StatusForbidden, "forbidden")
		})
	}
	expectError(t, e.do(http.MethodGet, "/api/tenants/tenant-a/canvases/main", nil), http.StatusUnauthorized, "unauthorized")
}

func TestSignInRateLimitPerAccountAndIP(t *testing.T) {
	limits := httpapi.Limits{
		SignInPerAccount: httpapi.Rate{Limit: 3, Window: time.Minute},
		SignInPerIP:      httpapi.Rate{Limit: 5, Window: time.Minute},
		RegisterPerIP:    httpapi.Rate{Limit: 100, Window: time.Minute},
	}
	e := newEnv(t, limits)
	e.register("op-http-rate-00001", "alice@example.com")
	e.register("op-http-rate-00002", "bob@example.com")

	for range 3 {
		expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong password!!"), fromIP("198.51.100.1")), http.StatusUnauthorized, "unauthorized")
	}
	verifications := e.hasher.Verifications()
	// The account limit holds even from another address, and even with the right password.
	res := e.do(http.MethodPost, "/api/sessions", credentials("ALICE@example.com", password), fromIP("198.51.100.2"))
	if got := res.Header.Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	if e.hasher.Verifications() != verifications {
		t.Fatal("a rate-limited attempt reached password verification")
	}
	// Another account is independent.
	e.signIn("bob@example.com", fromIP("198.51.100.3"))

	// Recovery after the window.
	e.clock.Advance(time.Minute)
	e.signIn("alice@example.com", fromIP("198.51.100.4"))

	// The per-IP limit covers attempts across accounts from one address.
	for i := range 5 {
		email := "user" + strconv.Itoa(i) + "@example.com"
		expectError(t, e.do(http.MethodPost, "/api/sessions", credentials(email, password), fromIP("203.0.113.9")), http.StatusUnauthorized, "unauthorized")
	}
	expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("bob@example.com", password), fromIP("203.0.113.9")), http.StatusTooManyRequests, "rate_limited")
	e.signIn("bob@example.com", fromIP("203.0.113.10"))
}

func TestRegistrationRateLimitPerIP(t *testing.T) {
	limits := httpapi.DefaultLimits
	limits.RegisterPerIP = httpapi.Rate{Limit: 2, Window: time.Hour}
	e := newEnv(t, limits)
	e.do(http.MethodPost, "/api/accounts", registration("op-http-regrate-01", "a@example.com", password), fromIP("198.51.100.7"))
	e.do(http.MethodPost, "/api/accounts", registration("op-http-regrate-02", "b@example.com", password), fromIP("198.51.100.7"))
	res := e.do(http.MethodPost, "/api/accounts", registration("op-http-regrate-03", "c@example.com", password), fromIP("198.51.100.7"))
	if res.Header.Get("Retry-After") != "3600" {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	if accounts, _ := e.store.Counts(); accounts != 2 {
		t.Fatalf("accounts = %d, want 2", accounts)
	}
	if res := e.do(http.MethodPost, "/api/accounts", registration("op-http-regrate-04", "d@example.com", password), fromIP("198.51.100.8")); res.StatusCode != http.StatusCreated {
		t.Fatalf("another address = %d", res.StatusCode)
	}
	e.clock.Advance(time.Hour)
	if res := e.do(http.MethodPost, "/api/accounts", registration("op-http-regrate-05", "e@example.com", password), fromIP("198.51.100.7")); res.StatusCode != http.StatusCreated {
		t.Fatalf("after the window = %d", res.StatusCode)
	}
}

func TestDefaultLimits(t *testing.T) {
	want := httpapi.Limits{
		SignInPerAccount: httpapi.Rate{Limit: 10, Window: 15 * time.Minute},
		SignInPerIP:      httpapi.Rate{Limit: 50, Window: 15 * time.Minute},
		RegisterPerIP:    httpapi.Rate{Limit: 10, Window: time.Hour},
	}
	if httpapi.DefaultLimits != want {
		t.Fatalf("DefaultLimits = %+v", httpapi.DefaultLimits)
	}
}

func TestConfigurationRejectsAnInexactOrigin(t *testing.T) {
	for _, o := range []string{"", "*", "pepicanvas.test", "https://pepicanvas.test/", "https://pepicanvas.test/app", "https://user@pepicanvas.test", "ftp://pepicanvas.test"} {
		_, err := httpapi.New(httpapi.Config{AllowedOrigin: o, Limits: httpapi.DefaultLimits})
		if err == nil {
			t.Fatalf("AllowedOrigin %q accepted", o)
		}
	}
}

func TestSecretsNeverReachLogsOrErrorBodies(t *testing.T) {
	e := newEnv(t, httpapi.Limits{
		SignInPerAccount: httpapi.Rate{Limit: 2, Window: time.Minute},
		SignInPerIP:      httpapi.Rate{Limit: 100, Window: time.Minute},
		RegisterPerIP:    httpapi.Rate{Limit: 100, Window: time.Minute},
	})
	e.register("op-http-secrets-01", "alice@example.com")
	alice := e.signIn("alice@example.com")
	e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong-"+password))
	e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong-"+password))
	e.do(http.MethodPost, "/api/accounts", registration("op-http-secrets-01", "bob@example.com", password))
	e.do(http.MethodPost, "/api/accounts", registration("op-http-secrets-02", "alice@example.com", password))
	e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token), withCSRF("x"))
	e.do(http.MethodGet, "/api/tenants/tenant-a/canvases/main", nil, withSession(alice.token))
	e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password), withOrigin("https://evil.test"))
	e.do(http.MethodGet, "/api/session", nil, withSession(alice.token))

	logs := e.logs.String()
	if !strings.Contains(logs, `"correlation_id"`) {
		t.Fatal("logs carry no correlation ID")
	}
	for name, s := range map[string]string{
		"password":      password,
		"session token": alice.token,
		"CSRF token":    alice.csrf,
		"email":         "alice@example.com",
		"other email":   "bob@example.com",
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
	for _, body := range e.bodies {
		for name, s := range map[string]string{"password": password, "session token": alice.token, "other email": "bob@example.com"} {
			if strings.Contains(body, s) {
				t.Errorf("a response body contains the %s: %s", name, body)
			}
		}
	}
}

func TestRegistrationRetainsTheRequestCorrelation(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	res := e.do(http.MethodPost, "/api/accounts", registration("op-http-correlate1", "alice@example.com", password))
	if got := e.store.Correlation("op-http-correlate1"); got == "" || got != res.Header.Get(httpx.CorrelationHeader) {
		t.Fatalf("retained correlation %q, response %q", got, res.Header.Get(httpx.CorrelationHeader))
	}
}
