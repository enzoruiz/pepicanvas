package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// mediaState is every committed row of one canvas: usage, assets, references,
// retained operations, and the canvas itself with its objects and outbox
// records, which an object-creating completion also writes.
type mediaState struct {
	usage, assets, references, operations, canvas string
}

func mediaSnapshot(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) mediaState {
	t.Helper()
	var state mediaState
	err := pool.QueryRow(context.Background(), `
		SELECT
			COALESCE((SELECT format('%s/%s/%s', quota_bytes, used_bytes, reserved_bytes) FROM media_usage
				WHERE tenant_id = $1 AND canvas_id = $2), ''),
			COALESCE((SELECT string_agg(format('%s %s %s %s %s %s %s', asset_id, state, kind, declared_bytes, storage_key, replaces_reference, reserved_at), ',' ORDER BY asset_id)
				FROM media_assets WHERE tenant_id = $1 AND canvas_id = $2), ''),
			COALESCE((SELECT string_agg(reference_id || '>' || asset_id, ',' ORDER BY reference_id)
				FROM media_references WHERE tenant_id = $1 AND canvas_id = $2), ''),
			COALESCE((SELECT string_agg(format('%s %s %s %s %s/%s/%s %s %s %s %s', operation_id, encode(fingerprint, 'hex'), asset_id, storage_key,
					quota_bytes, used_bytes, reserved_bytes, correlation_id, accepted_at, abandon_reason, rejection_code), ',' ORDER BY operation_id)
				FROM media_operations WHERE tenant_id = $1 AND canvas_id = $2), ''),
			COALESCE((SELECT format('%s/%s', revision, next_z) FROM canvases WHERE tenant_id = $1 AND canvas_id = $2), '') || ' ' ||
			COALESCE((SELECT string_agg(format('%s %s %s %s,%s %sx%s %s %s', object_id, kind, content, x, y, width, height, rotation, z), ',' ORDER BY z)
				FROM canvas_objects WHERE tenant_id = $1 AND canvas_id = $2), '') || ' ' ||
			COALESCE((SELECT string_agg(format('%s %s %s %s %s %s %s %s', id, kind, revision, operation_id, correlation_id, object_id, z, mutation), ',' ORDER BY id)
				FROM canvas_outbox WHERE tenant_id = $1 AND canvas_id = $2), '')`,
		key.TenantID, key.CanvasID).Scan(&state.usage, &state.assets, &state.references, &state.operations, &state.canvas)
	if err != nil {
		t.Fatalf("media snapshot: %v", err)
	}
	return state
}

func mediaOperationRows(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_operations WHERE tenant_id = $1 AND canvas_id = $2",
		key.TenantID, key.CanvasID).Scan(&rows); err != nil {
		t.Fatalf("count media operations: %v", err)
	}
	return rows
}

func mustApplyMedia(t *testing.T, store media.Store, key media.CanvasKey, operation media.Operation, command media.Command) media.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, operation, command)
	if err != nil {
		t.Fatalf("Apply(%s, %#v): %v", operation.ID, command, err)
	}
	return result
}

// mediaCase prepares a canvas for one command kind and describes the single
// effect the command must have.
type mediaCase struct {
	name string
	// setup runs before the command and returns it.
	setup                  func(t *testing.T, store media.Store) media.Command
	wantUsed, wantReserved int64
	// check verifies the effect beyond the usage counters.
	check func(t *testing.T, store media.Store, pool *pgxpool.Pool, result media.Result)
}

var mediaCases = []mediaCase{
	{
		name:  "reserve",
		setup: func(*testing.T, media.Store) media.Command { return media.Reserve{DeclaredBytes: 100} },
		// One pending asset holds the reservation.
		wantReserved: 100,
		check: func(t *testing.T, _ media.Store, pool *pgxpool.Pool, result media.Result) {
			if got := assetState(t, pool, mediaA, result.AssetID); got != string(media.StatePending) {
				t.Fatalf("reserved asset state = %s, want pending", got)
			}
			if pending := countAssets(t, pool, mediaA, media.StatePending); pending != 1 {
				t.Fatalf("pending assets = %d, want 1", pending)
			}
		},
	},
	{
		name: "complete",
		setup: func(t *testing.T, store media.Store) media.Command {
			reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 100})
			return media.Complete{AssetID: reserved.AssetID, ReferenceID: "object-1", Kind: media.KindImage}
		},
		wantUsed: 100,
		check: func(t *testing.T, store media.Store, pool *pgxpool.Pool, result media.Result) {
			if asset, err := store.Ready(context.Background(), mediaA, result.AssetID); err != nil || asset.References != 1 {
				t.Fatalf("completed asset = %+v, %v; want ready with one reference", asset, err)
			}
			if result.ObjectID != "" {
				t.Fatalf("completion without an object = %+v, want no object ID", result)
			}
			wantMediaCanvas(t, pool, mediaA, 0)
		},
	},
	{
		// US-011 S8: the completion of an image upload also creates its object.
		name: "complete with object",
		setup: func(t *testing.T, store media.Store) media.Command {
			reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 100})
			return media.Complete{AssetID: reserved.AssetID, ReferenceID: "object-1", Kind: media.KindImage, Object: true, ObjectWidth: 4096, ObjectHeight: 4096}
		},
		wantUsed: 100,
		check: func(t *testing.T, store media.Store, pool *pgxpool.Pool, result media.Result) {
			if asset, err := store.Ready(context.Background(), mediaA, result.AssetID); err != nil || asset.References != 1 {
				t.Fatalf("completed asset = %+v, %v; want ready with one reference", asset, err)
			}
			if result.ObjectID != result.AssetID {
				t.Fatalf("completion with an object = %+v, want the asset ID as object ID", result)
			}
			wantMediaCanvas(t, pool, mediaA, 1, canvas.Object{
				ID: result.AssetID, Kind: canvas.KindImage, Content: "object-1",
				Transform: canvas.Transform{X: 420, Width: 1080, Height: 1080}, Z: 1,
			})
		},
	},
	{
		name: "abandon",
		setup: func(t *testing.T, store media.Store) media.Command {
			reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 100})
			return media.Abandon{AssetID: reserved.AssetID, Reason: media.AbandonFailed}
		},
		check: func(t *testing.T, _ media.Store, pool *pgxpool.Pool, result media.Result) {
			if got := assetState(t, pool, mediaA, result.AssetID); got != string(media.StateDeleting) {
				t.Fatalf("abandoned asset state = %s, want deleting", got)
			}
		},
	},
	{
		name: "attach",
		setup: func(t *testing.T, store media.Store) media.Command {
			return media.Attach{AssetID: upload(t, store, mediaA, 100, "object-1"), ReferenceID: "object-2"}
		},
		wantUsed: 100,
		check: func(t *testing.T, store media.Store, _ *pgxpool.Pool, result media.Result) {
			if asset, err := store.Ready(context.Background(), mediaA, result.AssetID); err != nil || asset.References != 2 {
				t.Fatalf("attached asset = %+v, %v; want two references", asset, err)
			}
		},
	},
	{
		name: "detach",
		setup: func(t *testing.T, store media.Store) media.Command {
			upload(t, store, mediaA, 100, "object-1")
			return media.Detach{ReferenceID: "object-1"}
		},
		check: func(t *testing.T, _ media.Store, pool *pgxpool.Pool, result media.Result) {
			if got := assetState(t, pool, mediaA, result.AssetID); got != string(media.StateDeleting) {
				t.Fatalf("released asset state = %s, want deleting", got)
			}
		},
	},
}

func countAssets(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey, state media.State) int {
	t.Helper()
	var assets int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE tenant_id = $1 AND canvas_id = $2 AND state = $3",
		key.TenantID, key.CanvasID, string(state)).Scan(&assets); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	return assets
}

func TestMediaEqualRetryReplaysTheSameResultWithOneEffect(t *testing.T) {
	for _, tc := range mediaCases {
		t.Run(tc.name, func(t *testing.T) {
			store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
			command := tc.setup(t, store)
			operation := nextMediaOperation()

			first := mustApplyMedia(t, store, mediaA, operation, command)
			want := mediaSnapshot(t, pool, mediaA)
			retry := media.Operation{ID: operation.ID, CorrelationID: "fedcba9876543210fedcba9876543210"}
			if second := mustApplyMedia(t, store, mediaA, retry, command); second != first {
				t.Fatalf("retry = %+v, want the committed %+v", second, first)
			}
			if got := mediaSnapshot(t, pool, mediaA); got != want {
				t.Fatalf("retry produced a second effect:\n got %+v\nwant %+v", got, want)
			}
			if first.Usage != (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: tc.wantUsed, ReservedBytes: tc.wantReserved}) {
				t.Fatalf("result usage = %+v, want used %d reserved %d", first.Usage, tc.wantUsed, tc.wantReserved)
			}
			wantUsage(t, store, pool, mediaA, tc.wantUsed, tc.wantReserved)
			tc.check(t, store, pool, first)

			var correlation string
			if err := pool.QueryRow(context.Background(), "SELECT correlation_id FROM media_operations WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3",
				mediaA.TenantID, mediaA.CanvasID, operation.ID).Scan(&correlation); err != nil || correlation != operation.CorrelationID {
				t.Fatalf("retained correlation = %q, %v; want the accepted %q", correlation, err, operation.CorrelationID)
			}
		})
	}
}

func TestMediaReusedOperationIDWithADifferentCommandIsRejected(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, mediaB})
	asset := upload(t, store, mediaA, 300, "object-1")
	operation := nextMediaOperation()
	reserved := mustApplyMedia(t, store, mediaA, operation, media.Reserve{DeclaredBytes: 100})
	want := mediaSnapshot(t, pool, mediaA)

	for _, command := range []media.Command{
		media.Reserve{DeclaredBytes: 101},
		media.Reserve{DeclaredBytes: 100, ReplacesReference: "object-1"},
		media.Complete{AssetID: reserved.AssetID, ReferenceID: "object-2", Kind: media.KindImage},
		media.Abandon{AssetID: reserved.AssetID, Reason: media.AbandonCancelled},
		media.Attach{AssetID: asset, ReferenceID: "object-2"},
		media.Detach{ReferenceID: "object-1"},
	} {
		if result, err := store.Apply(context.Background(), mediaA, operation, command); !errors.Is(err, media.ErrOperationConflict) {
			t.Fatalf("%#v under a used operation ID = %+v, %v; want ErrOperationConflict", command, result, err)
		}
	}
	if got := mediaSnapshot(t, pool, mediaA); got != want {
		t.Fatalf("conflicting commands changed the canvas:\n got %+v\nwant %+v", got, want)
	}
	if replay := mustApplyMedia(t, store, mediaA, operation, media.Reserve{DeclaredBytes: 100}); replay != reserved {
		t.Fatalf("original retry after conflicts = %+v, want %+v", replay, reserved)
	}
	wantUsage(t, store, pool, mediaA, 300, 100)

	// Operation IDs are scoped to their canvas.
	if other := mustApplyMedia(t, store, mediaB, operation, media.Reserve{DeclaredBytes: 7}); other.AssetID == reserved.AssetID || other.Usage.ReservedBytes != 7 {
		t.Fatalf("same operation ID in another tenant = %+v, want its own reservation", other)
	}
}

func TestMediaConcurrentDuplicatesProduceOneReservation(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	operation := nextMediaOperation()
	command := media.Reserve{DeclaredBytes: 100}

	var wg sync.WaitGroup
	results := make([]media.Result, 16)
	for i := range results {
		wg.Go(func() {
			result, err := store.Apply(context.Background(), mediaA, operation, command)
			if err != nil {
				t.Errorf("concurrent duplicate Reserve: %v", err)
			}
			results[i] = result
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	for i, result := range results {
		if result != results[0] || result.AssetID == "" {
			t.Fatalf("duplicate %d result = %+v, want %+v", i, result, results[0])
		}
	}
	if pending, rows := countAssets(t, pool, mediaA, media.StatePending), mediaOperationRows(t, pool, mediaA); pending != 1 || rows != 1 {
		t.Fatalf("duplicates left %d pending assets and %d operations, want one of each", pending, rows)
	}
	wantUsage(t, store, pool, mediaA, 0, 100)
}

func TestMediaLostAnswerAfterCommitReplaysTheSameResultAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	store, pool := openMedia(t, server, []media.CanvasKey{mediaA})

	var first, second, third media.Result
	steps := []struct {
		name    string
		command func() media.Command
		keep    *media.Result
	}{
		{"reserve", func() media.Command { return media.Reserve{DeclaredBytes: 100} }, &first},
		{"complete", func() media.Command {
			return media.Complete{AssetID: first.AssetID, ReferenceID: "object-1", Kind: media.KindImage}
		}, nil},
		{"attach", func() media.Command { return media.Attach{AssetID: first.AssetID, ReferenceID: "object-2"} }, nil},
		{"detach", func() media.Command { return media.Detach{ReferenceID: "object-2"} }, nil},
		{"reserve again", func() media.Command { return media.Reserve{DeclaredBytes: 50} }, &second},
		{"abandon", func() media.Command { return media.Abandon{AssetID: second.AssetID, Reason: media.AbandonCancelled} }, nil},
		{"reserve an image", func() media.Command { return media.Reserve{DeclaredBytes: 30} }, &third},
		{"complete with object", func() media.Command {
			return media.Complete{AssetID: third.AssetID, ReferenceID: "object-3", Kind: media.KindGIF, Object: true, ObjectWidth: 20, ObjectHeight: 10}
		}, nil},
	}
	for _, step := range steps {
		command := step.command()
		operation := nextMediaOperation()
		// The committed answer is lost on the way back; the client only keeps
		// it here to compare with the replay.
		lost := mustApplyMedia(t, store, mediaA, operation, command)
		if step.keep != nil {
			*step.keep = lost
		}
		want := mediaSnapshot(t, pool, mediaA)

		server.Restart(t)
		store, pool = openMedia(t, server, nil)

		// The retry arrives over a new connection with a new correlation ID.
		reconnected := media.Operation{ID: operation.ID, CorrelationID: "fedcba9876543210fedcba9876543210"}
		if got := mustApplyMedia(t, store, mediaA, reconnected, command); got != lost {
			t.Fatalf("%s retry after restart = %+v, want the committed %+v", step.name, got, lost)
		}
		if got := mediaSnapshot(t, pool, mediaA); got != want {
			t.Fatalf("%s retry produced a second effect:\n got %+v\nwant %+v", step.name, got, want)
		}
	}
	wantUsage(t, store, pool, mediaA, 130, 0)
}

func TestMediaFailureBeforeCommitLeavesNoTraceAndRetryAppliesOnce(t *testing.T) {
	injected := errors.New("injected fault")
	faults := []struct {
		name  string
		hook  func(context.Context) error
		ctx   func() (context.Context, context.CancelFunc)
		check func(error) bool
	}{
		{
			name:  "hook error",
			hook:  func(context.Context) error { return injected },
			ctx:   func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			check: func(err error) bool { return errors.Is(err, injected) },
		},
		{
			name: "context deadline",
			hook: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
			check: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
		},
	}
	for _, tc := range mediaCases {
		for _, fault := range faults {
			t.Run(tc.name+"/"+fault.name, func(t *testing.T) {
				store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
				usageOf(t, store, mediaA)
				command := tc.setup(t, store)
				want := mediaSnapshot(t, pool, mediaA)
				rows := mediaOperationRows(t, pool, mediaA)
				operation := nextMediaOperation()

				postgres.SetMediaBeforeCommit(store, fault.hook)
				ctx, cancel := fault.ctx()
				result, err := store.Apply(ctx, mediaA, operation, command)
				cancel()
				if !fault.check(err) {
					t.Fatalf("Apply with fault before commit = %+v, %v", result, err)
				}
				if got := mediaSnapshot(t, pool, mediaA); got != want {
					t.Fatalf("failed commit changed the canvas:\n got %+v\nwant %+v", got, want)
				}

				postgres.SetMediaBeforeCommit(store, nil)
				first := mustApplyMedia(t, store, mediaA, operation, command)
				if second := mustApplyMedia(t, store, mediaA, operation, command); second != first {
					t.Fatalf("retry after fault: first %+v, second %+v", first, second)
				}
				if got := mediaOperationRows(t, pool, mediaA); got != rows+1 {
					t.Fatalf("retry after fault retained %d operations, want %d", got, rows+1)
				}
				wantUsage(t, store, pool, mediaA, tc.wantUsed, tc.wantReserved)
				tc.check(t, store, pool, first)
			})
		}
	}
}

func TestMediaRejectedCommandsAreNotRetained(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	upload(t, store, mediaA, media.QuotaBytes-50, "object-1")
	rows := mediaOperationRows(t, pool, mediaA)
	operation := nextMediaOperation()
	command := media.Reserve{DeclaredBytes: 100}

	if result, err := store.Apply(context.Background(), mediaA, operation, command); !errors.Is(err, media.ErrQuotaExceeded) {
		t.Fatalf("Reserve over the quota = %+v, %v; want ErrQuotaExceeded", result, err)
	}
	if got := mediaOperationRows(t, pool, mediaA); got != rows {
		t.Fatalf("rejected command retained %d operations, want %d", got, rows)
	}
	wantUsage(t, store, pool, mediaA, media.QuotaBytes-50, 0)

	run(t, store, mediaA, media.Detach{ReferenceID: "object-1"})
	accepted := mustApplyMedia(t, store, mediaA, operation, command)
	if accepted.AssetID == "" || accepted.Usage.ReservedBytes != 100 {
		t.Fatalf("Reserve under the rejected operation ID after freeing space = %+v", accepted)
	}
	if replay := mustApplyMedia(t, store, mediaA, operation, command); replay != accepted {
		t.Fatalf("retry = %+v, want %+v", replay, accepted)
	}
	wantUsage(t, store, pool, mediaA, 0, 100)
}

func TestMediaRetentionExpiresOperations(t *testing.T) {
	const ttl = time.Minute
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA},
		postgres.WithMediaClock(clock.Now), postgres.WithMediaRetention(ttl), postgres.WithReservationTTL(time.Hour))
	reused, other := nextMediaOperation(), nextMediaOperation()
	command := media.Reserve{DeclaredBytes: 100}
	first := mustApplyMedia(t, store, mediaA, reused, command)
	mustApplyMedia(t, store, mediaA, other, media.Reserve{DeclaredBytes: 10})

	clock.Set(start.Add(ttl - time.Microsecond))
	if replay := mustApplyMedia(t, store, mediaA, reused, command); replay != first {
		t.Fatalf("retry just before expiry = %+v, want %+v", replay, first)
	}
	if rows := mediaOperationRows(t, pool, mediaA); rows != 2 {
		t.Fatalf("operations before expiry = %d, want 2", rows)
	}
	wantUsage(t, store, pool, mediaA, 0, 110)

	clock.Set(start.Add(ttl))
	again := mustApplyMedia(t, store, mediaA, reused, command)
	if again.AssetID == first.AssetID || again.StorageKey == first.StorageKey || again.Usage.ReservedBytes != 210 {
		t.Fatalf("expired operation ID = %+v, want a new reservation (first %+v)", again, first)
	}
	wantUsage(t, store, pool, mediaA, 0, 210)
	var acceptedAt time.Time
	if err := pool.QueryRow(context.Background(), "SELECT accepted_at FROM media_operations WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3",
		mediaA.TenantID, mediaA.CanvasID, reused.ID).Scan(&acceptedAt); err != nil || !acceptedAt.Equal(start.Add(ttl)) {
		t.Fatalf("reused operation accepted_at = %v (err %v), want %v", acceptedAt, err, start.Add(ttl))
	}
	if rows := mediaOperationRows(t, pool, mediaA); rows != 1 {
		t.Fatalf("operations after expiry = %d, want only the new one", rows)
	}
}

// S9: the outcome of an abandoned upload is retained with its operation, so a
// retry can repeat the same response.
func TestMediaAbandonedReturnsTheCommittedAbandon(t *testing.T) {
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, mediaB})
	ctx := context.Background()
	abandons := []media.Abandon{
		{Reason: media.AbandonRejected, Rejection: media.RejectionDimensionsExceeded},
		{Reason: media.AbandonFailed},
		{Reason: media.AbandonCancelled},
	}
	for _, abandon := range abandons {
		reserve := nextMediaOperation()
		reserved := mustApplyMedia(t, store, mediaA, reserve, media.Reserve{DeclaredBytes: 10})
		abandon.AssetID = reserved.AssetID
		operation := nextMediaOperation()
		mustApplyMedia(t, store, mediaA, operation, abandon)
		if got, err := store.Abandoned(ctx, mediaA, operation.ID); err != nil || got != abandon {
			t.Fatalf("Abandoned(%s) = %+v, %v; want %+v", operation.ID, got, err, abandon)
		}
		if got, err := store.Abandoned(ctx, mediaA, reserve.ID); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("Abandoned(reserve operation) = %+v, %v; want ErrNotFound", got, err)
		}
		if got, err := store.Abandoned(ctx, mediaB, operation.ID); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("Abandoned(other tenant) = %+v, %v; want ErrNotFound", got, err)
		}
	}
	if got, err := store.Abandoned(ctx, mediaA, "unknown-operation"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Abandoned(unknown) = %+v, %v; want ErrNotFound", got, err)
	}
	for name, id := range map[string]string{"empty": "", "oversized": strings.Repeat("o", media.MaxIdentifierBytes+1)} {
		if got, err := store.Abandoned(ctx, mediaA, id); !errors.Is(err, media.ErrInvalid) {
			t.Fatalf("Abandoned(%s identifier) = %+v, %v; want ErrInvalid", name, got, err)
		}
	}
	if got, err := store.Abandoned(ctx, media.CanvasKey{CanvasID: "canvas-1"}, "op"); !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("Abandoned(invalid key) = %+v, %v; want ErrInvalid", got, err)
	}
}

func TestMediaAbandonedExpiresWithRetention(t *testing.T) {
	const ttl = time.Minute
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA},
		postgres.WithMediaClock(clock.Now), postgres.WithMediaRetention(ttl), postgres.WithReservationTTL(time.Hour))
	ctx := context.Background()
	reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 10})
	operation := nextMediaOperation()
	abandon := media.Abandon{AssetID: reserved.AssetID, Reason: media.AbandonRejected, Rejection: media.RejectionInvalidMetadata}
	mustApplyMedia(t, store, mediaA, operation, abandon)

	clock.Set(start.Add(ttl - time.Microsecond))
	if got, err := store.Abandoned(ctx, mediaA, operation.ID); err != nil || got != abandon {
		t.Fatalf("Abandoned just before expiry = %+v, %v; want %+v", got, err, abandon)
	}
	clock.Set(start.Add(ttl))
	if got, err := store.Abandoned(ctx, mediaA, operation.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Abandoned at expiry = %+v, %v; want ErrNotFound", got, err)
	}
}
