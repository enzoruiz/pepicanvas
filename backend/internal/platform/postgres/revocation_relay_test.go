package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// moderatorOfBoth seeds owners of tenants A and B and one moderator of both.
func moderatorOfBoth(t *testing.T, f invitations) (ownerA access.AccountID, tenantA string, ownerB access.AccountID, tenantB string, moderator access.AccountID) {
	t.Helper()
	ownerA, tenantA = seedOwner(t, f, uniqueEmail("owner-a"))
	ownerB, tenantB = seedOwner(t, f, uniqueEmail("owner-b"))
	moderator = seedModerator(t, f, ownerA, uniqueEmail("moderator"))
	token := newToken(t)
	mustCreateInvitation(t, f, ownerB, token)
	if _, err := acceptWith(f.svc, moderator, token); err != nil {
		t.Fatalf("Accept in B: %v", err)
	}
	return ownerA, tenantA, ownerB, tenantB, moderator
}

func editorGrantIn(t *testing.T, svc *invitation.Service, account access.AccountID, session, tenantID string, version uint64) revocation.Grant {
	t.Helper()
	grant, err := svc.EditorGrant(context.Background(), account, session, tenantID)
	want := revocation.Grant{TenantID: tenantID, AccountID: string(account), SessionID: session, Client: revocation.ClientEditor, Version: version}
	if err != nil || grant != want {
		t.Fatalf("EditorGrant(%s in %s) = %+v, %v; want %+v", account, tenantID, grant, err, want)
	}
	return grant
}

func registerConn(t *testing.T, registry *revocation.Registry, grant revocation.Grant) (*revocation.Lease, *closingConn) {
	t.Helper()
	conn := &closingConn{}
	lease, err := registry.Register(grant, conn)
	if err != nil {
		t.Fatalf("Register(%+v): %v", grant, err)
	}
	return lease, conn
}

func requireClosedWith(t *testing.T, lease *revocation.Lease, conn *closingConn, reason revocation.Reason) {
	t.Helper()
	if !errors.Is(lease.Err(), revocation.ErrAccessRevoked) {
		t.Fatalf("lease Err = %v, want ErrAccessRevoked", lease.Err())
	}
	if reasons := conn.closed(); len(reasons) != 1 || reasons[0] != reason {
		t.Fatalf("conn closed with %v, want %s once", reasons, reason)
	}
}

func requireOpen(t *testing.T, lease *revocation.Lease, conn *closingConn) {
	t.Helper()
	if lease.Err() != nil || len(conn.closed()) != 0 {
		t.Fatalf("lease Err = %v, closes %v; want a live lease", lease.Err(), conn.closed())
	}
}

// S11 (CTRL-CONS-01): the revocation and its cut commit together; a process
// that stops before applying it, or whose registry refused it, leaves it
// pending, and the next process applies it once at start and sets
// applied_at. A revocation that failed before its commit cuts nothing.
func TestRelayAppliesARevocationLeftPendingByACrashOnce(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	ctx := context.Background()
	crashed := openInvitations(t, server, clock)
	ownerA, tenantA, ownerB, tenantB, moderator := moderatorOfBoth(t, crashed)
	pre := editorGrantIn(t, crashed.svc, moderator, "session-mod-a", tenantA, 1)
	preOwner := editorGrantIn(t, crashed.svc, ownerA, "session-owner-a", tenantA, 1)
	preB := editorGrantIn(t, crashed.svc, moderator, "session-mod-b", tenantB, 1)

	// A revocation in B that fails at its commit writes nothing.
	injected := errors.New("injected commit failure")
	postgres.SetInvitationFault(crashed.store, func(_ context.Context, step string) error {
		if step == "commit" {
			return injected
		}
		return nil
	})
	if _, err := revokeMemberWith(crashed.svc, ownerB, nextInvitationOp("op-revoke-ghost"), moderator); !errors.Is(err, injected) {
		t.Fatalf("RevokeMember with a failed commit = %v, want the injected error", err)
	}
	postgres.SetInvitationFault(crashed.store, nil)
	if rows := countRevocationRows(t, crashed.pool, tenantB); rows != (revocationRows{moderators: 1, version: 1}) {
		t.Fatalf("B rows after the failed revocation = %+v, want unchanged", rows)
	}

	if _, err := revokeMemberWith(crashed.svc, ownerA, nextInvitationOp("op-revoke-crash"), moderator); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	// The crashed process's registry was already closed: the relay keeps
	// the cut pending.
	closed, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(ctx); err != nil {
		t.Fatal(err)
	}
	refusing := &countingCutter{registry: closed}
	crashedCuts := signalingCuts{SubscriptionStore: crashed.subs.store, reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	crashedRelay := startRelay(t, crashedCuts, refusing)
	receive(t, crashedCuts.reads, "the crashed startup pass")
	if err := crashedRelay.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(refusing.applied()); n == 0 || len(crashedCuts.settles) != 0 {
		t.Fatalf("refused applies = %d, settles = %d; want at least one refusal and no settle", n, len(crashedCuts.settles))
	}
	crashed.pool.Close()

	restarted := openInvitations(t, server, clock)
	if rows := countRevocationRows(t, restarted.pool, tenantA); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("A rows after the crash = %+v, want the committed revocation and one pending cut", rows)
	}
	if rows := countRevocationRows(t, restarted.pool, tenantB); rows != (revocationRows{moderators: 1, version: 1}) {
		t.Fatalf("B rows after the crash = %+v, want unchanged", rows)
	}
	pending, err := restarted.subs.store.PendingCuts(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Kind != subscription.CutRevocation || pending[0].AccountID != moderator ||
		pending[0].TenantID != tenantA || pending[0].Version != 2 || pending[0].CorrelationID != revocationCorrelation {
		t.Fatalf("PendingCuts = %+v, %v; want the revocation of the moderator in A at 2", pending, err)
	}

	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Clients reconnecting with their pre-cut grants before the relay runs.
	lease, conn := registerConn(t, registry, pre)
	ownerLease, ownerConn := registerConn(t, registry, preOwner)
	leaseB, connB := registerConn(t, registry, preB)
	cutter := &countingCutter{registry: registry}
	store := signalingCuts{SubscriptionStore: restarted.subs.store, reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	relay := startRelay(t, store, cutter)
	receive(t, store.reads, "the startup pass")
	if err := receive(t, store.settles, "the settle"); err != nil {
		t.Fatalf("SettleCut: %v", err)
	}

	want := revocation.Cut{
		OperationID:   fmt.Sprintf("cut-%d", pending[0].ID),
		Type:          revocation.TypeRevocation,
		Target:        revocation.Target{Kind: revocation.TargetAccount, TenantID: tenantA, ID: string(moderator)},
		Version:       2,
		CorrelationID: revocationCorrelation,
	}
	if cuts := cutter.applied(); len(cuts) != 1 || cuts[0] != want {
		t.Fatalf("cuts = %+v, want one %+v", cuts, want)
	}
	requireClosedWith(t, lease, conn, revocation.ReasonRevoked)
	requireOpen(t, ownerLease, ownerConn)
	requireOpen(t, leaseB, connB)
	if n := count(t, restarted.pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND kind = 'revocation' AND applied_at IS NOT NULL", tenantA); n != 1 {
		t.Fatal("the recovered cut has no applied_at")
	}
	if rows := countRevocationRows(t, restarted.pool, tenantA); rows != (revocationRows{version: 2, cuts: 1, records: 1}) {
		t.Fatalf("A rows = %+v, want the cut settled", rows)
	}

	// S4: the stale grant never reconnects and a fresh read is denied; the
	// owner and the moderator's access to B are unchanged.
	if _, err := registry.Register(pre, &closingConn{}); !errors.Is(err, revocation.ErrAccessRevoked) {
		t.Fatalf("Register(pre-cut) after recovery = %v, want ErrAccessRevoked", err)
	}
	if grant, err := restarted.svc.EditorGrant(ctx, moderator, "session-mod-a", tenantA); !errors.Is(err, access.ErrAccessDenied) || grant != (revocation.Grant{}) {
		t.Fatalf("EditorGrant after revocation = %+v, %v; want ErrAccessDenied", grant, err)
	}
	registerConn(t, registry, editorGrantIn(t, restarted.svc, ownerA, "session-owner-a2", tenantA, 2))
	registerConn(t, registry, editorGrantIn(t, restarted.svc, moderator, "session-mod-b2", tenantB, 1))

	// Later passes apply nothing more.
	relay.Notify()
	receive(t, store.reads, "a second pass")
	if err := relay.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again := signalingCuts{SubscriptionStore: postgres.NewSubscriptionStore(restarted.pool), reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	startRelay(t, again, cutter)
	receive(t, again.reads, "the next startup pass")
	if n := len(cutter.applied()); n != 1 {
		t.Fatalf("cuts applied = %d, want still 1", n)
	}
	if reasons := conn.closed(); len(reasons) != 1 {
		t.Fatalf("conn closed %d times, want once", len(reasons))
	}
	requireOpen(t, leaseB, connB)
	if len(store.settles) != 0 || len(again.settles) != 0 {
		t.Fatal("a later pass settled a cut")
	}
	if n := count(t, restarted.pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1", tenantB); n != 0 {
		t.Fatalf("B cuts = %d, want none from the failed revocation", n)
	}
}

// S7: a revocation in A and a concurrent deactivation of B commit to one
// outbox and each cuts only its own connections, with its own reason.
func TestRelayAppliesARevocationAndAConcurrentDeactivation(t *testing.T) {
	f := openInvitations(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, f.subs, uniqueEmail("operator"))
	emailB := uniqueEmail("owner-b")
	ownerA, tenantA := seedOwner(t, f, uniqueEmail("owner-a"))
	seedAccount(t, f.subs, emailB)
	b := mustActivate(t, f.subs, operator, nextInvitationOp("op-activate-b"), emailB)
	moderator := seedModerator(t, f, ownerA, uniqueEmail("moderator"))
	token := newToken(t)
	mustCreateInvitation(t, f, b.AccountID, token)
	if _, err := acceptWith(f.svc, moderator, token); err != nil {
		t.Fatalf("Accept in B: %v", err)
	}

	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	modA, modConnA := registerConn(t, registry, editorGrantIn(t, f.svc, moderator, "session-mod-a", tenantA, 1))
	ownA, ownConnA := registerConn(t, registry, editorGrantIn(t, f.svc, ownerA, "session-owner-a", tenantA, 1))
	modB, modConnB := registerConn(t, registry, editorGrantIn(t, f.svc, moderator, "session-mod-b", b.TenantID, 1))
	ownB, ownConnB := registerConn(t, registry, editorGrantIn(t, f.svc, b.AccountID, "session-owner-b", b.TenantID, 1))
	cutter := &countingCutter{registry: registry}
	store := signalingCuts{SubscriptionStore: f.subs.store, reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	relay := startRelay(t, store, cutter)
	receive(t, store.reads, "the startup pass")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() {
		_, err := revokeMemberWith(f.svc, ownerA, nextInvitationOp("op-revoke-concurrent"), moderator)
		errs <- err
	})
	wg.Go(func() {
		_, err := deactivateIn(f.subs, operator, nextInvitationOp("op-deactivate-b"), emailB)
		errs <- err
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent commit: %v", err)
		}
	}
	relay.Notify()
	for range 2 {
		if err := receive(t, store.settles, "a settle"); err != nil {
			t.Fatalf("SettleCut: %v", err)
		}
	}

	revoked := revocation.Target{Kind: revocation.TargetAccount, TenantID: tenantA, ID: string(moderator)}
	deactivated := revocation.Target{Kind: revocation.TargetTenant, TenantID: b.TenantID}
	cuts := cutter.applied()
	if len(cuts) != 2 {
		t.Fatalf("cuts = %+v, want two", cuts)
	}
	seen := map[revocation.Target]revocation.Type{}
	for _, cut := range cuts {
		if cut.Version != 2 {
			t.Fatalf("cut %+v, want version 2", cut)
		}
		seen[cut.Target] = cut.Type
	}
	if seen[revoked] != revocation.TypeRevocation || seen[deactivated] != revocation.TypeDeactivation {
		t.Fatalf("cuts = %+v, want the revocation in A and the deactivation of B", cuts)
	}
	requireClosedWith(t, modA, modConnA, revocation.ReasonRevoked)
	requireOpen(t, ownA, ownConnA)
	requireClosedWith(t, modB, modConnB, revocation.ReasonDeactivated)
	requireClosedWith(t, ownB, ownConnB, revocation.ReasonDeactivated)
	if n := count(t, f.pool, "SELECT count(*) FROM access_cuts WHERE applied_at IS NULL"); n != 0 {
		t.Fatalf("pending cuts = %d, want none", n)
	}
	editorGrantIn(t, f.svc, ownerA, "session-owner-a2", tenantA, 2)
	for _, account := range []access.AccountID{moderator, b.AccountID} {
		if _, err := f.svc.EditorGrant(ctx, account, "session-b2", b.TenantID); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("EditorGrant(%s in inactive B) = %v, want ErrAccessDenied", account, err)
		}
	}
}
