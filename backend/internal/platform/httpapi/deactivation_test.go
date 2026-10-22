package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

func (e *env) deactivate(s signedIn, op, email string) map[string]any {
	e.t.Helper()
	res := e.do(http.MethodPost, "/api/deactivations", activation(op, email), withSession(s.token), withCSRF(s.csrf))
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("deactivate %s: status %d %s", email, res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	return decode(e.t, res)
}

// streamers signs in an operator and activates alice and bob.
func (e *env) streamers() (op signedIn, a, b map[string]any) {
	e.t.Helper()
	op = e.operator("op-http-dea-oper01", "operator@example.com")
	e.register("op-http-dea-alice1", "alice@example.com")
	e.register("op-http-dea-bob001", "bob@example.com")
	a = e.activate(op, "op-http-dea-act-a1", "alice@example.com")
	b = e.activate(op, "op-http-dea-act-b1", "bob@example.com")
	return op, a, b
}

func (e *env) status(tenantID string) access.SubscriptionStatus {
	e.t.Helper()
	for _, tenant := range e.subs.Tenants() {
		if tenant.ID == tenantID {
			return tenant.Status
		}
	}
	e.t.Fatalf("no tenant %s", tenantID)
	return ""
}

// expectUnchanged asserts that both tenants are active and no deactivation
// record or access cut was stored.
func (e *env) expectUnchanged(a, b map[string]any) {
	e.t.Helper()
	for _, tenant := range []map[string]any{a, b} {
		if got := e.status(tenant["tenant_id"].(string)); got != access.SubscriptionActive {
			e.t.Fatalf("tenant %v status = %s, want active", tenant["tenant_id"], got)
		}
	}
	if n := len(e.subs.Deactivations()); n != 0 {
		e.t.Fatalf("deactivation records = %d, want 0", n)
	}
	if n := len(e.subs.Cuts()); n != 0 {
		e.t.Fatalf("access cuts = %d, want 0", n)
	}
}

// S1, S11, S15: an operator deactivates a streamer by email; a replay
// returns the same answer and reactivation is the existing activation.
func TestOperatorDeactivatesAStreamer(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()

	res := e.do(http.MethodPost, "/api/deactivations", activation("op-http-deact-0001", "Alice@Example.com"), withSession(op.token), withCSRF(op.csrf))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deactivate = %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	firstRaw := e.bodies[len(e.bodies)-1]
	first := decode(t, res)
	if first["account_id"] != a["account_id"] || first["tenant_id"] != a["tenant_id"] || first["canvas_id"] != a["canvas_id"] ||
		first["deactivated"] != true || len(first) != 4 {
		t.Fatalf("deactivation body = %v, activation %v", first, a)
	}
	if got := e.status(a["tenant_id"].(string)); got != access.SubscriptionInactive {
		t.Fatalf("alice status = %s, want inactive", got)
	}
	if got := e.status(b["tenant_id"].(string)); got != access.SubscriptionActive {
		t.Fatalf("bob status = %s, want active", got)
	}
	cuts := e.subs.Cuts()
	if len(cuts) != 1 || cuts[0].TenantID != a["tenant_id"] || cuts[0].Version != 2 {
		t.Fatalf("cuts = %+v", cuts)
	}

	// S11: a retry under the same operation ID returns the identical answer.
	e.deactivate(op, "op-http-deact-0001", "alice@example.com")
	if again := e.bodies[len(e.bodies)-1]; again != firstRaw {
		t.Fatalf("replay body = %s, want %s", again, firstRaw)
	}
	// A different payload under the same operation ID is rejected.
	expectError(t, e.do(http.MethodPost, "/api/deactivations", activation("op-http-deact-0001", "bob@example.com"), withSession(op.token), withCSRF(op.csrf)), http.StatusConflict, "conflict")
	if got := e.status(b["tenant_id"].(string)); got != access.SubscriptionActive {
		t.Fatalf("bob status after conflict = %s, want active", got)
	}
	// A new operation ID for an inactive tenant changes nothing.
	repeat := e.deactivate(op, "op-http-deact-0002", "alice@example.com")
	if repeat["tenant_id"] != a["tenant_id"] || repeat["canvas_id"] != a["canvas_id"] || repeat["deactivated"] != false || len(repeat) != 4 {
		t.Fatalf("repeated deactivation = %v", repeat)
	}
	if n := len(e.subs.Cuts()); n != 1 {
		t.Fatalf("cuts = %d, want 1", n)
	}
	if n := len(e.subs.Deactivations()); n != 2 {
		t.Fatalf("deactivation records = %d, want 2", n)
	}

	// S5: reactivation is the activation and keeps the tenant and canvas.
	again := e.activate(op, "op-http-dea-react1", "alice@example.com")
	if again["tenant_id"] != a["tenant_id"] || again["canvas_id"] != a["canvas_id"] || again["created"] != false {
		t.Fatalf("reactivation = %v, first %v", again, a)
	}
	if got := e.status(a["tenant_id"].(string)); got != access.SubscriptionActive {
		t.Fatalf("alice status after reactivation = %s", got)
	}
	if n := len(e.subs.Tenants()); n != 2 {
		t.Fatalf("tenants = %d, want 2", n)
	}
}

// L3(g), S15: the owner of a deactivated tenant learns why its canvas is
// denied; nobody else does.
func TestDeactivatedOwnerIsToldTheSubscriptionIsInactive(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	e.register("op-http-dea-carol1", "carol@example.com")
	alice := e.signIn("alice@example.com")
	bob := e.signIn("bob@example.com")
	carol := e.signIn("carol@example.com")

	own := func(s signedIn) string {
		t.Helper()
		res := e.do(http.MethodGet, "/api/canvas", nil, withSession(s.token), withoutOrigin())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/canvas = %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
		}
		return e.bodies[len(e.bodies)-1]
	}
	before := own(alice)
	bobBefore := own(bob)

	e.deactivate(op, "op-http-deact-0001", "alice@example.com")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(alice.token)), http.StatusForbidden, "subscription_inactive")
	// S8: bob keeps reading the same canvas.
	if got := own(bob); got != bobBefore {
		t.Fatalf("bob canvas = %s, want %s", got, bobBefore)
	}
	// A never-activated account and an operator keep the shared denial.
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(carol.token)), http.StatusForbidden, "forbidden")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(op.token)), http.StatusForbidden, "forbidden")
	// Bob still cannot read alice's canvas by tenant.
	alicePath := "/api/tenants/" + a["tenant_id"].(string) + "/canvases/" + a["canvas_id"].(string)
	expectError(t, e.do(http.MethodGet, alicePath, nil, withSession(bob.token)), http.StatusForbidden, "forbidden")

	// S5: after reactivation alice reads the same canvas again.
	e.activate(op, "op-http-dea-react1", "alice@example.com")
	if got := own(alice); got != before {
		t.Fatalf("canvas after reactivation = %s, want %s", got, before)
	}
	if strings.Contains(before, b["tenant_id"].(string)) {
		t.Fatalf("alice's canvas names bob's tenant: %s", before)
	}
}

func TestOwnCanvasDenialFailsClosedWhenTheTenantCannotBeRead(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, _, _ := e.streamers()
	alice := e.signIn("alice@example.com")
	e.deactivate(op, "op-http-deact-0001", "alice@example.com")
	e.faults.ownedTenant = errors.New("tenant read failed")
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(alice.token)), http.StatusInternalServerError, "internal")
}

// S1, S4: the tenant canvas route denies owner and moderator from the
// deactivation commit, and reactivation restores the same canvas.
func TestDeactivationCutsTheTenantCanvasRoute(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	moderatorID := e.register("op-http-dea-mod001", "moderator@example.com")
	e.moderators.Store([2]string{a["tenant_id"].(string), string(moderatorID)}, true)
	for _, tenant := range []map[string]any{a, b} {
		key := canvas.Key{TenantID: tenant["tenant_id"].(string), CanvasID: tenant["canvas_id"].(string)}
		e.canvases.byID[key] = canvas.Canvas{Key: key, Dimensions: canvas.Dimensions{Width: 1280, Height: 720}, Revision: 4, Objects: []canvas.Object{
			{ID: "obj-1", Kind: canvas.KindText, Content: "content of " + key.TenantID, Transform: canvas.Transform{X: 5, Y: 6, Width: 10, Height: 20}, Z: 1},
		}}
	}
	pathOf := func(tenant map[string]any) string {
		return "/api/tenants/" + tenant["tenant_id"].(string) + "/canvases/" + tenant["canvas_id"].(string)
	}
	read := func(s signedIn, path string) string {
		t.Helper()
		res := e.do(http.MethodGet, path, nil, withSession(s.token), withoutOrigin())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, res.StatusCode, e.bodies[len(e.bodies)-1])
		}
		return e.bodies[len(e.bodies)-1]
	}
	alice := e.signIn("alice@example.com")
	moderator := e.signIn("moderator@example.com")
	bob := e.signIn("bob@example.com")
	owned := read(alice, pathOf(a))
	if moderated := read(moderator, pathOf(a)); moderated != owned {
		t.Fatalf("moderator body = %s, owner %s", moderated, owned)
	}
	for _, want := range []string{`"width":1280`, `"height":720`, `"revision":4`, `"content of ` + a["tenant_id"].(string)} {
		if !strings.Contains(owned, want) {
			t.Fatalf("canvas body %s lacks %s", owned, want)
		}
	}
	bobBefore := read(bob, pathOf(b))

	e.deactivate(op, "op-http-deact-0001", "alice@example.com")
	// Every response, the denials included, is no-store (checked by e.do), so
	// no cache restores the canvas.
	for name, s := range map[string]signedIn{"owner": alice, "moderator": moderator} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.do(http.MethodGet, pathOf(a), nil, withSession(s.token)), http.StatusForbidden, "forbidden")
		})
	}
	if got := read(bob, pathOf(b)); got != bobBefore {
		t.Fatalf("bob canvas = %s, want %s", got, bobBefore)
	}

	e.activate(op, "op-http-dea-react1", "alice@example.com")
	if got := read(alice, pathOf(a)); got != owned {
		t.Fatalf("owner canvas after reactivation = %s, want %s", got, owned)
	}
	if got := read(moderator, pathOf(a)); got != owned {
		t.Fatalf("moderator canvas after reactivation = %s, want %s", got, owned)
	}
}

// S6: only an operator deactivates, and a denial happens before any account
// lookup, so emails cannot be enumerated.
func TestNonOperatorCannotDeactivate(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	e.register("op-http-dea-mall01", "mallory@example.com")
	mallory := e.signIn("mallory@example.com")
	alice := e.signIn("alice@example.com")
	lookups := e.subs.AccountLookups()

	for name, s := range map[string]signedIn{"registered": mallory, "owner": alice} {
		for _, email := range []string{"alice@example.com", "mallory@example.com", "nobody@example.com"} {
			t.Run(name+" "+email, func(t *testing.T) {
				expectError(t, e.do(http.MethodPost, "/api/deactivations", activation("op-http-deact-deny", email), withSession(s.token), withCSRF(s.csrf)), http.StatusForbidden, "forbidden")
				e.expectUnchanged(a, b)
				if n := e.subs.AccountLookups(); n != lookups {
					t.Fatalf("account lookups = %d, want %d", n, lookups)
				}
			})
		}
	}
	// A revoked operator is denied as well.
	e.subs.RevokeOperator(access.AccountID(e.sessionAccount(op)))
	expectError(t, e.do(http.MethodPost, "/api/deactivations", activation("op-http-deact-deny", "alice@example.com"), withSession(op.token), withCSRF(op.csrf)), http.StatusForbidden, "forbidden")
	e.expectUnchanged(a, b)
}

func (e *env) sessionAccount(s signedIn) string {
	e.t.Helper()
	return decode(e.t, e.do(http.MethodGet, "/api/session", nil, withSession(s.token)))["account_id"].(string)
}

func TestDeactivationRejectsInvalidRequests(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	e.register("op-http-dea-carol1", "carol@example.com")
	auth := []option{withSession(op.token), withCSRF(op.csrf)}
	send := func(body any, extra ...option) *http.Response {
		return e.do(http.MethodPost, "/api/deactivations", body, append(append([]option{}, auth...), extra...)...)
	}

	// An unknown email and an account that owns no tenant look the same.
	expectError(t, send(activation("op-http-dea-nobody", "nobody@example.com")), http.StatusNotFound, "not_found")
	expectError(t, send(activation("op-http-dea-carol2", "carol@example.com")), http.StatusNotFound, "not_found")
	expectError(t, send(activation("op-http-dea-badem1", "not-an-email")), http.StatusBadRequest, "bad_request")
	expectError(t, send(activation("short", "alice@example.com")), http.StatusBadRequest, "bad_request")
	e.expectUnchanged(a, b)
	transactions := e.subs.Transactions()

	for name, opts := range map[string][]option{
		"not JSON":      {withBody("application/x-www-form-urlencoded", "email=alice@example.com")},
		"unknown field": {withBody("application/json", `{"operation_id":"op-http-dea-bad001","email":"alice@example.com","tenant_id":"x"}`)},
		"oversized":     {withBody("application/json", `{"operation_id":"op-http-dea-bad002","email":"`+strings.Repeat("x", 8192)+`"}`)},
		"trailing data": {withBody("application/json", `{"operation_id":"op-http-dea-bad003","email":"alice@example.com"} {}`)},
		"empty body":    {withBody("application/json", "")},
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, send(nil, opts...), http.StatusBadRequest, "bad_request")
			if e.subs.Transactions() != transactions {
				t.Fatal("a malformed body reached the domain")
			}
			e.expectUnchanged(a, b)
		})
	}

	// A store failure is an internal error and commits nothing.
	e.subs.FailOnce("commit", errors.New("commit failed for alice@example.com"))
	expectError(t, send(activation("op-http-dea-fail01", "alice@example.com")), http.StatusInternalServerError, "internal")
	e.expectUnchanged(a, b)
	if strings.Contains(e.bodies[len(e.bodies)-1], "alice@example.com") {
		t.Fatal("the internal error body names the email")
	}
}

// S12, CTRL-WEB-01 early primitive: Origin and CSRF are checked before the
// body is read, the domain runs, or storage changes.
func TestDeactivationChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	other := e.signIn("operator@example.com")
	body := activation("op-http-dea-web001", "alice@example.com")
	transactions := e.subs.Transactions()

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
			expectError(t, e.do(http.MethodPost, "/api/deactivations", body, c.opts...), c.status, c.code)
			if n := e.subs.Transactions(); n != transactions {
				t.Fatalf("transactions = %d, want %d", n, transactions)
			}
			e.expectUnchanged(a, b)
		})
	}

	// Valid CSRF from the allowed Origin reaches the domain.
	if got := e.deactivate(op, "op-http-dea-web001", "alice@example.com"); got["deactivated"] != true {
		t.Fatalf("deactivation = %v", got)
	}
	if n := e.subs.Transactions(); n != transactions+1 {
		t.Fatalf("transactions = %d, want %d", n, transactions+1)
	}
}

// S7, S13: the stored record and cut keep the response correlation; logs
// carry it with IDs only, and no log or error body holds a secret, an email,
// or another tenant's IDs.
func TestDeactivationIsCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op, a, b := e.streamers()
	e.register("op-http-dea-mall01", "mallory@example.com")
	mallory := e.signIn("mallory@example.com")
	alice := e.signIn("alice@example.com")
	bob := e.signIn("bob@example.com")

	res := e.do(http.MethodPost, "/api/deactivations", activation("op-http-dea-log001", "alice@example.com"), withSession(op.token), withCSRF(op.csrf))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("deactivate = %d", res.StatusCode)
	}
	correlationID := res.Header.Get(httpx.CorrelationHeader)
	if got := e.subs.Deactivations()["op-http-dea-log001"].CorrelationID; got == "" || got != correlationID {
		t.Fatalf("record correlation %q, response %q", got, correlationID)
	}
	if cuts := e.subs.Cuts(); len(cuts) != 1 || cuts[0].CorrelationID != correlationID {
		t.Fatalf("cuts = %+v, response correlation %q", cuts, correlationID)
	}

	var errorBodies []string
	collect := func(res *http.Response) {
		if res.StatusCode >= http.StatusBadRequest {
			errorBodies = append(errorBodies, e.bodies[len(e.bodies)-1])
		}
	}
	deactivations := func(op, email string, opts ...option) *http.Response {
		return e.do(http.MethodPost, "/api/deactivations", activation(op, email), opts...)
	}
	collect(deactivations("op-http-dea-log001", "bob@example.com", withSession(op.token), withCSRF(op.csrf)))
	collect(deactivations("op-http-dea-log002", "nobody@example.com", withSession(op.token), withCSRF(op.csrf)))
	collect(deactivations("op-http-dea-log003", "not-an-email", withSession(op.token), withCSRF(op.csrf)))
	collect(deactivations("op-http-dea-log004", "bob@example.com", withSession(op.token), withCSRF("not-the-token")))
	collect(deactivations("op-http-dea-log005", "bob@example.com", withSession(mallory.token), withCSRF(mallory.csrf)))
	collect(deactivations("op-http-dea-log006", "bob@example.com", withSession(alice.token), withCSRF(alice.csrf)))
	collect(deactivations("op-http-dea-log007", "bob@example.com", withCSRF(op.csrf)))
	collect(deactivations("op-http-dea-log008", "bob@example.com", withSession(op.token), withCSRF(op.csrf), withOrigin("https://evil.test")))
	collect(e.do(http.MethodGet, "/api/canvas", nil, withSession(alice.token)))
	collect(e.do(http.MethodGet, "/api/canvas", nil, withSession(mallory.token)))
	collect(e.do(http.MethodGet, "/api/tenants/"+b["tenant_id"].(string)+"/canvases/"+b["canvas_id"].(string), nil, withSession(alice.token)))
	collect(e.do(http.MethodGet, "/api/tenants/"+a["tenant_id"].(string)+"/canvases/"+a["canvas_id"].(string), nil, withSession(bob.token)))
	if len(errorBodies) != 12 {
		t.Fatalf("error responses = %d, want 12", len(errorBodies))
	}

	logs := e.logs.String()
	var confirmed string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"deactivation confirmed"`) {
			confirmed = line
		}
	}
	for _, want := range []string{`"correlation_id":"` + correlationID, `"account_id":"` + a["account_id"].(string), `"tenant_id":"` + a["tenant_id"].(string), `"canvas_id":"` + a["canvas_id"].(string)} {
		if !strings.Contains(confirmed, want) {
			t.Fatalf("deactivation log %q lacks %s", confirmed, want)
		}
	}
	if strings.Contains(confirmed, "version") {
		t.Fatalf("deactivation log %q names the access version", confirmed)
	}
	secrets := map[string]string{
		"email":             "alice@example.com",
		"other email":       "bob@example.com",
		"unknown email":     "nobody@example.com",
		"non-operator":      "mallory@example.com",
		"operator email":    "operator@example.com",
		"session token":     op.token,
		"CSRF token":        op.csrf,
		"streamer token":    alice.token,
		"streamer CSRF":     alice.csrf,
		"non-operator CSRF": mallory.csrf,
	}
	for name, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
	secrets["tenant A"] = a["tenant_id"].(string)
	secrets["canvas A"] = a["canvas_id"].(string)
	secrets["tenant B"] = b["tenant_id"].(string)
	secrets["canvas B"] = b["canvas_id"].(string)
	for _, body := range errorBodies {
		for name, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("error body %s contains the %s", body, name)
			}
		}
	}
}

var _ httpapi.Subscriptions = (*subscription.Service)(nil)
