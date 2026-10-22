package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
)

func activation(op, email string) map[string]string {
	return map[string]string{"operation_id": op, "email": email}
}

// operator registers an account, grants it the operator role, and signs it in.
func (e *env) operator(op, email string) signedIn {
	e.t.Helper()
	id := e.register(op, email)
	e.subs.GrantOperator(id)
	return e.signIn(email)
}

func (e *env) activate(s signedIn, op, email string) map[string]any {
	e.t.Helper()
	res := e.do(http.MethodPost, "/api/activations", activation(op, email), withSession(s.token), withCSRF(s.csrf))
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("activate %s: status %d %s", email, res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	return decode(e.t, res)
}

func TestOperatorActivatesAStreamer(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-act-oper01", "operator@example.com")
	alice := e.register("op-http-act-alice1", "alice@example.com")

	first := e.activate(op, "op-http-activate-01", "Alice@Example.com")
	firstRaw := e.bodies[len(e.bodies)-1]
	if first["account_id"] != string(alice) || first["created"] != true || len(first) != 4 {
		t.Fatalf("activation body = %v", first)
	}
	tenants := e.subs.Tenants()
	if len(tenants) != 1 || tenants[0].ID != first["tenant_id"] || tenants[0].CanvasID != first["canvas_id"] || tenants[0].Owner != alice {
		t.Fatalf("tenants = %+v, body %v", tenants, first)
	}

	// S8: a retry under the same operation ID returns the identical answer.
	e.activate(op, "op-http-activate-01", "alice@example.com")
	if again := e.bodies[len(e.bodies)-1]; again != firstRaw {
		t.Fatalf("replay body = %s, want %s", again, firstRaw)
	}
	// A different payload under the same operation ID is rejected.
	e.register("op-http-act-bob001", "bob@example.com")
	expectError(t, e.do(http.MethodPost, "/api/activations", activation("op-http-activate-01", "bob@example.com"), withSession(op.token), withCSRF(op.csrf)), http.StatusConflict, "conflict")
	// S2: a new operation ID for an active account keeps its tenant and canvas.
	repeat := e.activate(op, "op-http-activate-02", "alice@example.com")
	if repeat["tenant_id"] != first["tenant_id"] || repeat["canvas_id"] != first["canvas_id"] || repeat["created"] != false {
		t.Fatalf("repeated activation = %v, first %v", repeat, first)
	}
	if n := len(e.subs.Tenants()); n != 1 {
		t.Fatalf("tenants = %d, want 1", n)
	}
	if n := len(e.subs.Canvases()); n != 1 {
		t.Fatalf("canvases = %d, want 1", n)
	}
}

func TestNonOperatorCannotActivate(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-act-alice1", "alice@example.com")
	e.register("op-http-act-mall01", "mallory@example.com")
	mallory := e.signIn("mallory@example.com")

	for _, email := range []string{"alice@example.com", "mallory@example.com", "nobody@example.com"} {
		expectError(t, e.do(http.MethodPost, "/api/activations", activation("op-http-act-deny01", email), withSession(mallory.token), withCSRF(mallory.csrf)), http.StatusForbidden, "forbidden")
	}
	if n := len(e.subs.Tenants()); n != 0 {
		t.Fatalf("tenants = %d, want 0", n)
	}
	if n := len(e.subs.Records()); n != 0 {
		t.Fatalf("records = %d, want 0", n)
	}
	// Denied before any account lookup, so emails cannot be enumerated.
	if n := e.subs.AccountLookups(); n != 0 {
		t.Fatalf("account lookups = %d, want 0", n)
	}
}

func TestActivationRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-act-oper01", "operator@example.com")
	auth := []option{withSession(op.token), withCSRF(op.csrf)}
	send := func(body any, extra ...option) *http.Response {
		return e.do(http.MethodPost, "/api/activations", body, append(append([]option{}, auth...), extra...)...)
	}

	expectError(t, send(activation("op-http-act-nobody", "nobody@example.com")), http.StatusNotFound, "not_found")
	expectError(t, send(activation("op-http-act-badem1", "not-an-email")), http.StatusBadRequest, "bad_request")
	expectError(t, send(activation("short", "operator@example.com")), http.StatusBadRequest, "bad_request")
	transactions := e.subs.Transactions()

	for name, opts := range map[string][]option{
		"not JSON":      {withBody("application/x-www-form-urlencoded", "email=a@example.com")},
		"unknown field": {withBody("application/json", `{"operation_id":"op-http-act-bad001","email":"operator@example.com","role":"owner"}`)},
		"oversized":     {withBody("application/json", `{"operation_id":"op-http-act-bad002","email":"`+strings.Repeat("x", 8192)+`"}`)},
		"trailing data": {withBody("application/json", `{"operation_id":"op-http-act-bad003","email":"operator@example.com"} {}`)},
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, send(nil, opts...), http.StatusBadRequest, "bad_request")
			if e.subs.Transactions() != transactions {
				t.Fatal("a malformed body reached the domain")
			}
		})
	}
	if n := len(e.subs.Tenants()); n != 0 {
		t.Fatalf("tenants = %d, want 0", n)
	}
}

// S9, CTRL-WEB-01 early primitive: Origin and CSRF are checked before the
// body is read, the domain runs, or storage changes.
func TestActivationChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-act-oper01", "operator@example.com")
	other := e.signIn("operator@example.com")
	e.register("op-http-act-alice1", "alice@example.com")
	body := activation("op-http-act-web001", "alice@example.com")

	rejected := map[string]struct {
		opts   []option
		status int
		code   string
	}{
		"no session":         {[]option{withCSRF(op.csrf)}, http.StatusUnauthorized, "unauthorized"},
		"missing CSRF":       {[]option{withSession(op.token)}, http.StatusForbidden, "forbidden"},
		"invalid CSRF":       {[]option{withSession(op.token), withCSRF("not-the-token")}, http.StatusForbidden, "forbidden"},
		"other session CSRF": {[]option{withSession(op.token), withCSRF(other.csrf)}, http.StatusForbidden, "forbidden"},
		"absent Origin":      {[]option{withSession(op.token), withCSRF(op.csrf), withoutOrigin()}, http.StatusForbidden, "forbidden"},
		"hostile Origin":     {[]option{withSession(op.token), withCSRF(op.csrf), withOrigin("https://evil.test")}, http.StatusForbidden, "forbidden"},
		// The CSRF check runs before the body is parsed.
		"missing CSRF, malformed body": {[]option{withSession(op.token), withBody("application/json", "{")}, http.StatusForbidden, "forbidden"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.do(http.MethodPost, "/api/activations", body, c.opts...), c.status, c.code)
			if n := e.subs.Transactions(); n != 0 {
				t.Fatalf("transactions = %d, want 0", n)
			}
			if n := len(e.subs.Tenants()); n != 0 {
				t.Fatalf("tenants = %d, want 0", n)
			}
		})
	}

	// Valid CSRF from the allowed Origin reaches the domain.
	if got := e.activate(op, "op-http-act-web001", "alice@example.com"); got["created"] != true {
		t.Fatalf("activation = %v", got)
	}
	if n := e.subs.Transactions(); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
}

// S6: each streamer reads only its own 1920x1080 canvas.
func TestStreamersReadOnlyTheirOwnCanvas(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-act-oper01", "operator@example.com")
	e.register("op-http-act-alice1", "alice@example.com")
	e.register("op-http-act-bob001", "bob@example.com")
	e.register("op-http-act-carol1", "carol@example.com")
	a := e.activate(op, "op-http-act-two001", "alice@example.com")
	b := e.activate(op, "op-http-act-two002", "bob@example.com")
	if a["tenant_id"] == b["tenant_id"] || a["canvas_id"] == b["canvas_id"] {
		t.Fatalf("streamers share identities: %v %v", a, b)
	}

	own := func(email string) map[string]any {
		s := e.signIn(email)
		res := e.do(http.MethodGet, "/api/canvas", nil, withSession(s.token), withoutOrigin())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: GET /api/canvas = %d", email, res.StatusCode)
		}
		return decode(t, res)
	}
	for email, want := range map[string]map[string]any{"alice@example.com": a, "bob@example.com": b} {
		got := own(email)
		if got["tenant_id"] != want["tenant_id"] || got["canvas_id"] != want["canvas_id"] ||
			got["width"] != float64(1920) || got["height"] != float64(1080) || got["revision"] != float64(0) {
			t.Fatalf("%s canvas = %v, activation %v", email, got, want)
		}
		if objects, ok := got["objects"].([]any); !ok || len(objects) != 0 {
			t.Fatalf("%s objects = %v", email, got["objects"])
		}
	}

	bob := e.signIn("bob@example.com")
	alicePath := "/api/tenants/" + a["tenant_id"].(string) + "/canvases/" + a["canvas_id"].(string)
	expectError(t, e.do(http.MethodGet, alicePath, nil, withSession(bob.token)), http.StatusForbidden, "forbidden")
	alice := e.signIn("alice@example.com")
	if res := e.do(http.MethodGet, alicePath, nil, withSession(alice.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("owner tenant canvas route = %d", res.StatusCode)
	}

	// A never-activated account and an operator get the same denial as the
	// tenant canvas route; the owner of an inactive tenant learns why (US-003).
	carol := e.signIn("carol@example.com")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(carol.token)), http.StatusForbidden, "forbidden")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(op.token)), http.StatusForbidden, "forbidden")
	e.subs.SetStatus(b["tenant_id"].(string), access.SubscriptionInactive)
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(bob.token)), http.StatusForbidden, "subscription_inactive")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil), http.StatusUnauthorized, "unauthorized")
}

func TestSessionReportsTheOperatorFlag(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-act-oper01", "operator@example.com")
	e.register("op-http-act-alice1", "alice@example.com")
	operatorID := e.register("op-http-act-oper02", "operator2@example.com")
	e.subs.GrantOperator(operatorID)

	for email, want := range map[string]bool{"alice@example.com": false, "operator2@example.com": true} {
		res := e.do(http.MethodPost, "/api/sessions", credentials(email, password))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("sign in %s = %d", email, res.StatusCode)
		}
		token := sessionCookie(res).Value
		if got := decode(t, res); got["operator"] != want || got["csrf_token"] == "" {
			t.Fatalf("%s sign-in body = %v, want operator %v", email, got, want)
		}
		res = e.do(http.MethodGet, "/api/session", nil, withSession(token))
		if got := decode(t, res); got["operator"] != want || len(got) != 4 {
			t.Fatalf("%s session body = %v, want operator %v", email, got, want)
		}
	}
}

// S11: activation logs carry the correlation and no email or token, and the
// stored record keeps the request correlation.
func TestActivationLogsAreCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-act-oper01", "operator@example.com")
	e.register("op-http-act-alice1", "alice@example.com")
	e.register("op-http-act-mall01", "mallory@example.com")
	mallory := e.signIn("mallory@example.com")

	res := e.do(http.MethodPost, "/api/activations", activation("op-http-act-log001", "alice@example.com"), withSession(op.token), withCSRF(op.csrf))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("activate = %d", res.StatusCode)
	}
	result := decode(t, res)
	if got := e.subs.Records()["op-http-act-log001"].CorrelationID; got == "" || got != res.Header.Get(httpx.CorrelationHeader) {
		t.Fatalf("record correlation %q, response %q", got, res.Header.Get(httpx.CorrelationHeader))
	}
	e.do(http.MethodPost, "/api/activations", activation("op-http-act-log001", "alice@example.com"), withSession(op.token), withCSRF(op.csrf))
	e.do(http.MethodPost, "/api/activations", activation("op-http-act-log001", "mallory@example.com"), withSession(op.token), withCSRF(op.csrf))
	e.do(http.MethodPost, "/api/activations", activation("op-http-act-log002", "nobody@example.com"), withSession(op.token), withCSRF(op.csrf))
	e.do(http.MethodPost, "/api/activations", activation("op-http-act-log003", "alice@example.com"), withSession(op.token), withCSRF("not-the-token"))
	e.do(http.MethodPost, "/api/activations", activation("op-http-act-log004", "alice@example.com"), withSession(mallory.token), withCSRF(mallory.csrf))
	e.do(http.MethodGet, "/api/canvas", nil, withSession(mallory.token))
	alice := e.signIn("alice@example.com")
	e.do(http.MethodGet, "/api/canvas", nil, withSession(alice.token))

	logs := e.logs.String()
	var confirmed string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"activation confirmed"`) {
			confirmed = line
		}
	}
	for _, want := range []string{`"correlation_id"`, `"account_id":"` + result["account_id"].(string), `"tenant_id":"` + result["tenant_id"].(string), `"canvas_id":"` + result["canvas_id"].(string)} {
		if !strings.Contains(confirmed, want) {
			t.Fatalf("activation log %q lacks %s", confirmed, want)
		}
	}
	for name, s := range map[string]string{
		"email":             "alice@example.com",
		"other email":       "mallory@example.com",
		"unknown email":     "nobody@example.com",
		"operator email":    "operator@example.com",
		"session token":     op.token,
		"CSRF token":        op.csrf,
		"streamer token":    alice.token,
		"streamer CSRF":     alice.csrf,
		"non-operator CSRF": mallory.csrf,
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
}
