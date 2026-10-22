package httpapi_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	invitationsPath = "/api/invitations"
	unknownID       = "0123456789abcdef0123456789abcdef"
)

func newToken(t *testing.T) string {
	t.Helper()
	token, err := secret.Generate(secret.KindInvitationToken)
	if err != nil {
		t.Fatal(err)
	}
	return token.Reveal()
}

func creation(op, token string) map[string]string {
	return map[string]string{"operation_id": op, "token": token}
}

func revocation(op string) map[string]string {
	return map[string]string{"operation_id": op}
}

func revokePath(id string) string { return invitationsPath + "/" + id + "/revoke" }

// owner registers an account that owns tenantID with the given status, and
// signs it in.
func (e *env) owner(op, email, tenantID string, status access.SubscriptionStatus) signedIn {
	e.t.Helper()
	id := e.register(op, email)
	e.invs.AddTenant(tenantID, id, status)
	return e.signIn(email)
}

func auth(s signedIn) []option { return []option{withSession(s.token), withCSRF(s.csrf)} }

func (e *env) createInvitation(s signedIn, op, token string) map[string]any {
	e.t.Helper()
	res := e.do(http.MethodPost, invitationsPath, creation(op, token), auth(s)...)
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("create invitation: status %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	return decode(e.t, res)
}

func (e *env) listInvitations(s signedIn) []any {
	e.t.Helper()
	res := e.do(http.MethodGet, invitationsPath, nil, withSession(s.token), withoutOrigin())
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("list invitations: status %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	body := decode(e.t, res)
	list, ok := body["invitations"].([]any)
	if !ok || len(body) != 1 {
		e.t.Fatalf("list body = %v", body)
	}
	return list
}

// withoutCorrelation removes the per-request correlation ID from a body.
func withoutCorrelation(res *http.Response, body string) string {
	return strings.Replace(body, res.Header.Get(httpx.CorrelationHeader), "", 1)
}

func TestOwnerCreatesListsAndRevokesInvitations(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token := newToken(t)

	created := e.createInvitation(alice, "op-http-inv-create1", token)
	id, _ := created["invitation_id"].(string)
	if len(created) != 3 || len(id) != 32 {
		t.Fatalf("create body = %v", created)
	}
	want := e.clock.Now()
	if created["created_at"] != want.Format(time.RFC3339Nano) || created["expires_at"] != want.Add(7*24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("create times = %v, now %s", created, want)
	}
	if stored := e.invs.Invitations()[id]; stored.TenantID != "tenant-a" {
		t.Fatalf("stored invitation = %+v", stored)
	}

	list := e.listInvitations(alice)
	if len(list) != 1 {
		t.Fatalf("pending = %v", list)
	}
	if item := list[0].(map[string]any); item["invitation_id"] != id || item["created_at"] != created["created_at"] || item["expires_at"] != created["expires_at"] || len(item) != 3 {
		t.Fatalf("pending item = %v, created %v", item, created)
	}

	revoke := func(op string) (int, string) {
		res := e.do(http.MethodPost, revokePath(id), revocation(op), auth(alice)...)
		return res.StatusCode, e.bodies[len(e.bodies)-1]
	}
	status, first := revoke("op-http-inv-revoke1")
	if status != http.StatusOK || first != `{"invitation_id":"`+id+`","status":"revoked"}`+"\n" {
		t.Fatalf("revoke = %d %s", status, first)
	}
	// A second revocation under a new operation answers revoked again.
	if status, again := revoke("op-http-inv-revoke2"); status != http.StatusOK || again != first {
		t.Fatalf("second revoke = %d %s", status, again)
	}
	if n := len(e.listInvitations(alice)); n != 0 {
		t.Fatalf("pending after revoke = %d", n)
	}
	for _, body := range e.bodies {
		if strings.Contains(body, token) {
			t.Fatalf("a response body contains the token: %s", body)
		}
	}
}

// S2: moderators, strangers, and owners of an inactive subscription are
// denied on every route before any invitation or operation is read.
func TestOnlyAnActiveOwnerManagesInvitations(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	id := e.createInvitation(alice, "op-http-inv-create1", newToken(t))["invitation_id"].(string)
	moderatorID := e.register("op-http-inv-moder1", "moderator@example.com")
	e.invs.AddMember("tenant-a", moderatorID, access.RoleModerator)
	e.register("op-http-inv-strng1", "stranger@example.com")
	e.owner("op-http-inv-inact1", "inactive@example.com", "tenant-c", access.SubscriptionInactive)

	for _, email := range []string{"moderator@example.com", "stranger@example.com", "inactive@example.com"} {
		t.Run(email, func(t *testing.T) {
			s := e.signIn(email)
			invitations, records := e.invs.Invitations(), e.invs.Records()
			lookups := e.invs.InvitationLookups() + e.invs.OperationLookups()
			expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-deny01", newToken(t)), auth(s)...), http.StatusForbidden, "forbidden")
			expectError(t, e.do(http.MethodGet, invitationsPath, nil, withSession(s.token)), http.StatusForbidden, "forbidden")
			expectError(t, e.do(http.MethodPost, revokePath(id), revocation("op-http-inv-deny02"), auth(s)...), http.StatusForbidden, "forbidden")
			// The domain checks TE-001 on the locked tenant before it reads
			// any invitation or operation.
			if n := e.invs.InvitationLookups() + e.invs.OperationLookups() - lookups; n != 0 {
				t.Fatalf("invitation and operation lookups = %d, want 0", n)
			}
			if got := e.invs.Invitations(); len(got) != len(invitations) || got[id] != invitations[id] {
				t.Fatalf("invitations changed: %v", got)
			}
			if len(e.invs.Records()) != len(records) {
				t.Fatal("a denied request recorded an operation")
			}
		})
	}
}

// S5: an unknown ID and another tenant's ID get the same answer.
func TestRevokingAnUnknownOrForeignInvitationIsNotFound(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	bob := e.owner("op-http-inv-bob001", "bob@example.com", "tenant-b", access.SubscriptionActive)
	bobsID := e.createInvitation(bob, "op-http-inv-create2", newToken(t))["invitation_id"].(string)

	foreign := e.do(http.MethodPost, revokePath(bobsID), revocation("op-http-inv-nf0001"), auth(alice)...)
	foreignBody := withoutCorrelation(foreign, e.bodies[len(e.bodies)-1])
	unknown := e.do(http.MethodPost, revokePath(unknownID), revocation("op-http-inv-nf0002"), auth(alice)...)
	unknownBody := withoutCorrelation(unknown, e.bodies[len(e.bodies)-1])
	if foreign.StatusCode != http.StatusNotFound || unknown.StatusCode != http.StatusNotFound || foreignBody != unknownBody {
		t.Fatalf("foreign = %d %s, unknown = %d %s", foreign.StatusCode, foreignBody, unknown.StatusCode, unknownBody)
	}
	expectError(t, foreign, http.StatusNotFound, "not_found")
	if stored := e.invs.Invitations()[bobsID]; !stored.RevokedAt.IsZero() {
		t.Fatal("another tenant revoked the invitation")
	}
	if n := len(e.listInvitations(bob)); n != 1 {
		t.Fatalf("bob's pending = %d, want 1", n)
	}
	if n := len(e.listInvitations(alice)); n != 0 {
		t.Fatalf("alice sees %d invitations of another tenant", n)
	}
}

// S9, CTRL-IDEM-01: retries replay the first answer, and a different
// payload under the same operation ID is rejected.
func TestInvitationOperationsReplayTheirAnswer(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token := newToken(t)

	created := e.createInvitation(alice, "op-http-inv-create1", token)
	first := e.bodies[len(e.bodies)-1]
	e.clock.Advance(time.Minute)
	e.createInvitation(alice, "op-http-inv-create1", token)
	if again := e.bodies[len(e.bodies)-1]; again != first {
		t.Fatalf("replay body = %s, want %s", again, first)
	}
	expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-create1", newToken(t)), auth(alice)...), http.StatusConflict, "conflict")
	// The same token under another operation is a conflict too.
	expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-create2", token), auth(alice)...), http.StatusConflict, "conflict")
	if n := len(e.invs.Invitations()); n != 1 {
		t.Fatalf("invitations = %d, want 1", n)
	}

	id := created["invitation_id"].(string)
	res := e.do(http.MethodPost, revokePath(id), revocation("op-http-inv-revoke1"), auth(alice)...)
	revoked := e.bodies[len(e.bodies)-1]
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d %s", res.StatusCode, revoked)
	}
	revokedAt := e.invs.Invitations()[id].RevokedAt
	e.clock.Advance(time.Minute)
	if res := e.do(http.MethodPost, revokePath(id), revocation("op-http-inv-revoke1"), auth(alice)...); res.StatusCode != http.StatusOK || e.bodies[len(e.bodies)-1] != revoked {
		t.Fatalf("revoke replay = %d %s, want %s", res.StatusCode, e.bodies[len(e.bodies)-1], revoked)
	}
	if got := e.invs.Invitations()[id].RevokedAt; !got.Equal(revokedAt) {
		t.Fatalf("revoked_at moved from %s to %s", revokedAt, got)
	}
	expectError(t, e.do(http.MethodPost, revokePath(unknownID), revocation("op-http-inv-revoke1"), auth(alice)...), http.StatusConflict, "conflict")
}

func TestInvitationRequestsRejectMalformedInput(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token := newToken(t)

	// Rejected by the domain before storage.
	expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-bad001", "pci_short"), auth(alice)...), http.StatusBadRequest, "bad_request")
	expectError(t, e.do(http.MethodPost, invitationsPath, creation("short", token), auth(alice)...), http.StatusBadRequest, "bad_request")
	expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-bad002", "pcs_"+token[4:]), auth(alice)...), http.StatusBadRequest, "bad_request")
	expectError(t, e.do(http.MethodPost, revokePath("not-an-id"), revocation("op-http-inv-bad003"), auth(alice)...), http.StatusBadRequest, "bad_request")
	transactions := e.invs.Transactions()

	for name, c := range map[string]struct{ path, body string }{
		"create not JSON":      {invitationsPath, "token=" + token},
		"create unknown field": {invitationsPath, `{"operation_id":"op-http-inv-bad004","token":"` + token + `","tenant_id":"tenant-b"}`},
		"create oversized":     {invitationsPath, `{"operation_id":"op-http-inv-bad005","token":"` + strings.Repeat("x", 8192) + `"}`},
		"create trailing data": {invitationsPath, `{"operation_id":"op-http-inv-bad006","token":"` + token + `"} {}`},
		"revoke unknown field": {revokePath(unknownID), `{"operation_id":"op-http-inv-bad007","status":"accepted"}`},
		"revoke malformed":     {revokePath(unknownID), `{"operation_id":`},
	} {
		t.Run(name, func(t *testing.T) {
			contentType := "application/json"
			if name == "create not JSON" {
				contentType = "application/x-www-form-urlencoded"
			}
			expectError(t, e.do(http.MethodPost, c.path, nil, append(auth(alice), withBody(contentType, c.body))...), http.StatusBadRequest, "bad_request")
			if e.invs.Transactions() != transactions {
				t.Fatal("a malformed body reached the store")
			}
		})
	}
	if n := len(e.invs.Invitations()); n != 0 {
		t.Fatalf("invitations = %d, want 0", n)
	}
}

// S10, CTRL-WEB-01 early primitive: session, Origin, and CSRF are checked
// before the body is read or the domain runs.
func TestInvitationRoutesCheckSessionOriginAndCSRFFirst(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	other := e.signIn("alice@example.com")
	token := newToken(t)

	rejected := map[string]struct {
		opts   []option
		status int
		code   string
	}{
		"no session":                   {[]option{withCSRF(alice.csrf)}, http.StatusUnauthorized, "unauthorized"},
		"missing CSRF":                 {[]option{withSession(alice.token)}, http.StatusForbidden, "forbidden"},
		"invalid CSRF":                 {[]option{withSession(alice.token), withCSRF("not-the-token")}, http.StatusForbidden, "forbidden"},
		"other session CSRF":           {[]option{withSession(alice.token), withCSRF(other.csrf)}, http.StatusForbidden, "forbidden"},
		"absent Origin":                {append(auth(alice), withoutOrigin()), http.StatusForbidden, "forbidden"},
		"hostile Origin":               {append(auth(alice), withOrigin("https://evil.test")), http.StatusForbidden, "forbidden"},
		"missing CSRF, malformed body": {[]option{withSession(alice.token), withBody("application/json", "{")}, http.StatusForbidden, "forbidden"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			expectError(t, e.do(http.MethodPost, invitationsPath, creation("op-http-inv-web001", token), c.opts...), c.status, c.code)
			expectError(t, e.do(http.MethodPost, revokePath(unknownID), revocation("op-http-inv-web002"), c.opts...), c.status, c.code)
			if n := e.invs.Transactions(); n != 0 {
				t.Fatalf("transactions = %d, want 0", n)
			}
		})
	}
	expectError(t, e.do(http.MethodGet, invitationsPath, nil), http.StatusUnauthorized, "unauthorized")
	if n := e.invs.Transactions(); n != 0 {
		t.Fatalf("transactions = %d, want 0", n)
	}

	e.createInvitation(alice, "op-http-inv-web001", token)
	if n := e.invs.Transactions(); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
}

// S11, CTRL-RATE-01 early primitive: creation is limited per account.
func TestInvitationCreationIsLimitedPerAccount(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	bob := e.owner("op-http-inv-bob001", "bob@example.com", "tenant-b", access.SubscriptionActive)

	for i := range 20 {
		e.createInvitation(alice, "op-http-inv-rate"+strconv.Itoa(100+i), newToken(t))
	}
	transactions := e.invs.Transactions()
	e.clock.Advance(30 * time.Minute)
	res := e.do(http.MethodPost, invitationsPath, creation("op-http-inv-rate200", newToken(t)), auth(alice)...)
	if got := res.Header.Get("Retry-After"); got != "1800" {
		t.Fatalf("Retry-After = %q, want 1800", got)
	}
	expectError(t, res, http.StatusTooManyRequests, "rate_limited")
	// The limit is checked before the body is read and the domain runs.
	expectError(t, e.do(http.MethodPost, invitationsPath, nil, append(auth(alice), withBody("application/json", "{"))...), http.StatusTooManyRequests, "rate_limited")
	if e.invs.Transactions() != transactions {
		t.Fatal("a rate-limited request reached the store")
	}
	// Listing and revoking stay available, and another owner has its own budget.
	if n := len(e.listInvitations(alice)); n != 20 {
		t.Fatalf("pending = %d, want 20", n)
	}
	e.createInvitation(bob, "op-http-inv-rate300", newToken(t))

	e.clock.Advance(30 * time.Minute)
	e.createInvitation(alice, "op-http-inv-rate201", newToken(t))
}

// S6, S12: invitation logs carry the correlation and never a token, the
// CSRF token, the session token, or an email; stored operations keep the
// request correlation.
func TestInvitationLogsAreCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.Limits{
		SignInPerAccount:      httpapi.Rate{Limit: 100, Window: time.Minute},
		SignInPerIP:           httpapi.Rate{Limit: 100, Window: time.Minute},
		RegisterPerIP:         httpapi.Rate{Limit: 100, Window: time.Minute},
		InvitationsPerAccount: httpapi.Rate{Limit: 4, Window: time.Minute},
	})
	alice := e.owner("op-http-inv-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	e.register("op-http-inv-mall01", "mallory@example.com")
	mallory := e.signIn("mallory@example.com")
	token, other, malformed := newToken(t), newToken(t), newToken(t)

	res := e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log001", token), auth(alice)...)
	created := decode(t, res)
	id := created["invitation_id"].(string)
	if got := e.invs.Records()["op-http-inv-log001"].CorrelationID; got == "" || got != res.Header.Get(httpx.CorrelationHeader) {
		t.Fatalf("record correlation %q, response %q", got, res.Header.Get(httpx.CorrelationHeader))
	}
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log001", token), auth(alice)...)
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log001", other), auth(alice)...)
	e.do(http.MethodPost, invitationsPath, nil, append(auth(alice), withBody("application/json", `{"operation_id":"op-http-inv-log002","token":"`+malformed+`","x":1}`))...)
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log003", other), auth(alice)...)
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log004", other+"x"), auth(alice)...)
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log005", other), withSession(alice.token), withCSRF("not-the-token"))
	e.do(http.MethodPost, invitationsPath, creation("op-http-inv-log006", other), auth(mallory)...)
	e.do(http.MethodGet, invitationsPath, nil, withSession(alice.token))
	e.do(http.MethodGet, invitationsPath, nil, withSession(mallory.token))
	res = e.do(http.MethodPost, revokePath(id), revocation("op-http-inv-log007"), auth(alice)...)
	if got := e.invs.Records()["op-http-inv-log007"].CorrelationID; got == "" || got != res.Header.Get(httpx.CorrelationHeader) {
		t.Fatalf("revoke record correlation %q, response %q", got, res.Header.Get(httpx.CorrelationHeader))
	}
	e.do(http.MethodPost, revokePath(unknownID), revocation("op-http-inv-log008"), auth(alice)...)
	e.do(http.MethodPost, revokePath(id), revocation("op-http-inv-log009"), auth(mallory)...)

	logs := e.logs.String()
	found := map[string]string{}
	for _, line := range strings.Split(logs, "\n") {
		for _, msg := range []string{"invitation created", "invitation revoked"} {
			if strings.Contains(line, `"msg":"`+msg+`"`) {
				found[msg] = line
			}
		}
	}
	// invitation.Revocation carries no tenant ID, so only creation logs it.
	for msg, wants := range map[string][]string{
		"invitation created": {`"correlation_id"`, `"tenant_id":"tenant-a"`, `"invitation_id":"` + id + `"`},
		"invitation revoked": {`"correlation_id"`, `"invitation_id":"` + id + `"`, `"status":"revoked"`},
	} {
		for _, want := range wants {
			if !strings.Contains(found[msg], want) {
				t.Fatalf("%s log %q lacks %s", msg, found[msg], want)
			}
		}
	}
	if len(found) != 2 {
		t.Fatalf("invitation logs = %v", found)
	}
	secrets := map[string]string{
		"token":           token,
		"other token":     other,
		"malformed token": malformed,
		"token body":      token[4:],
		"session token":   alice.token,
		"CSRF token":      alice.csrf,
		"mallory token":   mallory.token,
		"mallory CSRF":    mallory.csrf,
		"email":           "alice@example.com",
		"other email":     "mallory@example.com",
	}
	for name, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
	for _, body := range e.bodies {
		for _, s := range []string{token, other, malformed, token[4:]} {
			if strings.Contains(body, s) {
				t.Errorf("a response body contains an invitation token: %s", body)
			}
		}
	}
}

func TestConfigurationRequiresInvitationsAndItsLimit(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	if _, err := httpapi.New(e.config); err != nil {
		t.Fatalf("complete configuration: %v", err)
	}
	noInvitations := e.config
	noInvitations.Invitations = nil
	noLimit := e.config
	noLimit.Limits.InvitationsPerAccount = httpapi.Rate{}
	noWindow := e.config
	noWindow.Limits.InvitationsPerAccount = httpapi.Rate{Limit: 20}
	for name, config := range map[string]httpapi.Config{
		"no invitations": noInvitations,
		"no limit":       noLimit,
		"no window":      noWindow,
	} {
		if _, err := httpapi.New(config); err == nil {
			t.Errorf("%s: configuration accepted", name)
		}
	}
}
