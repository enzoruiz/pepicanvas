package invitation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation/invitationtest"
)

const revokeOpID = "revoke-member-0000001"

// memberFixture is fixture with emails for every account and a service whose
// committed hook counts the revocations that wrote a cut.
func memberFixture(t *testing.T) (*invitation.Service, *invitationtest.MemoryStore, *clock, *int) {
	t.Helper()
	_, store, c := fixture(t)
	for _, account := range []access.AccountID{ownerA, ownerB, inactiveOwner, moderatorA, stranger, guest} {
		store.AddAccount(account, string(account)+"@example.com")
	}
	committed := new(int)
	service := invitation.NewService(store, invitation.WithClock(c.now), invitation.WithCommitted(func() { *committed++ }))
	return service, store, c, committed
}

func revokeMember(op string, account access.AccountID) invitation.RevokeMemberRequest {
	return invitation.RevokeMemberRequest{OwnerID: ownerA, OperationID: op, AccountID: account, CorrelationID: "corr-" + op}
}

// revocationState renders everything a membership revocation can write.
func revocationState(store *invitationtest.MemoryStore) string {
	memberships := map[string]access.Role{}
	for _, tenant := range []string{tenantA, tenantB, tenantC} {
		for _, account := range []access.AccountID{ownerA, ownerB, inactiveOwner, moderatorA, stranger, guest, unknownActor} {
			if role, ok := store.Membership(tenant, account); ok {
				memberships[tenant+"/"+string(account)] = role
			}
		}
	}
	return fmt.Sprintf("members %v versions %d %d %d cuts %+v revocations %+v records %+v invitations %+v",
		memberships, store.AccessVersion(tenantA), store.AccessVersion(tenantB), store.AccessVersion(tenantC),
		store.Cuts(), store.MemberRevocations(), store.Records(), store.Invitations())
}

func TestMembersListsTheModeratorsOfTheOwnersTenantWithEmailAndAcceptance(t *testing.T) {
	service, _, c, _ := memberFixture(t)
	mustCreate(t, service, opID, 1)
	c.advance(time.Minute)
	if _, err := service.Accept(context.Background(), accept(guest, 1)); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := service.Create(context.Background(), invitation.CreateRequest{OwnerID: ownerB, OperationID: "operation-b-0000001", Token: token(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(context.Background(), accept(stranger, 2)); err != nil {
		t.Fatalf("Accept in B: %v", err)
	}

	got, err := service.Members(context.Background(), ownerA)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	// A membership without an accepted invitation (seeded) has the zero
	// acceptance time and is listed first. The owner and B's moderator are
	// not listed.
	want := []invitation.Member{
		{AccountID: moderatorA, Email: string(moderatorA) + "@example.com"},
		{AccountID: guest, Email: string(guest) + "@example.com", AcceptedAt: start.Add(time.Minute)},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Members = %+v, want %+v", got, want)
	}
	gotB, err := service.Members(context.Background(), ownerB)
	if wantB := []invitation.Member{{AccountID: stranger, Email: string(stranger) + "@example.com", AcceptedAt: start.Add(time.Minute)}}; err != nil || fmt.Sprint(gotB) != fmt.Sprint(wantB) {
		t.Fatalf("Members(B) = %+v, %v; want %+v", gotB, err, wantB)
	}
}

func TestMembersIsDeniedToEveryoneButTheActiveOwner(t *testing.T) {
	service, _, _, _ := memberFixture(t)
	for _, actor := range []access.AccountID{moderatorA, stranger, inactiveOwner, unknownActor} {
		if got, err := service.Members(context.Background(), actor); !errors.Is(err, access.ErrAccessDenied) || got != nil {
			t.Fatalf("%s Members = %+v, %v; want ErrAccessDenied", actor, got, err)
		}
	}
}

// S1, S5, S14: the owner revokes a moderator; the membership goes, the access
// version increments, one revocation cut carries the correlation, and the
// answer is recorded. Invitations (content) stay.
func TestRevokeMemberCutsTheModeratorInOneCommit(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	created := mustCreate(t, service, opID, 1)
	invitations := store.Invitations()

	got, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA))
	if err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	if want := (invitation.MemberRevocation{TenantID: tenantA, AccountID: moderatorA}); got != want {
		t.Fatalf("RevokeMember = %+v, want %+v", got, want)
	}
	if _, ok := store.Membership(tenantA, moderatorA); ok {
		t.Fatal("the moderator membership survived")
	}
	if role, ok := store.Membership(tenantA, ownerA); !ok || role != access.RoleOwner {
		t.Fatal("the owner membership changed")
	}
	if v := store.AccessVersion(tenantA); v != 2 {
		t.Fatalf("access version = %d, want 2", v)
	}
	wantCut := invitation.RevocationCut{TenantID: tenantA, AccountID: moderatorA, Version: 2, CorrelationID: "corr-" + revokeOpID, CreatedAt: start}
	if cuts := store.Cuts(); len(cuts) != 1 || cuts[0] != wantCut {
		t.Fatalf("cuts = %+v, want %+v", cuts, wantCut)
	}
	record, ok := store.MemberRevocations()[revokeOpID]
	if !ok || record.OwnerID != ownerA || record.Result != got || record.Version != 2 ||
		record.CorrelationID != "corr-"+revokeOpID || !record.AcceptedAt.Equal(start) {
		t.Fatalf("record = %+v, %v", record, ok)
	}
	if fmt.Sprint(store.Invitations()) != fmt.Sprint(invitations) || store.Invitations()[created.ID] != invitations[created.ID] {
		t.Fatal("the revocation changed invitations")
	}
	if *committed != 1 {
		t.Fatalf("committed hook ran %d times, want 1", *committed)
	}
}

// S12: a retry replays the answer while retained without a second effect; a
// retry at exactly the retention starts anew and finds no member.
func TestRevokeMemberReplaysWhileRetainedAndStartsAnewAtExpiry(t *testing.T) {
	service, store, c, committed := memberFixture(t)
	first, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA))
	if err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	state := revocationState(store)

	c.advance(invitation.OperationRetention - time.Nanosecond)
	if again, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil || again != first {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
	if revocationState(store) != state || *committed != 1 {
		t.Fatalf("the replay wrote or notified (hook %d)", *committed)
	}

	c.advance(time.Nanosecond)
	if again, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); !errors.Is(err, invitation.ErrNotFound) || again != (invitation.MemberRevocation{}) {
		t.Fatalf("retry at expiry = %+v, %v; want ErrNotFound", again, err)
	}
	if revocationState(store) != state || *committed != 1 {
		t.Fatal("the retry at expiry wrote or notified")
	}
}

func TestRevokeMemberRejectsTheSameOperationForAnotherAccount(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	store.AddMember(tenantA, guest, access.RoleModerator)
	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	state := revocationState(store)

	if got, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, guest)); !errors.Is(err, invitation.ErrOperationConflict) || got != (invitation.MemberRevocation{}) {
		t.Fatalf("RevokeMember(guest) = %+v, %v; want ErrOperationConflict", got, err)
	}
	other := revokeMember(revokeOpID, moderatorA)
	other.OwnerID = ownerB
	store.AddMember(tenantB, moderatorA, access.RoleModerator)
	state = revocationState(store)
	if _, err := service.RevokeMember(context.Background(), other); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("RevokeMember by owner B: err = %v, want ErrOperationConflict", err)
	}
	if revocationState(store) != state || *committed != 1 {
		t.Fatal("a conflicting revocation wrote or notified")
	}
}

func TestRevokeMemberOfAnUnknownAccountANonMemberOrTheOwnerIsNotFound(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	store.AddMember(tenantB, guest, access.RoleModerator)
	state := revocationState(store)
	for i, account := range []access.AccountID{access.AccountID(unknownID), stranger, ownerA, ownerB, guest} {
		got, err := service.RevokeMember(context.Background(), revokeMember(fmt.Sprintf("revoke-member-%07d", i+2), account))
		if !errors.Is(err, invitation.ErrNotFound) || got != (invitation.MemberRevocation{}) {
			t.Fatalf("%s: RevokeMember = %+v, %v; want ErrNotFound", account, got, err)
		}
	}
	if revocationState(store) != state || *committed != 0 {
		t.Fatal("a not found revocation wrote or notified")
	}
}

// S6: a moderator, a stranger, an unknown account, and the owner of an
// inactive tenant get the shared denial before any operation is read.
func TestRevokeMemberIsDeniedToEveryoneButTheActiveOwner(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	store.AddMember(tenantA, guest, access.RoleModerator)
	store.AddMember(tenantC, guest, access.RoleModerator)
	state, lookups := revocationState(store), store.OperationLookups()
	for _, actor := range []access.AccountID{moderatorA, stranger, inactiveOwner, unknownActor} {
		request := revokeMember(revokeOpID, guest)
		request.OwnerID = actor
		if got, err := service.RevokeMember(context.Background(), request); !errors.Is(err, access.ErrAccessDenied) || got != (invitation.MemberRevocation{}) {
			t.Fatalf("%s: RevokeMember = %+v, %v; want ErrAccessDenied", actor, got, err)
		}
	}
	if revocationState(store) != state || store.OperationLookups() != lookups || *committed != 0 {
		t.Fatal("a denied revocation read an operation, wrote, or notified")
	}
}

func TestRevokeMemberReauthorizesItsReplays(t *testing.T) {
	service, store, _, _ := memberFixture(t)
	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	store.SetStatus(tenantA, access.SubscriptionInactive)
	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("replay after deactivation: err = %v, want ErrAccessDenied", err)
	}
}

func TestMalformedRevokeMemberRequestsAreRejectedBeforeAnyTransaction(t *testing.T) {
	service, store, _, _ := memberFixture(t)
	transactions := store.Transactions()
	for name, request := range map[string]invitation.RevokeMemberRequest{
		"short operation ID":   revokeMember("short", moderatorA),
		"operation ID symbols": revokeMember("operation-000000!", moderatorA),
		"long operation ID":    revokeMember(strings.Repeat("a", 129), moderatorA),
		"empty account":        revokeMember(revokeOpID, ""),
		"short account":        revokeMember(revokeOpID, moderatorA[1:]),
		"upper case account":   revokeMember(revokeOpID, access.AccountID(strings.ToUpper(string(moderatorA)))),
		"account symbols":      revokeMember(revokeOpID, moderatorA[1:]+"!"),
	} {
		if got, err := service.RevokeMember(context.Background(), request); !errors.Is(err, invitation.ErrInvalid) || got != (invitation.MemberRevocation{}) {
			t.Fatalf("%s: RevokeMember = %+v, %v; want ErrInvalid", name, got, err)
		}
	}
	if store.Transactions() != transactions {
		t.Fatal("a malformed request started a transaction")
	}
}

// S7: revoking an account in tenant A keeps its moderation of tenant B.
func TestRevokeMemberInOneTenantKeepsTheOther(t *testing.T) {
	service, store, _, _ := memberFixture(t)
	store.AddMember(tenantB, moderatorA, access.RoleModerator)
	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	if role, ok := store.Membership(tenantB, moderatorA); !ok || role != access.RoleModerator {
		t.Fatalf("membership in B = %q, %v; want moderator", role, ok)
	}
	if store.AccessVersion(tenantB) != 1 {
		t.Fatalf("B access version = %d, want 1", store.AccessVersion(tenantB))
	}
	for _, cut := range store.Cuts() {
		if cut.TenantID != tenantA {
			t.Fatalf("cut %+v names another tenant", cut)
		}
	}
	moderations, err := service.Moderations(context.Background(), moderatorA)
	if err != nil || len(moderations) != 1 || moderations[0].TenantID != tenantB {
		t.Fatalf("Moderations = %+v, %v; want only B", moderations, err)
	}
}

// S11: a failure at any step leaves the membership, version, cut, and record
// unchanged; the retry under the same operation ID revokes once.
func TestAFailureAtAnyRevokeMemberStepLeavesNothing(t *testing.T) {
	steps := []string{"LockOwnedTenantForUpdate", "MemberRevocation", "LockMembership", "RemoveModerator", "IncrementAccessVersion", "RecordCut", "RecordMemberRevocation", "commit"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			service, store, _, committed := memberFixture(t)
			state := revocationState(store)
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if revocationState(store) != state || *committed != 0 {
				t.Fatal("a failed revocation left state behind or notified")
			}
			want := invitation.MemberRevocation{TenantID: tenantA, AccountID: moderatorA}
			for range 2 {
				if got, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil || got != want {
					t.Fatalf("retry = %+v, %v; want %+v", got, err, want)
				}
			}
			if len(store.Cuts()) != 1 || store.AccessVersion(tenantA) != 2 || *committed != 1 {
				t.Fatalf("cuts = %d, version = %d, hook = %d; want one revocation", len(store.Cuts()), store.AccessVersion(tenantA), *committed)
			}
		})
	}
}

// A concurrent revocation that commits the same operation first is replayed.
func TestAConcurrentCommitOfTheSameMemberRevocationIsReplayed(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	var winner invitation.MemberRevocation
	store.BeforeOnce("RecordMemberRevocation", func() {
		var err error
		if winner, err = service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil {
			t.Errorf("concurrent RevokeMember: %v", err)
		}
	})

	got, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA))
	if err != nil || got != winner || got.AccountID != moderatorA {
		t.Fatalf("RevokeMember = %+v, %v; want the committed %+v", got, err, winner)
	}
	if len(store.Cuts()) != 1 || store.AccessVersion(tenantA) != 2 || len(store.MemberRevocations()) != 1 || *committed != 1 {
		t.Fatalf("cuts = %d, version = %d, records = %d, hook = %d; want one revocation",
			len(store.Cuts()), store.AccessVersion(tenantA), len(store.MemberRevocations()), *committed)
	}
}

func TestTheSameRevokeMemberOperationCommittedConcurrentlyForAnotherAccountIsAConflict(t *testing.T) {
	service, store, _, committed := memberFixture(t)
	store.AddMember(tenantA, guest, access.RoleModerator)
	store.BeforeOnce("RecordMemberRevocation", func() {
		if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, guest)); err != nil {
			t.Errorf("concurrent RevokeMember: %v", err)
		}
	})

	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("RevokeMember: err = %v, want ErrOperationConflict", err)
	}
	if role, ok := store.Membership(tenantA, moderatorA); !ok || role != access.RoleModerator {
		t.Fatal("the conflicting revocation removed its moderator")
	}
	if _, ok := store.Membership(tenantA, guest); ok || len(store.Cuts()) != 1 || *committed != 1 {
		t.Fatal("want only the committed revocation of guest")
	}
}
