package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// TestMediaCommandsAndMemberRevocation runs the US-006 media race tests on
// one server. Each test seeds its own tenants.
func TestMediaCommandsAndMemberRevocation(t *testing.T) {
	f := openInvitations(t, pgtest.Start(t), newAccountClock())
	for _, tc := range []struct {
		name string
		run  func(*testing.T, invitations)
	}{
		{"CommandInFlightBeforeTheRevocationCommits", testMediaCommandInFlightBeforeTheRevocationCommits},
		{"CommandWaitingBehindTheRevocationIsRejected", testMediaCommandWaitingBehindTheRevocationIsRejected},
		{"RevokedActorWritesNothingAndReplaysNothing", testRevokedActorWritesNothingAndReplaysNothing},
		{"RevocationInOneTenantKeepsTheOther", testMediaRevocationInOneTenantKeepsTheOther},
		{"InactiveTenantTakesPrecedenceOverAMissingActor", testInactiveTenantTakesPrecedenceOverAMissingActor},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, f) })
	}
}

// moderatedCanvas seeds an owner with one ready asset under "reference-kept"
// and a moderator of its tenant, and returns them with the media key.
func moderatedCanvas(t *testing.T, f invitations) (owner, moderator access.AccountID, store *postgres.MediaStore, key media.CanvasKey, assetID string) {
	t.Helper()
	owner, tenant := seedOwner(t, f, uniqueEmail("media-owner"))
	moderator = seedModerator(t, f, owner, uniqueEmail("media-moderator"))
	store = postgres.NewMediaStore(f.pool)
	key = media.CanvasKey{TenantID: tenant, CanvasID: canvasOf(t, f.pool, tenant)}
	assetID = upload(t, store, key, 4000, "reference-kept")
	return owner, moderator, store, key, assetID
}

// actedBy is a new operation of account.
func actedBy(account access.AccountID) media.Operation {
	operation := nextMediaOperation()
	operation.AccountID = string(account)
	return operation
}

// mediaWrites renders the canvas, its media, its retained operations, and
// its media usage outbox records.
func mediaWrites(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) string {
	t.Helper()
	var outbox string
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(json_agg(o ORDER BY id)::text, '')
		FROM canvas_outbox o WHERE tenant_id = $1 AND canvas_id = $2 AND kind = 'media_usage'`, key.TenantID, key.CanvasID).Scan(&outbox)
	if err != nil {
		t.Fatalf("read media outbox: %v", err)
	}
	return mediaRows(t, pool, key) + "\n" + outbox
}

// S2: a moderator's command holding the tenant row commits before the
// revocation that waits for it; the reference persists and the revocation
// then succeeds.
func testMediaCommandInFlightBeforeTheRevocationCommits(t *testing.T, f invitations) {
	owner, moderator, _, key, assetID := moderatedCanvas(t, f)

	held := postgres.NewMediaStore(f.pool)
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	postgres.SetMediaBeforeCommit(held, func(context.Context) error {
		close(reached)
		<-release
		return nil
	})
	type applied struct {
		result media.Result
		err    error
	}
	attach := make(chan applied, 1)
	go func() {
		result, err := held.Apply(context.Background(), key, actedBy(moderator), media.Attach{AssetID: assetID, ReferenceID: "reference-moderator"})
		attach <- applied{result, err}
	}()
	revocation, finished := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case a := <-attach:
		close(finished)
		t.Fatalf("Attach ended before commit: %+v", a)
	}
	go func() {
		defer close(finished)
		_, err := revokeMemberWith(f.svc, owner, nextInvitationOp("op-revoke-media-held"), moderator)
		revocation <- err
	}()

	// The revocation waits for the tenant row the Attach holds.
	waitForLockWaiters(t, f.pool, 1)
	if rows := countRevocationRows(t, f.pool, key.TenantID); rows != (revocationRows{moderators: 1, version: 1}) {
		t.Fatalf("rows while the Attach is in flight = %+v", rows)
	}
	releaseHeld()
	if a := <-attach; a.err != nil || a.result.AssetID != assetID {
		t.Fatalf("in-flight Attach = %+v, %v", a.result, a.err)
	}
	if err := <-revocation; err != nil {
		t.Fatalf("RevokeMember after the Attach: %v", err)
	}
	if rows := countRevocationRows(t, f.pool, key.TenantID); rows != (revocationRows{version: 2, cuts: 1, pending: 1, records: 1}) {
		t.Fatalf("rows after the revocation = %+v", rows)
	}
	if n := count(t, f.pool, "SELECT count(*) FROM media_references WHERE tenant_id = $1 AND reference_id = 'reference-moderator'", key.TenantID); n != 1 {
		t.Fatalf("moderator references = %d, want 1", n)
	}
}

// S2, S4, S9: a moderator's command that reaches the tenant row while a
// revocation holds it waits, and is rejected without a write once the
// revocation commits, although its route check already passed.
func testMediaCommandWaitingBehindTheRevocationIsRejected(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, moderator, store, key, assetID := moderatedCanvas(t, f)
	svc, invitationStore := f.service(f.clock.Now)
	reached, release := hold(t, invitationStore, "commit")
	revocation, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, err := revokeMemberWith(svc, owner, nextInvitationOp("op-revoke-media-first"), moderator)
		revocation <- err
	}()
	t.Cleanup(func() {
		release()
		<-finished
	})
	select {
	case <-reached:
	case err := <-revocation:
		t.Fatalf("revocation ended before commit: %v", err)
	}

	// The route check reads the membership the uncommitted revocation still
	// keeps.
	if _, found, err := f.subs.accounts.MembershipOf(ctx, moderator, key.TenantID); err != nil || !found {
		t.Fatalf("route check membership = %v, %v; want found", found, err)
	}
	rows := mediaWrites(t, f.pool, key)
	attach := make(chan error, 1)
	go func() {
		_, err := store.Apply(context.Background(), key, actedBy(moderator), media.Attach{AssetID: assetID, ReferenceID: "reference-late"})
		attach <- err
	}()
	waitForLockWaiters(t, f.pool, 1)
	release()
	if err := <-revocation; err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	if err := <-attach; !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("Attach after the revocation: error = %v, want ErrAccessDenied", err)
	}
	if got := mediaWrites(t, f.pool, key); got != rows {
		t.Fatalf("rows after the rejected Attach:\n%s\nwant\n%s", got, rows)
	}
}

// S2, S4, S5: after the revocation commits, every command of the revoked
// moderator is rejected and writes nothing, including an equal retry of a
// command committed before it; the owner's commands, system work, and the
// release of a pending upload keep working, and the content is kept.
func testRevokedActorWritesNothingAndReplaysNothing(t *testing.T, f invitations) {
	ctx := context.Background()
	owner, moderator, store, key, assetID := moderatedCanvas(t, f)
	committedOp := actedBy(moderator)
	committed, err := store.Apply(ctx, key, committedOp, media.Attach{AssetID: assetID, ReferenceID: "reference-before"})
	if err != nil {
		t.Fatalf("Attach before the revocation: %v", err)
	}
	pending, err := store.Apply(ctx, key, actedBy(moderator), media.Reserve{DeclaredBytes: 300})
	if err != nil {
		t.Fatalf("Reserve before the revocation: %v", err)
	}
	if _, err := revokeMemberWith(f.svc, owner, nextInvitationOp("op-revoke-media-done"), moderator); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	content := contentText(t, f.pool, key.TenantID)
	rows := mediaWrites(t, f.pool, key)

	for _, c := range []struct {
		name      string
		operation media.Operation
		command   media.Command
	}{
		{"reserve", actedBy(moderator), media.Reserve{DeclaredBytes: 1}},
		{"complete", actedBy(moderator), media.Complete{AssetID: pending.AssetID, ReferenceID: "reference-new", Kind: media.KindImage}},
		{"attach", actedBy(moderator), media.Attach{AssetID: assetID, ReferenceID: "reference-after"}},
		{"detach", actedBy(moderator), media.Detach{ReferenceID: "reference-kept"}},
		{"equal retry", committedOp, media.Attach{AssetID: assetID, ReferenceID: "reference-before"}},
		{"reused ID", committedOp, media.Attach{AssetID: assetID, ReferenceID: "reference-other"}},
	} {
		if result, err := store.Apply(ctx, key, c.operation, c.command); !errors.Is(err, access.ErrAccessDenied) || result != (media.Result{}) {
			t.Fatalf("%s: Apply = %+v, %v; want ErrAccessDenied", c.name, result, err)
		}
	}
	if got := mediaWrites(t, f.pool, key); got != rows {
		t.Fatalf("rows after rejected commands:\n%s\nwant\n%s", got, rows)
	}
	if got := contentText(t, f.pool, key.TenantID); got != content {
		t.Fatalf("content = %s, want %s", got, content)
	}

	// A failed upload always releases its reservation, whoever started it.
	abandonOp := actedBy(moderator)
	if _, err := store.Apply(ctx, key, abandonOp, media.Abandon{AssetID: pending.AssetID, Reason: media.AbandonFailed}); err != nil {
		t.Fatalf("Abandon of the revoked moderator's upload: %v", err)
	}
	wantUsage(t, store, f.pool, key, 4000, 0)
	// System work has no actor.
	run(t, store, key, media.Reserve{DeclaredBytes: 5})
	// The owner is unaffected.
	if got, err := store.Apply(ctx, key, actedBy(owner), media.Detach{ReferenceID: "reference-before"}); err != nil || got.AssetID != committed.AssetID {
		t.Fatalf("owner Detach = %+v, %v", got, err)
	}
	wantUsage(t, store, f.pool, key, 4000, 5)
}

// S7: revoking a moderator of tenant A leaves its moderator membership in
// tenant B usable, even while A's revocation holds A's tenant row.
func testMediaRevocationInOneTenantKeepsTheOther(t *testing.T, f invitations) {
	ctx := context.Background()
	ownerA, moderator, storeA, keyA, assetA := moderatedCanvas(t, f)
	ownerB, tenantB := seedOwner(t, f, uniqueEmail("media-owner-b"))
	token := newToken(t)
	mustCreateInvitation(t, f, ownerB, token)
	if _, err := acceptWith(f.svc, moderator, token); err != nil {
		t.Fatalf("Accept in B: %v", err)
	}
	storeB := postgres.NewMediaStore(f.pool)
	keyB := media.CanvasKey{TenantID: tenantB, CanvasID: canvasOf(t, f.pool, tenantB)}
	assetB := upload(t, storeB, keyB, 2000, "reference-b")

	svc, invitationStore := f.service(f.clock.Now)
	reached, release := hold(t, invitationStore, "commit")
	revocation, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, err := revokeMemberWith(svc, ownerA, nextInvitationOp("op-revoke-media-a"), moderator)
		revocation <- err
	}()
	t.Cleanup(func() {
		release()
		<-finished
	})
	<-reached

	// B's tenant row is not held: the command commits at once.
	done := make(chan error, 1)
	go func() {
		_, err := storeB.Apply(ctx, keyB, actedBy(moderator), media.Attach{AssetID: assetB, ReferenceID: "reference-b-during"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach in B while A's revocation is held: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Attach in B waited for A's revocation")
	}
	release()
	if err := <-revocation; err != nil {
		t.Fatalf("RevokeMember in A: %v", err)
	}

	if _, err := storeA.Apply(ctx, keyA, actedBy(moderator), media.Attach{AssetID: assetA, ReferenceID: "reference-a-after"}); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("Attach in A after the revocation = %v, want ErrAccessDenied", err)
	}
	if _, err := storeB.Apply(ctx, keyB, actedBy(moderator), media.Detach{ReferenceID: "reference-b-during"}); err != nil {
		t.Fatalf("Detach in B after A's revocation: %v", err)
	}
	if rows := countRevocationRows(t, f.pool, tenantB); rows != (revocationRows{moderators: 1, version: 1}) {
		t.Fatalf("B rows = %+v", rows)
	}
}

// A tenant whose subscription is inactive answers media.ErrInactive whether
// or not the actor is still a member: the dead tenant decides first, and
// neither case writes or replays.
func testInactiveTenantTakesPrecedenceOverAMissingActor(t *testing.T, f invitations) {
	owner, moderator, store, key, assetID := moderatedCanvas(t, f)
	retained := actedBy(moderator)
	if _, err := store.Apply(context.Background(), key, retained, media.Attach{AssetID: assetID, ReferenceID: "reference-before"}); err != nil {
		t.Fatalf("Attach before the revocation: %v", err)
	}
	if _, err := revokeMemberWith(f.svc, owner, nextInvitationOp("op-revoke-media-inactive"), moderator); err != nil {
		t.Fatalf("RevokeMember: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = $1", key.TenantID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	before := mediaWrites(t, f.pool, key)

	for name, apply := range map[string]struct {
		operation media.Operation
		command   media.Command
	}{
		"revoked actor":             {actedBy(moderator), media.Attach{AssetID: assetID, ReferenceID: "reference-after"}},
		"revoked actor equal retry": {retained, media.Attach{AssetID: assetID, ReferenceID: "reference-before"}},
		"owner still a member":      {actedBy(owner), media.Attach{AssetID: assetID, ReferenceID: "reference-owner"}},
	} {
		if _, err := store.Apply(context.Background(), key, apply.operation, apply.command); !errors.Is(err, media.ErrInactive) {
			t.Fatalf("%s on an inactive tenant: error = %v, want media.ErrInactive", name, err)
		}
	}
	if after := mediaWrites(t, f.pool, key); after != before {
		t.Fatalf("writes changed:\n%s\nwant\n%s", after, before)
	}
}
