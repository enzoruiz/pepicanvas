package httpapi_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
)

func dimensions(op string, width, height int) map[string]any {
	return map[string]any{"operation_id": op, "width": width, "height": height}
}

// streamer is an activated account signed in to its own canvas.
type streamer struct {
	signedIn
	tenantID string
	canvasID string
	path     string
}

// streamer registers, activates, and signs in the account of email.
func (e *env) streamer(op signedIn, name, email string) streamer {
	e.t.Helper()
	e.register("op-http-dim-reg-"+name, email)
	a := e.activate(op, "op-http-dim-act-"+name, email)
	tenantID, canvasID := a["tenant_id"].(string), a["canvas_id"].(string)
	return streamer{
		signedIn: e.signIn(email),
		tenantID: tenantID,
		canvasID: canvasID,
		path:     "/api/tenants/" + tenantID + "/canvases/" + canvasID,
	}
}

// resize sends a dimensions change for the canvas at path with the session
// and CSRF token of s.
func (e *env) resize(s signedIn, path string, body any, extra ...option) *http.Response {
	e.t.Helper()
	opts := append([]option{withSession(s.token), withCSRF(s.csrf)}, extra...)
	return e.do(http.MethodPut, path+"/dimensions", body, opts...)
}

// expectCanvas checks the committed dimensions and revision the owner reads
// on both canvas routes.
func (e *env) expectCanvas(s streamer, width, height, revision int) {
	e.t.Helper()
	for _, path := range []string{s.path, "/api/canvas"} {
		res := e.do(http.MethodGet, path, nil, withSession(s.token))
		if res.StatusCode != http.StatusOK {
			e.t.Fatalf("GET %s = %d", path, res.StatusCode)
		}
		got := decode(e.t, res)
		if got["canvas_id"] != s.canvasID || got["width"] != float64(width) || got["height"] != float64(height) || got["revision"] != float64(revision) {
			e.t.Fatalf("GET %s = %v, want %dx%d revision %d", path, got, width, height, revision)
		}
	}
}

func (e *env) expectApplies(want int) {
	e.t.Helper()
	if got := e.canvases.Applies(); got != want {
		e.t.Fatalf("applies = %d, want %d", got, want)
	}
}

// expectResized checks a successful resize answer has exactly four fields.
func expectResized(t *testing.T, res *http.Response, canvasID string, width, height, revision int) {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("resize status = %d, want 200", res.StatusCode)
	}
	got := decode(t, res)
	if got["canvas_id"] != canvasID || got["width"] != float64(width) || got["height"] != float64(height) || got["revision"] != float64(revision) || len(got) != 4 {
		t.Fatalf("resize body = %v, want %s %dx%d revision %d", got, canvasID, width, height, revision)
	}
}

// S1: the canvas starts at 1920x1080, and both inclusive limits and values in
// range are accepted and read back on both canvas routes.
func TestOwnerResizesWithinInclusiveBounds(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	e.expectCanvas(alice, 1920, 1080, 0)

	for i, size := range [][2]int{{320, 180}, {3840, 2160}, {1280, 720}, {1920, 1080}, {321, 2159}} {
		res := e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-ok-000"+strconv.Itoa(i), size[0], size[1]))
		expectResized(t, res, alice.canvasID, size[0], size[1], i+1)
		e.expectCanvas(alice, size[0], size[1], i+1)
	}
	e.expectApplies(5)
}

// S2: invalid values are rejected without changing state or retaining the
// operation ID.
func TestInvalidDimensionsAreRejectedWithoutEffect(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-seed01", 1280, 720)), alice.canvasID, 1280, 720, 1)

	const reused = `"operation_id":"op-http-dim-bad001"`
	for name, raw := range map[string]string{
		"width 319":             `{` + reused + `,"width":319,"height":720}`,
		"height 179":            `{` + reused + `,"width":1280,"height":179}`,
		"width 3841":            `{` + reused + `,"width":3841,"height":720}`,
		"height 2161":           `{` + reused + `,"width":1280,"height":2161}`,
		"negative width":        `{` + reused + `,"width":-1280,"height":720}`,
		"empty width":           `{` + reused + `,"width":"","height":720}`,
		"empty height":          `{` + reused + `,"width":1280,"height":""}`,
		"fractional width":      `{` + reused + `,"width":1.5,"height":720}`,
		"fractional height":     `{` + reused + `,"width":1280,"height":720.5}`,
		"non-numeric width":     `{` + reused + `,"width":"abc","height":720}`,
		"numeric string height": `{` + reused + `,"width":1280,"height":"720"}`,
		"null width":            `{` + reused + `,"width":null,"height":720}`,
		"null height":           `{` + reused + `,"width":1280,"height":null}`,
		"missing width":         `{` + reused + `,"height":720}`,
		"missing height":        `{` + reused + `,"width":1280}`,
		"missing operation ID":  `{"width":1280,"height":720}`,
		"empty operation ID":    `{"operation_id":"","width":1280,"height":720}`,
		"unknown field":         `{` + reused + `,"width":1280,"height":720,"objects":[]}`,
		"trailing data":         `{` + reused + `,"width":1280,"height":720} {}`,
		"empty body":            ``,
		"not an object":         `[1280,720]`,
		"oversized":             `{` + reused + `,"width":1280,"height":720,"pad":"` + strings.Repeat("x", 8192) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.resize(alice.signedIn, alice.path, nil, withBody("application/json", raw)), http.StatusBadRequest, "bad_request")
			e.expectApplies(1)
			e.expectCanvas(alice, 1280, 720, 1)
		})
	}
	expectError(t, e.resize(alice.signedIn, alice.path, nil, withBody("application/x-www-form-urlencoded", "width=1280&height=720")), http.StatusBadRequest, "bad_request")
	e.expectApplies(1)

	// No rejected request retained the operation ID.
	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-bad001", 1600, 900)), alice.canvasID, 1600, 900, 2)
}

// S4: only the owner changes dimensions. Every other caller gets the same
// forbidden answer whatever the body, before it is read.
func TestOnlyTheOwnerResizes(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	bob := e.streamer(op, "bob001", "bob@example.com")
	carol := e.streamer(op, "carol1", "carol@example.com")
	e.subs.SetStatus(carol.tenantID, access.SubscriptionInactive)
	moderatorID := e.register("op-http-dim-reg-mod001", "moderator@example.com")
	e.store.SetMembership(moderatorID, access.Membership{TenantID: alice.tenantID, Role: access.RoleModerator, Subscription: access.SubscriptionActive})
	moderator := e.signIn("moderator@example.com")
	e.register("op-http-dim-reg-nobody", "nobody@example.com")
	nobody := e.signIn("nobody@example.com")

	// The moderator can read the canvas, so the denial is about the action.
	if res := e.do(http.MethodGet, alice.path, nil, withSession(moderator.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderator GET = %d", res.StatusCode)
	}

	callers := map[string]struct {
		session signedIn
		path    string
	}{
		"moderator":                {moderator, alice.path},
		"another active streamer":  {bob.signedIn, alice.path},
		"operator":                 {op, alice.path},
		"account without a tenant": {nobody, alice.path},
		"inactive streamer":        {carol.signedIn, carol.path},
	}
	bodies := map[string]option{
		"valid":     withBody("application/json", `{"operation_id":"op-http-dim-deny01","width":1280,"height":720}`),
		"invalid":   withBody("application/json", `{"operation_id":"op-http-dim-deny01","width":1,"height":720}`),
		"malformed": withBody("application/json", `{`),
		"not JSON":  withBody("text/plain", `width=1280`),
	}
	for caller, c := range callers {
		for body, b := range bodies {
			t.Run(caller+", "+body, func(t *testing.T) {
				expectError(t, e.resize(c.session, c.path, nil, b), http.StatusForbidden, "forbidden")
				e.expectApplies(0)
			})
		}
	}
	expectError(t, e.do(http.MethodPut, alice.path+"/dimensions", dimensions("op-http-dim-deny02", 1280, 720)), http.StatusUnauthorized, "unauthorized")
	e.expectApplies(0)
	e.expectCanvas(alice, 1920, 1080, 0)

	// The inactive streamer was denied for the subscription, not the canvas.
	e.subs.SetStatus(carol.tenantID, access.SubscriptionActive)
	expectResized(t, e.resize(carol.signedIn, carol.path, dimensions("op-http-dim-deny01", 1280, 720)), carol.canvasID, 1280, 720, 1)
}

// S7: resizing one canvas neither changes nor reveals another.
func TestResizingOneCanvasLeavesTheOtherUnchanged(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	bob := e.streamer(op, "bob001", "bob@example.com")

	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-iso001", 1280, 720)), alice.canvasID, 1280, 720, 1)
	e.expectCanvas(alice, 1280, 720, 1)
	e.expectCanvas(bob, 1920, 1080, 0)
	for _, body := range e.bodies[len(e.bodies)-2:] {
		if strings.Contains(body, alice.tenantID) || strings.Contains(body, alice.canvasID) {
			t.Fatalf("bob's canvas reveals alice's: %s", body)
		}
	}

	for name, c := range map[string]struct {
		session signedIn
		path    string
	}{
		"bob resizes alice's canvas":         {bob.signedIn, alice.path},
		"alice's tenant with bob's canvas":   {alice.signedIn, "/api/tenants/" + alice.tenantID + "/canvases/" + bob.canvasID},
		"bob's tenant with alice's canvas":   {bob.signedIn, "/api/tenants/" + bob.tenantID + "/canvases/" + alice.canvasID},
		"alice resizes bob's canvas":         {alice.signedIn, bob.path},
		"alice's tenant with missing canvas": {alice.signedIn, "/api/tenants/" + alice.tenantID + "/canvases/missing"},
		"oversized identifier":               {alice.signedIn, "/api/tenants/" + strings.Repeat("t", 200) + "/canvases/" + alice.canvasID},
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.resize(c.session, c.path, dimensions("op-http-dim-iso002", 3840, 2160)), http.StatusForbidden, "forbidden")
			e.expectApplies(1)
		})
	}
	e.expectCanvas(alice, 1280, 720, 1)
	e.expectCanvas(bob, 1920, 1080, 0)
}

// S10, CTRL-IDEM-01: a retry returns the same answer with one effect; a
// different payload under the same operation ID is rejected.
func TestResizeReplayReturnsTheSameAnswer(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	other := e.signIn("alice@example.com")

	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-idem01", 1280, 720)), alice.canvasID, 1280, 720, 1)
	first := e.bodies[len(e.bodies)-1]

	// A retry after a timeout, and over another session after a reconnection.
	for _, s := range []signedIn{alice.signedIn, other} {
		e.resize(s, alice.path, dimensions("op-http-dim-idem01", 1280, 720))
		if again := e.bodies[len(e.bodies)-1]; again != first {
			t.Fatalf("replay body = %s, want %s", again, first)
		}
	}
	e.expectApplies(1)
	e.expectCanvas(alice, 1280, 720, 1)

	for name, body := range map[string]map[string]any{
		"other width":  dimensions("op-http-dim-idem01", 1600, 720),
		"other height": dimensions("op-http-dim-idem01", 1280, 900),
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.resize(alice.signedIn, alice.path, body), http.StatusConflict, "conflict")
			e.expectApplies(1)
			e.expectCanvas(alice, 1280, 720, 1)
		})
	}

	// A later change does not alter the answer of the earlier operation.
	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-idem02", 1600, 900)), alice.canvasID, 1600, 900, 2)
	e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-idem01", 1280, 720))
	if again := e.bodies[len(e.bodies)-1]; again != first {
		t.Fatalf("replay after a later change = %s, want %s", again, first)
	}
	e.expectApplies(2)
	e.expectCanvas(alice, 1600, 900, 2)
}

// S11, CTRL-WEB-01 (TE-014 ingress): the resize route is a Session route, so
// Origin and the session's CSRF token are checked before the session is read,
// the body is decoded, the domain runs, or storage changes. Every refusal is
// the ingress's exact forbidden answer, counted under its reason.
func TestResizeChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	other := e.signIn("alice@example.com")
	body := dimensions("op-http-dim-web001", 1280, 720)
	path := alice.path + "/dimensions"

	rejected := map[string]struct {
		opts   []option
		status int
		code   string
		reason ingress.Reason
	}{
		"no session":                   {[]option{withCSRF(alice.csrf)}, http.StatusUnauthorized, "unauthorized", ""},
		"no session, malformed body":   {[]option{withCSRF(alice.csrf), withBody("application/json", "{")}, http.StatusUnauthorized, "unauthorized", ""},
		"missing CSRF":                 {[]option{withSession(alice.token)}, http.StatusForbidden, "forbidden", ingress.ReasonCSRF},
		"invalid CSRF":                 {[]option{withSession(alice.token), withCSRF("not-the-token")}, http.StatusForbidden, "forbidden", ingress.ReasonCSRF},
		"other session CSRF":           {[]option{withSession(alice.token), withCSRF(other.csrf)}, http.StatusForbidden, "forbidden", ingress.ReasonCSRF},
		"absent Origin":                {[]option{withSession(alice.token), withCSRF(alice.csrf), withoutOrigin()}, http.StatusForbidden, "forbidden", ingress.ReasonOrigin},
		"hostile Origin":               {[]option{withSession(alice.token), withCSRF(alice.csrf), withOrigin("https://evil.test")}, http.StatusForbidden, "forbidden", ingress.ReasonOrigin},
		"null Origin":                  {[]option{withSession(alice.token), withCSRF(alice.csrf), withOrigin("null")}, http.StatusForbidden, "forbidden", ingress.ReasonOrigin},
		"suffix Origin":                {[]option{withSession(alice.token), withCSRF(alice.csrf), withOrigin(origin + ".evil.test")}, http.StatusForbidden, "forbidden", ingress.ReasonOrigin},
		"hostile Origin, no CSRF":      {[]option{withSession(alice.token), withOrigin("https://evil.test")}, http.StatusForbidden, "forbidden", ingress.ReasonOrigin},
		"missing CSRF, malformed body": {[]option{withSession(alice.token), withBody("application/json", "{")}, http.StatusForbidden, "forbidden", ingress.ReasonCSRF},
		"missing CSRF, invalid size":   {[]option{withSession(alice.token), withBody("application/json", `{"operation_id":"op-http-dim-web001","width":1,"height":1}`)}, http.StatusForbidden, "forbidden", ingress.ReasonCSRF},
	}
	for name, c := range rejected {
		before := e.effects()
		var count uint64
		if c.reason != "" {
			count = reasonCount(e.ingress, c.reason)
		}
		res := e.do(http.MethodPut, path, body, c.opts...)
		if sessionCookie(res) != nil {
			t.Fatalf("%s: a refused resize touched the session cookie", name)
		}
		expectError(t, res, c.status, c.code)
		if c.reason == "" {
			// Without a cookie there is no token to forge: the handler reads
			// the absent session and answers 401 before the body.
			after := e.effects()
			if after.storage != before.storage || after.calls["Apply"] != before.calls["Apply"] {
				t.Fatalf("%s reached the canvas: %+v -> %+v", name, before, after)
			}
			continue
		}
		e.expectNoEffect("resize with "+name, before)
		if got := reasonCount(e.ingress, c.reason); got != count+1 {
			t.Fatalf("%s: %s rejections = %d, want %d", name, c.reason, got, count+1)
		}
	}
	if got := e.canvases.ApplyCalls(); got != 0 {
		t.Fatalf("Apply calls = %d, want 0", got)
	}
	e.expectCanvas(alice, 1920, 1080, 0)

	// Valid CSRF from the allowed Origin reaches the domain.
	expectResized(t, e.resize(alice.signedIn, alice.path, body), alice.canvasID, 1280, 720, 1)
	e.expectApplies(1)
}

// S10, S11: a resize the ingress refuses (Origin, CSRF, the per-session
// mutation budget of Session routes) retains no operation, so a retry under
// the same operation ID has exactly one effect and replays it.
func TestResizeRefusedByTheIngressLeavesNoOperation(t *testing.T) {
	limits := ingress.DefaultLimits
	limits.MutationsPerSession = ratelimit.Rate{Limit: 2, Window: time.Minute}
	e := newEnvWith(t, httpapi.DefaultLimits, limits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	spent := e.signIn("alice@example.com")
	const id = "op-http-dim-ing001"

	for range 2 {
		expectError(t, e.resize(spent, alice.path, nil, withBody("application/json", "{")), http.StatusBadRequest, "bad_request")
	}
	for _, c := range []struct {
		name   string
		s      signedIn
		opts   []option
		status int
		code   string
	}{
		{"hostile Origin", alice.signedIn, []option{withOrigin("https://evil.test")}, http.StatusForbidden, "forbidden"},
		{"absent Origin", alice.signedIn, []option{withoutOrigin()}, http.StatusForbidden, "forbidden"},
		{"missing CSRF", alice.signedIn, []option{func(r *http.Request) { r.Header.Del(httpapi.CSRFHeader) }}, http.StatusForbidden, "forbidden"},
		{"invalid CSRF", alice.signedIn, []option{withCSRF("not-the-token")}, http.StatusForbidden, "forbidden"},
		{"session limit", spent, nil, http.StatusTooManyRequests, "rate_limited"},
	} {
		before := e.effects()
		res := e.resize(c.s, alice.path, dimensions(id, 1280, 720), c.opts...)
		if c.status == http.StatusTooManyRequests && res.Header.Get("Retry-After") != "60" {
			t.Fatalf("%s: Retry-After = %q, want 60", c.name, res.Header.Get("Retry-After"))
		}
		expectError(t, res, c.status, c.code)
		e.expectNoEffect(c.name, before)
		if e.canvases.Correlation(id) != "" {
			t.Fatalf("%s retained the operation", c.name)
		}
	}
	if got := e.canvases.ApplyCalls(); got != 0 {
		t.Fatalf("Apply calls = %d, want 0", got)
	}
	e.expectCanvas(alice, 1920, 1080, 0)

	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions(id, 1280, 720)), alice.canvasID, 1280, 720, 1)
	first := e.bodies[len(e.bodies)-1]
	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions(id, 1280, 720)), alice.canvasID, 1280, 720, 1)
	if again := e.bodies[len(e.bodies)-1]; again != first {
		t.Fatalf("replay body = %s, want %s", again, first)
	}
	e.expectApplies(1)
	e.expectCanvas(alice, 1280, 720, 1)
}

// S12: the confirmation log and the retained operation carry the request
// correlation, and logs carry no email or token.
func TestResizeLogsAreCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	bob := e.streamer(op, "bob001", "bob@example.com")

	res := e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-log001", 1280, 720))
	correlationID := res.Header.Get(httpx.CorrelationHeader)
	expectResized(t, res, alice.canvasID, 1280, 720, 1)
	if got := e.canvases.Correlation("op-http-dim-log001"); got == "" || got != correlationID {
		t.Fatalf("retained correlation %q, response %q", got, correlationID)
	}
	e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-log001", 1600, 900))
	e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-log002", 1, 1))
	e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-log003", 1280, 720), withCSRF("not-the-token"))
	e.resize(bob.signedIn, alice.path, dimensions("op-http-dim-log004", 1280, 720))

	logs := e.logs.String()
	var confirmed []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"msg":"canvas dimensions confirmed"`) {
			confirmed = append(confirmed, line)
		}
	}
	if len(confirmed) != 1 {
		t.Fatalf("confirmation logs = %q, want one", confirmed)
	}
	for _, want := range []string{
		`"correlation_id":"` + correlationID + `"`,
		`"tenant_id":"` + alice.tenantID + `"`,
		`"canvas_id":"` + alice.canvasID + `"`,
		`"width":1280`,
		`"height":720`,
		`"revision":1`,
	} {
		if !strings.Contains(confirmed[0], want) {
			t.Fatalf("confirmation log %q lacks %s", confirmed[0], want)
		}
	}
	if strings.Contains(confirmed[0], "account_id") {
		t.Fatalf("confirmation log %q carries the account", confirmed[0])
	}
	for name, s := range map[string]string{
		"email":          "alice@example.com",
		"other email":    "bob@example.com",
		"operator email": "operator@example.com",
		"session token":  alice.token,
		"CSRF token":     alice.csrf,
		"other token":    bob.token,
		"other CSRF":     bob.csrf,
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

// A storage failure answers internal and changes nothing; a retry under the
// same operation ID then succeeds.
func TestResizeStoreFailureChangesNothing(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-dim-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")

	e.canvases.FailNext(errors.New("database unavailable"))
	expectError(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-fail01", 1280, 720)), http.StatusInternalServerError, "internal")
	e.expectApplies(0)
	e.expectCanvas(alice, 1920, 1080, 0)

	expectResized(t, e.resize(alice.signedIn, alice.path, dimensions("op-http-dim-fail01", 1280, 720)), alice.canvasID, 1280, 720, 1)
}
