package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
)

const membersPath = "/api/members"

func memberRevokePath(accountID string) string { return membersPath + "/" + accountID + "/revoke" }

// countingInvitations counts the member calls the API makes, so tests can
// prove a rejected request never reached the domain.
type countingInvitations struct {
	*invitation.Service
	calls *calls
}

func (c countingInvitations) Members(ctx context.Context, owner access.AccountID) ([]invitation.Member, error) {
	c.calls.add("Members")
	return c.Service.Members(ctx, owner)
}

func (c countingInvitations) RevokeMember(ctx context.Context, request invitation.RevokeMemberRequest) (invitation.MemberRevocation, error) {
	c.calls.add("RevokeMember")
	return c.Service.RevokeMember(ctx, request)
}

var _ httpapi.Invitations = countingInvitations{}

// moderated returns alice, the owner of tenant-a, and bob, who accepted
// one of her links, with bob's account ID.
func (e *env) moderated() (alice, bob signedIn, bobID access.AccountID) {
	e.t.Helper()
	alice = e.owner("op-http-mem-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	token, _ := e.link(alice, "op-http-mem-link01")
	bob = e.invitee("op-http-mem-bob001", "bob@example.com")
	if res, body := e.accept(bob, "op-http-mem-acc001", token); res.StatusCode != http.StatusOK {
		e.t.Fatalf("accept = %d %s", res.StatusCode, body)
	}
	return alice, bob, access.AccountID(e.sessionAccount(bob))
}

func (e *env) revokeMember(s signedIn, op, accountID string, extra ...option) (*http.Response, string) {
	e.t.Helper()
	res := e.do(http.MethodPost, memberRevokePath(accountID), revocation(op), append(auth(s), extra...)...)
	return res, e.bodies[len(e.bodies)-1]
}

func (e *env) members(s signedIn) string {
	e.t.Helper()
	res := e.do(http.MethodGet, membersPath, nil, withSession(s.token), withoutOrigin())
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("members: status %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	return e.bodies[len(e.bodies)-1]
}

func revokedBody(tenantID string, accountID access.AccountID) string {
	return `{"tenant_id":"` + tenantID + `","account_id":"` + string(accountID) + `"}` + "\n"
}

// memberState is every effect a revocation may leave on the invitation store.
type memberState struct {
	role        access.Role
	member      bool
	version     uint64
	cuts        int
	revocations int
}

func (e *env) memberState(tenantID string, account access.AccountID) memberState {
	role, member := e.invs.Membership(tenantID, account)
	return memberState{role, member, e.invs.AccessVersion(tenantID), len(e.invs.Cuts()), len(e.invs.MemberRevocations())}
}

// S1, S4, S12, S16: the owner lists the moderator by email, revokes it, and
// the revocation is effective at the answer; a retry replays the same bytes.
func TestOwnerListsAndRevokesAModerator(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice, bob, bobID := e.moderated()
	acceptedAt := e.clock.Now().UTC().Format(time.RFC3339Nano)

	want := `{"members":[{"account_id":"` + string(bobID) + `","email":"bob@example.com","accepted_at":"` + acceptedAt + `"}]}` + "\n"
	if got := e.members(alice); got != want {
		t.Fatalf("members = %s, want %s", got, want)
	}
	if res := e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderated canvas before = %d", res.StatusCode)
	}

	e.clock.Advance(time.Minute)
	res, first := e.revokeMember(alice, "op-http-mem-rev001", string(bobID))
	if res.StatusCode != http.StatusOK || first != revokedBody("tenant-a", bobID) {
		t.Fatalf("revoke = %d %s", res.StatusCode, first)
	}
	correlationID := res.Header.Get(httpx.CorrelationHeader)

	// The cut is effective at the answer.
	expectError(t, e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)), http.StatusForbidden, "forbidden")
	if got := e.memberships(bob); got != moderationsBody() {
		t.Fatalf("memberships after = %s", got)
	}
	if got := e.members(alice); got != `{"members":[]}`+"\n" {
		t.Fatalf("members after = %s", got)
	}
	cuts := e.invs.Cuts()
	if len(cuts) != 1 || cuts[0].TenantID != "tenant-a" || cuts[0].AccountID != bobID || cuts[0].CorrelationID != correlationID {
		t.Fatalf("cuts = %+v, response correlation %q", cuts, correlationID)
	}
	if got := e.invs.MemberRevocations()["op-http-mem-rev001"].CorrelationID; got == "" || got != correlationID {
		t.Fatalf("record correlation %q, response %q", got, correlationID)
	}

	// CTRL-IDEM-01: the retry answers the same bytes and writes nothing.
	before := e.memberState("tenant-a", bobID)
	e.clock.Advance(time.Minute)
	if res, again := e.revokeMember(alice, "op-http-mem-rev001", string(bobID)); res.StatusCode != http.StatusOK || again != first {
		t.Fatalf("replay = %d %s, want %s", res.StatusCode, again, first)
	}
	if got := e.memberState("tenant-a", bobID); got != before {
		t.Fatalf("replay changed the store: %+v -> %+v", before, got)
	}
	// A new operation for the revoked account finds no membership.
	expectError(t, e.do(http.MethodPost, memberRevokePath(string(bobID)), revocation("op-http-mem-rev002"), auth(alice)...), http.StatusNotFound, "not_found")
	if got := e.memberState("tenant-a", bobID); got != before {
		t.Fatalf("a second revocation changed the store: %+v -> %+v", before, got)
	}
}

// L5: a moderator whose membership no invitation records lists with a null
// accepted_at.
func TestMemberWithoutAnAcceptedInvitationListsANullAcceptance(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice := e.owner("op-http-mem-alice1", "alice@example.com", "tenant-a", access.SubscriptionActive)
	carolID := e.register("op-http-mem-carol1", "carol@example.com")
	e.invs.AddMember("tenant-a", carolID, access.RoleModerator)

	want := `{"members":[{"account_id":"` + string(carolID) + `","email":"carol@example.com","accepted_at":null}]}` + "\n"
	if got := e.members(alice); got != want {
		t.Fatalf("members = %s, want %s", got, want)
	}
}

// S6: moderators, strangers, and owners of an inactive tenant are refused
// both routes, and a refused revocation changes nothing.
func TestOnlyAnActiveOwnerListsAndRevokesMembers(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	_, bob, bobID := e.moderated()
	carolID := e.register("op-http-mem-carol1", "carol@example.com")
	e.invs.AddMember("tenant-a", carolID, access.RoleModerator)
	carol := e.signIn("carol@example.com")
	stranger := e.invitee("op-http-mem-strng1", "stranger@example.com")
	inactive := e.owner("op-http-mem-inact1", "inactive@example.com", "tenant-c", access.SubscriptionInactive)

	for name, s := range map[string]signedIn{"moderator": bob, "other moderator": carol, "stranger": stranger, "inactive owner": inactive} {
		t.Run(name, func(t *testing.T) {
			before := map[access.AccountID]memberState{bobID: e.memberState("tenant-a", bobID), carolID: e.memberState("tenant-a", carolID)}
			expectError(t, e.do(http.MethodGet, membersPath, nil, withSession(s.token)), http.StatusForbidden, "forbidden")
			for _, target := range []access.AccountID{bobID, carolID} {
				expectError(t, e.do(http.MethodPost, memberRevokePath(string(target)), revocation("op-http-mem-deny01"), auth(s)...), http.StatusForbidden, "forbidden")
			}
			for account, state := range before {
				if got := e.memberState("tenant-a", account); got != state {
					t.Fatalf("a refused revocation changed %s: %+v -> %+v", account, state, got)
				}
			}
		})
	}
	if res := e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderated canvas = %d", res.StatusCode)
	}
}

// S1, S7, S9: unknown, foreign, and owner targets are not found; malformed
// input is a bad request; a reused operation ID is a conflict. None writes.
func TestMemberRevocationRejectsInvalidTargetsWithoutEffect(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice, _, bobID := e.moderated()
	aliceID := access.AccountID(e.sessionAccount(alice))
	dave := e.owner("op-http-mem-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	erinID := e.register("op-http-mem-erin01", "erin@example.com")
	e.invs.AddMember("tenant-d", erinID, access.RoleModerator)

	expectError(t, e.do(http.MethodGet, membersPath, nil), http.StatusUnauthorized, "unauthorized")
	expectError(t, e.do(http.MethodPost, memberRevokePath(string(bobID)), revocation("op-http-mem-bad000"), withCSRF(alice.csrf)), http.StatusUnauthorized, "unauthorized")
	if n := e.callsTo("Members") + e.callsTo("RevokeMember"); n != 0 {
		t.Fatalf("a request without a session reached the domain %d times", n)
	}

	before := e.memberState("tenant-a", bobID)
	transactions := e.invs.Transactions()
	for name, c := range map[string]struct {
		path   string
		body   string
		status int
		code   string
	}{
		"unknown account":   {memberRevokePath(unknownID), `{"operation_id":"op-http-mem-bad001"}`, http.StatusNotFound, "not_found"},
		"other tenant's":    {memberRevokePath(string(erinID)), `{"operation_id":"op-http-mem-bad002"}`, http.StatusNotFound, "not_found"},
		"the owner":         {memberRevokePath(string(aliceID)), `{"operation_id":"op-http-mem-bad003"}`, http.StatusNotFound, "not_found"},
		"malformed account": {memberRevokePath("not-an-id"), `{"operation_id":"op-http-mem-bad004"}`, http.StatusBadRequest, "bad_request"},
		"malformed op ID":   {memberRevokePath(string(bobID)), `{"operation_id":"short"}`, http.StatusBadRequest, "bad_request"},
		"unknown field":     {memberRevokePath(string(bobID)), `{"operation_id":"op-http-mem-bad005","tenant_id":"tenant-d"}`, http.StatusBadRequest, "bad_request"},
		"malformed JSON":    {memberRevokePath(string(bobID)), `{"operation_id":`, http.StatusBadRequest, "bad_request"},
		"trailing data":     {memberRevokePath(string(bobID)), `{"operation_id":"op-http-mem-bad006"} {}`, http.StatusBadRequest, "bad_request"},
		"not JSON":          {memberRevokePath(string(bobID)), `operation_id=op-http-mem-bad007`, http.StatusBadRequest, "bad_request"},
	} {
		t.Run(name, func(t *testing.T) {
			contentType := "application/json"
			if name == "not JSON" {
				contentType = "application/x-www-form-urlencoded"
			}
			expectError(t, e.do(http.MethodPost, c.path, nil, append(auth(alice), withBody(contentType, c.body))...), c.status, c.code)
			if got := e.memberState("tenant-a", bobID); got != before {
				t.Fatalf("store changed: %+v -> %+v", before, got)
			}
		})
	}
	// Bodies that do not decode never reach the store.
	if n := e.invs.Transactions() - transactions; n != 3 {
		t.Fatalf("transactions = %d, want 3 (the not-found targets)", n)
	}
	if role, ok := e.invs.Membership("tenant-d", erinID); !ok || role != access.RoleModerator {
		t.Fatal("tenant D lost its moderator")
	}

	// The same operation ID for another account is a conflict.
	if res, body := e.revokeMember(alice, "op-http-mem-rev001", string(bobID)); res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d %s", res.StatusCode, body)
	}
	carolID := e.register("op-http-mem-carol1", "carol@example.com")
	e.invs.AddMember("tenant-a", carolID, access.RoleModerator)
	after := e.memberState("tenant-a", carolID)
	expectError(t, e.do(http.MethodPost, memberRevokePath(string(carolID)), revocation("op-http-mem-rev001"), auth(alice)...), http.StatusConflict, "conflict")
	if got := e.memberState("tenant-a", carolID); got != after {
		t.Fatalf("a conflict changed the store: %+v -> %+v", after, got)
	}
	// Dave's tenant is untouched by alice's revocations.
	if got := e.members(dave); !strings.Contains(got, string(erinID)) {
		t.Fatalf("dave's members = %s", got)
	}
}

// S9, S14: the record and cut keep the response correlation, the log line
// carries it with the tenant, and no log or error body holds an email, a
// token, a CSRF token, or another tenant's ID.
func TestMemberRevocationIsCorrelatedAndRedacted(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice, bob, bobID := e.moderated()
	dave := e.owner("op-http-mem-dave01", "dave@example.com", "tenant-d", access.SubscriptionActive)
	erinID := e.register("op-http-mem-erin01", "erin@example.com")
	e.invs.AddMember("tenant-d", erinID, access.RoleModerator)
	erin := e.signIn("erin@example.com")

	var errorBodies []string
	collect := func(res *http.Response) {
		if res.StatusCode >= http.StatusBadRequest {
			errorBodies = append(errorBodies, e.bodies[len(e.bodies)-1])
		}
	}
	revoke := func(s signedIn, op, account string, extra ...option) *http.Response {
		res, _ := e.revokeMember(s, op, account, extra...)
		return res
	}
	collect(revoke(alice, "op-http-mem-log001", unknownID))
	collect(revoke(alice, "op-http-mem-log002", string(erinID)))
	collect(revoke(alice, "op-http-mem-log003", "not-an-id"))
	collect(revoke(bob, "op-http-mem-log004", string(bobID)))
	collect(revoke(erin, "op-http-mem-log005", string(bobID)))
	collect(revoke(dave, "op-http-mem-log006", string(bobID)))
	collect(revoke(alice, "op-http-mem-log007", string(bobID), withCSRF("not-the-token")))
	collect(revoke(alice, "op-http-mem-log008", string(bobID), withOrigin("https://evil.test")))
	collect(e.do(http.MethodGet, membersPath, nil, withSession(bob.token)))
	collect(e.do(http.MethodGet, membersPath, nil, withSession(erin.token)))

	res := revoke(alice, "op-http-mem-log009", string(bobID))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	correlationID := res.Header.Get(httpx.CorrelationHeader)
	collect(revoke(alice, "op-http-mem-log009", string(erinID)))
	collect(e.do(http.MethodGet, canvasPath("tenant-a"), nil, withSession(bob.token)))
	if len(errorBodies) != 12 {
		t.Fatalf("error responses = %d, want 12", len(errorBodies))
	}

	logs := e.logs.String()
	record := logLine(logs, "moderator revoked", correlationID)
	if record == nil || record["tenant_id"] != "tenant-a" {
		t.Fatalf("moderator revoked log = %v\n%s", record, logs)
	}
	if n := strings.Count(logs, `"msg":"moderator revoked"`); n != 1 {
		t.Fatalf("moderator revoked logs = %d, want 1", n)
	}
	secrets := map[string]string{
		"owner email":       "alice@example.com",
		"moderator email":   "bob@example.com",
		"other owner email": "dave@example.com",
		"other email":       "erin@example.com",
		"session token":     alice.token,
		"CSRF token":        alice.csrf,
		"moderator token":   bob.token,
		"moderator CSRF":    bob.csrf,
		"other token":       erin.token,
	}
	for name, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
	secrets["tenant D"] = "tenant-d"
	secrets["tenant A"] = "tenant-a"
	for _, body := range errorBodies {
		for name, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("error body %s contains the %s", body, name)
			}
		}
	}
}

// S13, CTRL-WEB-01: Origin and the CSRF token are refused before the session
// is read, so neither the domain nor storage runs. No WebSocket upgrade exists.
func TestMemberRevocationChecksOriginAndCSRFBeforeTheDomain(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	alice, _, bobID := e.moderated()
	other := e.signIn("alice@example.com")
	path := memberRevokePath(string(bobID))
	body := revocation("op-http-mem-web001")
	transactions := e.invs.Transactions()
	before := e.memberState("tenant-a", bobID)
	const route = "POST /api/members/{accountID}/revoke"

	rejected := map[string]struct {
		opts   []option
		reason ingress.Reason
	}{
		"missing CSRF":       {[]option{withSession(alice.token)}, ingress.ReasonCSRF},
		"invalid CSRF":       {[]option{withSession(alice.token), withCSRF("not-the-token")}, ingress.ReasonCSRF},
		"other session CSRF": {[]option{withSession(alice.token), withCSRF(other.csrf)}, ingress.ReasonCSRF},
		"absent Origin":      {[]option{withSession(alice.token), withCSRF(alice.csrf), withoutOrigin()}, ingress.ReasonOrigin},
		"hostile Origin":     {[]option{withSession(alice.token), withCSRF(alice.csrf), withOrigin("https://evil.test")}, ingress.ReasonOrigin},
		// The CSRF check runs before the body is parsed.
		"missing CSRF, malformed body": {[]option{withSession(alice.token), withBody("application/json", "{")}, ingress.ReasonCSRF},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			effects := e.effects()
			counts := map[ingress.Reason]uint64{}
			for _, r := range []ingress.Reason{ingress.ReasonOrigin, ingress.ReasonCSRF, ingress.ReasonUnclassified} {
				counts[r] = reasonCount(e.ingress, r)
			}
			res := e.do(http.MethodPost, path, body, c.opts...)
			correlationID := res.Header.Get(httpx.CorrelationHeader)
			expectError(t, res, http.StatusForbidden, "forbidden")

			e.expectNoEffect(name, effects)
			if n := e.invs.Transactions(); n != transactions {
				t.Fatalf("invitation transactions = %d, want %d", n, transactions)
			}
			if got := e.memberState("tenant-a", bobID); got != before {
				t.Fatalf("store changed: %+v -> %+v", before, got)
			}
			for r, n := range counts {
				want := n
				if r == c.reason {
					want++
				}
				if got := reasonCount(e.ingress, r); got != want {
					t.Fatalf("ingress %s rejections = %d, want %d", r, got, want)
				}
			}
			record := logLine(e.logs.String(), "request failed", correlationID)
			if record == nil {
				t.Fatalf("no rejection logged with correlation %q", correlationID)
			}
			if got, want := record["error"], "ingress rejected: surface=http reason="+string(c.reason)+" route="+route; got != want {
				t.Fatalf("rejection log error = %v, want %q", got, want)
			}
		})
	}

	// Without a session cookie the handler answers 401 before the body.
	csrf := reasonCount(e.ingress, ingress.ReasonCSRF)
	expectError(t, e.do(http.MethodPost, path, body, withCSRF(alice.csrf)), http.StatusUnauthorized, "unauthorized")
	if got := reasonCount(e.ingress, ingress.ReasonCSRF); got != csrf {
		t.Fatalf("ingress csrf rejections = %d, want %d", got, csrf)
	}
	if e.callsTo("RevokeMember") != 0 || e.invs.Transactions() != transactions {
		t.Fatal("a request without a session reached the domain")
	}

	// Valid CSRF from the allowed Origin revokes once.
	if res, got := e.revokeMember(alice, "op-http-mem-web001", string(bobID)); res.StatusCode != http.StatusOK || got != revokedBody("tenant-a", bobID) {
		t.Fatalf("revoke = %d %s", res.StatusCode, got)
	}
	if n := e.callsTo("RevokeMember"); n != 1 {
		t.Fatalf("RevokeMember calls = %d, want 1", n)
	}
	if _, member := e.invs.Membership("tenant-a", bobID); member || len(e.invs.Cuts()) != 1 {
		t.Fatal("the accepted revocation did not take effect")
	}
}
