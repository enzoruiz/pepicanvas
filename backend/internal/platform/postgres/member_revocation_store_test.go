package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
)

const revocationCorrelation = "revocation-correlation-0001"

func revokeMemberWith(svc *invitation.Service, owner access.AccountID, op string, account access.AccountID) (invitation.MemberRevocation, error) {
	return svc.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
		OwnerID: owner, OperationID: op, AccountID: account, CorrelationID: revocationCorrelation,
	})
}

// seedModerator registers an account and makes it a moderator of the tenant
// of owner by accepting a new invitation.
func seedModerator(t *testing.T, f invitations, owner access.AccountID, email string) access.AccountID {
	t.Helper()
	account := seedAccount(t, f.subs, email)
	token := newToken(t)
	mustCreateInvitation(t, f, owner, token)
	if _, err := acceptWith(f.svc, account, token); err != nil {
		t.Fatalf("Accept(%s): %v", email, err)
	}
	return account
}

// revocationRows reads what a membership revocation of a tenant writes.
type revocationRows struct {
	moderators, version, cuts, pending, records int
}

func countRevocationRows(t *testing.T, pool *pgxpool.Pool, tenantID string) revocationRows {
	t.Helper()
	return revocationRows{
		moderators: count(t, pool, "SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'moderator'", tenantID),
		version:    count(t, pool, "SELECT access_version FROM tenants WHERE tenant_id = $1", tenantID),
		cuts:       count(t, pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1", tenantID),
		pending:    count(t, pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND applied_at IS NULL", tenantID),
		records:    count(t, pool, "SELECT count(*) FROM membership_revocations WHERE tenant_id = $1", tenantID),
	}
}

// contentText renders the tenant's canvas, its objects, and its media usage.
func contentText(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var text string
	err := pool.QueryRow(context.Background(), `SELECT concat_ws('|',
		(SELECT string_agg(row_to_json(c)::text, ',') FROM canvases c WHERE tenant_id = $1),
		(SELECT string_agg(row_to_json(o)::text, ',' ORDER BY object_id) FROM canvas_objects o WHERE tenant_id = $1),
		(SELECT string_agg(row_to_json(u)::text, ',') FROM media_usage u WHERE tenant_id = $1))`, tenantID).Scan(&text)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	return text
}

// S1, S5, S11, S14: one commit deletes the membership, increments the access
// version, writes one pending revocation cut and the record, both with the
// correlation, and keeps the content.
func testMemberRevocationCommitsEverythingAtOnce(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	email := uniqueEmail("moderator")
	moderator := seedModerator(t, f, owner, email)
	acceptedAt := f.clock.Now()
	canvasID := canvasOf(t, f.pool, tenant)
	exec(t, f.pool, `INSERT INTO canvas_objects (tenant_id, canvas_id, object_id, kind, content, x, y, width, height, rotation, z)
		VALUES ($1, $2, 'object-1', 'text', 'kept', 1, 2, 3, 4, 0, 1)`, tenant, canvasID)
	content := contentText(t, f.pool, tenant)

	members, err := f.svc.Members(ctx, owner)
	if want := []invitation.Member{{AccountID: moderator, Email: email, AcceptedAt: acceptedAt}}; err != nil || len(members) != 1 || members[0] != want[0] {
		t.Fatalf("Members = %+v, %v; want %+v", members, err, want)
	}

	op := nextInvitationOp("op-revoke-member")
	got, err := revokeMemberWith(f.svc, owner, op, moderator)
	if want := (invitation.MemberRevocation{TenantID: tenant, AccountID: moderator}); err != nil || got != want {
		t.Fatalf("RevokeMember = %+v, %v; want %+v", got, err, want)
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	if n := count(t, f.pool, `SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND kind = 'revocation' AND account_id = $2
		AND version = 2 AND correlation_id = $3 AND created_at = $4`, tenant, string(moderator), revocationCorrelation, f.clock.Now()); n != 1 {
		t.Fatal("the cut does not carry the kind, account, version, correlation, and time")
	}
	if n := count(t, f.pool, `SELECT count(*) FROM membership_revocations WHERE operation_id = $1 AND owner_account_id = $2 AND tenant_id = $3
		AND account_id = $4 AND version = 2 AND correlation_id = $5`, op, string(owner), tenant, string(moderator), revocationCorrelation); n != 1 {
		t.Fatal("the record does not carry the answer, version, and correlation")
	}
	if _, found, err := f.subs.accounts.MembershipOf(ctx, moderator, tenant); err != nil || found {
		t.Fatalf("membership found = %v, %v; want none", found, err)
	}
	if role, found, err := f.subs.accounts.MembershipOf(ctx, owner, tenant); err != nil || !found || role.Role != access.RoleOwner {
		t.Fatalf("owner membership = %+v, %v, %v", role, found, err)
	}
	if got := contentText(t, f.pool, tenant); got != content {
		t.Fatalf("content = %s, want %s", got, content)
	}
	if members, err := f.svc.Members(ctx, owner); err != nil || len(members) != 0 {
		t.Fatalf("Members after revocation = %+v, %v; want none", members, err)
	}
	// The checks tie the account to the kind: a revocation cut must name
	// an account, and a cut of the default kind, a deactivation, must not.
	if err := pgxExecErr(f.pool, "INSERT INTO access_cuts (tenant_id, kind, version, created_at) VALUES ($1, 'revocation', 9, now())", tenant); err == nil {
		t.Fatal("a revocation cut without an account was accepted")
	}
	if err := pgxExecErr(f.pool, "INSERT INTO access_cuts (tenant_id, account_id, version, created_at) VALUES ($1, $2, 9, now())", tenant, string(moderator)); err == nil {
		t.Fatal("a deactivation cut with an account was accepted")
	}
}

func pgxExecErr(pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}

// S11: a failure at any step before the commit leaves the membership, the
// version, the cut, and the record unchanged; the retry revokes once.
func testFailedMemberRevocationLeavesNothing(t *testing.T, f invitations) {
	injected := errors.New("injected fault")
	t.Cleanup(func() { postgres.SetInvitationFault(f.store, nil) })
	steps := []string{"LockOwnedTenantForUpdate", "MemberRevocation", "LockMembership", "RemoveModerator", "IncrementAccessVersion", "RecordCut", "RecordMemberRevocation", "commit"}
	for _, step := range steps {
		owner, tenant := seedOwner(t, f, uniqueEmail("revoke-member-"+strings.ToLower(step)))
		moderator := seedModerator(t, f, owner, uniqueEmail("moderator-"+strings.ToLower(step)))
		op := nextInvitationOp("op-fault-revoke-member")
		postgres.SetInvitationFault(f.store, func(_ context.Context, at string) error {
			if at == step {
				return injected
			}
			return nil
		})
		if _, err := revokeMemberWith(f.svc, owner, op, moderator); !errors.Is(err, injected) {
			t.Fatalf("%s: error = %v, want the injected fault", step, err)
		}
		if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{moderators: 1, version: 1}) {
			t.Fatalf("%s: failed revocation left %+v", step, rows)
		}
		postgres.SetInvitationFault(f.store, nil)
		want := invitation.MemberRevocation{TenantID: tenant, AccountID: moderator}
		for range 2 {
			if got, err := revokeMemberWith(f.svc, owner, op, moderator); err != nil || got != want {
				t.Fatalf("%s: retry = %+v, %v; want %+v", step, got, err, want)
			}
		}
		if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
			t.Fatalf("%s: rows after retry = %+v", step, rows)
		}
	}
}

// S12: a retry replays while retained, the same operation ID for another
// account is a conflict, and at exactly the retention the ID starts anew.
func testMemberRevocationReplaysConflictsAndExpires(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	moderator := seedModerator(t, f, owner, uniqueEmail("moderator"))
	other := seedModerator(t, f, owner, uniqueEmail("other"))
	clock := &accountClock{now: f.clock.Now()}
	svc, _ := f.service(clock.Now)
	op := nextInvitationOp("op-revoke-member-retention")
	first, err := revokeMemberWith(svc, owner, op, moderator)
	if err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	after := countRevocationRows(t, f.pool, tenant)

	clock.Advance(invitation.OperationRetention - time.Microsecond)
	if again, err := revokeMemberWith(svc, owner, op, moderator); err != nil || again != first {
		t.Fatalf("replay before expiry = %+v, %v; want %+v", again, err, first)
	}
	if _, err := revokeMemberWith(svc, owner, op, other); !errors.Is(err, invitation.ErrOperationConflict) {
		t.Fatalf("same operation for another account: error = %v, want ErrOperationConflict", err)
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != after || after != (revocationRows{moderators: 1, version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows = %+v, want %+v", rows, after)
	}
	clock.Advance(time.Microsecond)
	if again, err := revokeMemberWith(svc, owner, op, moderator); !errors.Is(err, invitation.ErrNotFound) || again != (invitation.MemberRevocation{}) {
		t.Fatalf("retry at expiry = %+v, %v; want ErrNotFound", again, err)
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != after {
		t.Fatalf("rows after the retry at expiry = %+v, want %+v", rows, after)
	}
	// At expiry the operation ID revokes another account anew.
	if got, err := revokeMemberWith(svc, owner, op, other); err != nil || got.AccountID != other {
		t.Fatalf("expired operation ID for another account = %+v, %v", got, err)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM membership_revocations WHERE operation_id = $1 AND account_id = $2", op, string(other)); n != 1 {
		t.Fatal("the expired record was not replaced")
	}
}

// S6, S9: unknown accounts, non-members, and the owner are not found;
// moderators and strangers are denied; none of them writes.
func testMemberRevocationRejectionsWriteNothing(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	otherOwner, _ := seedOwner(t, f, uniqueEmail("other-owner"))
	moderator := seedModerator(t, f, owner, uniqueEmail("moderator"))
	peer := seedModerator(t, f, owner, uniqueEmail("peer"))
	stranger := seedAccount(t, f.subs, uniqueEmail("stranger"))
	before := countRevocationRows(t, f.pool, tenant)

	for _, account := range []access.AccountID{access.AccountID(unknownInvitationID), stranger, owner, otherOwner} {
		if _, err := revokeMemberWith(f.svc, owner, nextInvitationOp("op-revoke-member-missing"), account); !errors.Is(err, invitation.ErrNotFound) {
			t.Fatalf("revoke %s: error = %v, want ErrNotFound", account, err)
		}
	}
	for _, actor := range []access.AccountID{moderator, stranger} {
		if _, err := revokeMemberWith(f.svc, actor, nextInvitationOp("op-revoke-member-denied"), peer); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s revokes: error = %v, want ErrAccessDenied", actor, err)
		}
		if _, err := f.svc.Members(context.Background(), actor); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("%s lists: error = %v, want ErrAccessDenied", actor, err)
		}
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != before {
		t.Fatalf("rows = %+v, want %+v", rows, before)
	}
}

// S7: revoking an account in tenant A keeps its moderation of tenant B.
func testMemberRevocationOfTwoTenantsNeverCross(t *testing.T, f invitations) {
	ctx := context.Background()
	ownerA, tenantA := seedOwner(t, f, uniqueEmail("owner-a"))
	ownerB, tenantB := seedOwner(t, f, uniqueEmail("owner-b"))
	moderator := seedModerator(t, f, ownerA, uniqueEmail("moderator"))
	token := newToken(t)
	mustCreateInvitation(t, f, ownerB, token)
	if _, err := acceptWith(f.svc, moderator, token); err != nil {
		t.Fatalf("Accept in B: %v", err)
	}
	beforeB := countRevocationRows(t, f.pool, tenantB)

	if _, err := revokeMemberWith(f.svc, ownerA, nextInvitationOp("op-revoke-member-a"), moderator); err != nil {
		t.Fatalf("RevokeMember in A: %v", err)
	}
	if rows := countRevocationRows(t, f.pool, tenantB); rows != beforeB {
		t.Fatalf("B rows = %+v, want %+v", rows, beforeB)
	}
	if membership, found, err := f.subs.accounts.MembershipOf(ctx, moderator, tenantB); err != nil || !found || membership.Role != access.RoleModerator {
		t.Fatalf("membership in B = %+v, %v, %v; want moderator", membership, found, err)
	}
	moderations, err := f.svc.Moderations(ctx, moderator)
	if err != nil || len(moderations) != 1 || moderations[0].TenantID != tenantB {
		t.Fatalf("Moderations = %+v, %v; want only B", moderations, err)
	}
	// Revoking it in B afterwards leaves A's rows as A's revocation wrote them.
	if _, err := revokeMemberWith(f.svc, ownerB, nextInvitationOp("op-revoke-member-b"), moderator); err != nil {
		t.Fatalf("RevokeMember in B: %v", err)
	}
	if rows := countRevocationRows(t, f.pool, tenantA); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("A rows after B's revocation = %+v", rows)
	}
}

// Concurrent revocations of one moderator give one effect: under different
// operation IDs one succeeds and the rest find no member; under one
// operation ID all get the same answer.
func testConcurrentMemberRevocationsGiveOneEffect(t *testing.T, f invitations) {
	const n = 16
	run := func(op func(int) string) (owner access.AccountID, tenant string, answers []invitation.MemberRevocation, errs []error) {
		owner, tenant = seedOwner(t, f, uniqueEmail("owner"))
		moderator := seedModerator(t, f, owner, uniqueEmail("moderator"))
		answers, errs = make([]invitation.MemberRevocation, n), make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				<-start
				answers[i], errs[i] = revokeMemberWith(f.svc, owner, op(i), moderator)
			})
		}
		close(start)
		wg.Wait()
		return owner, tenant, answers, errs
	}

	_, tenant, answers, errs := run(func(int) string { return nextInvitationOp("op-concurrent-revoke-member") })
	succeeded := 0
	for i := range n {
		switch {
		case errs[i] == nil:
			succeeded++
		case !errors.Is(errs[i], invitation.ErrNotFound):
			t.Fatalf("revocation %d = %+v, %v; want success or ErrNotFound", i, answers[i], errs[i])
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d revocations succeeded, want 1", succeeded)
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows = %+v", rows)
	}

	op := nextInvitationOp("op-concurrent-revoke-member-same")
	_, tenant, answers, errs = run(func(int) string { return op })
	for i := range n {
		if errs[i] != nil || answers[i] != answers[0] || answers[0].TenantID != tenant {
			t.Fatalf("revocation %d = %+v, %v; want %+v", i, answers[i], errs[i], answers[0])
		}
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

// S2: the revocation takes the tenant row FOR UPDATE, so it waits for a
// mutation holding the tenant FOR SHARE and commits after it.
func testMemberRevocationWaitsForASharedTenantLock(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	moderator := seedModerator(t, f, owner, uniqueEmail("moderator"))
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, "SELECT 1 FROM tenants WHERE tenant_id = $1 FOR SHARE", tenant); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := revokeMemberWith(f.svc, owner, nextInvitationOp("op-revoke-member-held"), moderator)
		done <- err
	}()
	waitForLockWait(t, f.pool)
	select {
	case err := <-done:
		t.Fatalf("the revocation committed under a shared tenant lock: %v", err)
	default:
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RevokeMember after the holder committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revocation did not finish after the holder committed")
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
}

// A correlation longer than the stored 128 octets is ErrInvalid before any
// write; exactly 128 octets is stored on the record and the cut.
func testMemberRevocationRejectsAnOverlongCorrelation(t *testing.T, f invitations) {
	owner, tenant := seedOwner(t, f, uniqueEmail("owner"))
	moderator := seedModerator(t, f, owner, uniqueEmail("moderator"))
	before := countRevocationRows(t, f.pool, tenant)

	_, err := f.svc.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
		OwnerID: owner, OperationID: nextInvitationOp("op-revoke-long-corr"), AccountID: moderator,
		CorrelationID: strings.Repeat("c", 129),
	})
	if !errors.Is(err, invitation.ErrInvalid) {
		t.Fatalf("129-octet correlation: error = %v, want ErrInvalid", err)
	}
	if rows := countRevocationRows(t, f.pool, tenant); rows != before {
		t.Fatalf("rows = %+v, want %+v", rows, before)
	}

	longest := strings.Repeat("c", 128)
	if _, err := f.svc.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
		OwnerID: owner, OperationID: nextInvitationOp("op-revoke-max-corr"), AccountID: moderator,
		CorrelationID: longest,
	}); err != nil {
		t.Fatalf("128-octet correlation: %v", err)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND correlation_id = $2", tenant, longest); n != 1 {
		t.Fatalf("cuts with the correlation = %d, want 1", n)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM membership_revocations WHERE tenant_id = $1 AND correlation_id = $2", tenant, longest); n != 1 {
		t.Fatalf("records with the correlation = %d, want 1", n)
	}
}
