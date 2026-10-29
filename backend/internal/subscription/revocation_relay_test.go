package subscription_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation/invitationtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// US-006 revocation cuts reach the relay through the access_cuts outbox they
// share with deactivations; invitationtest.MemoryStore keeps that outbox.

const (
	revTenantA     = "tenant-a"
	revTenantB     = "tenant-b"
	revOwnerA      = access.AccountID("000000000000000000000000000000a1")
	revOwnerB      = access.AccountID("000000000000000000000000000000b1")
	revModerator   = access.AccountID("000000000000000000000000000000d1")
	moderatorEmail = "moderator@example.com"
	revokeOp       = "revoke-member-0000001"
	revInviteToken = "pci_0000000000000000000000000000000000000000001"
)

// moderated seeds tenants A and B, each with its owner, and the moderator in
// both, and returns a service whose committed hook is notify (nil for none).
func moderated(t *testing.T, notify func()) (*invitation.Service, *invitationtest.MemoryStore) {
	t.Helper()
	store := invitationtest.NewMemoryStore()
	store.AddTenant(revTenantA, revOwnerA, access.SubscriptionActive)
	store.AddTenant(revTenantB, revOwnerB, access.SubscriptionActive)
	for _, account := range []access.AccountID{revOwnerA, revOwnerB} {
		store.AddAccount(account, string(account)+"@example.com")
	}
	store.AddAccount(revModerator, moderatorEmail)
	store.AddMember(revTenantA, revModerator, access.RoleModerator)
	store.AddMember(revTenantB, revModerator, access.RoleModerator)
	var options []invitation.Option
	if notify != nil {
		options = append(options, invitation.WithCommitted(notify))
	}
	return invitation.NewService(store, options...), store
}

func revokeModeratorInA(t *testing.T, service *invitation.Service) {
	t.Helper()
	got, err := service.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
		OwnerID: revOwnerA, OperationID: revokeOp, AccountID: revModerator, CorrelationID: cutCorrelation,
	})
	if want := (invitation.MemberRevocation{TenantID: revTenantA, AccountID: revModerator}); err != nil || got != want {
		t.Fatalf("RevokeMember = %+v, %v; want %+v", got, err, want)
	}
}

func memberGrant(t *testing.T, service *invitation.Service, account access.AccountID, session, tenantID string) revocation.Grant {
	t.Helper()
	grant, err := service.EditorGrant(context.Background(), account, session, tenantID)
	if err != nil {
		t.Fatalf("EditorGrant(%s in %s): %v", account, tenantID, err)
	}
	want := revocation.Grant{TenantID: tenantID, AccountID: string(account), SessionID: session, Client: revocation.ClientEditor, Version: grant.Version}
	if grant != want || grant.Version == 0 {
		t.Fatalf("EditorGrant = %+v, want %+v at a confirmed version", grant, want)
	}
	return grant
}

func wantRevocationCut(pending subscription.PendingCut) revocation.Cut {
	return revocation.Cut{
		OperationID:   wantCut(pending).OperationID,
		Type:          revocation.TypeRevocation,
		Target:        revocation.Target{Kind: revocation.TargetAccount, TenantID: revTenantA, ID: string(revModerator)},
		Version:       pending.Version,
		CorrelationID: pending.CorrelationID,
	}
}

func onlyOutboxCut(t *testing.T, store *invitationtest.MemoryStore) subscription.PendingCut {
	t.Helper()
	cuts := store.Outbox()
	if len(cuts) != 1 {
		t.Fatalf("outbox = %+v, want one cut", cuts)
	}
	return cuts[0]
}

func requireOutboxPending(t *testing.T, store *invitationtest.MemoryStore, want int) {
	t.Helper()
	pending, err := store.PendingCuts(context.Background(), 100)
	if err != nil || len(pending) != want {
		t.Fatalf("PendingCuts = %+v, %v; want %d", pending, err, want)
	}
}

func requireClosedOnceWith(t *testing.T, conn *fakeConn, want revocation.Reason, aborted bool) {
	t.Helper()
	if reason, closes, gotAborted := conn.state(); reason != want || closes != 1 || gotAborted != aborted {
		t.Fatalf("conn reason=%q closes=%d aborted=%v; want %q once, aborted=%v", reason, closes, gotAborted, want, aborted)
	}
}

// S3, S14: the cut closes the moderator's editor leases in A within the
// budget with access_revoked, once; the owner of A and the moderator's lease
// in B stay live; the logs carry the correlation and no personal data.
func TestARevocationClosesOnlyTheModeratorsLeasesInThatTenant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store := moderated(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay

		modConnA, hungConnA := newConn(100*time.Millisecond), newConn(-1)
		ownerConnA, modConnB, ownerConnB := newConn(0), newConn(0), newConn(0)
		leaseModA := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-a1", revTenantA), modConnA)
		leaseHungA := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-a2", revTenantA), hungConnA)
		leaseOwnerA := register(t, live.registry, memberGrant(t, service, revOwnerA, "session-owner-a", revTenantA), ownerConnA)
		leaseModB := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-b", revTenantB), modConnB)
		leaseOwnerB := register(t, live.registry, memberGrant(t, service, revOwnerB, "session-owner-b", revTenantB), ownerConnB)
		live.start(t)

		start := time.Now()
		revokeModeratorInA(t, service)
		synctest.Wait()
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("cut applied after %v, want at once", elapsed)
		}
		requireCut(t, leaseModA)
		requireCut(t, leaseHungA)
		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()

		cut := onlyOutboxCut(t, store)
		if cut.Kind != subscription.CutRevocation || cut.AccountID != revModerator || cut.TenantID != revTenantA || cut.Version != 2 || cut.CorrelationID != cutCorrelation {
			t.Fatalf("outbox cut = %+v, want the revocation of the moderator in A at 2", cut)
		}
		calls := live.cutter.recorded()
		if len(calls) != 1 || calls[0].cut != wantRevocationCut(cut) || calls[0].err != nil || calls[0].correlation != cutCorrelation {
			t.Fatalf("calls = %+v, want one clean Apply of %+v carrying the correlation", calls, wantRevocationCut(cut))
		}
		report := calls[0].report
		if len(report.Closures) != 2 || report.Exceeded != 0 || report.MaxLatency > revocation.DefaultBudget || report.MaxLatency > 2*time.Second {
			t.Fatalf("report = %+v, want two closes within the budget", report)
		}
		requireClosedOnceWith(t, modConnA, revocation.ReasonRevoked, false)
		requireClosedOnceWith(t, hungConnA, revocation.ReasonRevoked, true)

		// S7: the owner of A and every lease of B are untouched.
		for _, lease := range []*revocation.Lease{leaseOwnerA, leaseModB, leaseOwnerB} {
			requireLive(t, lease)
		}
		for _, conn := range []*fakeConn{ownerConnA, modConnB, ownerConnB} {
			requireUntouched(t, conn)
		}

		at, settled := store.AppliedAt(cut.ID)
		if !settled || at.Before(start) || at.Sub(start) > revocation.DefaultBudget {
			t.Fatalf("AppliedAt = %v, %v; want settled within the budget", at, settled)
		}
		requireOutboxPending(t, store, 0)
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1})

		// S14: the relay and the registry log the cut under its correlation
		// and tenant, and no record names the moderator or a secret.
		records := live.logs()
		applied := recordsWith(records, "access cut applied")
		if len(applied) != 1 || applied[0][logging.CorrelationKey] != cutCorrelation || applied[0]["tenant_id"] != revTenantA ||
			applied[0]["cut_id"] != float64(cut.ID) || applied[0]["version"] != float64(2) || applied[0]["affected"] != float64(2) {
			t.Fatalf("applied records = %v", applied)
		}
		confirmed := recordsWith(records, "access cut confirmed")
		closed := recordsWith(records, "connection closed")
		if len(confirmed) != 1 || confirmed[0]["type"] != string(revocation.TypeRevocation) || len(closed) != 2 {
			t.Fatalf("confirmed=%v closed=%d, want one revocation and 2", confirmed, len(closed))
		}
		for _, record := range append(confirmed, closed...) {
			if record[logging.CorrelationKey] != cutCorrelation || record["tenant_id"] != revTenantA {
				t.Fatalf("registry record %v lacks the cut's correlation or tenant", record)
			}
		}
		raw, _ := json.Marshal(records)
		for _, leak := range []string{moderatorEmail, "@example.com", "session-", string(revModerator), string(revOwnerA), "pci_"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("logs contain %q: %s", leak, raw)
			}
		}
	})
}

// S4: a grant read before the revocation never reconnects, a fresh read is
// denied, the moderator's access to B is unchanged, and only a new
// acceptance yields a grant that registers.
func TestARevokedModeratorsGrantsNeverRestoreAccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store := moderated(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay
		live.start(t)

		pre := memberGrant(t, service, revModerator, "session-mod-a", revTenantA)
		preOwner := memberGrant(t, service, revOwnerA, "session-owner-a", revTenantA)
		preB := memberGrant(t, service, revModerator, "session-mod-b", revTenantB)
		revokeModeratorInA(t, service)
		synctest.Wait()
		requireOutboxPending(t, store, 0)

		// An existing tab reconnecting with its old grant.
		conn := newConn(0)
		if _, err := live.registry.Register(pre, conn); !errors.Is(err, revocation.ErrAccessRevoked) {
			t.Fatalf("Register(pre-revocation) = %v, want ErrAccessRevoked", err)
		}
		requireUntouched(t, conn)
		// A fresh read is the shared denial and no grant.
		if grant, err := service.EditorGrant(context.Background(), revModerator, "session-mod-a", revTenantA); !errors.Is(err, access.ErrAccessDenied) || grant != (revocation.Grant{}) {
			t.Fatalf("EditorGrant after revocation = %+v, %v; want ErrAccessDenied", grant, err)
		}
		// The owner's grant and the moderator's grant in B still register.
		requireLive(t, register(t, live.registry, preOwner, newConn(0)))
		requireLive(t, register(t, live.registry, preB, newConn(0)))
		if fresh := memberGrant(t, service, revModerator, "session-mod-b", revTenantB); fresh.Version != 1 {
			t.Fatalf("grant in B = %+v, want B's version unchanged at 1", fresh)
		}

		// A new invitation accepted again yields a grant at the new version.
		invite, err := service.Create(context.Background(), invitation.CreateRequest{
			OwnerID: revOwnerA, OperationID: "create-invite-0000001", Token: secret.New(secret.KindInvitationToken, revInviteToken),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := service.Accept(context.Background(), invitation.AcceptRequest{
			AccountID: revModerator, OperationID: "accept-invite-0000001", Token: secret.New(secret.KindInvitationToken, revInviteToken),
		}); err != nil {
			t.Fatalf("Accept(%s): %v", invite.ID, err)
		}
		fresh := memberGrant(t, service, revModerator, "session-mod-a", revTenantA)
		if fresh.Version != 2 {
			t.Fatalf("fresh grant = %+v, want version 2", fresh)
		}
		requireLive(t, register(t, live.registry, fresh, newConn(0)))
		if _, err := live.registry.Register(pre, newConn(0)); !errors.Is(err, revocation.ErrAccessRevoked) {
			t.Fatalf("Register(pre-revocation) after acceptance = %v, want ErrAccessRevoked", err)
		}
		if n := len(store.Outbox()); n != 1 {
			t.Fatalf("outbox = %d cuts, want only the revocation's", n)
		}
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1})
	})
}

// S7: a revocation in A and a concurrent deactivation of B each cut only
// their own connections, with their own reason.
func TestARevocationAndAConcurrentDeactivationOfAnotherTenantBothApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store := moderated(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay

		modConnA, ownerConnA, modConnB, ownerConnB := newConn(0), newConn(0), newConn(0), newConn(0)
		leaseModA := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-a", revTenantA), modConnA)
		leaseOwnerA := register(t, live.registry, memberGrant(t, service, revOwnerA, "session-owner-a", revTenantA), ownerConnA)
		leaseModB := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-b", revTenantB), modConnB)
		leaseOwnerB := register(t, live.registry, memberGrant(t, service, revOwnerB, "session-owner-b", revTenantB), ownerConnB)
		live.start(t)

		var wg sync.WaitGroup
		wg.Go(func() { revokeModeratorInA(t, service) })
		wg.Go(func() {
			store.DeactivateTenant(revTenantB, cutCorrelation, time.Now())
			relay.Notify()
		})
		wg.Wait()
		synctest.Wait()
		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()

		requireCut(t, leaseModA)
		requireCut(t, leaseModB)
		requireCut(t, leaseOwnerB)
		requireLive(t, leaseOwnerA)
		requireClosedOnceWith(t, modConnA, revocation.ReasonRevoked, false)
		requireClosedOnceWith(t, modConnB, revocation.ReasonDeactivated, false)
		requireClosedOnceWith(t, ownerConnB, revocation.ReasonDeactivated, false)
		requireUntouched(t, ownerConnA)

		cuts := store.Outbox()
		calls := live.cutter.recorded()
		if len(cuts) != 2 || len(calls) != 2 {
			t.Fatalf("outbox=%+v calls=%+v, want two of each", cuts, calls)
		}
		for i, pending := range cuts {
			want := wantCut(pending)
			if pending.Kind == subscription.CutRevocation {
				want = wantRevocationCut(pending)
			}
			if calls[i].cut != want || calls[i].err != nil {
				t.Fatalf("call %d = %+v, want %+v", i, calls[i], want)
			}
		}
		requireOutboxPending(t, store, 0)
		requireCounters(t, relay, subscription.RelayCounters{Applied: 2})

		if grant := memberGrant(t, service, revOwnerA, "session-owner-a", revTenantA); grant.Version != 2 {
			t.Fatalf("owner grant in A = %+v, want version 2", grant)
		}
		for _, account := range []access.AccountID{revModerator, revOwnerB} {
			if _, err := service.EditorGrant(context.Background(), account, "session-b", revTenantB); !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("EditorGrant(%s in inactive B) = %v, want ErrAccessDenied", account, err)
			}
		}
	})
}

// S11: a revocation committed by a process that stopped before applying it is
// applied once by the next process at start; one that failed before its
// commit closes nothing.
func TestARevocationLeftPendingByACrashIsAppliedAtStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crashed, store := moderated(t, nil)
		pre := memberGrant(t, crashed, revModerator, "session-mod-a", revTenantA)
		preOwner := memberGrant(t, crashed, revOwnerA, "session-owner-a", revTenantA)
		revokeModeratorInA(t, crashed)
		requireOutboxPending(t, store, 1)

		restarted := newRig(t, store, nil)
		conn, ownerConn := newConn(50*time.Millisecond), newConn(0)
		lease := register(t, restarted.registry, pre, conn)
		ownerLease := register(t, restarted.registry, preOwner, ownerConn)
		start := time.Now()
		restarted.start(t)
		synctest.Wait()
		if time.Since(start) != 0 {
			t.Fatal("the pending cut was not applied at Start")
		}
		requireCut(t, lease)
		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()
		requireClosedOnceWith(t, conn, revocation.ReasonRevoked, false)
		requireLive(t, ownerLease)
		requireUntouched(t, ownerConn)

		cut := onlyOutboxCut(t, store)
		if calls := restarted.cutter.recorded(); len(calls) != 1 || calls[0].cut != wantRevocationCut(cut) {
			t.Fatalf("calls = %+v, want one Apply of the pending revocation", calls)
		}
		if _, settled := store.AppliedAt(cut.ID); !settled {
			t.Fatal("the recovered cut was not settled")
		}
		requireOutboxPending(t, store, 0)
		requireCounters(t, restarted.relay, subscription.RelayCounters{Applied: 1})

		// Later passes apply nothing more.
		time.Sleep(3 * subscription.DefaultRelayPollInterval)
		synctest.Wait()
		if n := len(restarted.cutter.recorded()); n != 1 {
			t.Fatalf("calls = %d, want still 1", n)
		}

		// A revocation whose commit failed writes no cut and closes nothing.
		again := newConn(0)
		bLease := register(t, restarted.registry, memberGrant(t, crashed, revModerator, "session-mod-b", revTenantB), again)
		store.FailOnce("commit", errors.New("commit lost"))
		if _, err := crashed.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
			OwnerID: revOwnerB, OperationID: "revoke-member-0000002", AccountID: revModerator, CorrelationID: cutCorrelation,
		}); err == nil {
			t.Fatal("RevokeMember with a failed commit succeeded")
		}
		restarted.relay.Notify()
		time.Sleep(3 * subscription.DefaultRelayPollInterval)
		synctest.Wait()
		requireLive(t, bLease)
		requireUntouched(t, again)
		if n := len(store.Outbox()); n != 1 || len(restarted.cutter.recorded()) != 1 {
			t.Fatalf("outbox = %d, calls = %d; want no ghost cut", n, len(restarted.cutter.recorded()))
		}
	})
}

// kindStore rewrites the kind of every pending cut, as a row written by a
// newer release this relay does not know.
type kindStore struct {
	*invitationtest.MemoryStore
	kind subscription.CutKind
}

func (s kindStore) PendingCuts(ctx context.Context, limit int) ([]subscription.PendingCut, error) {
	cuts, err := s.MemoryStore.PendingCuts(ctx, limit)
	for i := range cuts {
		cuts[i].Kind = s.kind
	}
	return cuts, err
}

// A cut of an unknown kind is never applied as a tenant cut: it settles as
// rejected with an ERROR record and every lease stays live.
func TestACutOfAnUnknownKindIsRejectedWithoutReachingTheRegistry(t *testing.T) {
	for _, kind := range []subscription.CutKind{"", "suspension"} {
		t.Run(string(kind), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var relay *subscription.CutRelay
				service, store := moderated(t, func() { relay.Notify() })
				live := newRig(t, kindStore{store, kind}, nil)
				relay = live.relay
				ownerConn, modConn := newConn(0), newConn(0)
				ownerLease := register(t, live.registry, memberGrant(t, service, revOwnerA, "session-owner-a", revTenantA), ownerConn)
				modLease := register(t, live.registry, memberGrant(t, service, revModerator, "session-mod-a", revTenantA), modConn)
				live.start(t)

				revokeModeratorInA(t, service)
				synctest.Wait()
				time.Sleep(revocation.DefaultBudget)
				synctest.Wait()

				if calls := live.cutter.recorded(); len(calls) != 0 {
					t.Fatalf("calls = %+v, want none", calls)
				}
				requireLive(t, ownerLease)
				requireLive(t, modLease)
				requireUntouched(t, ownerConn)
				requireUntouched(t, modConn)
				cut := onlyOutboxCut(t, store)
				if _, settled := store.AppliedAt(cut.ID); !settled {
					t.Fatal("the unknown cut was not settled")
				}
				requireCounters(t, relay, subscription.RelayCounters{Rejected: 1})
				found := recordsWith(live.logs(), "access cut rejected")
				if len(found) != 1 || found[0]["level"] != "ERROR" || found[0]["tenant_id"] != revTenantA ||
					found[0][logging.CorrelationKey] != cutCorrelation || found[0]["cut_id"] != float64(cut.ID) {
					t.Fatalf("rejected records = %v, want one ERROR record of the cut", found)
				}
			})
		})
	}
}
