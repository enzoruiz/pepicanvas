package postgres_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	invitationCorrelation = "invitation-correlation-0001"
	unknownInvitationID   = "0123456789abcdef0123456789abcdef"
)

type invitations struct {
	subs  subscriptions
	svc   *invitation.Service
	store *postgres.InvitationStore
	pool  *pgxpool.Pool
	clock *accountClock
}

func openInvitations(t *testing.T, server *pgtest.Server, clock *accountClock) invitations {
	t.Helper()
	subs := openSubscriptions(t, server, clock)
	store := postgres.NewInvitationStore(subs.pool)
	return invitations{
		subs:  subs,
		svc:   invitation.NewService(store, invitation.WithClock(clock.Now)),
		store: store,
		pool:  subs.pool,
		clock: clock,
	}
}

// service returns another service over its own store on the same pool, so a
// fault hook set on that store affects only its transactions.
func (f invitations) service(now func() time.Time) (*invitation.Service, *postgres.InvitationStore) {
	store := postgres.NewInvitationStore(f.pool)
	return invitation.NewService(store, invitation.WithClock(now)), store
}

var invitationSeq atomic.Int64

func nextInvitationOp(prefix string) string {
	return fmt.Sprintf("%s-%010d", prefix, invitationSeq.Add(1))
}

// uniqueEmail keeps accounts distinct across tests that share a server.
func uniqueEmail(name string) string {
	return fmt.Sprintf("%s-%d@example.com", name, invitationSeq.Add(1))
}

// TestInvitationStore runs the invitation tests that need no restart on one
// server. Each test seeds its own owners and counts only its own tenants.
func TestInvitationStore(t *testing.T) {
	f := openInvitations(t, pgtest.Start(t), newAccountClock())
	for _, tc := range []struct {
		name string
		run  func(*testing.T, invitations)
	}{
		{"ExpiresSevenDaysAfterCreation", testInvitationExpiresSevenDaysAfterCreation},
		{"TableRejectsInconsistentRows", testInvitationTableRejectsInconsistentRows},
		{"OnlyTheActiveOwnerAdministers", testOnlyTheActiveOwnerAdministersInvitations},
		{"CreatingGrantsNoAccess", testCreatingAnInvitationGrantsNoAccess},
		{"RevocationAndAcceptanceKeepTheFirstCommit", testRevocationAndAcceptanceKeepTheFirstCommit},
		{"HeldAcceptanceMakesTheWaitingRevocationAnswerAccepted", testHeldAcceptanceMakesTheWaitingRevocationAnswerAccepted},
		{"HeldRevocationMakesTheWaitingAcceptanceFail", testHeldRevocationMakesTheWaitingAcceptanceFail},
		{"TwoTenantsNeverCross", testInvitationsOfTwoTenantsNeverCross},
		{"ConcurrentRevocationsRevokeOnce", testConcurrentRevocationsRevokeOnce},
		{"ConcurrentCreatesUnderOneOperationGiveOneInvitation", testConcurrentCreatesUnderOneOperationGiveOneInvitation},
		{"StoreClockReplaysIdenticalAnswers", testStoreClockReplaysIdenticalAnswers},
		{"RetentionExpires", testInvitationRetentionExpiresInPostgres},
		{"FailedStepLeavesNothingAndRetrySucceedsOnce", testFailedInvitationStepLeavesNothingAndRetrySucceedsOnce},
		{"StoreReportsTakenTokensAndRetainedOperations", testInvitationStoreReportsTakenTokensAndRetainedOperations},
		{"AcceptanceGrantsExactlyThatTenant", testInvitationAcceptanceGrantsExactlyThatTenant},
		{"ConcurrentAcceptancesOfOneLinkGrantOneMembership", testConcurrentAcceptancesOfOneLinkGrantOneMembership},
		{"ConcurrentAcceptancesUnderOneOperationGiveOneEffect", testConcurrentAcceptancesUnderOneOperationGiveOneEffect},
		{"WaitingRetryReplaysTheHeldAcceptance", testWaitingRetryReplaysTheHeldAcceptance},
		{"AcceptanceOperationConflicts", testInvitationAcceptanceOperationConflicts},
		{"AcceptanceRetentionExpires", testInvitationAcceptanceRetentionExpires},
		{"LostAcceptanceAnswerReplaysTheCommit", testLostAcceptanceAnswerReplaysTheCommit},
		{"UnusableLinksWriteNothing", testUnusableInvitationLinksWriteNothing},
		{"OneAccountAcceptingTwoLinksOfOneTenantGetsOneMembership", testOneAccountAcceptingTwoLinksOfOneTenantGetsOneMembership},
		{"AcceptancesOfTwoTenantsNeverCross", testInvitationAcceptancesOfTwoTenantsNeverCross},
		{"StoreReportsTakenMemberships", testInvitationStoreReportsTakenMemberships},
		{"MemberRevocationCommitsEverythingAtOnce", testMemberRevocationCommitsEverythingAtOnce},
		{"FailedMemberRevocationLeavesNothing", testFailedMemberRevocationLeavesNothing},
		{"MemberRevocationReplaysConflictsAndExpires", testMemberRevocationReplaysConflictsAndExpires},
		{"MemberRevocationRejectionsWriteNothing", testMemberRevocationRejectionsWriteNothing},
		{"MemberRevocationOfTwoTenantsNeverCross", testMemberRevocationOfTwoTenantsNeverCross},
		{"ConcurrentMemberRevocationsGiveOneEffect", testConcurrentMemberRevocationsGiveOneEffect},
		{"MemberRevocationWaitsForASharedTenantLock", testMemberRevocationWaitsForASharedTenantLock},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, f) })
	}
}

// seedOwner activates a new account and returns it and its tenant.
func seedOwner(t *testing.T, f invitations, email string) (access.AccountID, string) {
	t.Helper()
	operator := seedOperator(t, f.subs, "operator-"+email)
	seedAccount(t, f.subs, email)
	activation := mustActivate(t, f.subs, operator, nextInvitationOp("op-owner"), email)
	return activation.AccountID, activation.TenantID
}

func newToken(t *testing.T) secret.Value {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return secret.New(secret.KindInvitationToken, "pci_"+base64.RawURLEncoding.EncodeToString(buf))
}

func createWith(svc *invitation.Service, owner access.AccountID, op string, token secret.Value, correlation string) (invitation.Invitation, error) {
	return svc.Create(context.Background(), invitation.CreateRequest{OwnerID: owner, OperationID: op, Token: token, CorrelationID: correlation})
}

func mustCreateInvitation(t *testing.T, f invitations, owner access.AccountID, token secret.Value) invitation.Invitation {
	t.Helper()
	created, err := createWith(f.svc, owner, nextInvitationOp("op-create"), token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func revokeWith(svc *invitation.Service, owner access.AccountID, op, invitationID, correlation string) (invitation.Revocation, error) {
	return svc.Revoke(context.Background(), invitation.RevokeRequest{OwnerID: owner, OperationID: op, InvitationID: invitationID, CorrelationID: correlation})
}

// acceptWith accepts under a new operation ID.
func acceptWith(svc *invitation.Service, account access.AccountID, token secret.Value) (invitation.Acceptance, error) {
	return acceptOp(svc, account, nextInvitationOp("op-accept"), token, invitationCorrelation)
}

func acceptOp(svc *invitation.Service, account access.AccountID, op string, token secret.Value, correlation string) (invitation.Acceptance, error) {
	return svc.Accept(context.Background(), invitation.AcceptRequest{AccountID: account, OperationID: op, Token: token, CorrelationID: correlation})
}

// canvasOf returns the canvas of a tenant.
func canvasOf(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var canvas string
	if err := pool.QueryRow(context.Background(), "SELECT canvas_id FROM tenants WHERE tenant_id = $1", tenantID).Scan(&canvas); err != nil {
		t.Fatalf("read canvas of %s: %v", tenantID, err)
	}
	return canvas
}

// invitationRows counts what invitation operations write, scoped to a tenant.
type invitationRows struct{ invitations, revoked, accepted, records, moderators int }

func countInvitationRows(t *testing.T, pool *pgxpool.Pool, tenantID string) invitationRows {
	t.Helper()
	return invitationRows{
		invitations: count(t, pool, "SELECT count(*) FROM invitations WHERE tenant_id = $1", tenantID),
		revoked:     count(t, pool, "SELECT count(*) FROM invitations WHERE tenant_id = $1 AND revoked_at IS NOT NULL", tenantID),
		accepted:    count(t, pool, "SELECT count(*) FROM invitations WHERE tenant_id = $1 AND accepted_at IS NOT NULL", tenantID),
		records:     count(t, pool, "SELECT count(*) FROM invitation_operations WHERE tenant_id = $1", tenantID),
		moderators:  count(t, pool, "SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'moderator'", tenantID),
	}
}

// rowText renders every column of the invitation as text.
func rowText(t *testing.T, pool *pgxpool.Pool, invitationID string) string {
	t.Helper()
	var text string
	if err := pool.QueryRow(context.Background(), "SELECT row_to_json(i)::text FROM invitations i WHERE invitation_id = $1", invitationID).Scan(&text); err != nil {
		t.Fatalf("read invitation row: %v", err)
	}
	return text
}

// hold makes the transactions of store wait at step until release runs. It
// releases and drains on cleanup, before the pool closes.
func hold(t *testing.T, store *postgres.InvitationStore, step string) (<-chan struct{}, func()) {
	t.Helper()
	reached, released := make(chan struct{}), make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	postgres.SetInvitationFault(store, func(_ context.Context, at string) error {
		if at == step {
			reachedOnce.Do(func() { close(reached) })
			<-released
		}
		return nil
	})
	t.Cleanup(release)
	return reached, release
}

// waitForLockWait returns once a backend waits on a lock.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for count(t, pool, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no transaction waits on a lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tickClock advances one millisecond on every read.
type tickClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *tickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(time.Millisecond)
	return now
}

func testInvitationExpiresSevenDaysAfterCreation(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	late := seedAccount(t, f.subs, uniqueEmail("late"))

	token, lateToken := newToken(t), newToken(t)
	created := mustCreateInvitation(t, f, owner, token)
	mustCreateInvitation(t, f, owner, lateToken)
	if created.TenantID != tenant || created.ExpiresAt.Sub(created.CreatedAt) != invitation.Lifetime || !created.CreatedAt.Equal(f.clock.Now()) {
		t.Fatalf("created = %+v, want tenant %s expiring %v after now", created, tenant, invitation.Lifetime)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitations WHERE invitation_id = $1 AND tenant_id = $2 AND created_by = $3 AND created_at = $4 AND expires_at = $5",
		created.ID, tenant, string(owner), created.CreatedAt, created.ExpiresAt); n != 1 {
		t.Fatal("the stored invitation does not keep the answered creation and expiry")
	}

	// Pending until one microsecond before expiry, expired at exactly expiry.
	f.clock.Advance(invitation.Lifetime - time.Microsecond)
	if pending, err := f.svc.List(ctx, owner); err != nil || len(pending) != 2 {
		t.Fatalf("List before expiry = %+v, %v; want both invitations", pending, err)
	}
	if _, err := acceptWith(f.svc, guest, token); err != nil {
		t.Fatalf("Accept before expiry: %v", err)
	}
	f.clock.Advance(time.Microsecond)
	if pending, err := f.svc.List(ctx, owner); err != nil || len(pending) != 0 {
		t.Fatalf("List at expiry = %+v, %v; want none", pending, err)
	}
	if _, err := acceptWith(f.svc, late, lateToken); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("Accept at expiry: error = %v, want ErrUnavailable", err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, late, tenant); err != nil || found {
		t.Fatalf("expired acceptance membership found = %v, %v; want none", found, err)
	}
}

func testInvitationTableRejectsInconsistentRows(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("pci_check"))
	insert := "INSERT INTO invitations (invitation_id, tenant_id, token_hash, created_by, created_at, expires_at, revoked_at, accepted_at, accepted_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"

	for _, tc := range []struct {
		name       string
		id         string
		hash       []byte
		expires    time.Time
		revoked    *time.Time
		accepted   *time.Time
		acceptedBy *string
	}{
		{name: "expiry one microsecond late", id: unknownInvitationID, hash: hash[:], expires: created.Add(invitation.Lifetime + time.Microsecond)},
		{name: "expiry one day early", id: unknownInvitationID, hash: hash[:], expires: created.Add(invitation.Lifetime - 24*time.Hour)},
		{name: "malformed invitation ID", id: "NOT-HEX", hash: hash[:], expires: created.Add(invitation.Lifetime)},
		{name: "short token hash", id: unknownInvitationID, hash: hash[:31], expires: created.Add(invitation.Lifetime)},
		{name: "revoked and accepted", id: unknownInvitationID, hash: hash[:], expires: created.Add(invitation.Lifetime), revoked: &created, accepted: &created, acceptedBy: (*string)(&owner)},
		{name: "accepted without acceptor", id: unknownInvitationID, hash: hash[:], expires: created.Add(invitation.Lifetime), accepted: &created},
		{name: "acceptor without acceptance", id: unknownInvitationID, hash: hash[:], expires: created.Add(invitation.Lifetime), acceptedBy: (*string)(&owner)},
	} {
		_, err := f.pool.Exec(context.Background(), insert, tc.id, tenant, tc.hash, string(owner), created, tc.expires, tc.revoked, tc.accepted, tc.acceptedBy)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: error = %v, want check_violation", tc.name, err)
		}
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitations WHERE tenant_id = $1", tenant); n != 0 {
		t.Fatalf("invitations = %d, want 0", n)
	}
	exec(t, f.pool, insert, "fedcba9876543210fedcba9876543210", tenant, hash[:], string(owner), created, created.Add(invitation.Lifetime), nil, nil, nil)
}

func testOnlyTheActiveOwnerAdministersInvitations(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	moderator := seedAccount(t, f.subs, uniqueEmail("moderator"))
	stranger := seedAccount(t, f.subs, uniqueEmail("stranger"))
	inactive, inactiveTenant := seedOwner(t, f, uniqueEmail("inactive"))

	token := newToken(t)
	mustCreateInvitation(t, f, owner, token)
	if _, err := acceptWith(f.svc, moderator, token); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	pending := mustCreateInvitation(t, f, owner, newToken(t))
	stale := mustCreateInvitation(t, f, inactive, newToken(t))
	exec(t, f.pool, "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = $1", inactiveTenant)

	before := countInvitationRows(t, f.pool, tenant)
	beforeInactive := countInvitationRows(t, f.pool, inactiveTenant)
	beforeRow := rowText(t, f.pool, pending.ID)
	for _, actor := range []struct {
		name    string
		account access.AccountID
		target  string
	}{
		{"moderator", moderator, pending.ID},
		{"stranger", stranger, pending.ID},
		{"unknown account", "ffffffffffffffffffffffffffffffff", pending.ID},
		{"inactive owner", inactive, stale.ID},
	} {
		if _, err := createWith(f.svc, actor.account, nextInvitationOp("op-denied"), newToken(t), invitationCorrelation); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s Create: error = %v, want ErrAccessDenied", actor.name, err)
		}
		if _, err := revokeWith(f.svc, actor.account, nextInvitationOp("op-denied"), actor.target, invitationCorrelation); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s Revoke: error = %v, want ErrAccessDenied", actor.name, err)
		}
		if _, err := f.svc.List(ctx, actor.account); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s List: error = %v, want ErrAccessDenied", actor.name, err)
		}
	}
	if got := countInvitationRows(t, f.pool, tenant); got != before {
		t.Fatalf("denied requests changed the tenant: %+v, want %+v", got, before)
	}
	if got := countInvitationRows(t, f.pool, inactiveTenant); got != beforeInactive {
		t.Fatalf("denied requests changed the inactive tenant: %+v, want %+v", got, beforeInactive)
	}
	if got := rowText(t, f.pool, pending.ID); got != beforeRow {
		t.Fatalf("denied revocation changed the row: %s, want %s", got, beforeRow)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitation_operations WHERE operation_id LIKE 'op-denied-%'"); n != 0 {
		t.Fatalf("denied requests recorded %d operations", n)
	}
}

func testCreatingAnInvitationGrantsNoAccess(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	memberships := count(t, f.pool, "SELECT count(*) FROM memberships WHERE tenant_id = $1", tenant)

	mustCreateInvitation(t, f, owner, newToken(t))
	if n := count(t, f.pool, "SELECT count(*) FROM memberships WHERE tenant_id = $1", tenant); n != memberships {
		t.Fatalf("memberships = %d after creation, want %d", n, memberships)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || found {
		t.Fatalf("guest membership found = %v, %v; want none", found, err)
	}
}

func testRevocationAndAcceptanceKeepTheFirstCommit(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))

	// Revoked first: the later acceptance is refused.
	token := newToken(t)
	revokedFirst := mustCreateInvitation(t, f, owner, token)
	if got, err := revokeWith(f.svc, owner, nextInvitationOp("op-revoke"), revokedFirst.ID, invitationCorrelation); err != nil || got.Status != access.InvitationRevoked {
		t.Fatalf("Revoke = %+v, %v; want revoked", got, err)
	}
	if _, err := acceptWith(f.svc, guest, token); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("Accept after revocation: error = %v, want ErrUnavailable", err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || found {
		t.Fatalf("membership after a refused acceptance found = %v, %v; want none", found, err)
	}

	// Accepted first: the later revocation answers accepted and keeps it.
	token = newToken(t)
	acceptedFirst := mustCreateInvitation(t, f, owner, token)
	accepted, err := acceptWith(f.svc, guest, token)
	if err != nil || accepted != (invitation.Acceptance{TenantID: tenant, CanvasID: canvasOf(t, f.pool, tenant), InvitationID: acceptedFirst.ID}) {
		t.Fatalf("Accept = %+v, %v", accepted, err)
	}
	if got, err := revokeWith(f.svc, owner, nextInvitationOp("op-revoke"), acceptedFirst.ID, invitationCorrelation); err != nil || got.Status != access.InvitationAccepted {
		t.Fatalf("Revoke after acceptance = %+v, %v; want accepted", got, err)
	}
	if m, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || !found || m.Role != access.RoleModerator {
		t.Fatalf("membership = %+v, %v, %v; want moderator", m, found, err)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitations WHERE invitation_id = $1 AND revoked_at IS NULL AND accepted_by = $2", acceptedFirst.ID, string(guest)); n != 1 {
		t.Fatal("the accepted invitation was revoked")
	}
	// A token is single use.
	other := seedAccount(t, f.subs, uniqueEmail("other"))
	if _, err := acceptWith(f.svc, other, token); !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("second acceptance: error = %v, want ErrUnavailable", err)
	}
}

func testHeldAcceptanceMakesTheWaitingRevocationAnswerAccepted(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)

	acceptSvc, acceptStore := f.service(f.clock.Now)
	reached, release := hold(t, acceptStore, "commit")
	accepted, revoked := make(chan error, 1), make(chan error, 1)
	var answer invitation.Revocation
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	wg.Go(func() {
		_, err := acceptWith(acceptSvc, guest, token)
		accepted <- err
	})
	select {
	case <-reached:
	case err := <-accepted:
		t.Fatalf("acceptance ended before commit: %v", err)
	}
	wg.Go(func() {
		var err error
		answer, err = revokeWith(f.svc, owner, nextInvitationOp("op-revoke"), created.ID, invitationCorrelation)
		revoked <- err
	})
	waitForLockWait(t, f.pool)
	select {
	case err := <-revoked:
		t.Fatalf("revocation did not wait for the held acceptance: %v", err)
	default:
	}
	release()
	if err := <-accepted; err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := <-revoked; err != nil || answer != (invitation.Revocation{InvitationID: created.ID, Status: access.InvitationAccepted}) {
		t.Fatalf("Revoke = %+v, %v; want accepted", answer, err)
	}
	if m, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || !found || m.Role != access.RoleModerator {
		t.Fatalf("membership = %+v, %v, %v; want moderator", m, found, err)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 3, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

func testHeldRevocationMakesTheWaitingAcceptanceFail(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)

	revokeSvc, revokeStore := f.service(f.clock.Now)
	reached, release := hold(t, revokeStore, "commit")
	accepted, revoked := make(chan error, 1), make(chan error, 1)
	var answer invitation.Revocation
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	wg.Go(func() {
		var err error
		answer, err = revokeWith(revokeSvc, owner, nextInvitationOp("op-revoke"), created.ID, invitationCorrelation)
		revoked <- err
	})
	select {
	case <-reached:
	case err := <-revoked:
		t.Fatalf("revocation ended before commit: %v", err)
	}
	wg.Go(func() {
		_, err := acceptWith(f.svc, guest, token)
		accepted <- err
	})
	waitForLockWait(t, f.pool)
	select {
	case err := <-accepted:
		t.Fatalf("acceptance did not wait for the held revocation: %v", err)
	default:
	}
	release()
	if err := <-revoked; err != nil || answer.Status != access.InvitationRevoked {
		t.Fatalf("Revoke = %+v, %v; want revoked", answer, err)
	}
	if err := <-accepted; !errors.Is(err, invitation.ErrUnavailable) {
		t.Fatalf("Accept after the held revocation: error = %v, want ErrUnavailable", err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || found {
		t.Fatalf("membership found = %v, %v; want none", found, err)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, revoked: 1, records: 2}) {
		t.Fatalf("rows = %+v", rows)
	}
}

func testInvitationsOfTwoTenantsNeverCross(t *testing.T, f invitations) {
	ctx := context.Background()
	ownerA, tenantA := seedOwner(t, f, uniqueEmail("owner-a"))
	ownerB, tenantB := seedOwner(t, f, uniqueEmail("owner-b"))
	guestA := seedAccount(t, f.subs, uniqueEmail("guest-a"))
	guestB := seedAccount(t, f.subs, uniqueEmail("guest-b"))

	// Both owners create at the same time.
	tokens := []secret.Value{newToken(t), newToken(t)}
	owners := []access.AccountID{ownerA, ownerB}
	created := make([]invitation.Invitation, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range owners {
		wg.Go(func() {
			created[i], errs[i] = createWith(f.svc, owners[i], nextInvitationOp("op-tenant"), tokens[i], invitationCorrelation)
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Create errors: %v, %v", errs[0], errs[1])
	}
	if created[0].TenantID != tenantA || created[1].TenantID != tenantB {
		t.Fatalf("created = %+v, want one per tenant", created)
	}

	// Another tenant's invitation ID is not found, exactly like an unknown one.
	rowA := rowText(t, f.pool, created[0].ID)
	for _, id := range []string{created[0].ID, unknownInvitationID} {
		if _, err := revokeWith(f.svc, ownerB, nextInvitationOp("op-cross"), id, invitationCorrelation); !errors.Is(err, invitation.ErrNotFound) {
			t.Fatalf("Revoke(%s) by the other owner: error = %v, want ErrNotFound", id, err)
		}
	}
	if got := rowText(t, f.pool, created[0].ID); got != rowA {
		t.Fatalf("cross-tenant revocation changed the row: %s", got)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitation_operations WHERE operation_id LIKE 'op-cross-%'"); n != 0 {
		t.Fatalf("not-found revocations recorded %d operations", n)
	}
	for i, owner := range owners {
		pending, err := f.svc.List(ctx, owner)
		if err != nil || len(pending) != 1 || pending[0] != created[i] {
			t.Fatalf("List(%s) = %+v, %v; want only %+v", owner, pending, err, created[i])
		}
	}

	// Accepting tenant A's token grants tenant A only.
	accepted, errA := acceptWith(f.svc, guestA, tokens[0])
	if errA != nil || accepted.TenantID != tenantA {
		t.Fatalf("Accept(A) = %+v, %v", accepted, errA)
	}
	if m, found, err := f.subs.accounts.MembershipOf(ctx, guestA, tenantA); err != nil || !found || m.Role != access.RoleModerator {
		t.Fatalf("MembershipOf(guest A, A) = %+v, %v, %v; want moderator", m, found, err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guestA, tenantB); err != nil || found {
		t.Fatalf("MembershipOf(guest A, B) found = %v, %v; want none", found, err)
	}
	if _, err := acceptWith(f.svc, guestB, tokens[1]); err != nil {
		t.Fatalf("Accept(B): %v", err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guestB, tenantA); err != nil || found {
		t.Fatalf("MembershipOf(guest B, A) found = %v, %v; want none", found, err)
	}
}

func testConcurrentRevocationsRevokeOnce(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	created := mustCreateInvitation(t, f, owner, newToken(t))

	clock := &tickClock{now: f.clock.Now().Add(time.Second)}
	svc, _ := f.service(clock.Now)
	const n = 16
	answers := make([]invitation.Revocation, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			answers[i], errs[i] = revokeWith(svc, owner, nextInvitationOp("op-concurrent-revoke"), created.ID, invitationCorrelation)
		})
	}
	close(start)
	wg.Wait()
	want := invitation.Revocation{InvitationID: created.ID, Status: access.InvitationRevoked}
	for i := range n {
		if errs[i] != nil || answers[i] != want {
			t.Fatalf("revocation %d = %+v, %v; want %+v", i, answers[i], errs[i], want)
		}
	}
	// Exactly one revocation wrote revoked_at: the one whose record carries it.
	if m := count(t, f.pool, "SELECT count(*) FROM invitation_operations o JOIN invitations i USING (invitation_id) WHERE i.tenant_id = $1 AND o.kind = 'revoke' AND o.accepted_at = i.revoked_at", tenant); m != 1 {
		t.Fatalf("records accepted at revoked_at = %d, want 1", m)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, revoked: 1, records: n + 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	row := rowText(t, f.pool, created.ID)
	if again, err := revokeWith(svc, owner, nextInvitationOp("op-concurrent-revoke"), created.ID, invitationCorrelation); err != nil || again != want {
		t.Fatalf("later revocation = %+v, %v", again, err)
	}
	if got := rowText(t, f.pool, created.ID); got != row {
		t.Fatalf("later revocation changed the row: %s, want %s", got, row)
	}
}

func testConcurrentCreatesUnderOneOperationGiveOneInvitation(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	token := newToken(t)

	const n = 16
	op := nextInvitationOp("op-concurrent-create")
	answers := make([]invitation.Invitation, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			answers[i], errs[i] = createWith(f.svc, owner, op, token, invitationCorrelation)
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || answers[i] != answers[0] {
			t.Fatalf("create %d = %+v, %v; want %+v", i, answers[i], errs[i], answers[0])
		}
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, records: 1}) {
		t.Fatalf("rows = %+v, want one invitation and one record", rows)
	}
}

func TestInvitationAnswersReplayAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	f := openInvitations(t, server, clock)
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))

	token := newToken(t)
	createOp, revokeOp := nextInvitationOp("op-restart-create"), nextInvitationOp("op-restart-revoke")
	created, err := createWith(f.svc, owner, createOp, token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	doomed := mustCreateInvitation(t, f, owner, newToken(t))
	revoked, err := revokeWith(f.svc, owner, revokeOp, doomed.ID, invitationCorrelation)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := countInvitationRows(t, f.pool, tenant)

	// The answers are lost; the owner retries over a new pool after a server
	// restart, with a new correlation ID.
	server.Restart(t)
	f = openInvitations(t, server, clock)
	clock.Advance(invitation.OperationRetention - time.Microsecond)

	if again, err := createWith(f.svc, owner, createOp, token, "retry-correlation-0002"); err != nil || again != created {
		t.Fatalf("create retry = %+v, %v; want %+v", again, err, created)
	}
	if again, err := revokeWith(f.svc, owner, revokeOp, doomed.ID, "retry-correlation-0002"); err != nil || again != revoked {
		t.Fatalf("revoke retry = %+v, %v; want %+v", again, err, revoked)
	}
	if _, err := createWith(f.svc, owner, createOp, newToken(t), invitationCorrelation); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("create with a different token: error = %v, want ErrOperationConflict", err)
	}
	if _, err := revokeWith(f.svc, owner, revokeOp, created.ID, invitationCorrelation); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("revoke of a different invitation: error = %v, want ErrOperationConflict", err)
	}
	if _, err := revokeWith(f.svc, owner, createOp, created.ID, invitationCorrelation); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("revoke under a create operation ID: error = %v, want ErrOperationConflict", err)
	}
	if got := countInvitationRows(t, f.pool, tenant); got != before {
		t.Fatalf("rows = %+v, want the single first effects %+v", got, before)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitation_operations WHERE operation_id IN ($1, $2) AND correlation_id = $3", createOp, revokeOp, invitationCorrelation); n != 2 {
		t.Fatal("the records lost the first correlation ID")
	}

	// A pool that never saw the writes reads the pending invitation.
	pending, err := f.svc.List(ctx, owner)
	if err != nil || len(pending) != 1 || pending[0] != created {
		t.Fatalf("List after restart = %+v, %v; want %+v", pending, err, created)
	}

	// Only the token hash is stored.
	hash := sha256.Sum256([]byte(token.Reveal()))
	stored := rowText(t, f.pool, created.ID)
	var records string
	if err := f.pool.QueryRow(ctx, "SELECT string_agg(row_to_json(o)::text, ',') FROM invitation_operations o").Scan(&records); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(token.Reveal(), "pci_")
	for _, text := range []string{stored, records} {
		if strings.Contains(text, body) {
			t.Fatalf("a stored row contains the token: %s", text)
		}
	}
	if !strings.Contains(stored, hex.EncodeToString(hash[:])) {
		t.Fatalf("the stored row %s does not hold the token hash", stored)
	}
}

func testStoreClockReplaysIdenticalAnswers(t *testing.T, f invitations) {
	owner, _ := seedOwner(t, f, uniqueEmail("owner"))
	store := postgres.NewInvitationStore(f.pool)
	svc := invitation.NewService(store, invitation.WithClock(store.Now))

	token, op := newToken(t), nextInvitationOp("op-store-clock")
	first, err := createWith(svc, owner, op, token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	again, err := createWith(svc, owner, op, token, invitationCorrelation)
	if err != nil || again != first {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
}

func testInvitationRetentionExpiresInPostgres(t *testing.T, f invitations) {
	clock := f.clock
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))

	op := nextInvitationOp("op-retention")
	first, err := createWith(f.svc, owner, op, newToken(t), invitationCorrelation)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustCreateInvitation(t, f, owner, newToken(t))
	clock.Advance(invitation.OperationRetention)

	second, err := createWith(f.svc, owner, op, newToken(t), invitationCorrelation)
	if err != nil || second.ID == first.ID {
		t.Fatalf("expired operation ID = %+v, %v; want a new invitation", second, err)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitation_operations WHERE operation_id = $1 AND invitation_id = $2", op, second.ID); n != 1 {
		t.Fatal("the expired record was not replaced")
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 3, records: 1}) {
		t.Fatalf("rows = %+v, want three invitations and only the unexpired record", rows)
	}
}

func testFailedInvitationStepLeavesNothingAndRetrySucceedsOnce(t *testing.T, f invitations) {
	injected := errors.New("injected fault")
	t.Cleanup(func() { postgres.SetInvitationFault(f.store, nil) })
	fail := func(store *postgres.InvitationStore, step string) {
		postgres.SetInvitationFault(store, func(_ context.Context, at string) error {
			if at == step {
				return injected
			}
			return nil
		})
	}

	t.Run("create", func(t *testing.T) {
		for _, step := range []string{"LockOwnedTenant", "Operation", "InsertInvitation", "RecordOperation", "commit"} {
			owner, tenant := seedOwner(t, f, uniqueEmail("create-"+strings.ToLower(step)))
			token, op := newToken(t), nextInvitationOp("op-fault-create")
			fail(f.store, step)
			if _, err := createWith(f.svc, owner, op, token, invitationCorrelation); !errors.Is(err, injected) {
				t.Fatalf("%s: error = %v, want the injected fault", step, err)
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{}) {
				t.Fatalf("%s: failed creation left %+v", step, rows)
			}
			postgres.SetInvitationFault(f.store, nil)
			first, err := createWith(f.svc, owner, op, token, invitationCorrelation)
			if err != nil {
				t.Fatalf("%s: retry: %v", step, err)
			}
			if again, err := createWith(f.svc, owner, op, token, invitationCorrelation); err != nil || again != first {
				t.Fatalf("%s: replay = %+v, %v; want %+v", step, again, err, first)
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, records: 1}) {
				t.Fatalf("%s: rows after retry = %+v", step, rows)
			}
		}
	})

	t.Run("revoke", func(t *testing.T) {
		for _, step := range []string{"LockOwnedTenant", "Operation", "LockInvitation", "RevokeInvitation", "RecordOperation", "commit"} {
			owner, tenant := seedOwner(t, f, uniqueEmail("revoke-"+strings.ToLower(step)))
			created := mustCreateInvitation(t, f, owner, newToken(t))
			op := nextInvitationOp("op-fault-revoke")
			fail(f.store, step)
			if _, err := revokeWith(f.svc, owner, op, created.ID, invitationCorrelation); !errors.Is(err, injected) {
				t.Fatalf("%s: error = %v, want the injected fault", step, err)
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, records: 1}) {
				t.Fatalf("%s: failed revocation left %+v", step, rows)
			}
			postgres.SetInvitationFault(f.store, nil)
			for range 2 {
				if got, err := revokeWith(f.svc, owner, op, created.ID, invitationCorrelation); err != nil || got.Status != access.InvitationRevoked {
					t.Fatalf("%s: retry = %+v, %v", step, got, err)
				}
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, revoked: 1, records: 2}) {
				t.Fatalf("%s: rows after retry = %+v", step, rows)
			}
		}
	})

	// S5: a fault before the commit leaves the link pending and grants
	// nothing; the retry under the same operation ID accepts once.
	t.Run("accept", func(t *testing.T) {
		for _, step := range []string{"LockInvitationByToken", "Operation", "LockTenant", "IsMember", "AddModerator", "MarkAccepted", "RecordOperation", "commit"} {
			owner, tenant := seedOwner(t, f, uniqueEmail("accept-"+strings.ToLower(step)))
			guest := seedAccount(t, f.subs, uniqueEmail("guest-"+strings.ToLower(step)))
			token, op := newToken(t), nextInvitationOp("op-fault-accept")
			created := mustCreateInvitation(t, f, owner, token)
			fail(f.store, step)
			if _, err := acceptOp(f.svc, guest, op, token, invitationCorrelation); !errors.Is(err, injected) {
				t.Fatalf("%s: error = %v, want the injected fault", step, err)
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, records: 1}) {
				t.Fatalf("%s: failed acceptance left %+v", step, rows)
			}
			if _, found, err := f.subs.accounts.MembershipOf(context.Background(), guest, tenant); err != nil || found {
				t.Fatalf("%s: membership found = %v, %v; want none", step, found, err)
			}
			postgres.SetInvitationFault(f.store, nil)
			want := invitation.Acceptance{TenantID: tenant, CanvasID: canvasOf(t, f.pool, tenant), InvitationID: created.ID}
			for range 2 {
				if got, err := acceptOp(f.svc, guest, op, token, invitationCorrelation); err != nil || got != want {
					t.Fatalf("%s: retry = %+v, %v; want %+v", step, got, err, want)
				}
			}
			if _, err := acceptWith(f.svc, guest, token); !errors.Is(err, invitation.ErrUnavailable) {
				t.Fatalf("%s: acceptance under another operation: error = %v, want ErrUnavailable", step, err)
			}
			if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
				t.Fatalf("%s: rows after retry = %+v", step, rows)
			}
		}
	})
}

func testInvitationStoreReportsTakenTokensAndRetainedOperations(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	token, op := newToken(t), nextInvitationOp("op-store-ports")
	created, err := createWith(f.svc, owner, op, token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	now := f.clock.Now()
	err = f.store.InTx(ctx, func(tx invitation.Tx) error {
		return tx.InsertInvitation(ctx, invitation.Stored{
			ID: unknownInvitationID, TenantID: tenant, TokenHash: sha256.Sum256([]byte(token.Reveal())),
			CreatedBy: owner, CreatedAt: now, ExpiresAt: now.Add(invitation.Lifetime),
		})
	})
	if !errors.Is(err, invitation.ErrTokenTaken) {
		t.Fatalf("InsertInvitation of a stored token: error = %v, want ErrTokenTaken", err)
	}
	err = f.store.InTx(ctx, func(tx invitation.Tx) error {
		return tx.RecordOperation(ctx, invitation.Record{
			OperationID: op, Kind: invitation.OperationCreate, OwnerID: owner, Invitation: created, AcceptedAt: now,
		}, now.Add(-invitation.OperationRetention))
	})
	if !errors.Is(err, invitation.ErrOperationRetained) {
		t.Fatalf("RecordOperation of a retained ID: error = %v, want ErrOperationRetained", err)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, records: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

// acceptRecord is the stored row of an acceptance operation.
type acceptRecord struct {
	kind, owner, tenant, invitation, canvas, status, correlation string
}

func readAcceptRecord(t *testing.T, pool *pgxpool.Pool, op string) acceptRecord {
	t.Helper()
	var r acceptRecord
	err := pool.QueryRow(context.Background(), `SELECT kind, owner_account_id, tenant_id, invitation_id, coalesce(canvas_id, ''), coalesce(status, ''), coalesce(correlation_id, '')
		FROM invitation_operations WHERE operation_id = $1 AND created_at IS NULL AND expires_at IS NULL`, op).
		Scan(&r.kind, &r.owner, &r.tenant, &r.invitation, &r.canvas, &r.status, &r.correlation)
	if err != nil {
		t.Fatalf("read record %s: %v", op, err)
	}
	return r
}

func testInvitationAcceptanceGrantsExactlyThatTenant(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	_, otherTenant := seedOwner(t, f, uniqueEmail("other-owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)
	pending := mustCreateInvitation(t, f, owner, newToken(t))
	pendingRow := rowText(t, f.pool, pending.ID)

	op := nextInvitationOp("op-accept")
	got, err := acceptOp(f.svc, guest, op, token, invitationCorrelation)
	want := invitation.Acceptance{TenantID: tenant, CanvasID: canvasOf(t, f.pool, tenant), InvitationID: created.ID}
	if err != nil || got != want {
		t.Fatalf("Accept = %+v, %v; want %+v", got, err, want)
	}
	if m, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || !found || m.Role != access.RoleModerator {
		t.Fatalf("membership = %+v, %v, %v; want moderator", m, found, err)
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, guest, otherTenant); err != nil || found {
		t.Fatalf("membership in the other tenant found = %v, %v; want none", found, err)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitations WHERE invitation_id = $1 AND accepted_by = $2 AND accepted_at = $3 AND revoked_at IS NULL",
		created.ID, string(guest), f.clock.Now()); n != 1 {
		t.Fatal("the invitation was not consumed by the guest at the acceptance time")
	}
	if got := rowText(t, f.pool, pending.ID); got != pendingRow {
		t.Fatalf("the other pending invitation changed: %s", got)
	}
	record := readAcceptRecord(t, f.pool, op)
	if record != (acceptRecord{"accept", string(guest), tenant, created.ID, want.CanvasID, "accepted", invitationCorrelation}) {
		t.Fatalf("record = %+v", record)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 2, accepted: 1, records: 3, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	moderations, err := f.svc.Moderations(ctx, guest)
	if err != nil || len(moderations) != 1 || moderations[0] != (invitation.Moderation{TenantID: tenant, CanvasID: want.CanvasID, Role: access.RoleModerator}) {
		t.Fatalf("Moderations = %+v, %v", moderations, err)
	}
	if moderations, err := f.svc.Moderations(ctx, owner); err != nil || len(moderations) != 0 {
		t.Fatalf("owner Moderations = %+v, %v; want none", moderations, err)
	}
}

// S3: of 16 accounts accepting one link at once, one gets the membership and
// every other gets the one safe rejection.
func testConcurrentAcceptancesOfOneLinkGrantOneMembership(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)
	const n = 16
	guests := make([]access.AccountID, n)
	for i := range n {
		guests[i] = seedAccount(t, f.subs, uniqueEmail("guest"))
	}

	answers := make([]invitation.Acceptance, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			answers[i], errs[i] = acceptWith(f.svc, guests[i], token)
		})
	}
	close(start)
	wg.Wait()
	winner := -1
	for i := range n {
		switch {
		case errs[i] == nil && winner < 0 && answers[i].InvitationID == created.ID:
			winner = i
		case errors.Is(errs[i], invitation.ErrUnavailable) && answers[i] == (invitation.Acceptance{}):
		default:
			t.Fatalf("acceptance %d = %+v, %v", i, answers[i], errs[i])
		}
	}
	if winner < 0 {
		t.Fatal("no acceptance succeeded")
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM memberships WHERE tenant_id = $1 AND account_id = $2 AND role = 'moderator'", tenant, string(guests[winner])); n != 1 {
		t.Fatal("the membership is not the winner's")
	}
}

// S7/S12: retries of one operation racing each other give one effect and
// the same answer.
func testConcurrentAcceptancesUnderOneOperationGiveOneEffect(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	mustCreateInvitation(t, f, owner, token)

	const n = 16
	op := nextInvitationOp("op-concurrent-accept")
	answers := make([]invitation.Acceptance, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			answers[i], errs[i] = acceptOp(f.svc, guest, op, token, invitationCorrelation)
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || answers[i] != answers[0] || answers[0].TenantID != tenant {
			t.Fatalf("acceptance %d = %+v, %v; want %+v", i, answers[i], errs[i], answers[0])
		}
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

func testInvitationAcceptanceOperationConflicts(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	other := seedAccount(t, f.subs, uniqueEmail("other"))
	token, second := newToken(t), newToken(t)
	mustCreateInvitation(t, f, owner, token)
	secondInvitation := mustCreateInvitation(t, f, owner, second)
	op := nextInvitationOp("op-accept")
	if _, err := acceptOp(f.svc, guest, op, token, invitationCorrelation); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	before, secondRow := countInvitationRows(t, f.pool, tenant), rowText(t, f.pool, secondInvitation.ID)

	for name, call := range map[string]func() error{
		"another token":   func() error { _, err := acceptOp(f.svc, guest, op, second, invitationCorrelation); return err },
		"another account": func() error { _, err := acceptOp(f.svc, other, op, token, invitationCorrelation); return err },
		"revoke under the accept ID": func() error {
			_, err := revokeWith(f.svc, owner, op, secondInvitation.ID, invitationCorrelation)
			return err
		},
	} {
		if err := call(); !errors.Is(err, invitation.ErrOperationConflict) {
			t.Fatalf("%s: error = %v, want ErrOperationConflict", name, err)
		}
	}
	if got := countInvitationRows(t, f.pool, tenant); got != before {
		t.Fatalf("rows = %+v, want %+v", got, before)
	}
	if got := rowText(t, f.pool, secondInvitation.ID); got != secondRow {
		t.Fatalf("a conflicting acceptance changed the second invitation: %s", got)
	}
}

// S12: the operation ID replays until one microsecond before retention
// expiry; at expiry the retry is a new acceptance of a consumed link.
func testInvitationAcceptanceRetentionExpires(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	mustCreateInvitation(t, f, owner, token)
	clock := &accountClock{now: f.clock.Now()}
	svc, _ := f.service(clock.Now)
	op := nextInvitationOp("op-accept-retention")
	first, err := acceptOp(svc, guest, op, token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	clock.Advance(invitation.OperationRetention - time.Microsecond)
	if again, err := acceptOp(svc, guest, op, token, invitationCorrelation); err != nil || again != first {
		t.Fatalf("replay before expiry = %+v, %v; want %+v", again, err, first)
	}
	clock.Advance(time.Microsecond)
	if again, err := acceptOp(svc, guest, op, token, invitationCorrelation); !errors.Is(err, invitation.ErrUnavailable) || again != (invitation.Acceptance{}) {
		t.Fatalf("retry at expiry = %+v, %v; want ErrUnavailable", again, err)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

// S6: the commit succeeds but the answer is lost; the retry under the same
// operation ID returns the committed answer and one membership exists.
func testLostAcceptanceAnswerReplaysTheCommit(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)
	op := nextInvitationOp("op-accept-lost")

	// The acceptance commits; its answer never reaches the caller.
	if _, err := acceptOp(f.svc, guest, op, token, invitationCorrelation); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	want := invitation.Acceptance{TenantID: tenant, CanvasID: canvasOf(t, f.pool, tenant), InvitationID: created.ID}
	for range 2 {
		if got, err := acceptOp(f.svc, guest, op, token, "retry-correlation-0002"); err != nil || got != want {
			t.Fatalf("retry = %+v, %v; want %+v", got, err, want)
		}
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	if record := readAcceptRecord(t, f.pool, op); record.correlation != invitationCorrelation {
		t.Fatalf("record correlation = %q, want the first", record.correlation)
	}
}

// S4: expired, revoked, consumed, inactive-subscription, member, and owner
// acceptances write nothing and record no operation.
func testUnusableInvitationLinksWriteNothing(t *testing.T, f invitations) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, owner access.AccountID, tenant string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID)
	}{
		{"expired", func(t *testing.T, owner access.AccountID, _ string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			created := mustCreateInvitation(t, f, owner, token)
			svc, _ := f.service(func() time.Time { return created.ExpiresAt })
			return svc, guest
		}},
		{"revoked", func(t *testing.T, owner access.AccountID, _ string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			created := mustCreateInvitation(t, f, owner, token)
			if _, err := revokeWith(f.svc, owner, nextInvitationOp("op-revoke"), created.ID, invitationCorrelation); err != nil {
				t.Fatal(err)
			}
			return f.svc, guest
		}},
		{"accepted", func(t *testing.T, owner access.AccountID, _ string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			mustCreateInvitation(t, f, owner, token)
			if _, err := acceptWith(f.svc, seedAccount(t, f.subs, uniqueEmail("first")), token); err != nil {
				t.Fatal(err)
			}
			return f.svc, guest
		}},
		{"inactive subscription", func(t *testing.T, owner access.AccountID, tenant string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			mustCreateInvitation(t, f, owner, token)
			exec(t, f.pool, "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = $1", tenant)
			return f.svc, guest
		}},
		{"already a moderator", func(t *testing.T, owner access.AccountID, _ string, guest access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			earlier := newToken(t)
			mustCreateInvitation(t, f, owner, earlier)
			if _, err := acceptWith(f.svc, guest, earlier); err != nil {
				t.Fatal(err)
			}
			mustCreateInvitation(t, f, owner, token)
			return f.svc, guest
		}},
		{"the owner", func(t *testing.T, owner access.AccountID, _ string, _ access.AccountID, token secret.Value) (*invitation.Service, access.AccountID) {
			mustCreateInvitation(t, f, owner, token)
			return f.svc, owner
		}},
	} {
		owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
		guest := seedAccount(t, f.subs, uniqueEmail("guest"))
		token := newToken(t)
		svc, account := tc.setup(t, owner, tenant, guest, token)
		before := countInvitationRows(t, f.pool, tenant)
		role, member, err := f.subs.accounts.MembershipOf(ctx, account, tenant)
		if err != nil {
			t.Fatal(err)
		}

		op := nextInvitationOp("op-unusable")
		if got, err := acceptOp(svc, account, op, token, invitationCorrelation); !errors.Is(err, invitation.ErrUnavailable) || got != (invitation.Acceptance{}) {
			t.Fatalf("%s: Accept = %+v, %v; want ErrUnavailable", tc.name, got, err)
		}
		if got := countInvitationRows(t, f.pool, tenant); got != before {
			t.Fatalf("%s: rows = %+v, want %+v", tc.name, got, before)
		}
		if again, found, err := f.subs.accounts.MembershipOf(ctx, account, tenant); err != nil || found != member || again != role {
			t.Fatalf("%s: membership = %+v, %v, %v; want %+v, %v", tc.name, again, found, err, role, member)
		}
		if n := count(t, f.pool, "SELECT count(*) FROM invitation_operations WHERE operation_id = $1", op); n != 0 {
			t.Fatalf("%s: the refused acceptance recorded its operation", tc.name)
		}
	}
}

// US-004 advisory A1: while one acceptance of an account holds its commit,
// the same account's acceptance of another link of the tenant waits on the
// membership key and then gets the safe rejection, not a raw error.
func testOneAccountAcceptingTwoLinksOfOneTenantGetsOneMembership(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	first, second := newToken(t), newToken(t)
	mustCreateInvitation(t, f, owner, first)
	pending := mustCreateInvitation(t, f, owner, second)

	heldSvc, heldStore := f.service(f.clock.Now)
	reached, release := hold(t, heldStore, "commit")
	held, waiting := make(chan error, 1), make(chan error, 1)
	var answer invitation.Acceptance
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	wg.Go(func() {
		_, err := acceptWith(heldSvc, guest, first)
		held <- err
	})
	select {
	case <-reached:
	case err := <-held:
		t.Fatalf("acceptance ended before commit: %v", err)
	}
	wg.Go(func() {
		var err error
		answer, err = acceptWith(f.svc, guest, second)
		waiting <- err
	})
	waitForLockWait(t, f.pool)
	release()
	if err := <-held; err != nil {
		t.Fatalf("held acceptance: %v", err)
	}
	if err := <-waiting; err != invitation.ErrUnavailable || answer != (invitation.Acceptance{}) {
		t.Fatalf("waiting acceptance = %+v, %v; want exactly ErrUnavailable", answer, err)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 2, accepted: 1, records: 3, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM invitations WHERE invitation_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL", pending.ID); n != 1 {
		t.Fatal("the rejected acceptance consumed its link")
	}
}

// S9: simultaneous acceptances of two tenants' links bind each account to its
// own tenant and canvas, and each lists only its own moderated canvas.
func testInvitationAcceptancesOfTwoTenantsNeverCross(t *testing.T, f invitations) {
	ctx := context.Background()
	ownerA, tenantA := seedOwner(t, f, uniqueEmail("owner-a"))
	ownerB, tenantB := seedOwner(t, f, uniqueEmail("owner-b"))
	guests := []access.AccountID{seedAccount(t, f.subs, uniqueEmail("guest-a")), seedAccount(t, f.subs, uniqueEmail("guest-b"))}
	tenants := []string{tenantA, tenantB}
	tokens := []secret.Value{newToken(t), newToken(t)}
	created := make([]invitation.Invitation, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, owner := range []access.AccountID{ownerA, ownerB} {
		wg.Go(func() {
			created[i], errs[i] = createWith(f.svc, owner, nextInvitationOp("op-two-create"), tokens[i], invitationCorrelation)
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Create: %v, %v", errs[0], errs[1])
	}
	ops := []string{nextInvitationOp("op-two-accept"), nextInvitationOp("op-two-accept")}
	answers := make([]invitation.Acceptance, 2)
	start := make(chan struct{})
	for i := range guests {
		wg.Go(func() {
			<-start
			answers[i], errs[i] = acceptOp(f.svc, guests[i], ops[i], tokens[i], invitationCorrelation)
		})
	}
	close(start)
	wg.Wait()

	for i, tenant := range tenants {
		canvas := canvasOf(t, f.pool, tenant)
		want := invitation.Acceptance{TenantID: tenant, CanvasID: canvas, InvitationID: created[i].ID}
		if errs[i] != nil || answers[i] != want {
			t.Fatalf("Accept %d = %+v, %v; want %+v", i, answers[i], errs[i], want)
		}
		if record := readAcceptRecord(t, f.pool, ops[i]); record.tenant != tenant || record.canvas != canvas || record.invitation != created[i].ID {
			t.Fatalf("record %d = %+v", i, record)
		}
		for j, other := range tenants {
			m, found, err := f.subs.accounts.MembershipOf(ctx, guests[i], other)
			if err != nil || found != (i == j) || (found && m.Role != access.RoleModerator) {
				t.Fatalf("MembershipOf(guest %d, tenant %d) = %+v, %v, %v", i, j, m, found, err)
			}
		}
		moderations, err := f.svc.Moderations(ctx, guests[i])
		if err != nil || len(moderations) != 1 || moderations[0] != (invitation.Moderation{TenantID: tenant, CanvasID: canvas, Role: access.RoleModerator}) {
			t.Fatalf("Moderations(guest %d) = %+v, %v", i, moderations, err)
		}
	}

	// A moderator of both lists both by tenant ID; an inactive one drops out.
	both := seedAccount(t, f.subs, uniqueEmail("both"))
	for i := range tenants {
		token := newToken(t)
		mustCreateInvitation(t, f, []access.AccountID{ownerA, ownerB}[i], token)
		if _, err := acceptWith(f.svc, both, token); err != nil {
			t.Fatalf("Accept by both %d: %v", i, err)
		}
	}
	sorted := append([]string(nil), tenants...)
	if sorted[0] > sorted[1] {
		sorted[0], sorted[1] = sorted[1], sorted[0]
	}
	moderations, err := f.svc.Moderations(ctx, both)
	if err != nil || len(moderations) != 2 || moderations[0].TenantID != sorted[0] || moderations[1].TenantID != sorted[1] {
		t.Fatalf("Moderations(both) = %+v, %v; want %v in order", moderations, err, sorted)
	}
	exec(t, f.pool, "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = $1", tenantA)
	moderations, err = f.svc.Moderations(ctx, both)
	if err != nil || len(moderations) != 1 || moderations[0].TenantID != tenantB {
		t.Fatalf("Moderations(both) after deactivating A = %+v, %v; want only B", moderations, err)
	}
}

func testInvitationStoreReportsTakenMemberships(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	before := countInvitationRows(t, f.pool, tenant)
	err := f.store.InTx(ctx, func(tx invitation.Tx) error { return tx.AddModerator(ctx, tenant, owner) })
	if !errors.Is(err, invitation.ErrMemberTaken) {
		t.Fatalf("AddModerator of the owner: error = %v, want ErrMemberTaken", err)
	}
	if got := countInvitationRows(t, f.pool, tenant); got != before {
		t.Fatalf("rows = %+v, want %+v", got, before)
	}
}

// S12/S16: an acceptance replays across a server restart with its first
// correlation; the records hold only the token hash.
func TestInvitationAcceptanceReplaysAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	f := openInvitations(t, server, clock)
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	created := mustCreateInvitation(t, f, owner, token)
	op := nextInvitationOp("op-restart-accept")
	first, err := acceptOp(f.svc, guest, op, token, invitationCorrelation)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	before := countInvitationRows(t, f.pool, tenant)

	server.Restart(t)
	f = openInvitations(t, server, clock)
	clock.Advance(invitation.OperationRetention - time.Microsecond)

	if again, err := acceptOp(f.svc, guest, op, token, "retry-correlation-0002"); err != nil || again != first {
		t.Fatalf("retry after restart = %+v, %v; want %+v", again, err, first)
	}
	if _, err := acceptOp(f.svc, guest, op, newToken(t), invitationCorrelation); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("another token under the operation: error = %v, want ErrOperationConflict", err)
	}
	if got := countInvitationRows(t, f.pool, tenant); got != before {
		t.Fatalf("rows = %+v, want %+v", got, before)
	}
	if record := readAcceptRecord(t, f.pool, op); record.correlation != invitationCorrelation || record.invitation != created.ID {
		t.Fatalf("record = %+v, want the first correlation", record)
	}
	if m, found, err := f.subs.accounts.MembershipOf(ctx, guest, tenant); err != nil || !found || m.Role != access.RoleModerator {
		t.Fatalf("membership after restart = %+v, %v, %v", m, found, err)
	}
	if moderations, err := f.svc.Moderations(ctx, guest); err != nil || len(moderations) != 1 || moderations[0].TenantID != tenant {
		t.Fatalf("Moderations after restart = %+v, %v", moderations, err)
	}

	body := strings.TrimPrefix(token.Reveal(), "pci_")
	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT concat_ws(',',
		(SELECT string_agg(row_to_json(i)::text, ',') FROM invitations i),
		(SELECT string_agg(row_to_json(o)::text, ',') FROM invitation_operations o),
		(SELECT string_agg(row_to_json(m)::text, ',') FROM memberships m))`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, body) {
		t.Fatal("a stored row contains the token")
	}
}

// S6/S7: a retry that waits on the invitation lock while the first attempt
// commits reads the operation after the wait and replays the committed answer.
func testWaitingRetryReplaysTheHeldAcceptance(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	guest := seedAccount(t, f.subs, uniqueEmail("guest"))
	token := newToken(t)
	mustCreateInvitation(t, f, owner, token)
	op := nextInvitationOp("op-accept-held")

	heldSvc, heldStore := f.service(f.clock.Now)
	reached, release := hold(t, heldStore, "commit")
	held, waiting := make(chan error, 1), make(chan error, 1)
	var first, retry invitation.Acceptance
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	wg.Go(func() {
		var err error
		first, err = acceptOp(heldSvc, guest, op, token, invitationCorrelation)
		held <- err
	})
	select {
	case <-reached:
	case err := <-held:
		t.Fatalf("acceptance ended before commit: %v", err)
	}
	wg.Go(func() {
		var err error
		retry, err = acceptOp(f.svc, guest, op, token, "retry-correlation-0002")
		waiting <- err
	})
	waitForLockWait(t, f.pool)
	release()
	if err := <-held; err != nil {
		t.Fatalf("held acceptance: %v", err)
	}
	if err := <-waiting; err != nil || retry != first || first.TenantID != tenant {
		t.Fatalf("waiting retry = %+v, %v; want %+v", retry, err, first)
	}
	if rows := countInvitationRows(t, f.pool, tenant); rows != (invitationRows{invitations: 1, accepted: 1, records: 2, moderators: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}
