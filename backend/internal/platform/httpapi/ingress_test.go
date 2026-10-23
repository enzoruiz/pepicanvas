package httpapi_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// effects is every domain and storage effect a rejected request must leave
// unchanged.
type effects struct {
	calls   map[string]int
	storage storage
}

type storage struct {
	accounts      int
	sessions      int
	verifications int
	transactions  int
	records       int
}

func (e *env) effects() effects {
	accounts, sessions := e.store.Counts()
	return effects{calls: e.calls.snapshot(), storage: storage{
		accounts:      accounts,
		sessions:      sessions,
		verifications: e.hasher.Verifications(),
		transactions:  e.subs.Transactions(),
		records:       len(e.subs.Records()),
	}}
}

func (e *env) expectNoEffect(name string, before effects) {
	e.t.Helper()
	after := e.effects()
	if !maps.Equal(before.calls, after.calls) {
		e.t.Fatalf("%s reached the domain: calls %v -> %v", name, before.calls, after.calls)
	}
	if before.storage != after.storage {
		e.t.Fatalf("%s changed storage: %+v -> %+v", name, before.storage, after.storage)
	}
}

func (e *env) callsTo(name string) int {
	return e.calls.snapshot()[name]
}

func reasonCount(in *ingress.Ingress, reason ingress.Reason) uint64 {
	for _, c := range in.Snapshot() {
		if c.Surface == ingress.SurfaceHTTP && c.Reason == reason {
			return c.Value
		}
	}
	return 0
}

// expectSafeLogs checks that every rejection line carries the correlation ID
// and that no secret or email reached the logs.
func expectSafeLogs(t *testing.T, logs string, secrets map[string]string) {
	t.Helper()
	rejections := 0
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["msg"] != "request failed" {
			continue
		}
		rejections++
		if id, _ := record["correlation_id"].(string); id == "" {
			t.Errorf("rejection log without correlation_id: %s", line)
		}
	}
	if rejections == 0 {
		t.Fatal("no rejection was logged")
	}
	for name, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

// S4, S5, CTRL-WEB-01: every unsafe route refuses an absent or hostile Origin,
// and every session route a missing, invalid, or other-session CSRF token,
// before the domain runs or storage changes.
func TestEveryUnsafeRouteChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-web-oper01", "operator@example.com")
	other := e.signIn("operator@example.com")
	e.register("op-http-web-alice1", "alice@example.com")
	leaver := e.signIn("alice@example.com")

	type route struct {
		name, method, path string
		body               any
		session            *signedIn
		status             int
		effect             string
	}
	routes := []route{
		{"register", http.MethodPost, "/api/accounts", registration("op-http-web-reg001", "bob@example.com", password), nil, http.StatusCreated, "Register"},
		{"sign-in", http.MethodPost, "/api/sessions", credentials("alice@example.com", password), nil, http.StatusCreated, "SignIn"},
		{"activation", http.MethodPost, "/api/activations", activation("op-http-web-act001", "alice@example.com"), &op, http.StatusOK, "Activate"},
		{"sign-out", http.MethodDelete, "/api/session", nil, &leaver, http.StatusNoContent, "SignOut"},
	}
	origins := map[string]option{
		"absent Origin":       withoutOrigin(),
		"hostile Origin":      withOrigin("https://evil.test"),
		"null Origin":         withOrigin("null"),
		"other scheme Origin": withOrigin("http://pepicanvas.test"),
		"suffix Origin":       withOrigin(origin + ".evil.test"),
	}
	for _, rt := range routes {
		var auth []option
		if rt.session != nil {
			auth = []option{withSession(rt.session.token), withCSRF(rt.session.csrf)}
		}
		cases := map[string][]option{}
		for name, o := range origins {
			cases[name] = append(append([]option{}, auth...), o)
		}
		if rt.session != nil {
			cases["missing CSRF"] = []option{withSession(rt.session.token)}
			cases["invalid CSRF"] = []option{withSession(rt.session.token), withCSRF("not-the-token")}
			cases["other session CSRF"] = []option{withSession(rt.session.token), withCSRF(other.csrf)}
		}
		for name, opts := range cases {
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				before := e.effects()
				expectError(t, e.do(rt.method, rt.path, rt.body, opts...), http.StatusForbidden, "forbidden")
				e.expectNoEffect(rt.name+" with "+name, before)
			})
		}
		t.Run(rt.name+"/allowed Origin and valid CSRF", func(t *testing.T) {
			before := e.callsTo(rt.effect)
			if res := e.do(rt.method, rt.path, rt.body, auth...); res.StatusCode != rt.status {
				t.Fatalf("status = %d, want %d: %s", res.StatusCode, rt.status, e.bodies[len(e.bodies)-1])
			}
			if got := e.callsTo(rt.effect); got != before+1 {
				t.Fatalf("%s calls = %d, want %d", rt.effect, got, before+1)
			}
		})
	}
	if res := e.do(http.MethodGet, "/api/session", nil, withSession(leaver.token)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the accepted sign-out kept the session: %d", res.StatusCode)
	}
}

// L2b: the CSRF token is checked against the cookie before Authenticate, which
// reads and may delete sessions, so a stale session without its token is
// refused with 403 and the session store is untouched.
func TestStaleSessionWithoutItsCSRFTokenIsRefusedBeforeAuthenticate(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-stale-op01", "operator@example.com")
	e.register("op-http-stale-al01", "alice@example.com")
	alice := e.signIn("alice@example.com")
	e.clock.Advance(access.SessionIdleTimeout)

	for name, c := range map[string]struct {
		method, path string
		body         any
		opts         []option
	}{
		"sign-out, missing CSRF":   {http.MethodDelete, "/api/session", nil, []option{withSession(alice.token)}},
		"sign-out, invalid CSRF":   {http.MethodDelete, "/api/session", nil, []option{withSession(alice.token), withCSRF("not-the-token")}},
		"activation, missing CSRF": {http.MethodPost, "/api/activations", activation("op-http-stale-ac01", "alice@example.com"), []option{withSession(op.token)}},
		"activation, other CSRF":   {http.MethodPost, "/api/activations", activation("op-http-stale-ac01", "alice@example.com"), []option{withSession(op.token), withCSRF(alice.csrf)}},
	} {
		t.Run(name, func(t *testing.T) {
			before := e.effects()
			res := e.do(c.method, c.path, c.body, c.opts...)
			if sessionCookie(res) != nil {
				t.Fatal("a refused request touched the session cookie")
			}
			expectError(t, res, http.StatusForbidden, "forbidden")
			e.expectNoEffect(name, before)
		})
	}

	// With the session's own token the ended session keeps its answers.
	if res := e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token), withCSRF(alice.csrf)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-out of an ended session = %d, want 204", res.StatusCode)
	}
	expectError(t, e.do(http.MethodPost, "/api/activations", activation("op-http-stale-ac02", "alice@example.com"), withSession(op.token), withCSRF(op.csrf)), http.StatusUnauthorized, "unauthorized")
}

// L2c: CORS stays closed. Every preflight is refused before routing and no
// answer carries an Access-Control-* header.
func TestPreflightsAreRefusedWithoutCORSHeaders(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	for _, path := range []string{"/api/accounts", "/api/sessions", "/api/session", "/api/activations", "/api/canvas", "/api/tenants/t/canvases/c", "/api/unknown"} {
		t.Run(path, func(t *testing.T) {
			before := e.effects()
			res := e.do(http.MethodOptions, path, nil, withOrigin(origin), func(r *http.Request) {
				r.Header.Set("Access-Control-Request-Method", http.MethodPost)
				r.Header.Set("Access-Control-Request-Headers", "content-type, x-csrf-token")
			})
			expectError(t, res, http.StatusForbidden, "forbidden")
			e.expectNoEffect("a preflight", before)
		})
	}
	if got := reasonCount(e.ingress, ingress.ReasonPreflight); got != 7 {
		t.Fatalf("preflight rejections = %d, want 7", got)
	}
}

// S1: API answers, accepted or refused, carry the ingress security headers.
func TestAPIAnswersCarryTheSecurityHeaders(t *testing.T) {
	limits := httpapi.DefaultLimits
	limits.RegisterPerIP = ratelimit.Rate{Limit: 1, Window: time.Hour}
	e := newEnv(t, limits)
	e.register("op-http-headers-01", "alice@example.com")
	alice := e.signIn("alice@example.com")

	answers := map[string]*http.Response{
		"session 200":      e.do(http.MethodGet, "/api/session", nil, withSession(alice.token)),
		"sign-in 201":      e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password)),
		"sign-out 204":     e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token), withCSRF(alice.csrf)),
		"origin 403":       e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password), withOrigin("https://evil.test")),
		"csrf 403":         e.do(http.MethodDelete, "/api/session", nil, withSession(alice.token)),
		"unauthorized 401": e.do(http.MethodGet, "/api/canvas", nil),
		"rate limit 429":   e.do(http.MethodPost, "/api/accounts", registration("op-http-headers-02", "bob@example.com", password)),
		"bad request 400":  e.do(http.MethodPost, "/api/sessions", nil, withBody("text/plain", "x")),
		"not found 404":    e.do(http.MethodGet, "/api/unknown", nil),
		"method 405":       e.do(http.MethodPut, "/api/session", nil),
	}
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Strict-Transport-Security":    "max-age=31536000",
		"Cache-Control":                "no-store",
		"Referrer-Policy":              "no-referrer",
	}
	for name, res := range answers {
		for k, v := range want {
			if got := res.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
	}
}

// S7: one streamer spending its session budget does not limit another
// streamer's session from the same address; the address budget is the one
// declared shared dimension.
func TestOneStreamersMutationBudgetLeavesAnotherUnaffected(t *testing.T) {
	limits := ingress.DefaultLimits
	limits.UnsafePerIP = ratelimit.Rate{Limit: 6, Window: time.Minute}
	limits.MutationsPerSession = ratelimit.Rate{Limit: 2, Window: time.Minute}
	e := newEnvWith(t, httpapi.DefaultLimits, limits)
	e.register("op-http-iso-alice1", "alice@example.com", fromIP("198.51.100.20"))
	e.register("op-http-iso-bob001", "bob@example.com", fromIP("198.51.100.21"))

	const shared = "203.0.113.50"
	alice := e.signIn("alice@example.com", fromIP(shared))
	bob := e.signIn("bob@example.com", fromIP(shared))
	mutate := func(s signedIn, op string) *http.Response {
		return e.do(http.MethodPost, "/api/activations", activation(op, "alice@example.com"), withSession(s.token), withCSRF(s.csrf), fromIP(shared))
	}

	// Alice spends her session budget; the domain denies a non-operator.
	expectError(t, mutate(alice, "op-http-iso-act001"), http.StatusForbidden, "forbidden")
	expectError(t, mutate(alice, "op-http-iso-act002"), http.StatusForbidden, "forbidden")
	before := e.effects()
	res := mutate(alice, "op-http-iso-act003")
	if got := res.Header.Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	e.expectNoEffect("a session-limited mutation", before)

	// Bob's session from the same address still reaches the domain.
	activations := e.callsTo("Activate")
	expectError(t, mutate(bob, "op-http-iso-act004"), http.StatusForbidden, "forbidden")
	if got := e.callsTo("Activate"); got != activations+1 {
		t.Fatal("another streamer's mutation was limited by the first streamer's session budget")
	}

	// The declared shared dimension: the address budget is spent by both.
	before = e.effects()
	expectError(t, mutate(bob, "op-http-iso-act005"), http.StatusTooManyRequests, "rate_limited")
	e.expectNoEffect("an address-limited mutation", before)
	if res := e.do(http.MethodDelete, "/api/session", nil, withSession(bob.token), withCSRF(bob.csrf), fromIP("198.51.100.22")); res.StatusCode != http.StatusNoContent {
		t.Fatalf("bob from another address = %d, want 204", res.StatusCode)
	}
	if s, ip := reasonCount(e.ingress, ingress.ReasonRateSession), reasonCount(e.ingress, ingress.ReasonRateIP); s != 1 || ip != 1 {
		t.Fatalf("rate_session = %d, rate_ip = %d, want 1 and 1", s, ip)
	}
	expectSafeLogs(t, e.logs.String(), map[string]string{
		"password": password, "alice token": alice.token, "alice CSRF": alice.csrf, "bob token": bob.token,
		"bob CSRF": bob.csrf, "alice email": "alice@example.com", "bob email": "bob@example.com",
	})
}

// S7: one account's sign-in limit leaves another account unaffected, even from
// the same address.
func TestOneStreamersSignInLimitLeavesAnotherUnaffected(t *testing.T) {
	e := newEnv(t, httpapi.Limits{
		SignInPerAccount: ratelimit.Rate{Limit: 2, Window: time.Minute},
		SignInPerIP:      ratelimit.Rate{Limit: 100, Window: time.Minute},
		RegisterPerIP:    ratelimit.Rate{Limit: 100, Window: time.Minute},
	})
	e.register("op-http-iso-alice2", "alice@example.com")
	e.register("op-http-iso-bob002", "bob@example.com")
	const shared = "203.0.113.51"

	for range 2 {
		expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", "wrong-"+password), fromIP(shared)), http.StatusUnauthorized, "unauthorized")
	}
	before := e.effects()
	expectError(t, e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password), fromIP(shared)), http.StatusTooManyRequests, "rate_limited")
	e.expectNoEffect("an account-limited sign-in", before)
	bob := e.signIn("bob@example.com", fromIP(shared))

	if a, ip := reasonCount(e.ingress, ingress.ReasonRateAccount), reasonCount(e.ingress, ingress.ReasonRateIP); a != 1 || ip != 0 {
		t.Fatalf("rate_account = %d, rate_ip = %d, want 1 and 0", a, ip)
	}
	expectSafeLogs(t, e.logs.String(), map[string]string{
		"password": password, "bob token": bob.token, "bob CSRF": bob.csrf,
		"alice email": "alice@example.com", "bob email": "bob@example.com",
	})
}

// S9, CTRL-IDEM-01: an activation the ingress refuses records no operation, so
// a retry under the same operation ID has exactly one effect and replays it.
func TestActivationRefusedByTheIngressLeavesNoOperation(t *testing.T) {
	limits := ingress.DefaultLimits
	limits.MutationsPerSession = ratelimit.Rate{Limit: 2, Window: time.Minute}
	e := newEnvWith(t, httpapi.DefaultLimits, limits)
	op := e.operator("op-http-idem-oper1", "operator@example.com")
	spent := e.signIn("operator@example.com")
	alice := e.register("op-http-idem-alice", "alice@example.com")
	bob := e.register("op-http-idem-bob01", "bob@example.com")
	const id = "op-http-idem-act01"

	for range 2 {
		expectError(t, e.do(http.MethodPost, "/api/activations", nil, withSession(spent.token), withCSRF(spent.csrf), withBody("application/json", "{")), http.StatusBadRequest, "bad_request")
	}
	for _, c := range []struct {
		name   string
		opts   []option
		status int
		code   string
	}{
		{"hostile Origin", []option{withSession(op.token), withCSRF(op.csrf), withOrigin("https://evil.test")}, http.StatusForbidden, "forbidden"},
		{"absent Origin", []option{withSession(op.token), withCSRF(op.csrf), withoutOrigin()}, http.StatusForbidden, "forbidden"},
		{"missing CSRF", []option{withSession(op.token)}, http.StatusForbidden, "forbidden"},
		{"invalid CSRF", []option{withSession(op.token), withCSRF("not-the-token")}, http.StatusForbidden, "forbidden"},
		{"session limit", []option{withSession(spent.token), withCSRF(spent.csrf)}, http.StatusTooManyRequests, "rate_limited"},
	} {
		before := e.effects()
		expectError(t, e.do(http.MethodPost, "/api/activations", activation(id, "alice@example.com"), c.opts...), c.status, c.code)
		e.expectNoEffect(c.name, before)
		if _, ok := e.subs.Records()[id]; ok {
			t.Fatalf("%s recorded the operation", c.name)
		}
	}

	transactions := e.subs.Transactions()
	first := e.activate(op, id, "alice@example.com")
	firstRaw := e.bodies[len(e.bodies)-1]
	if first["account_id"] != string(alice) || first["created"] != true {
		t.Fatalf("activation = %v", first)
	}
	e.activate(op, id, "alice@example.com")
	if again := e.bodies[len(e.bodies)-1]; again != firstRaw {
		t.Fatalf("replay body = %s, want %s", again, firstRaw)
	}
	if n := len(e.subs.Tenants()); n != 1 {
		t.Fatalf("tenants = %d, want 1", n)
	}
	if got := e.subs.Transactions(); got != transactions+2 {
		t.Fatalf("transactions = %d, want %d", got, transactions+2)
	}

	e.clock.Advance(time.Minute) // the session budget recovers
	expectError(t, e.do(http.MethodPost, "/api/activations", activation(id, "bob@example.com"), withSession(op.token), withCSRF(op.csrf)), http.StatusConflict, "conflict")

	// After the retention the operation ID starts a new operation.
	e.clock.Advance(subscription.ActivationRetention)
	if got := e.activate(op, id, "bob@example.com"); got["account_id"] != string(bob) || got["created"] != true {
		t.Fatalf("activation after the retention = %v", got)
	}
	if n := len(e.subs.Tenants()); n != 2 {
		t.Fatalf("tenants = %d, want 2", n)
	}
}

// S9, CTRL-IDEM-01: a registration the ingress refuses (Origin, address limit)
// records no operation. Registration is an anonymous route, so CSRF does not
// apply.
func TestRegistrationRefusedByTheIngressLeavesNoOperation(t *testing.T) {
	limits := httpapi.DefaultLimits
	limits.RegisterPerIP = ratelimit.Rate{Limit: 1, Window: time.Hour}
	e := newEnv(t, limits)
	const id = "op-http-idem-reg01"
	register := func(email, ip string, options ...option) *http.Response {
		return e.do(http.MethodPost, "/api/accounts", registration(id, email, password), append([]option{fromIP(ip)}, options...)...)
	}

	e.register("op-http-idem-carol", "carol@example.com", fromIP("198.51.100.30"))
	for name, res := range map[string]func() *http.Response{
		"hostile Origin": func() *http.Response {
			return register("alice@example.com", "198.51.100.31", withOrigin("https://evil.test"))
		},
		"absent Origin": func() *http.Response { return register("alice@example.com", "198.51.100.31", withoutOrigin()) },
		"address limit": func() *http.Response { return register("alice@example.com", "198.51.100.30") },
	} {
		before := e.effects()
		r := res()
		if r.StatusCode == http.StatusTooManyRequests {
			expectError(t, r, http.StatusTooManyRequests, "rate_limited")
		} else {
			expectError(t, r, http.StatusForbidden, "forbidden")
		}
		e.expectNoEffect(name, before)
		if e.store.Correlation(id) != "" {
			t.Fatalf("%s recorded the operation", name)
		}
	}

	res := register("alice@example.com", "198.51.100.32")
	firstRaw := e.bodies[len(e.bodies)-1]
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d", res.StatusCode)
	}
	if res := register("alice@example.com", "198.51.100.33"); res.StatusCode != http.StatusCreated || e.bodies[len(e.bodies)-1] != firstRaw {
		t.Fatalf("replay = %d %s, want 201 %s", res.StatusCode, e.bodies[len(e.bodies)-1], firstRaw)
	}
	if accounts, _ := e.store.Counts(); accounts != 2 {
		t.Fatalf("accounts = %d, want 2", accounts)
	}
	expectError(t, register("bob@example.com", "198.51.100.34"), http.StatusConflict, "conflict")

	e.clock.Advance(access.RegistrationRetention)
	if res := register("bob@example.com", "198.51.100.35"); res.StatusCode != http.StatusCreated || e.bodies[len(e.bodies)-1] == firstRaw {
		t.Fatalf("registration after the retention = %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	if accounts, _ := e.store.Counts(); accounts != 3 {
		t.Fatalf("accounts = %d, want 3", accounts)
	}
}
