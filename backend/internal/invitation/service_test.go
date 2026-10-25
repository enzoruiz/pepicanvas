package invitation_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation/invitationtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	tenantA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tenantB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tenantC       = "cccccccccccccccccccccccccccccccc"
	ownerA        = access.AccountID("000000000000000000000000000000a1")
	ownerB        = access.AccountID("000000000000000000000000000000b1")
	inactiveOwner = access.AccountID("000000000000000000000000000000c1")
	moderatorA    = access.AccountID("000000000000000000000000000000a2")
	stranger      = access.AccountID("00000000000000000000000000000005")
	guest         = access.AccountID("00000000000000000000000000000006")
	unknownActor  = access.AccountID("ffffffffffffffffffffffffffffffff")
	unknownID     = "0123456789abcdef0123456789abcdef"
	opID          = "operation-0000001"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var start = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) (*invitation.Service, *invitationtest.MemoryStore, *clock) {
	t.Helper()
	store := invitationtest.NewMemoryStore()
	store.AddTenant(tenantA, ownerA, access.SubscriptionActive)
	store.AddTenant(tenantB, ownerB, access.SubscriptionActive)
	store.AddTenant(tenantC, inactiveOwner, access.SubscriptionInactive)
	store.AddMember(tenantA, moderatorA, access.RoleModerator)
	c := &clock{t: start}
	return invitation.NewService(store, invitation.WithClock(c.now)), store, c
}

// token returns a well-formed invitation token distinguished by n.
func token(n int) secret.Value {
	return secret.New(secret.KindInvitationToken, fmt.Sprintf("pci_%043d", n))
}

func create(op string, n int) invitation.CreateRequest {
	return invitation.CreateRequest{OwnerID: ownerA, OperationID: op, Token: token(n), CorrelationID: "corr-" + op}
}

func revoke(op, id string) invitation.RevokeRequest {
	return invitation.RevokeRequest{OwnerID: ownerA, OperationID: op, InvitationID: id, CorrelationID: "corr-" + op}
}

// accept returns the request of account for token n. Its operation ID is
// derived from both, so a repeated call is a retry of the same operation.
func accept(account access.AccountID, n int) invitation.AcceptRequest {
	op := fmt.Sprintf("accept-%s-%d", account, n)
	return invitation.AcceptRequest{AccountID: account, OperationID: op, Token: token(n), CorrelationID: "corr-" + op}
}

func canvas(tenantID string) string { return invitationtest.CanvasID(tenantID) }

func mustCreate(t *testing.T, service *invitation.Service, op string, n int) invitation.Invitation {
	t.Helper()
	got, err := service.Create(context.Background(), create(op, n))
	if err != nil {
		t.Fatalf("Create(%s): %v", op, err)
	}
	return got
}

func TestCreateRecordsAnExpiryExactlySevenDaysLater(t *testing.T) {
	service, store, _ := fixture(t)
	members := store.Memberships()

	got, err := service.Create(context.Background(), create(opID, 1))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if invitation.Lifetime != 168*time.Hour {
		t.Fatalf("Lifetime = %v, want 168h", invitation.Lifetime)
	}
	want := invitation.Invitation{ID: got.ID, TenantID: tenantA, CreatedAt: start, ExpiresAt: start.Add(168 * time.Hour)}
	if !hex32.MatchString(got.ID) || got != want {
		t.Fatalf("invitation = %+v, want %+v", got, want)
	}
	stored := store.Invitations()
	row, ok := stored[got.ID]
	if len(stored) != 1 || !ok {
		t.Fatalf("invitations = %+v, want only %s", stored, got.ID)
	}
	if row.TokenHash != sha256.Sum256([]byte(token(1).Reveal())) || row.CreatedBy != ownerA ||
		!row.CreatedAt.Equal(start) || !row.ExpiresAt.Equal(start.Add(invitation.Lifetime)) ||
		!row.RevokedAt.IsZero() || !row.AcceptedAt.IsZero() {
		t.Fatalf("stored = %+v", row)
	}
	// S3: creating the link grants nothing.
	if store.Memberships() != members {
		t.Fatalf("memberships = %d, want %d", store.Memberships(), members)
	}
	records := store.Records()
	record := records[opID]
	if len(records) != 1 || record.Kind != invitation.OperationCreate || record.OwnerID != ownerA ||
		record.CorrelationID != "corr-"+opID || !record.AcceptedAt.Equal(start) || record.Invitation != got {
		t.Fatalf("records = %+v", records)
	}
	if raw := token(1).Reveal(); strings.Contains(fmt.Sprintf("%+v %+v %+v", got, stored, records), raw) {
		t.Fatal("the raw token was stored or returned")
	}
}

func TestNonOwnersCannotCreateListOrRevokeAndLearnNothing(t *testing.T) {
	service, store, _ := fixture(t)
	existing := mustCreate(t, service, opID, 1)
	invitations, records := store.Invitations(), store.Records()
	invitationLookups, operationLookups := store.InvitationLookups(), store.OperationLookups()

	for _, actor := range []access.AccountID{moderatorA, stranger, inactiveOwner, unknownActor} {
		request := create("operation-0000002", 2)
		request.OwnerID = actor
		if _, err := service.Create(context.Background(), request); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s Create: err = %v, want ErrAccessDenied", actor, err)
		}
		if _, err := service.List(context.Background(), actor); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s List: err = %v, want ErrAccessDenied", actor, err)
		}
		for _, id := range []string{existing.ID, unknownID} {
			request := revoke("operation-0000003", id)
			request.OwnerID = actor
			if _, err := service.Revoke(context.Background(), request); !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("%s Revoke(%s): err = %v, want ErrAccessDenied", actor, id, err)
			}
		}
	}
	if store.InvitationLookups() != invitationLookups || store.OperationLookups() != operationLookups {
		t.Fatalf("lookups after denials: invitations %d->%d, operations %d->%d, want none",
			invitationLookups, store.InvitationLookups(), operationLookups, store.OperationLookups())
	}
	if fmt.Sprint(store.Invitations()) != fmt.Sprint(invitations) || fmt.Sprint(store.Records()) != fmt.Sprint(records) {
		t.Fatal("a denied request changed state")
	}
}

func TestAuthorizationIsReevaluatedOnReplays(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	store.SetStatus(tenantA, access.SubscriptionInactive)

	if _, err := service.Create(context.Background(), create(opID, 1)); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("replay after deactivation: err = %v, want ErrAccessDenied", err)
	}
	if _, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID)); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("revoke after deactivation: err = %v, want ErrAccessDenied", err)
	}
}

func TestMalformedRequestsAreRejectedBeforeStorage(t *testing.T) {
	service, store, _ := fixture(t)
	creates := map[string]invitation.CreateRequest{
		"short operation ID":   create("short", 1),
		"operation ID symbols": create("operation-000000!", 1),
		"long operation ID":    create(strings.Repeat("a", 129), 1),
	}
	for name, raw := range map[string]string{
		"wrong prefix":   "pcs_" + strings.Repeat("a", 43),
		"short token":    "pci_" + strings.Repeat("a", 42),
		"long token":     "pci_" + strings.Repeat("a", 44),
		"token symbols":  "pci_" + strings.Repeat("a", 42) + "=",
		"token padding":  " pci_" + strings.Repeat("a", 43),
		"no prefix":      strings.Repeat("a", 47),
		"token new line": "pci_" + strings.Repeat("a", 43) + "\n",
	} {
		request := create(opID, 1)
		request.Token = secret.New(secret.KindInvitationToken, raw)
		creates[name] = request
	}
	wrongKind := create(opID, 1)
	wrongKind.Token = secret.New(secret.KindSessionToken, token(1).Reveal())
	creates["wrong kind"] = wrongKind
	zero := create(opID, 1)
	zero.Token = secret.Value{}
	creates["zero token"] = zero
	for name, request := range creates {
		if _, err := service.Create(context.Background(), request); !errors.Is(err, invitation.ErrInvalid) {
			t.Fatalf("Create %s: err = %v, want ErrInvalid", name, err)
		}
	}
	revokes := map[string]invitation.RevokeRequest{
		"short operation ID":  revoke("short", unknownID),
		"uppercase ID":        revoke(opID, strings.ToUpper(unknownID)),
		"short ID":            revoke(opID, unknownID[:31]),
		"long ID":             revoke(opID, unknownID+"0"),
		"non-hex ID":          revoke(opID, "g123456789abcdef0123456789abcdef"),
		"empty invitation ID": revoke(opID, ""),
	}
	for name, request := range revokes {
		if _, err := service.Revoke(context.Background(), request); !errors.Is(err, invitation.ErrInvalid) {
			t.Fatalf("Revoke %s: err = %v, want ErrInvalid", name, err)
		}
	}
	if store.Transactions() != 0 {
		t.Fatalf("transactions = %d, want 0", store.Transactions())
	}
}

func TestListReturnsOnlyPendingInvitationsOldestFirst(t *testing.T) {
	service, _, c := fixture(t)
	first := mustCreate(t, service, "operation-0000001", 1)
	c.advance(time.Hour)
	revoked := mustCreate(t, service, "operation-0000002", 2)
	c.advance(time.Hour)
	mustCreate(t, service, "operation-0000003", 3) // accepted below
	c.advance(time.Hour)
	third := mustCreate(t, service, "operation-0000004", 4)
	c.advance(time.Hour)
	fourth := mustCreate(t, service, "operation-0000005", 5)
	other := invitation.CreateRequest{OwnerID: ownerB, OperationID: "operation-0000006", Token: token(6)}
	if _, err := service.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Revoke(context.Background(), revoke("operation-0000007", revoked.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(context.Background(), accept(guest, 3)); err != nil {
		t.Fatal(err)
	}

	assertList := func(want ...invitation.Invitation) {
		t.Helper()
		got, err := service.List(context.Background(), ownerA)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("List = %+v, want %+v", got, want)
		}
	}
	assertList(first, third, fourth)

	c.t = first.ExpiresAt.Add(-time.Nanosecond)
	assertList(first, third, fourth)
	c.t = first.ExpiresAt
	assertList(third, fourth)
	c.t = fourth.ExpiresAt
	assertList()
}

func TestRevokeThenAcceptIsUnavailableAndGrantsNothing(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)

	got, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || got != (invitation.Revocation{InvitationID: created.ID, Status: access.InvitationRevoked}) {
		t.Fatalf("Revoke = %+v, %v", got, err)
	}
	if _, err := service.Accept(context.Background(), accept(guest, 1)); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("Accept after revoke: err = %v, want ErrUnavailable", err)
	}
	if _, ok := store.Membership(tenantA, guest); ok {
		t.Fatal("a revoked invitation granted a membership")
	}
}

func TestAcceptThenRevokeKeepsTheMembership(t *testing.T) {
	service, store, c := fixture(t)
	created := mustCreate(t, service, opID, 1)
	c.advance(time.Minute)

	got, err := service.Accept(context.Background(), accept(guest, 1))
	if err != nil || got != (invitation.Acceptance{TenantID: tenantA, CanvasID: canvas(tenantA), InvitationID: created.ID}) {
		t.Fatalf("Accept = %+v, %v", got, err)
	}
	row := store.Invitations()[created.ID]
	if !row.AcceptedAt.Equal(start.Add(time.Minute)) || row.AcceptedBy != guest {
		t.Fatalf("stored = %+v", row)
	}
	revocation, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || revocation != (invitation.Revocation{InvitationID: created.ID, Status: access.InvitationAccepted}) {
		t.Fatalf("Revoke after accept = %+v, %v", revocation, err)
	}
	if role, ok := store.Membership(tenantA, guest); !ok || role != access.RoleModerator {
		t.Fatalf("membership = %q, %v; want moderator kept", role, ok)
	}
	if row := store.Invitations()[created.ID]; !row.RevokedAt.IsZero() {
		t.Fatalf("an accepted invitation was revoked: %+v", row)
	}
	if _, err := service.Accept(context.Background(), accept(stranger, 1)); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("second Accept: err = %v, want ErrUnavailable (single use)", err)
	}
}

// A revocation that commits while an acceptance waits for the invitation lock wins.
func TestARevocationCommittedFirstMakesTheAcceptanceUnavailable(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	store.BeforeOnce("LockInvitationByToken", func() {
		if _, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID)); err != nil {
			t.Errorf("concurrent Revoke: %v", err)
		}
	})

	if _, err := service.Accept(context.Background(), accept(guest, 1)); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("Accept: err = %v, want ErrUnavailable", err)
	}
	row := store.Invitations()[created.ID]
	if row.RevokedAt.IsZero() || !row.AcceptedAt.IsZero() {
		t.Fatalf("stored = %+v, want revoked only", row)
	}
	if _, ok := store.Membership(tenantA, guest); ok {
		t.Fatal("membership created after a confirmed revocation")
	}
}

// An acceptance that commits while a revocation waits for the invitation lock wins.
func TestAnAcceptanceCommittedFirstSurvivesTheRevocation(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	store.BeforeOnce("LockInvitation", func() {
		if _, err := service.Accept(context.Background(), accept(guest, 1)); err != nil {
			t.Errorf("concurrent Accept: %v", err)
		}
	})

	got, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || got.Status != access.InvitationAccepted {
		t.Fatalf("Revoke = %+v, %v; want accepted", got, err)
	}
	if role, ok := store.Membership(tenantA, guest); !ok || role != access.RoleModerator {
		t.Fatalf("membership = %q, %v; want moderator kept", role, ok)
	}
	if row := store.Invitations()[created.ID]; !row.RevokedAt.IsZero() || row.AcceptedBy != guest {
		t.Fatalf("stored = %+v", row)
	}
}

func TestAnotherTenantsInvitationIsIndistinguishableFromAnUnknownOne(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	before := store.Invitations()

	var errs []error
	for i, id := range []string{created.ID, unknownID} {
		request := revoke(fmt.Sprintf("operation-b-000000%d", i), id)
		request.OwnerID = ownerB
		got, err := service.Revoke(context.Background(), request)
		if got != (invitation.Revocation{}) {
			t.Fatalf("Revoke(%s) answered %+v", id, got)
		}
		errs = append(errs, err)
	}
	if errs[0] != invitation.ErrNotFound || errs[1] != invitation.ErrNotFound {
		t.Fatalf("errors = %v, want ErrNotFound for both", errs)
	}
	if fmt.Sprint(store.Invitations()) != fmt.Sprint(before) || len(store.Records()) != 1 {
		t.Fatal("a cross-tenant revocation changed state")
	}
	list, err := service.List(context.Background(), ownerB)
	if err != nil || len(list) != 0 {
		t.Fatalf("B's list = %+v, %v; want empty", list, err)
	}
	// A not-found revocation does not consume its operation ID.
	if _, err := service.Revoke(context.Background(), revoke("operation-b-0000000", created.ID)); err != nil {
		t.Fatalf("A reusing the ID after B's not found: %v", err)
	}
}

func TestEveryUnusableAcceptanceGetsTheSameAnswer(t *testing.T) {
	type setup func(t *testing.T, service *invitation.Service, store *invitationtest.MemoryStore, c *clock) invitation.AcceptRequest
	cases := map[string]setup{
		"unknown token": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			return accept(guest, 99)
		},
		"malformed token": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			request := accept(guest, 1)
			request.Token = secret.New(secret.KindInvitationToken, "pci_short")
			return request
		},
		"revoked": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			created := mustCreate(t, s, opID, 1)
			if _, err := s.Revoke(context.Background(), revoke("operation-0000002", created.ID)); err != nil {
				t.Fatal(err)
			}
			return accept(guest, 1)
		},
		"accepted": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			if _, err := s.Accept(context.Background(), accept(stranger, 1)); err != nil {
				t.Fatal(err)
			}
			return accept(guest, 1)
		},
		"expired": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, c *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			c.advance(invitation.Lifetime)
			return accept(guest, 1)
		},
		"inactive subscription": func(t *testing.T, s *invitation.Service, store *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			store.SetStatus(tenantA, access.SubscriptionInactive)
			return accept(guest, 1)
		},
		"already a moderator": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			return accept(moderatorA, 1)
		},
		"the owner": func(t *testing.T, s *invitation.Service, _ *invitationtest.MemoryStore, _ *clock) invitation.AcceptRequest {
			mustCreate(t, s, opID, 1)
			return accept(ownerA, 1)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			service, store, c := fixture(t)
			request := setup(t, service, store, c)
			invitations, members, records := store.Invitations(), store.Memberships(), store.Records()

			got, err := service.Accept(context.Background(), request)
			if err != invitation.ErrUnavailable || got != (invitation.Acceptance{}) {
				t.Fatalf("Accept = %+v, %v; want exactly ErrUnavailable", got, err)
			}
			if fmt.Sprint(store.Invitations()) != fmt.Sprint(invitations) || store.Memberships() != members ||
				fmt.Sprint(store.Records()) != fmt.Sprint(records) {
				t.Fatal("an unusable acceptance changed state")
			}
			// A refused acceptance does not consume its operation ID.
			if _, err := service.Accept(context.Background(), request); err != invitation.ErrUnavailable {
				t.Fatalf("retry: err = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestAcceptanceExpiresExactlyAtExpiresAt(t *testing.T) {
	service, store, c := fixture(t)
	early := mustCreate(t, service, "operation-0000001", 1)
	late := mustCreate(t, service, "operation-0000002", 2)

	c.t = early.ExpiresAt.Add(-time.Nanosecond)
	if _, err := service.Accept(context.Background(), accept(guest, 1)); err != nil {
		t.Fatalf("Accept 1ns before expiry: %v", err)
	}
	c.t = late.ExpiresAt
	if _, err := service.Accept(context.Background(), accept(stranger, 2)); err != invitation.ErrUnavailable {
		t.Fatalf("Accept at expiry: err = %v, want ErrUnavailable", err)
	}
	if _, ok := store.Membership(tenantA, stranger); ok {
		t.Fatal("an expired invitation granted a membership")
	}
}

func TestRevokingAnExpiredInvitationRevokesIt(t *testing.T) {
	service, store, c := fixture(t)
	created := mustCreate(t, service, opID, 1)
	c.t = created.ExpiresAt

	got, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || got.Status != access.InvitationRevoked {
		t.Fatalf("Revoke = %+v, %v", got, err)
	}
	if row := store.Invitations()[created.ID]; !row.RevokedAt.Equal(created.ExpiresAt) {
		t.Fatalf("revoked_at = %v, want %v", row.RevokedAt, created.ExpiresAt)
	}
}

func TestConcurrentRevocationsLeaveOneRevocation(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	const n = 8
	results := make([]invitation.Revocation, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = service.Revoke(context.Background(), revoke(fmt.Sprintf("operation-r-%07d", i), created.ID))
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || results[i] != (invitation.Revocation{InvitationID: created.ID, Status: access.InvitationRevoked}) {
			t.Fatalf("revocation %d = %+v, %v", i, results[i], errs[i])
		}
	}
	if row := store.Invitations()[created.ID]; !row.RevokedAt.Equal(start) {
		t.Fatalf("stored = %+v", row)
	}
}

// A revocation that commits while another waits for the row lock keeps its
// revoked_at; the waiting one answers revoked without writing again.
func TestARevocationWaitingOnTheLockDoesNotWriteAgain(t *testing.T) {
	service, store, c := fixture(t)
	created := mustCreate(t, service, opID, 1)
	store.BeforeOnce("LockInvitation", func() {
		c.advance(time.Second)
		if _, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID)); err != nil {
			t.Errorf("concurrent Revoke: %v", err)
		}
	})

	got, err := service.Revoke(context.Background(), revoke("operation-0000003", created.ID))
	if err != nil || got.Status != access.InvitationRevoked {
		t.Fatalf("Revoke = %+v, %v", got, err)
	}
	if row := store.Invitations()[created.ID]; !row.RevokedAt.Equal(start.Add(time.Second)) {
		t.Fatalf("revoked_at = %v, want the first commit at %v", row.RevokedAt, start.Add(time.Second))
	}
	if records := store.Records(); len(records) != 3 {
		t.Fatalf("records = %d, want create and two revocations", len(records))
	}
}

func TestRetriesReplayTheSameAnswerWhileRetained(t *testing.T) {
	service, store, c := fixture(t)
	first := mustCreate(t, service, opID, 1)
	revoked, err := service.Revoke(context.Background(), revoke("operation-0000002", first.ID))
	if err != nil {
		t.Fatal(err)
	}

	c.advance(invitation.OperationRetention - time.Nanosecond)
	retry := create(opID, 1)
	retry.CorrelationID = "corr-retry"
	replayed, err := service.Create(context.Background(), retry)
	if err != nil || replayed != first {
		t.Fatalf("create replay = %+v, %v; want %+v", replayed, err, first)
	}
	again, err := service.Revoke(context.Background(), invitation.RevokeRequest{OwnerID: ownerA, OperationID: "operation-0000002", InvitationID: first.ID, CorrelationID: "corr-retry"})
	if err != nil || again != revoked {
		t.Fatalf("revoke replay = %+v, %v; want %+v", again, err, revoked)
	}
	if len(store.Invitations()) != 1 {
		t.Fatalf("invitations = %d, want 1", len(store.Invitations()))
	}
	records := store.Records()
	if len(records) != 2 || records[opID].CorrelationID != "corr-"+opID || records["operation-0000002"].CorrelationID != "corr-operation-0000002" {
		t.Fatalf("records = %+v, want the first correlations kept", records)
	}
	if records["operation-0000002"].Kind != invitation.OperationRevoke || records["operation-0000002"].Revocation != revoked {
		t.Fatalf("revoke record = %+v", records["operation-0000002"])
	}
}

func TestAReusedOperationIDWithADifferentPayloadIsRejected(t *testing.T) {
	service, store, _ := fixture(t)
	first := mustCreate(t, service, opID, 1)
	second := mustCreate(t, service, "operation-0000002", 2)
	if _, err := service.Revoke(context.Background(), revoke("operation-0000003", first.ID)); err != nil {
		t.Fatal(err)
	}
	ownerBCreate := invitation.CreateRequest{OwnerID: ownerB, OperationID: opID, Token: token(1)}

	cases := map[string]func() error{
		"create with another token": func() error { _, err := service.Create(context.Background(), create(opID, 3)); return err },
		"create by another owner":   func() error { _, err := service.Create(context.Background(), ownerBCreate); return err },
		"revoke under a create ID":  func() error { _, err := service.Revoke(context.Background(), revoke(opID, first.ID)); return err },
		"revoke another invitation": func() error {
			_, err := service.Revoke(context.Background(), revoke("operation-0000003", second.ID))
			return err
		},
		"create under a revoke ID": func() error {
			_, err := service.Create(context.Background(), create("operation-0000003", 3))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, invitation.ErrOperationConflict) {
			t.Fatalf("%s: err = %v, want ErrOperationConflict", name, err)
		}
	}
	if len(store.Invitations()) != 2 || !store.Invitations()[second.ID].RevokedAt.IsZero() || len(store.Records()) != 3 {
		t.Fatal("a conflicting operation changed state")
	}
}

func TestATokenReusedUnderAnotherOperationIsAConflict(t *testing.T) {
	service, store, _ := fixture(t)
	mustCreate(t, service, opID, 1)
	ownerBCreate := invitation.CreateRequest{OwnerID: ownerB, OperationID: "operation-0000003", Token: token(1)}

	if _, err := service.Create(context.Background(), create("operation-0000002", 1)); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("same owner: err = %v, want ErrOperationConflict", err)
	}
	if _, err := service.Create(context.Background(), ownerBCreate); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("other tenant: err = %v, want ErrOperationConflict", err)
	}
	if len(store.Invitations()) != 1 || len(store.Records()) != 1 {
		t.Fatal("a reused token changed state")
	}
}

func TestAnOperationIDStartsANewOperationAtRetentionExpiry(t *testing.T) {
	service, store, c := fixture(t)
	first := mustCreate(t, service, opID, 1)

	c.advance(invitation.OperationRetention)
	got, err := service.Create(context.Background(), create(opID, 2))
	if err != nil {
		t.Fatalf("expired ID reused: %v", err)
	}
	if got.ID == first.ID || !got.CreatedAt.Equal(start.Add(invitation.OperationRetention)) {
		t.Fatalf("invitation = %+v", got)
	}
	records := store.Records()
	if len(records) != 1 || records[opID].Invitation != got {
		t.Fatalf("records = %+v, want only the new operation", records)
	}
	if len(store.Invitations()) != 2 {
		t.Fatalf("invitations = %d, want 2", len(store.Invitations()))
	}
}

func TestAFailureAtAnyCreateStepLeavesNothingAndTheRetryCreatesOnce(t *testing.T) {
	for _, step := range []string{"LockOwnedTenant", "Operation", "InsertInvitation", "RecordOperation", "commit"} {
		t.Run(step, func(t *testing.T) {
			service, store, _ := fixture(t)
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.Create(context.Background(), create(opID, 1)); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if len(store.Invitations()) != 0 || len(store.Records()) != 0 {
				t.Fatal("a failed creation left state behind")
			}
			got := mustCreate(t, service, opID, 1)
			if again := mustCreate(t, service, opID, 1); again != got || len(store.Invitations()) != 1 {
				t.Fatalf("retry = %+v, replay = %+v, invitations = %d", got, again, len(store.Invitations()))
			}
		})
	}
}

func TestAFailureAtAnyRevokeStepLeavesNothingAndTheRetryRevokesOnce(t *testing.T) {
	for _, step := range []string{"LockOwnedTenant", "Operation", "LockInvitation", "RevokeInvitation", "RecordOperation", "commit"} {
		t.Run(step, func(t *testing.T) {
			service, store, c := fixture(t)
			created := mustCreate(t, service, opID, 1)
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID)); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if row := store.Invitations()[created.ID]; !row.RevokedAt.IsZero() || len(store.Records()) != 1 {
				t.Fatal("a failed revocation left state behind")
			}
			c.advance(time.Second)
			got, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
			if err != nil || got.Status != access.InvitationRevoked {
				t.Fatalf("retry = %+v, %v", got, err)
			}
			if row := store.Invitations()[created.ID]; !row.RevokedAt.Equal(start.Add(time.Second)) {
				t.Fatalf("revoked_at = %v", row.RevokedAt)
			}
		})
	}
}

// S5: a failure before the commit leaves the link pending and grants
// nothing; the retry under the same operation ID accepts once.
func TestAFailureAtAnyAcceptStepLeavesTheLinkPending(t *testing.T) {
	steps := []string{"LockInvitationByToken", "Operation", "LockTenant", "IsMember", "AddModerator", "MarkAccepted", "RecordOperation", "commit"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			service, store, _ := fixture(t)
			created := mustCreate(t, service, opID, 1)
			members := store.Memberships()
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.Accept(context.Background(), accept(guest, 1)); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if _, ok := store.Membership(tenantA, guest); ok || !store.Invitations()[created.ID].Pending(start) ||
				store.Memberships() != members || len(store.Records()) != 1 {
				t.Fatal("a failed acceptance left state behind")
			}
			want := invitation.Acceptance{TenantID: tenantA, CanvasID: canvas(tenantA), InvitationID: created.ID}
			for range 2 {
				if got, err := service.Accept(context.Background(), accept(guest, 1)); err != nil || got != want {
					t.Fatalf("retry = %+v, %v; want %+v", got, err, want)
				}
			}
			if store.Memberships() != members+1 || len(store.Records()) != 2 {
				t.Fatalf("memberships = %d, records = %d; want one acceptance", store.Memberships(), len(store.Records()))
			}
			if _, err := service.Accept(context.Background(), accept(stranger, 1)); err != invitation.ErrUnavailable {
				t.Fatalf("another account after the retry: err = %v, want ErrUnavailable", err)
			}
		})
	}
}

// A concurrent creation that commits the same operation and token first is replayed.
func TestAConcurrentCommitOfTheSameCreationIsReplayed(t *testing.T) {
	for _, step := range []string{"InsertInvitation", "RecordOperation"} {
		t.Run(step, func(t *testing.T) {
			service, store, _ := fixture(t)
			var winner invitation.Invitation
			store.BeforeOnce(step, func() { winner = mustCreate(t, service, opID, 1) })

			got, err := service.Create(context.Background(), create(opID, 1))
			if err != nil || got != winner {
				t.Fatalf("Create = %+v, %v; want the committed %+v", got, err, winner)
			}
			if len(store.Invitations()) != 1 || len(store.Records()) != 1 {
				t.Fatalf("invitations = %d, records = %d; want 1 and 1", len(store.Invitations()), len(store.Records()))
			}
		})
	}
}

func TestAConcurrentCommitOfTheSameOperationWithAnotherTokenIsAConflict(t *testing.T) {
	service, store, _ := fixture(t)
	store.BeforeOnce("RecordOperation", func() { mustCreate(t, service, opID, 2) })

	if _, err := service.Create(context.Background(), create(opID, 1)); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
	if len(store.Invitations()) != 1 || len(store.Records()) != 1 {
		t.Fatal("the losing creation left state behind")
	}
}

func TestAConcurrentCommitOfTheSameRevocationIsReplayed(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	var winner invitation.Revocation
	store.BeforeOnce("RecordOperation", func() {
		var err error
		if winner, err = service.Revoke(context.Background(), revoke("operation-0000002", created.ID)); err != nil {
			t.Errorf("concurrent Revoke: %v", err)
		}
	})

	got, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || got != winner {
		t.Fatalf("Revoke = %+v, %v; want %+v", got, err, winner)
	}
	if len(store.Records()) != 2 {
		t.Fatalf("records = %d, want 2", len(store.Records()))
	}
}

func TestTwoTenantsKeepTheirInvitationsApart(t *testing.T) {
	service, _, _ := fixture(t)
	a := mustCreate(t, service, opID, 1)
	b, err := service.Create(context.Background(), invitation.CreateRequest{OwnerID: ownerB, OperationID: "operation-b-0000001", Token: token(2)})
	if err != nil {
		t.Fatalf("B Create: %v", err)
	}
	if a.TenantID != tenantA || b.TenantID != tenantB || a.ID == b.ID {
		t.Fatalf("a = %+v, b = %+v", a, b)
	}
	got, err := service.Accept(context.Background(), accept(guest, 2))
	if err != nil || got.TenantID != tenantB {
		t.Fatalf("Accept B's token = %+v, %v", got, err)
	}
}

func TestAcceptGrantsModerationOfExactlyThatTenant(t *testing.T) {
	service, store, c := fixture(t)
	created := mustCreate(t, service, opID, 1)
	mustCreate(t, service, "operation-0000002", 2)
	c.advance(time.Minute)
	members := store.Memberships()

	request := accept(guest, 1)
	got, err := service.Accept(context.Background(), request)
	want := invitation.Acceptance{TenantID: tenantA, CanvasID: canvas(tenantA), InvitationID: created.ID}
	if err != nil || got != want {
		t.Fatalf("Accept = %+v, %v; want %+v", got, err, want)
	}
	if role, ok := store.Membership(tenantA, guest); !ok || role != access.RoleModerator {
		t.Fatalf("membership in A = %q, %v; want moderator", role, ok)
	}
	for _, other := range []string{tenantB, tenantC} {
		if _, ok := store.Membership(other, guest); ok {
			t.Fatalf("membership in %s granted", other)
		}
	}
	if store.Memberships() != members+1 {
		t.Fatalf("memberships = %d, want %d", store.Memberships(), members+1)
	}
	// The link is consumed; the other pending invitation is untouched.
	for id, row := range store.Invitations() {
		consumed := id == created.ID
		if consumed != (row.AcceptedBy == guest && row.AcceptedAt.Equal(start.Add(time.Minute))) || !row.RevokedAt.IsZero() {
			t.Fatalf("invitation %s = %+v", id, row)
		}
	}
	record, ok := store.Records()[request.OperationID]
	fingerprint := sha256.New()
	fingerprint.Write([]byte("accept\x00" + string(guest) + "\x00"))
	hash := sha256.Sum256([]byte(token(1).Reveal()))
	fingerprint.Write(hash[:])
	if !ok || record.Kind != invitation.OperationAccept || record.OwnerID != guest || record.Acceptance != want ||
		record.CorrelationID != request.CorrelationID || !record.AcceptedAt.Equal(start.Add(time.Minute)) ||
		string(record.Fingerprint[:]) != string(fingerprint.Sum(nil)) || record.Invitation != (invitation.Invitation{}) {
		t.Fatalf("record = %+v, %v", record, ok)
	}
	moderations, err := service.Moderations(context.Background(), guest)
	if err != nil || fmt.Sprint(moderations) != fmt.Sprint([]invitation.Moderation{{TenantID: tenantA, CanvasID: canvas(tenantA), Role: access.RoleModerator}}) {
		t.Fatalf("Moderations = %+v, %v", moderations, err)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v", got, store.Records()), token(1).Reveal()) {
		t.Fatal("the raw token was stored or returned")
	}
}

func TestAcceptRejectsMalformedRequestsBeforeStorage(t *testing.T) {
	service, store, _ := fixture(t)
	mustCreate(t, service, opID, 1)
	transactions := store.Transactions()

	for name, op := range map[string]string{
		"empty operation ID":   "",
		"short operation ID":   "short",
		"operation ID symbols": "operation-000000!",
		"long operation ID":    strings.Repeat("a", 129),
	} {
		request := accept(guest, 1)
		request.OperationID = op
		if _, err := service.Accept(context.Background(), request); err != invitation.ErrInvalid {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
		request.Token = secret.New(secret.KindInvitationToken, "pci_short")
		if _, err := service.Accept(context.Background(), request); err != invitation.ErrInvalid {
			t.Fatalf("%s with a malformed token: err = %v, want ErrInvalid", name, err)
		}
	}
	malformed := accept(guest, 1)
	malformed.Token = secret.New(secret.KindSessionToken, token(1).Reveal())
	if _, err := service.Accept(context.Background(), malformed); err != invitation.ErrUnavailable {
		t.Fatalf("token of another kind: err = %v, want ErrUnavailable", err)
	}
	if store.Transactions() != transactions {
		t.Fatalf("transactions = %d, want %d", store.Transactions(), transactions)
	}
}

// S7/S12: a retry under the same operation ID replays the first answer with
// no second effect, even after the owner revoked the consumed link.
func TestAcceptReplaysTheSameAnswerWhileRetained(t *testing.T) {
	service, store, c := fixture(t)
	created := mustCreate(t, service, opID, 1)
	first, err := service.Accept(context.Background(), accept(guest, 1))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	invitations, members := store.Invitations(), store.Memberships()

	c.advance(invitation.OperationRetention - time.Nanosecond)
	revocation, err := service.Revoke(context.Background(), revoke("operation-0000002", created.ID))
	if err != nil || revocation.Status != access.InvitationAccepted {
		t.Fatalf("Revoke = %+v, %v; want accepted", revocation, err)
	}
	retry := accept(guest, 1)
	retry.CorrelationID = "corr-retry"
	for range 2 {
		again, err := service.Accept(context.Background(), retry)
		if err != nil || again != first {
			t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
		}
	}
	if fmt.Sprint(store.Invitations()) != fmt.Sprint(invitations) || store.Memberships() != members {
		t.Fatal("a replay changed state")
	}
	if record := store.Records()[retry.OperationID]; record.CorrelationID != accept(guest, 1).CorrelationID {
		t.Fatalf("record correlation = %q, want the first one", record.CorrelationID)
	}
}

func TestAReusedAcceptOperationIDWithADifferentPayloadIsRejected(t *testing.T) {
	service, store, _ := fixture(t)
	mustCreate(t, service, opID, 1)
	second := mustCreate(t, service, "operation-0000002", 2)
	first := accept(guest, 1)
	if _, err := service.Accept(context.Background(), first); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	invitations, members, records := store.Invitations(), store.Memberships(), store.Records()

	anotherToken := accept(guest, 2)
	anotherToken.OperationID = first.OperationID
	anotherAccount := accept(stranger, 1)
	anotherAccount.OperationID = first.OperationID
	createOperation := accept(stranger, 2)
	createOperation.OperationID = opID
	for name, request := range map[string]invitation.AcceptRequest{
		"another token":     anotherToken,
		"another account":   anotherAccount,
		"under a create ID": createOperation,
	} {
		if _, err := service.Accept(context.Background(), request); err != invitation.ErrOperationConflict {
			t.Fatalf("%s: err = %v, want ErrOperationConflict", name, err)
		}
	}
	if fmt.Sprint(store.Invitations()) != fmt.Sprint(invitations) || store.Memberships() != members ||
		fmt.Sprint(store.Records()) != fmt.Sprint(records) || !store.Invitations()[second.ID].Pending(start) {
		t.Fatal("a conflicting acceptance changed state")
	}
}

// S12: at exactly OperationRetention the operation ID is no longer retained,
// so the retry is a new acceptance of a consumed link.
func TestAnAcceptOperationIDIsNotRetainedAtRetentionExpiry(t *testing.T) {
	service, store, c := fixture(t)
	mustCreate(t, service, opID, 1)
	if _, err := service.Accept(context.Background(), accept(guest, 1)); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	members := store.Memberships()

	c.advance(invitation.OperationRetention)
	if got, err := service.Accept(context.Background(), accept(guest, 1)); err != invitation.ErrUnavailable || got != (invitation.Acceptance{}) {
		t.Fatalf("retry at retention expiry = %+v, %v; want ErrUnavailable", got, err)
	}
	if store.Memberships() != members {
		t.Fatal("the expired retry granted another membership")
	}
}

// S3: of concurrent acceptances of one link, one grants a membership and
// every other gets the one safe rejection.
func TestConcurrentAcceptancesOfOneLinkGrantOneMembership(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	members := store.Memberships()
	const n = 8
	errs := make([]error, n)
	answers := make([]invitation.Acceptance, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			answers[i], errs[i] = service.Accept(context.Background(), accept(access.AccountID(fmt.Sprintf("%032x", 0x100+i)), 1))
		})
	}
	wg.Wait()
	winners := 0
	for i := range n {
		switch errs[i] {
		case nil:
			winners++
			if answers[i].InvitationID != created.ID || answers[i].TenantID != tenantA {
				t.Fatalf("answer %d = %+v", i, answers[i])
			}
		case invitation.ErrUnavailable:
		default:
			t.Fatalf("acceptance %d: err = %v", i, errs[i])
		}
	}
	if winners != 1 || store.Memberships() != members+1 {
		t.Fatalf("winners = %d, memberships = %d; want one", winners, store.Memberships()-members)
	}
}

// An acceptance by another account that commits while this one waits for
// the invitation lock makes this one unavailable.
func TestAnAcceptanceCommittedFirstMakesTheWaitingOneUnavailable(t *testing.T) {
	service, store, _ := fixture(t)
	mustCreate(t, service, opID, 1)
	store.BeforeOnce("LockInvitationByToken", func() {
		if _, err := service.Accept(context.Background(), accept(stranger, 1)); err != nil {
			t.Errorf("concurrent Accept: %v", err)
		}
	})

	if _, err := service.Accept(context.Background(), accept(guest, 1)); err != invitation.ErrUnavailable {
		t.Fatalf("Accept: err = %v, want ErrUnavailable", err)
	}
	if _, ok := store.Membership(tenantA, guest); ok {
		t.Fatal("the waiting acceptance granted a membership")
	}
	if len(store.Records()) != 2 {
		t.Fatalf("records = %d, want create and one acceptance", len(store.Records()))
	}
}

// The same operation committed while this one waits for the invitation lock
// is replayed: the operation is read after the lock.
func TestAConcurrentCommitOfTheSameAcceptanceIsReplayed(t *testing.T) {
	service, store, _ := fixture(t)
	mustCreate(t, service, opID, 1)
	members := store.Memberships()
	var winner invitation.Acceptance
	store.BeforeOnce("LockInvitationByToken", func() {
		var err error
		if winner, err = service.Accept(context.Background(), accept(guest, 1)); err != nil {
			t.Errorf("concurrent Accept: %v", err)
		}
	})

	got, err := service.Accept(context.Background(), accept(guest, 1))
	if err != nil || got != winner || got.TenantID != tenantA {
		t.Fatalf("Accept = %+v, %v; want the committed %+v", got, err, winner)
	}
	if store.Memberships() != members+1 || len(store.Records()) != 2 {
		t.Fatalf("memberships = %d, records = %d; want one acceptance", store.Memberships()-members, len(store.Records()))
	}
}

func TestTheSameAcceptOperationCommittedConcurrentlyWithAnotherTokenIsAConflict(t *testing.T) {
	service, store, _ := fixture(t)
	created := mustCreate(t, service, opID, 1)
	if _, err := service.Create(context.Background(), invitation.CreateRequest{OwnerID: ownerB, OperationID: "operation-b-0000001", Token: token(2)}); err != nil {
		t.Fatal(err)
	}
	request := accept(guest, 1)
	store.BeforeOnce("RecordOperation", func() {
		other := accept(guest, 2)
		other.OperationID = request.OperationID
		if _, err := service.Accept(context.Background(), other); err != nil {
			t.Errorf("concurrent Accept: %v", err)
		}
	})

	if _, err := service.Accept(context.Background(), request); err != invitation.ErrOperationConflict {
		t.Fatalf("Accept: err = %v, want ErrOperationConflict", err)
	}
	if _, ok := store.Membership(tenantA, guest); ok || !store.Invitations()[created.ID].Pending(start) {
		t.Fatal("the conflicting acceptance left state behind")
	}
	if role, ok := store.Membership(tenantB, guest); !ok || role != access.RoleModerator {
		t.Fatalf("membership in B = %q, %v; want the committed acceptance", role, ok)
	}
}

// US-004 advisory A1: one account accepting two invitations of one tenant at
// once gets one membership; the other acceptance is the safe rejection.
func TestOneAccountAcceptingTwoLinksOfOneTenantConcurrentlyGetsOneMembership(t *testing.T) {
	service, store, _ := fixture(t)
	first := mustCreate(t, service, opID, 1)
	second := mustCreate(t, service, "operation-0000002", 2)
	members := store.Memberships()
	store.BeforeOnce("AddModerator", func() {
		if _, err := service.Accept(context.Background(), accept(guest, 2)); err != nil {
			t.Errorf("concurrent Accept: %v", err)
		}
	})

	got, err := service.Accept(context.Background(), accept(guest, 1))
	if err != invitation.ErrUnavailable || got != (invitation.Acceptance{}) {
		t.Fatalf("Accept = %+v, %v; want exactly ErrUnavailable", got, err)
	}
	if store.Memberships() != members+1 {
		t.Fatalf("memberships = %d, want one more", store.Memberships()-members)
	}
	rows := store.Invitations()
	if !rows[first.ID].Pending(start) || rows[second.ID].AcceptedBy != guest {
		t.Fatalf("invitations = %+v; want the first pending and the second accepted", rows)
	}
	if _, ok := store.Records()[accept(guest, 1).OperationID]; ok {
		t.Fatal("the rejected acceptance recorded its operation")
	}
}

// S9: simultaneous acceptances of two tenants' links bind each account to its
// own tenant and canvas only.
func TestAcceptancesOfTwoTenantsNeverCross(t *testing.T) {
	service, store, _ := fixture(t)
	guestA, guestB := access.AccountID("000000000000000000000000000000a7"), access.AccountID("000000000000000000000000000000b7")
	var wg sync.WaitGroup
	created := make([]invitation.Invitation, 2)
	createErrs := make([]error, 2)
	for i, owner := range []access.AccountID{ownerA, ownerB} {
		wg.Go(func() {
			created[i], createErrs[i] = service.Create(context.Background(), invitation.CreateRequest{OwnerID: owner, OperationID: fmt.Sprintf("operation-two-%04d", i), Token: token(i + 1)})
		})
	}
	wg.Wait()
	if createErrs[0] != nil || createErrs[1] != nil {
		t.Fatalf("Create: %v, %v", createErrs[0], createErrs[1])
	}
	answers := make([]invitation.Acceptance, 2)
	errs := make([]error, 2)
	for i, account := range []access.AccountID{guestA, guestB} {
		wg.Go(func() { answers[i], errs[i] = service.Accept(context.Background(), accept(account, i+1)) })
	}
	wg.Wait()

	for i, tenant := range []string{tenantA, tenantB} {
		want := invitation.Acceptance{TenantID: tenant, CanvasID: canvas(tenant), InvitationID: created[i].ID}
		if errs[i] != nil || answers[i] != want {
			t.Fatalf("Accept %d = %+v, %v; want %+v", i, answers[i], errs[i], want)
		}
	}
	for _, check := range []struct {
		account access.AccountID
		tenant  string
		member  bool
	}{{guestA, tenantA, true}, {guestA, tenantB, false}, {guestB, tenantB, true}, {guestB, tenantA, false}} {
		if _, ok := store.Membership(check.tenant, check.account); ok != check.member {
			t.Fatalf("membership of %s in %s = %v, want %v", check.account, check.tenant, ok, check.member)
		}
	}
	for account, tenant := range map[access.AccountID]string{guestA: tenantA, guestB: tenantB} {
		got, err := service.Moderations(context.Background(), account)
		want := []invitation.Moderation{{TenantID: tenant, CanvasID: canvas(tenant), Role: access.RoleModerator}}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("Moderations(%s) = %+v, %v; want %+v", account, got, err, want)
		}
	}
}

// S17: moderated canvases of active tenants only, by tenant ID; owner
// memberships are not listed.
func TestModerationsListsOnlyActiveModeratedCanvases(t *testing.T) {
	service, store, _ := fixture(t)
	store.AddMember(tenantC, guest, access.RoleModerator)
	store.AddMember(tenantB, guest, access.RoleModerator)
	store.AddMember(tenantA, guest, access.RoleModerator)
	store.AddMember(tenantB, ownerA, access.RoleModerator)

	list := func(account access.AccountID, want ...invitation.Moderation) {
		t.Helper()
		got, err := service.Moderations(context.Background(), account)
		if err != nil || got == nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("Moderations(%s) = %#v, %v; want %+v", account, got, err, want)
		}
	}
	moderation := func(tenant string) invitation.Moderation {
		return invitation.Moderation{TenantID: tenant, CanvasID: canvas(tenant), Role: access.RoleModerator}
	}
	list(guest, moderation(tenantA), moderation(tenantB))
	list(ownerA, moderation(tenantB))
	list(ownerB)
	list(inactiveOwner)
	list(stranger)
	store.SetStatus(tenantB, access.SubscriptionInactive)
	list(guest, moderation(tenantA))
	list(ownerA)
}

func TestModerationsReportsStorageFailures(t *testing.T) {
	service, store, _ := fixture(t)
	boom := errors.New("injected failure")
	store.FailOnce("ModeratedCanvases", boom)
	if _, err := service.Moderations(context.Background(), moderatorA); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
}
