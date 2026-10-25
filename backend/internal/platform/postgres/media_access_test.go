package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// mediaStreamer activates the account of email and returns a media store and
// the media key of its canvas.
func mediaStreamer(t *testing.T, s subscriptions, email string) (access.AccountID, *postgres.MediaStore, media.CanvasKey) {
	t.Helper()
	operator, _, key := streamer(t, s, email)
	return operator, postgres.NewMediaStore(s.pool), media.CanvasKey{TenantID: key.TenantID, CanvasID: key.CanvasID}
}

// mediaRows renders the canvas, its media, and its retained media operations.
func mediaRows(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) string {
	t.Helper()
	var operations string
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(json_agg(o ORDER BY operation_id)::text, '')
		FROM media_operations o WHERE tenant_id = $1 AND canvas_id = $2`, key.TenantID, key.CanvasID).Scan(&operations)
	if err != nil {
		t.Fatalf("read media operations: %v", err)
	}
	return canvasRows(t, pool, canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}) + "\n" + operations
}

// S7: deactivation keeps the usage; while inactive, every command but the
// release of a pending upload is rejected and writes nothing, and a retry of a
// command committed before the deactivation replays its result. Reactivation
// restores the same usage, without the released reservation.
func TestDeactivationKeepsMediaUsageAndRejectsNewCommands(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, store, key := mediaStreamer(t, s, "alice@example.com")

	kept := upload(t, store, key, 4000, "reference-kept")
	upload(t, store, key, 1000, "reference-other")
	attachOp := nextMediaOperation()
	attached, err := store.Apply(ctx, key, attachOp, media.Attach{AssetID: kept, ReferenceID: "reference-second"})
	if err != nil {
		t.Fatal(err)
	}
	released := run(t, store, key, media.Reserve{DeclaredBytes: 300})
	waiting := run(t, store, key, media.Reserve{DeclaredBytes: 200})
	wantUsage(t, store, s.pool, key, 5000, 500)
	before := usageOf(t, store, key)

	mustDeactivate(t, s, operator, "op-deactivate-media1", "alice@example.com")
	if got := usageOf(t, store, key); got != before {
		t.Fatalf("usage after deactivation = %+v, want %+v", got, before)
	}
	rows := mediaRows(t, s.pool, key)

	commands := []struct {
		name      string
		operation media.Operation
		command   media.Command
	}{
		{"reserve", nextMediaOperation(), media.Reserve{DeclaredBytes: 1}},
		{"replace", nextMediaOperation(), media.Reserve{DeclaredBytes: 1, ReplacesReference: "reference-kept"}},
		{"complete", nextMediaOperation(), media.Complete{AssetID: waiting.AssetID, ReferenceID: "reference-new", Kind: media.KindImage}},
		{"attach", nextMediaOperation(), media.Attach{AssetID: kept, ReferenceID: "reference-third"}},
		{"detach", nextMediaOperation(), media.Detach{ReferenceID: "reference-second"}},
		// The retained ID of the committed Attach with another command.
		{"reused ID", attachOp, media.Attach{AssetID: kept, ReferenceID: "reference-fourth"}},
	}
	for _, c := range commands {
		if _, err := store.Apply(ctx, key, c.operation, c.command); !errors.Is(err, media.ErrInactive) {
			t.Fatalf("%s: error = %v, want ErrInactive", c.name, err)
		}
	}
	if got := mediaRows(t, s.pool, key); got != rows {
		t.Fatalf("rows after rejected commands:\n%s\nwant\n%s", got, rows)
	}
	replay, err := store.Apply(ctx, key, attachOp, media.Attach{AssetID: kept, ReferenceID: "reference-second"})
	if err != nil || replay != attached {
		t.Fatalf("replay of an Attach committed before the cut = %+v, %v; want %+v", replay, err, attached)
	}
	if got := mediaRows(t, s.pool, key); got != rows {
		t.Fatalf("rows after replay:\n%s\nwant\n%s", got, rows)
	}

	// A failed upload always releases its reservation.
	abandoned := run(t, store, key, media.Abandon{AssetID: released.AssetID, Reason: media.AbandonFailed})
	if want := (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 5000, ReservedBytes: 200}); abandoned.Usage != want {
		t.Fatalf("Abandon usage = %+v, want %+v", abandoned.Usage, want)
	}
	if state := assetState(t, s.pool, key, released.AssetID); state != string(media.StateDeleting) {
		t.Fatalf("abandoned asset state = %s, want deleting", state)
	}
	wantUsage(t, store, s.pool, key, 5000, 200)

	mustActivate(t, s, operator, "op-reactivate-media1", "alice@example.com")
	wantUsage(t, store, s.pool, key, before.UsedBytes, before.ReservedBytes-300)
	run(t, store, key, media.Complete{AssetID: waiting.AssetID, ReferenceID: "reference-new", Kind: media.KindImage})
	run(t, store, key, media.Reserve{DeclaredBytes: 1})
	wantUsage(t, store, s.pool, key, 5200, 1)
}

// S9: a Complete that holds the tenant row commits before a deactivation that
// waits for it; the asset is ready and its bytes are counted.
func TestACompleteInFlightBeforeTheDeactivationCommits(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, store, key := mediaStreamer(t, s, "alice@example.com")
	pending := run(t, store, key, media.Reserve{DeclaredBytes: 700})

	held := postgres.NewMediaStore(s.pool)
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
	completion := make(chan applied, 1)
	go func() {
		result, err := held.Apply(context.Background(), key, nextMediaOperation(),
			media.Complete{AssetID: pending.AssetID, ReferenceID: "reference-race", Kind: media.KindImage})
		completion <- applied{result, err}
	}()
	deactivation, finished := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case c := <-completion:
		close(finished)
		t.Fatalf("Complete ended before commit: %+v", c)
	}
	go func() {
		defer close(finished)
		_, err := deactivateIn(s, operator, "op-deactivate-media2", "alice@example.com")
		deactivation <- err
	}()

	// The deactivation waits for the tenant row the Complete holds.
	waitForLockWaiters(t, s.pool, 1)
	if facts := factsOf(t, s.pool, key.TenantID); facts != (tenantFacts{"active", 1}) {
		t.Fatalf("tenant while the Complete is in flight = %+v, want active", facts)
	}
	releaseHeld()
	c := <-completion
	if want := (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 700}); c.err != nil || c.result.Usage != want {
		t.Fatalf("in-flight Complete = %+v, %v; want usage %+v", c.result, c.err, want)
	}
	if err := <-deactivation; err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	if facts := factsOf(t, s.pool, key.TenantID); facts != (tenantFacts{"inactive", 2}) {
		t.Fatalf("tenant = %+v, want inactive at version 2", facts)
	}
	if state := assetState(t, s.pool, key, pending.AssetID); state != string(media.StateReady) {
		t.Fatalf("asset state = %s, want ready", state)
	}
	wantUsage(t, store, s.pool, key, 700, 0)
}

// S9: a Complete that reaches the tenant row while a deactivation holds it
// waits, and is rejected without a write once the deactivation commits.
func TestACompleteWaitingBehindTheDeactivationIsRejected(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, store, key := mediaStreamer(t, s, "alice@example.com")
	pending := run(t, store, key, media.Reserve{DeclaredBytes: 700})
	rows := mediaRows(t, s.pool, key)

	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	postgres.SetSubscriptionFault(s.store, func(_ context.Context, step string) error {
		if step == "commit" {
			close(reached)
			<-release
		}
		return nil
	})
	deactivation, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, err := deactivateIn(s, operator, "op-deactivate-media3", "alice@example.com")
		deactivation <- err
	}()
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case err := <-deactivation:
		t.Fatalf("deactivation ended before commit: %v", err)
	}

	completion := make(chan error, 1)
	go func() {
		_, err := store.Apply(context.Background(), key, nextMediaOperation(),
			media.Complete{AssetID: pending.AssetID, ReferenceID: "reference-race", Kind: media.KindImage})
		completion <- err
	}()
	waitForLockWaiters(t, s.pool, 1)
	releaseHeld()
	if err := <-deactivation; err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	if err := <-completion; !errors.Is(err, media.ErrInactive) {
		t.Fatalf("Complete after the cut: error = %v, want ErrInactive", err)
	}
	if got := mediaRows(t, s.pool, key); got != rows {
		t.Fatalf("rows after the rejected Complete:\n%s\nwant\n%s", got, rows)
	}
	wantUsage(t, store, s.pool, key, 0, 700)
}

// S8: filling tenant A to the exact quota leaves tenant B's quota untouched,
// and A's canvas under B's tenant ID is not found and writes nothing.
func TestTwoTenantsHaveIndependentQuotas(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, store, keyA := mediaStreamer(t, s, "alice@example.com")
	seedAccount(t, s, "bob@example.com")
	b := mustActivate(t, s, operator, "op-activate-bob-0001", "bob@example.com")
	keyB := media.CanvasKey{TenantID: b.TenantID, CanvasID: b.CanvasID}

	filled := upload(t, store, keyA, media.QuotaBytes-1, "reference-a1")
	pendingA := run(t, store, keyA, media.Reserve{DeclaredBytes: 1})
	wantUsage(t, store, s.pool, keyA, media.QuotaBytes-1, 1)
	runErr(t, store, keyA, media.Reserve{DeclaredBytes: 1}, media.ErrQuotaExceeded)
	wantUsage(t, store, s.pool, keyB, 0, 0)

	cross := media.CanvasKey{TenantID: keyB.TenantID, CanvasID: keyA.CanvasID}
	if cross == keyB {
		t.Fatal("both tenants received the same canvas ID")
	}
	rowsA := mediaRows(t, s.pool, keyA)
	for _, command := range []media.Command{
		media.Reserve{DeclaredBytes: 1},
		media.Reserve{DeclaredBytes: 1, ReplacesReference: "reference-a1"},
		media.Complete{AssetID: pendingA.AssetID, ReferenceID: "reference-b1", Kind: media.KindImage},
		media.Abandon{AssetID: pendingA.AssetID, Reason: media.AbandonCancelled},
		media.Attach{AssetID: filled, ReferenceID: "reference-b1"},
		media.Detach{ReferenceID: "reference-a1"},
	} {
		runErr(t, store, cross, command, media.ErrNotFound)
	}
	if _, err := store.Usage(ctx, cross); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Usage(A's canvas under B) = %v, want ErrNotFound", err)
	}
	if got := mediaRows(t, s.pool, keyA); got != rowsA {
		t.Fatalf("A's rows after B's commands:\n%s\nwant\n%s", got, rowsA)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM media_usage WHERE tenant_id = $1 AND canvas_id = $2", cross.TenantID, cross.CanvasID); n != 0 {
		t.Fatalf("A's canvas under B has %d usage rows, want none", n)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM media_operations WHERE tenant_id = $1", keyB.TenantID); n != 0 {
		t.Fatalf("B retained %d operations, want none", n)
	}

	run(t, store, keyB, media.Reserve{DeclaredBytes: media.QuotaBytes})
	wantUsage(t, store, s.pool, keyB, 0, media.QuotaBytes)
	wantUsage(t, store, s.pool, keyA, media.QuotaBytes-1, 1)
}
