package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation/invitationtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
)

const (
	acceptPath      = "/api/invitations/accept"
	membershipsPath = "/api/memberships"
)

func acceptance(op, token string) map[string]string {
	return map[string]string{"operation_id": op, "token": token}
}

func canvasPath(tenantID string) string {
	return "/api/tenants/" + tenantID + "/canvases/" + invitationtest.CanvasID(tenantID)
}

// acceptedBody is the exact answer to a confirmed acceptance.
func acceptedBody(tenantID string) string {
	return `{"tenant_id":"` + tenantID + `","canvas_id":"` + invitationtest.CanvasID(tenantID) + `"}` + "\n"
}

// invitee registers and signs in an account without a tenant.
func (e *env) invitee(op, email string) signedIn {
	e.t.Helper()
	e.register(op, email)
	return e.signIn(email)
}

// link creates an invitation of the owner's tenant and returns its token
// and ID.
func (e *env) link(owner signedIn, op string) (token, id string) {
	e.t.Helper()
	token = newToken(e.t)
	return token, e.createInvitation(owner, op, token)["invitation_id"].(string)
}

// accept sends an acceptance and returns the answer and its raw body.
func (e *env) accept(s signedIn, op, token string, extra ...option) (*http.Response, string) {
	e.t.Helper()
	res := e.do(http.MethodPost, acceptPath, acceptance(op, token), append(auth(s), extra...)...)
	return res, e.bodies[len(e.bodies)-1]
}

func (e *env) memberships(s signedIn) string {
	e.t.Helper()
	res := e.do(http.MethodGet, membershipsPath, nil, withSession(s.token), withoutOrigin())
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("memberships: status %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	return e.bodies[len(e.bodies)-1]
}

func moderationsBody(tenantIDs ...string) string {
	items := make([]string, 0, len(tenantIDs))
	for _, id := range tenantIDs {
		items = append(items, `{"tenant_id":"`+id+`","canvas_id":"`+invitationtest.CanvasID(id)+`","role":"moderator"}`)
	}
	return `{"memberships":[` + strings.Join(items, ",") + `]}` + "\n"
}

// serve answers one request without the test bookkeeping of do, so
// goroutines may call it.
func (e *env) serve(method, path string, body any, options ...option) (int, string) {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.RemoteAddr = clientIP + ":40000"
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	for _, o := range options {
		o(r)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	got, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(got)
}

// S2, S17: an authenticated account accepts a pending link, the link is
// consumed, and the account lists and reads exactly that canvas.
func TestAccountAcceptsAnInvitationAndListsTheModeratedCanvas(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, id := e.link(alice, "op-http-acc-link01")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	bobID := access.AccountID(e.sessionAccount(bob))

	if got := e.memberships(bob); got != moderationsBody() {
		t.Fatalf("memberships before = %s", got)
	}
	expectError(t, e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)), http.StatusForbidden, "forbidden")

	res, body := e.accept(bob, "op-http-acc-acc001", token)
	if res.StatusCode != http.StatusOK || body != acceptedBody("tenant-a") {
		t.Fatalf("accept = %d %s", res.StatusCode, body)
	}
	if role, ok := e.invs.Membership("tenant-a", bobID); !ok || role != access.RoleModerator {
		t.Fatalf("membership = %v %v", role, ok)
	}
	if stored := e.invs.Invitations()[id]; stored.AcceptedAt.IsZero() {
		t.Fatal("the link was not consumed")
	}
	if n := len(e.listInvitations(alice)); n != 0 {
		t.Fatalf("pending after acceptance = %d", n)
	}
	if got := e.memberships(bob); got != moderationsBody("tenant-a") {
		t.Fatalf("memberships after = %s", got)
	}
	if res := e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderated canvas = %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	// The owner is not listed as a moderator of its own tenant.
	if got := e.memberships(alice); got != moderationsBody() {
		t.Fatalf("owner memberships = %s", got)
	}
}

// S1: acceptance and the moderated-canvas list need a session; the
// own-canvas route stays owner-only.
func TestAcceptAndMembershipsRequireASession(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, id := e.link(alice, "op-http-acc-link01")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	transactions := e.invs.Transactions()

	expectError(t, e.do(http.MethodPost, acceptPath, acceptance("op-http-acc-anon01", token)), http.StatusUnauthorized, "unauthorized")
	expectError(t, e.do(http.MethodPost, acceptPath, acceptance("op-http-acc-anon02", token), withCSRF(bob.csrf)), http.StatusUnauthorized, "unauthorized")
	expectError(t, e.do(http.MethodPost, acceptPath, acceptance("op-http-acc-anon03", token), withSession("pcs_ended"), withCSRF(bob.csrf)), http.StatusForbidden, "forbidden")
	expectError(t, e.do(http.MethodGet, membershipsPath, nil), http.StatusUnauthorized, "unauthorized")
	if n := e.invs.Transactions(); n != transactions {
		t.Fatalf("transactions = %d, want %d", n, transactions)
	}
	if !e.invs.Invitations()[id].AcceptedAt.IsZero() {
		t.Fatal("an anonymous request consumed the link")
	}

	if res, body := e.accept(bob, "op-http-acc-acc001", token); res.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d %s", res.StatusCode, body)
	}
	expectError(t, e.do(http.MethodGet, "/api/canvas", nil, withSession(bob.token)), http.StatusForbidden, "forbidden")
}

// S4: unknown, revoked, consumed, expired, inactive-tenant, already-member,
// and malformed links get one identical answer and write nothing.
func TestUnusableLinksGetOneIdenticalNotFound(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	e.owner("op-http-acc-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	expired, _ := e.link(alice, "op-http-acc-link01")
	e.clock.Advance(invitation.Lifetime)
	alice = e.signIn("alice@example.com")
	dave := e.signIn("dave@example.com")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	carol := e.invitee("op-http-acc-carol1", "carol@example.com")

	revoked, revokedID := e.link(alice, "op-http-acc-link02")
	if res := e.do(http.MethodPost, revokePath(revokedID), revocation("op-http-acc-rev001"), auth(alice)...); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	consumed, _ := e.link(alice, "op-http-acc-link03")
	if res, body := e.accept(carol, "op-http-acc-acc001", consumed); res.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d %s", res.StatusCode, body)
	}
	inactive, _ := e.link(dave, "op-http-acc-link04")
	e.setStatus("tenant-d", access.SubscriptionInactive)
	member, _ := e.link(alice, "op-http-acc-link05")

	invitations, records, memberships := e.invs.Invitations(), len(e.invs.Records()), e.invs.Memberships()
	var first string
	for i, c := range []struct {
		name  string
		as    signedIn
		token string
	}{
		{"unknown", bob, newToken(t)},
		{"revoked", bob, revoked},
		{"consumed", bob, consumed},
		{"expired", bob, expired},
		{"inactive tenant", bob, inactive},
		{"already member", carol, member},
		{"malformed token", bob, "pci_short"},
		{"session token", bob, bob.token},
	} {
		res, body := e.accept(c.as, "op-http-acc-nf"+strconv.Itoa(1000+i), c.token)
		expectError(t, res, http.StatusNotFound, "not_found")
		body = withoutCorrelation(res, body)
		if first == "" {
			first = body
		}
		if body != first {
			t.Fatalf("%s body = %s, want %s", c.name, body, first)
		}
	}
	if got := e.invs.Invitations(); len(got) != len(invitations) {
		t.Fatalf("invitations = %d, want %d", len(got), len(invitations))
	} else {
		for id, stored := range invitations {
			if got[id] != stored {
				t.Fatalf("invitation %s changed: %+v -> %+v", id, stored, got[id])
			}
		}
	}
	if len(e.invs.Records()) != records || e.invs.Memberships() != memberships {
		t.Fatal("an unusable link wrote an operation or a membership")
	}
	if got := e.memberships(bob); got != moderationsBody() {
		t.Fatalf("bob memberships = %s", got)
	}
}

func TestAcceptRejectsMalformedRequestsWithoutATransaction(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, _ := e.link(alice, "op-http-acc-link01")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	transactions := e.invs.Transactions()

	expectError(t, e.do(http.MethodPost, acceptPath, acceptance("short", token), auth(bob)...), http.StatusBadRequest, "bad_request")
	for name, body := range map[string]string{
		"unknown field": `{"operation_id":"op-http-acc-bad001","token":"` + token + `","tenant_id":"tenant-a"}`,
		"oversized":     `{"operation_id":"op-http-acc-bad002","token":"` + strings.Repeat("x", 8192) + `"}`,
		"trailing data": `{"operation_id":"op-http-acc-bad003","token":"` + token + `"} {}`,
		"malformed":     `{"operation_id":`,
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.do(http.MethodPost, acceptPath, nil, append(auth(bob), withBody("application/json", body))...), http.StatusBadRequest, "bad_request")
		})
	}
	expectError(t, e.do(http.MethodPost, acceptPath, nil, append(auth(bob), withBody("text/plain", "token="+token))...), http.StatusBadRequest, "bad_request")
	if n := e.invs.Transactions(); n != transactions {
		t.Fatalf("transactions = %d, want %d", n, transactions)
	}
	if e.invs.Memberships() != 1 {
		t.Fatal("a malformed request created a membership")
	}
}

// S12, CTRL-IDEM-01: a retry under the same operation ID replays the first
// answer byte for byte, another link under it is a conflict, and after the
// retention the consumed link is unavailable.
func TestAcceptReplaysTheSameAnswer(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	bobOwner := e.owner("op-http-acc-bobow1", "bob@example.com", "tenant-b", access.SubscriptionActive)
	token, _ := e.link(alice, "op-http-acc-link01")
	other, otherID := e.link(bobOwner, "op-http-acc-link02")
	carol := e.invitee("op-http-acc-carol1", "carol@example.com")

	res, first := e.accept(carol, "op-http-acc-acc001", token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d %s", res.StatusCode, first)
	}
	memberships := e.invs.Memberships()
	e.clock.Advance(time.Minute)
	if res, again := e.accept(carol, "op-http-acc-acc001", token); res.StatusCode != http.StatusOK || again != first {
		t.Fatalf("replay = %d %s, want %s", res.StatusCode, again, first)
	}
	res, _ = e.accept(carol, "op-http-acc-acc001", other)
	expectError(t, res, http.StatusConflict, "conflict")
	if e.invs.Memberships() != memberships || !e.invs.Invitations()[otherID].AcceptedAt.IsZero() {
		t.Fatal("a replay or a conflict wrote a second effect")
	}

	e.clock.Advance(invitation.OperationRetention)
	carol = e.signIn("carol@example.com")
	res, _ = e.accept(carol, "op-http-acc-acc001", token)
	expectError(t, res, http.StatusNotFound, "not_found")
	if got := e.memberships(carol); got != moderationsBody("tenant-a") {
		t.Fatalf("memberships = %s", got)
	}
}

// S13, CTRL-WEB-01: Origin and CSRF are checked before the session is read,
// the body is decoded, or the domain runs.
func TestAcceptChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, id := e.link(alice, "op-http-acc-link01")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	other := e.signIn("bob@example.com")

	for name, opts := range map[string][]option{
		"missing CSRF":                 {withSession(bob.token)},
		"invalid CSRF":                 {withSession(bob.token), withCSRF("not-the-token")},
		"other session CSRF":           {withSession(bob.token), withCSRF(other.csrf)},
		"another account CSRF":         {withSession(bob.token), withCSRF(alice.csrf)},
		"absent Origin":                {withSession(bob.token), withCSRF(bob.csrf), withoutOrigin()},
		"hostile Origin":               {withSession(bob.token), withCSRF(bob.csrf), withOrigin("https://evil.test")},
		"null Origin":                  {withSession(bob.token), withCSRF(bob.csrf), withOrigin("null")},
		"missing CSRF, malformed body": {withSession(bob.token), withBody("application/json", "{")},
	} {
		t.Run(name, func(t *testing.T) {
			transactions, lookups := e.invs.Transactions(), e.invs.InvitationLookups()+e.invs.OperationLookups()
			expectError(t, e.do(http.MethodPost, acceptPath, acceptance("op-http-acc-web001", token), opts...), http.StatusForbidden, "forbidden")
			if e.invs.Transactions() != transactions || e.invs.InvitationLookups()+e.invs.OperationLookups() != lookups {
				t.Fatal("a refused acceptance reached the invitation store")
			}
			if !e.invs.Invitations()[id].AcceptedAt.IsZero() || e.invs.Memberships() != 1 || len(e.invs.Records()) != 1 {
				t.Fatal("a refused acceptance changed storage")
			}
		})
	}

	transactions := e.invs.Transactions()
	if res, body := e.accept(bob, "op-http-acc-web001", token); res.StatusCode != http.StatusOK || body != acceptedBody("tenant-a") {
		t.Fatalf("allowed Origin and valid CSRF = %d %s", res.StatusCode, body)
	}
	if n := e.invs.Transactions(); n != transactions+1 {
		t.Fatalf("transactions = %d, want %d", n, transactions+1)
	}
}

// S14, CTRL-RATE-01: acceptance is limited per account before the body is
// read, recovers after the window, and one account's budget leaves another
// streamer's invitee unaffected.
func TestAcceptIsLimitedPerAccount(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	dave := e.owner("op-http-acc-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	carol := e.invitee("op-http-acc-carol1", "carol@example.com")
	token, _ := e.link(alice, "op-http-acc-link01")
	davesToken, _ := e.link(dave, "op-http-acc-link02")

	for i := range 20 {
		res, _ := e.accept(bob, "op-http-acc-rate"+strconv.Itoa(100+i), newToken(t))
		expectError(t, res, http.StatusNotFound, "not_found")
	}
	transactions, lookups := e.invs.Transactions(), e.invs.InvitationLookups()
	e.clock.Advance(30 * time.Minute)
	res, _ := e.accept(bob, "op-http-acc-rate200", token)
	if got := res.Header.Get("Retry-After"); got != "1800" {
		t.Fatalf("Retry-After = %q, want 1800", got)
	}
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	// A malformed body is not read: the limit answers first.
	expectError(t, e.do(http.MethodPost, acceptPath, nil, append(auth(bob), withBody("application/json", "{"))...), http.StatusTooManyRequests, "rate_limited")
	if e.invs.Transactions() != transactions || e.invs.InvitationLookups() != lookups {
		t.Fatal("a rate-limited acceptance reached the invitation store")
	}
	if got := reasonCount(e.ingress, ingress.ReasonRateAccount); got != 2 {
		t.Fatalf("rate_account = %d, want 2", got)
	}
	// The limited account still lists its memberships, and another
	// streamer's invitee keeps its own budget.
	e.memberships(bob)
	if res, body := e.accept(carol, "op-http-acc-car001", davesToken); res.StatusCode != http.StatusOK || body != acceptedBody("tenant-d") {
		t.Fatalf("carol accept = %d %s", res.StatusCode, body)
	}

	e.clock.Advance(30 * time.Minute)
	bob = e.signIn("bob@example.com")
	if res, body := e.accept(bob, "op-http-acc-rate201", token); res.StatusCode != http.StatusOK || body != acceptedBody("tenant-a") {
		t.Fatalf("accept after the window = %d %s", res.StatusCode, body)
	}
}

// S14: the ingress per-address limit on unsafe methods also covers the
// acceptance route.
func TestAcceptIsCoveredByTheIngressAddressLimit(t *testing.T) {
	limits := ingress.DefaultLimits
	limits.UnsafePerIP = ratelimit.Rate{Limit: 5, Window: time.Minute}
	e := newEnvWith(t, httpapi.DefaultLimits, limits)
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	const address = "203.0.113.70"

	for i := range 5 {
		res, _ := e.accept(bob, "op-http-acc-ip"+strconv.Itoa(1000+i), newToken(t), fromIP(address))
		expectError(t, res, http.StatusNotFound, "not_found")
	}
	transactions := e.invs.Transactions()
	res, _ := e.accept(bob, "op-http-acc-ip2000", newToken(t), fromIP(address))
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	if e.invs.Transactions() != transactions {
		t.Fatal("an address-limited acceptance reached the invitation store")
	}
	if ip, account := reasonCount(e.ingress, ingress.ReasonRateIP), reasonCount(e.ingress, ingress.ReasonRateAccount); ip != 1 || account != 0 {
		t.Fatalf("rate_ip = %d, rate_account = %d, want 1 and 0", ip, account)
	}
}

// S8: a new moderator is refused every owner, invitation, and operator
// route without a write, and reads only the moderated canvas.
func TestModeratorCapabilityMatrixOverHTTP(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	e.owner("op-http-acc-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	e.register("op-http-acc-erin01", "erin@example.com")
	token, _ := e.link(alice, "op-http-acc-link01")
	_, pendingID := e.link(alice, "op-http-acc-link02")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	if res, body := e.accept(bob, "op-http-acc-acc001", token); res.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d %s", res.StatusCode, body)
	}

	invitations, records := e.invs.Invitations(), len(e.invs.Records())
	subsRecords, tenants := len(e.subs.Records()), len(e.subs.Tenants())
	for name, c := range map[string]struct {
		method, path string
		body         any
	}{
		"resize":       {http.MethodPut, canvasPath("tenant-a") + "/dimensions", dimensions("op-http-acc-mx001", 1280, 720)},
		"create":       {http.MethodPost, invitationsPath, creation("op-http-acc-mx002", newToken(t))},
		"list":         {http.MethodGet, invitationsPath, nil},
		"revoke":       {http.MethodPost, revokePath(pendingID), revocation("op-http-acc-mx003")},
		"activation":   {http.MethodPost, "/api/activations", activation("op-http-acc-mx004", "erin@example.com")},
		"deactivation": {http.MethodPost, "/api/deactivations", activation("op-http-acc-mx005", "alice@example.com")},
		"own canvas":   {http.MethodGet, "/api/canvas", nil},
		"other tenant": {http.MethodGet, canvasPath("tenant-d"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.do(c.method, c.path, c.body, auth(bob)...), http.StatusForbidden, "forbidden")
		})
	}
	if e.canvases.ApplyCalls() != 0 {
		t.Fatal("a moderator reached the canvas store")
	}
	if got := e.invs.Invitations(); len(got) != len(invitations) || got[pendingID] != invitations[pendingID] || len(e.invs.Records()) != records {
		t.Fatal("a moderator changed an invitation")
	}
	if len(e.subs.Records()) != subsRecords || len(e.subs.Tenants()) != tenants {
		t.Fatal("a moderator changed a subscription")
	}
	if res := e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderated canvas = %d", res.StatusCode)
	}
}

// S9: two tenants' links accepted at the same time bind each account to
// exactly its tenant, with no access or metadata of the other.
func TestSimultaneousAcceptancesOfTwoTenantsBindExactly(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	dave := e.owner("op-http-acc-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	aliceToken, _ := e.link(alice, "op-http-acc-link01")
	daveToken, _ := e.link(dave, "op-http-acc-link02")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	carol := e.invitee("op-http-acc-carol1", "carol@example.com")

	type answer struct {
		status int
		body   string
	}
	answers := make([]answer, 2)
	var wg sync.WaitGroup
	for i, c := range []struct {
		as    signedIn
		token string
	}{{bob, aliceToken}, {carol, daveToken}} {
		wg.Go(func() {
			status, body := e.serve(http.MethodPost, acceptPath, acceptance("op-http-acc-two00"+strconv.Itoa(i), c.token), auth(c.as)...)
			answers[i] = answer{status, body}
		})
	}
	wg.Wait()
	if answers[0] != (answer{http.StatusOK, acceptedBody("tenant-a")}) || answers[1] != (answer{http.StatusOK, acceptedBody("tenant-d")}) {
		t.Fatalf("answers = %+v", answers)
	}

	if got := e.memberships(bob); got != moderationsBody("tenant-a") {
		t.Fatalf("bob memberships = %s", got)
	}
	if got := e.memberships(carol); got != moderationsBody("tenant-d") {
		t.Fatalf("carol memberships = %s", got)
	}
	for _, c := range []struct {
		as         signedIn
		own, other string
	}{{bob, "tenant-a", "tenant-d"}, {carol, "tenant-d", "tenant-a"}} {
		if res := e.do(http.MethodGet, canvasPath(c.own), nil, withSession(c.as.token)); res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", c.own, res.StatusCode)
		}
		if strings.Contains(e.bodies[len(e.bodies)-1], c.other) {
			t.Fatalf("the %s canvas mentions %s", c.own, c.other)
		}
		expectError(t, e.do(http.MethodGet, canvasPath(c.other), nil, withSession(c.as.token)), http.StatusForbidden, "forbidden")
	}
}

// The acceptance path has one segment fewer than the revocation route, so
// the mux never treats "accept" as an invitation ID.
func TestAcceptIsNotRoutedAsARevocation(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, _ := e.link(alice, "op-http-acc-link01")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")

	records := len(e.invs.Records())
	// An owner posting a revocation body to the acceptance path gets the
	// acceptance's answer to a missing token, not the revocation route's
	// answer to the malformed invitation ID "accept" (400).
	expectError(t, e.do(http.MethodPost, acceptPath, revocation("op-http-acc-rt0001"), auth(alice)...), http.StatusNotFound, "not_found")
	expectError(t, e.do(http.MethodPost, acceptPath+"/revoke", revocation("op-http-acc-rt0003"), auth(alice)...), http.StatusBadRequest, "bad_request")
	if len(e.invs.Records()) != records {
		t.Fatal("the acceptance path recorded an operation")
	}
	if res, body := e.accept(bob, "op-http-acc-rt0002", token); res.StatusCode != http.StatusOK || body != acceptedBody("tenant-a") {
		t.Fatalf("accept = %d %s", res.StatusCode, body)
	}
	if record := e.invs.Records()["op-http-acc-rt0002"]; record.Kind != invitation.OperationAccept {
		t.Fatalf("record kind = %q, want accept", record.Kind)
	}
}

// S10, S15: the acceptance log carries the correlation, the tenant, and the
// invitation only; no log line or response holds a link token, a CSRF or
// session token, or an email, on success or rejection.
func TestAcceptLogsAreCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-acc-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, id := e.link(alice, "op-http-acc-link01")
	other, _ := e.link(alice, "op-http-acc-link02")
	bob := e.invitee("op-http-acc-bob001", "bob@example.com")
	mallory := e.invitee("op-http-acc-mall01", "mallory@example.com")
	unknown, malformed := newToken(t), newToken(t)
	// Sign-in answers carry the account's own email; only the answers from
	// here on are checked.
	start := len(e.bodies)

	res, _ := e.accept(bob, "op-http-acc-log001", token)
	correlationID := res.Header.Get(httpx.CorrelationHeader)
	if got := e.invs.Records()["op-http-acc-log001"].CorrelationID; got == "" || got != correlationID {
		t.Fatalf("record correlation %q, response %q", got, correlationID)
	}
	e.accept(bob, "op-http-acc-log001", token)
	e.accept(bob, "op-http-acc-log001", other)
	e.accept(mallory, "op-http-acc-log002", token)
	e.accept(mallory, "op-http-acc-log003", unknown)
	e.accept(mallory, "op-http-acc-log004", unknown+"x")
	e.accept(mallory, "op-http-acc-log005", other, withCSRF("not-the-token"))
	e.accept(mallory, "op-http-acc-log006", other, withOrigin("https://evil.test"))
	e.do(http.MethodPost, acceptPath, nil, append(auth(mallory), withBody("application/json", `{"operation_id":"op-http-acc-log007","token":"`+malformed+`","x":1}`))...)
	e.do(http.MethodPost, acceptPath, acceptance("op-http-acc-log008", other))
	e.memberships(bob)

	logs := e.logs.String()
	accepted := 0
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["msg"] != "invitation accepted" {
			continue
		}
		accepted++
		want := map[string]any{"msg": "invitation accepted", "tenant_id": "tenant-a", "invitation_id": id}
		for k, v := range want {
			if record[k] != v {
				t.Fatalf("accepted log %s = %v, want %v", k, record[k], v)
			}
		}
		if got, _ := record["correlation_id"].(string); got == "" {
			t.Fatalf("accepted log without correlation_id: %s", line)
		}
		delete(record, "time")
		delete(record, "level")
		delete(record, "correlation_id")
		if len(record) != len(want) {
			t.Fatalf("accepted log has extra fields: %v", record)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted logs = %d, want 2 (the acceptance and its replay)", accepted)
	}
	if logLine(logs, "invitation accepted", correlationID) == nil {
		t.Fatal("no accepted log carries the request correlation")
	}
	for name, s := range map[string]string{
		"link prefix":     "pci_",
		"token body":      token[4:],
		"other token":     other[4:],
		"unknown token":   unknown[4:],
		"malformed token": malformed[4:],
		"bob session":     bob.token,
		"bob CSRF":        bob.csrf,
		"mallory session": mallory.token,
		"mallory CSRF":    mallory.csrf,
		"alice CSRF":      alice.csrf,
		"bob email":       "bob@example.com",
		"mallory email":   "mallory@example.com",
		"alice email":     "alice@example.com",
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
	for _, body := range e.bodies[start:] {
		for _, s := range []string{token[4:], other[4:], unknown[4:], malformed[4:], bob.token, bob.csrf, mallory.token, mallory.csrf, "bob@example.com", "mallory@example.com"} {
			if strings.Contains(body, s) {
				t.Errorf("a response body contains a secret or an email: %s", body)
			}
		}
	}
}

func TestConfigurationRequiresTheAcceptLimit(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	noLimit := e.config
	noLimit.Limits.InvitationAcceptsPerAccount = httpapi.Rate{}
	noWindow := e.config
	noWindow.Limits.InvitationAcceptsPerAccount = httpapi.Rate{Limit: 20}
	for name, config := range map[string]httpapi.Config{"no limit": noLimit, "no window": noWindow} {
		if _, err := httpapi.New(config); err == nil {
			t.Errorf("%s: configuration accepted", name)
		}
	}
	if httpapi.DefaultLimits.InvitationAcceptsPerAccount != (httpapi.Rate{Limit: 20, Window: time.Hour}) {
		t.Fatalf("default accept limit = %+v", httpapi.DefaultLimits.InvitationAcceptsPerAccount)
	}
}
