package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

var (
	mediaA = media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}
	mediaB = media.CanvasKey{TenantID: "tenant-b", CanvasID: "canvas-1"}
)

// openMedia migrates the server, creates the given canvases, and returns a
// media store over the same pool.
func openMedia(t *testing.T, server *pgtest.Server, keys []media.CanvasKey, options ...postgres.MediaStoreOption) (*postgres.MediaStore, *pgxpool.Pool) {
	t.Helper()
	canvases, pool := openStore(t, server)
	for _, key := range keys {
		mustCreate(t, canvases, canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID})
	}
	return postgres.NewMediaStore(pool, options...), pool
}

var mediaOperationSeq atomic.Int64

func nextMediaOperation() media.Operation {
	return media.Operation{ID: fmt.Sprintf("mop-%d", mediaOperationSeq.Add(1)), CorrelationID: "0123456789abcdef0123456789abcdef"}
}

func run(t *testing.T, store media.Store, key media.CanvasKey, command media.Command) media.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, nextMediaOperation(), command)
	if err != nil {
		t.Fatalf("Apply(%#v): %v", command, err)
	}
	return result
}

func runErr(t *testing.T, store media.Store, key media.CanvasKey, command media.Command, want error) {
	t.Helper()
	if _, err := store.Apply(context.Background(), key, nextMediaOperation(), command); !errors.Is(err, want) {
		t.Fatalf("Apply(%#v) = %v, want %v", command, err, want)
	}
}

// upload reserves, completes, and attaches reference to a ready asset.
func upload(t *testing.T, store media.Store, key media.CanvasKey, bytes int64, reference string) string {
	t.Helper()
	reserved := run(t, store, key, media.Reserve{DeclaredBytes: bytes})
	run(t, store, key, media.Complete{AssetID: reserved.AssetID, ReferenceID: reference, Kind: media.KindImage})
	return reserved.AssetID
}

func usageOf(t *testing.T, store media.Store, key media.CanvasKey) media.Usage {
	t.Helper()
	usage, err := store.Usage(context.Background(), key)
	if err != nil {
		t.Fatalf("Usage(%+v): %v", key, err)
	}
	return usage
}

func wantUsage(t *testing.T, store media.Store, pool *pgxpool.Pool, key media.CanvasKey, used, reserved int64) {
	t.Helper()
	want := media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: used, ReservedBytes: reserved}
	if got := usageOf(t, store, key); got != want {
		t.Fatalf("Usage(%+v) = %+v, want %+v", key, got, want)
	}
	assertAccounting(t, pool, key)
}

// assertAccounting checks the committed counters against the asset rows:
// used bytes are the ready assets, reserved bytes the pending ones, every
// reference points to a ready asset, and every ready asset has a reference.
func assertAccounting(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) {
	t.Helper()
	var used, reserved, sumReady, sumPending, badRefs, unreferenced int64
	err := pool.QueryRow(context.Background(), `
		SELECT u.used_bytes, u.reserved_bytes,
			COALESCE((SELECT sum(declared_bytes) FROM media_assets a WHERE a.tenant_id = u.tenant_id AND a.canvas_id = u.canvas_id AND a.state = 'ready'), 0),
			COALESCE((SELECT sum(declared_bytes) FROM media_assets a WHERE a.tenant_id = u.tenant_id AND a.canvas_id = u.canvas_id AND a.state = 'pending'), 0),
			(SELECT count(*) FROM media_references r JOIN media_assets a USING (tenant_id, canvas_id, asset_id)
				WHERE r.tenant_id = u.tenant_id AND r.canvas_id = u.canvas_id AND a.state <> 'ready'),
			(SELECT count(*) FROM media_assets a WHERE a.tenant_id = u.tenant_id AND a.canvas_id = u.canvas_id AND a.state = 'ready'
				AND NOT EXISTS (SELECT 1 FROM media_references r WHERE r.tenant_id = a.tenant_id AND r.canvas_id = a.canvas_id AND r.asset_id = a.asset_id))
		FROM media_usage u WHERE u.tenant_id = $1 AND u.canvas_id = $2`, key.TenantID, key.CanvasID,
	).Scan(&used, &reserved, &sumReady, &sumPending, &badRefs, &unreferenced)
	if err != nil {
		t.Fatalf("accounting query: %v", err)
	}
	if used != sumReady || reserved != sumPending || badRefs != 0 || unreferenced != 0 || used+reserved > media.QuotaBytes {
		t.Fatalf("accounting of %+v: used %d (ready %d), reserved %d (pending %d), refs to unusable %d, unreferenced ready %d",
			key, used, sumReady, reserved, sumPending, badRefs, unreferenced)
	}
}

func assetState(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey, assetID string) string {
	t.Helper()
	var state string
	err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3`,
		key.TenantID, key.CanvasID, assetID).Scan(&state)
	if err != nil {
		return "absent"
	}
	return state
}

func wantNotReady(t *testing.T, store media.Store, key media.CanvasKey, assetID string) {
	t.Helper()
	if _, err := store.Ready(context.Background(), key, assetID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Ready(%s) = %v, want ErrNotFound", assetID, err)
	}
}

func TestMediaQuotaAdmitsExactlyOneBillionBytes(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	wantUsage(t, store, pool, mediaA, 0, 0)

	full := run(t, store, mediaA, media.Reserve{DeclaredBytes: media.QuotaBytes})
	if full.Usage != (media.Usage{QuotaBytes: media.QuotaBytes, ReservedBytes: media.QuotaBytes}) {
		t.Fatalf("Reserve result usage = %+v", full.Usage)
	}
	if full.AssetID == "" || full.StorageKey == "" || full.AssetID == full.StorageKey {
		t.Fatalf("Reserve result = %+v, want distinct asset ID and storage key", full)
	}
	runErr(t, store, mediaA, media.Reserve{DeclaredBytes: 1}, media.ErrQuotaExceeded)
	wantUsage(t, store, pool, mediaA, 0, media.QuotaBytes)

	run(t, store, mediaA, media.Complete{AssetID: full.AssetID, ReferenceID: "object-1", Kind: media.KindVideo})
	wantUsage(t, store, pool, mediaA, media.QuotaBytes, 0)
	runErr(t, store, mediaA, media.Reserve{DeclaredBytes: 1}, media.ErrQuotaExceeded)

	released := run(t, store, mediaA, media.Detach{ReferenceID: "object-1"})
	if released.Usage.UsedBytes != 0 {
		t.Fatalf("Detach result usage = %+v, want released at once", released.Usage)
	}
	wantUsage(t, store, pool, mediaA, 0, 0)
	if _, err := pool.Exec(context.Background(), `UPDATE media_usage SET used_bytes = 1000000001 WHERE tenant_id = $1`, mediaA.TenantID); err == nil {
		t.Fatal("database accepted used bytes over the quota")
	}
}

func TestConcurrentReservationsNearTheLimitNeverExceedTheQuota(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	upload(t, store, mediaA, media.QuotaBytes-1000, "object-0")

	rounds := []struct {
		bytes   int64
		callers int
		accept  int
	}{
		{600, 32, 1}, // 1000 left: only one fits
		{100, 16, 4}, // 400 left: exactly four fit
		{1, 8, 0},    // nothing left
	}
	for _, round := range rounds {
		var accepted, rejected atomic.Int64
		var wg sync.WaitGroup
		for range round.callers {
			wg.Go(func() {
				_, err := store.Apply(context.Background(), mediaA, nextMediaOperation(), media.Reserve{DeclaredBytes: round.bytes})
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, media.ErrQuotaExceeded):
					rejected.Add(1)
				default:
					t.Errorf("concurrent Reserve: %v", err)
				}
			})
		}
		wg.Wait()
		if accepted.Load() != int64(round.accept) || rejected.Load() != int64(round.callers-round.accept) {
			t.Fatalf("%d x %d bytes: %d accepted, %d rejected, want %d accepted", round.callers, round.bytes, accepted.Load(), rejected.Load(), round.accept)
		}
		assertAccounting(t, pool, mediaA)
	}
	wantUsage(t, store, pool, mediaA, media.QuotaBytes-1000, 1000)
}

func TestSuccessConsumesQuotaAndEveryAbandonReleasesIt(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})

	pending := run(t, store, mediaA, media.Reserve{DeclaredBytes: 100})
	wantUsage(t, store, pool, mediaA, 0, 100)
	wantNotReady(t, store, mediaA, pending.AssetID)
	runErr(t, store, mediaA, media.Attach{AssetID: pending.AssetID, ReferenceID: "early"}, media.ErrNotFound)

	run(t, store, mediaA, media.Complete{AssetID: pending.AssetID, ReferenceID: "object-1", Kind: media.KindGIF})
	wantUsage(t, store, pool, mediaA, 100, 0)
	ready, err := store.Ready(context.Background(), mediaA, pending.AssetID)
	want := media.Asset{ID: pending.AssetID, State: media.StateReady, Kind: media.KindGIF, DeclaredBytes: 100, StorageKey: pending.StorageKey, References: 1}
	if err != nil || !reflect.DeepEqual(ready, want) {
		t.Fatalf("Ready = %+v, %v; want %+v", ready, err, want)
	}
	runErr(t, store, mediaA, media.Complete{AssetID: pending.AssetID, ReferenceID: "object-2", Kind: media.KindGIF}, media.ErrNotFound)
	runErr(t, store, mediaA, media.Abandon{AssetID: pending.AssetID, Reason: media.AbandonFailed}, media.ErrNotFound)

	wantStates := map[media.AbandonReason]media.State{
		media.AbandonCancelled: media.StateDeleting,
		media.AbandonFailed:    media.StateDeleting,
		media.AbandonRejected:  media.StateQuarantined,
	}
	garbage := map[string]string{}
	for reason, state := range wantStates {
		abandoned := run(t, store, mediaA, media.Reserve{DeclaredBytes: 50})
		wantUsage(t, store, pool, mediaA, 100, 50)
		result := run(t, store, mediaA, media.Abandon{AssetID: abandoned.AssetID, Reason: reason})
		if result.Usage.ReservedBytes != 0 {
			t.Fatalf("Abandon(%s) usage = %+v, want reservation released", reason, result.Usage)
		}
		wantUsage(t, store, pool, mediaA, 100, 0)
		if got := assetState(t, pool, mediaA, abandoned.AssetID); got != string(state) {
			t.Fatalf("Abandon(%s) state = %s, want %s", reason, got, state)
		}
		wantNotReady(t, store, mediaA, abandoned.AssetID)
		runErr(t, store, mediaA, media.Complete{AssetID: abandoned.AssetID, ReferenceID: "late", Kind: media.KindImage}, media.ErrNotFound)
		runErr(t, store, mediaA, media.Attach{AssetID: abandoned.AssetID, ReferenceID: "late"}, media.ErrNotFound)
		garbage[abandoned.AssetID] = abandoned.StorageKey
	}

	collected, err := store.Collect(context.Background(), 100)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]string{}
	for _, item := range collected {
		if item.Key != mediaA {
			t.Fatalf("Collect returned %+v from another canvas", item)
		}
		got[item.AssetID] = item.StorageKey
	}
	if !reflect.DeepEqual(got, garbage) {
		t.Fatalf("Collect = %v, want %v", got, garbage)
	}
	if limited, err := store.Collect(context.Background(), 1); err != nil || len(limited) != 1 {
		t.Fatalf("Collect(1) = %d items, %v; want 1", len(limited), err)
	}
	for assetID := range garbage {
		for range 2 {
			if err := store.Purge(context.Background(), mediaA, assetID); err != nil {
				t.Fatalf("Purge(%s): %v", assetID, err)
			}
		}
		if got := assetState(t, pool, mediaA, assetID); got != "absent" {
			t.Fatalf("purged asset state = %s", got)
		}
	}
	if err := store.Purge(context.Background(), mediaA, pending.AssetID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Purge(ready) = %v, want ErrNotFound", err)
	}
	wantUsage(t, store, pool, mediaA, 100, 0)
}

func TestDuplicatesCountSeparatelyAndOnlyTheLastReferenceReleases(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})

	first := upload(t, store, mediaA, 300, "object-1")
	second := upload(t, store, mediaA, 300, "object-2")
	if first == second {
		t.Fatal("duplicate uploads share an asset")
	}
	wantUsage(t, store, pool, mediaA, 600, 0)

	run(t, store, mediaA, media.Attach{AssetID: first, ReferenceID: "object-3"})
	run(t, store, mediaA, media.Attach{AssetID: first, ReferenceID: "object-4"})
	wantUsage(t, store, pool, mediaA, 600, 0)
	runErr(t, store, mediaA, media.Attach{AssetID: second, ReferenceID: "object-3"}, media.ErrReferenceExists)
	runErr(t, store, mediaA, media.Attach{AssetID: "missing", ReferenceID: "object-9"}, media.ErrNotFound)

	for _, step := range []struct {
		reference string
		left      int
	}{{"object-1", 2}, {"object-3", 1}} {
		run(t, store, mediaA, media.Detach{ReferenceID: step.reference})
		wantUsage(t, store, pool, mediaA, 600, 0)
		if asset, err := store.Ready(context.Background(), mediaA, first); err != nil || asset.References != step.left {
			t.Fatalf("after detaching %s: %+v, %v; want %d references", step.reference, asset, err, step.left)
		}
	}
	run(t, store, mediaA, media.Detach{ReferenceID: "object-4"})
	wantUsage(t, store, pool, mediaA, 300, 0)
	wantNotReady(t, store, mediaA, first)
	if got := assetState(t, pool, mediaA, first); got != string(media.StateDeleting) {
		t.Fatalf("released asset state = %s, want deleting", got)
	}
	runErr(t, store, mediaA, media.Detach{ReferenceID: "object-4"}, media.ErrNotFound)
	runErr(t, store, mediaA, media.Attach{AssetID: first, ReferenceID: "object-5"}, media.ErrNotFound)
}

func TestReplacementNeedsFullCapacityAndReleasesTheOldAssetOnlyOnSuccess(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})

	old := upload(t, store, mediaA, 600_000_000, "object-1")
	runErr(t, store, mediaA, media.Reserve{DeclaredBytes: 400_000_001, ReplacesReference: "object-1"}, media.ErrQuotaExceeded)
	wantUsage(t, store, pool, mediaA, 600_000_000, 0)
	if asset, err := store.Ready(context.Background(), mediaA, old); err != nil || asset.References != 1 {
		t.Fatalf("old asset after rejected replacement = %+v, %v", asset, err)
	}
	runErr(t, store, mediaA, media.Reserve{DeclaredBytes: 10, ReplacesReference: "missing"}, media.ErrNotFound)

	replacement := run(t, store, mediaA, media.Reserve{DeclaredBytes: 400_000_000, ReplacesReference: "object-1"})
	wantUsage(t, store, pool, mediaA, 600_000_000, 400_000_000)
	runErr(t, store, mediaA, media.Complete{AssetID: replacement.AssetID, ReferenceID: "object-2", Kind: media.KindImage}, media.ErrInvalid)
	if _, err := store.Ready(context.Background(), mediaA, old); err != nil {
		t.Fatalf("old asset before replacement succeeds: %v", err)
	}

	done := run(t, store, mediaA, media.Complete{AssetID: replacement.AssetID, ReferenceID: "object-1", Kind: media.KindImage})
	if done.Usage != (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 400_000_000}) {
		t.Fatalf("replacement result usage = %+v", done.Usage)
	}
	wantUsage(t, store, pool, mediaA, 400_000_000, 0)
	wantNotReady(t, store, mediaA, old)
	if asset, err := store.Ready(context.Background(), mediaA, replacement.AssetID); err != nil || asset.References != 1 {
		t.Fatalf("replacement asset = %+v, %v", asset, err)
	}

	// A replaced asset that keeps other references stays ready and counted.
	shared := upload(t, store, mediaA, 100, "object-2")
	run(t, store, mediaA, media.Attach{AssetID: shared, ReferenceID: "object-3"})
	next := run(t, store, mediaA, media.Reserve{DeclaredBytes: 50, ReplacesReference: "object-2"})
	run(t, store, mediaA, media.Complete{AssetID: next.AssetID, ReferenceID: "object-2", Kind: media.KindAudio})
	wantUsage(t, store, pool, mediaA, 400_000_150, 0)
	if asset, err := store.Ready(context.Background(), mediaA, shared); err != nil || asset.References != 1 {
		t.Fatalf("shared asset after replacing one reference = %+v, %v", asset, err)
	}

	// A replacement whose reference vanished meanwhile is not completed.
	orphan := run(t, store, mediaA, media.Reserve{DeclaredBytes: 10, ReplacesReference: "object-3"})
	run(t, store, mediaA, media.Detach{ReferenceID: "object-3"})
	runErr(t, store, mediaA, media.Complete{AssetID: orphan.AssetID, ReferenceID: "object-3", Kind: media.KindImage}, media.ErrNotFound)
	run(t, store, mediaA, media.Abandon{AssetID: orphan.AssetID, Reason: media.AbandonFailed})
	wantUsage(t, store, pool, mediaA, 400_000_050, 0)
}

func TestExpiredReservationsAreReleased(t *testing.T) {
	const ttl = 30 * time.Minute
	clock := &testClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	other := media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-2"}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, other},
		postgres.WithMediaClock(clock.Now), postgres.WithReservationTTL(ttl))

	stale := run(t, store, mediaA, media.Reserve{DeclaredBytes: 100})
	idle := run(t, store, other, media.Reserve{DeclaredBytes: 70})

	clock.Set(clock.Now().Add(ttl - time.Microsecond))
	wantUsage(t, store, pool, mediaA, 0, 100)

	clock.Set(clock.Now().Add(time.Microsecond))
	wantUsage(t, store, pool, mediaA, 0, 0)
	if got := assetState(t, pool, mediaA, stale.AssetID); got != string(media.StateDeleting) {
		t.Fatalf("expired asset state = %s, want deleting", got)
	}
	runErr(t, store, mediaA, media.Complete{AssetID: stale.AssetID, ReferenceID: "object-1", Kind: media.KindImage}, media.ErrNotFound)

	// Collect releases a canvas that nobody touches again.
	collected, err := store.Collect(context.Background(), 10)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	keys := map[string]bool{}
	for _, item := range collected {
		keys[item.AssetID] = true
	}
	if !keys[stale.AssetID] || !keys[idle.AssetID] || len(keys) != 2 {
		t.Fatalf("Collect = %+v, want both expired assets", collected)
	}
	var reserved int64
	if err := pool.QueryRow(context.Background(), `SELECT reserved_bytes FROM media_usage WHERE tenant_id = $1 AND canvas_id = $2`,
		other.TenantID, other.CanvasID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("idle canvas reserved bytes = %d, %v; want 0", reserved, err)
	}
	assertAccounting(t, pool, other)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func TestMediaIsolationBetweenTenants(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, mediaB})

	asset := upload(t, store, mediaA, 900, "object-1")
	pending := run(t, store, mediaA, media.Reserve{DeclaredBytes: 50})
	wantUsage(t, store, pool, mediaB, 0, 0)

	runErr(t, store, mediaB, media.Complete{AssetID: pending.AssetID, ReferenceID: "object-1", Kind: media.KindImage}, media.ErrNotFound)
	runErr(t, store, mediaB, media.Abandon{AssetID: pending.AssetID, Reason: media.AbandonCancelled}, media.ErrNotFound)
	runErr(t, store, mediaB, media.Attach{AssetID: asset, ReferenceID: "object-1"}, media.ErrNotFound)
	runErr(t, store, mediaB, media.Detach{ReferenceID: "object-1"}, media.ErrNotFound)
	runErr(t, store, mediaB, media.Reserve{DeclaredBytes: 1, ReplacesReference: "object-1"}, media.ErrNotFound)
	wantNotReady(t, store, mediaB, asset)
	if err := store.Purge(context.Background(), mediaB, asset); err != nil {
		t.Fatalf("Purge from another tenant: %v", err)
	}

	run(t, store, mediaB, media.Reserve{DeclaredBytes: media.QuotaBytes})
	wantUsage(t, store, pool, mediaA, 900, 50)
	wantUsage(t, store, pool, mediaB, 0, media.QuotaBytes)
	if _, err := store.Ready(context.Background(), mediaA, asset); err != nil {
		t.Fatalf("tenant A asset after tenant B commands: %v", err)
	}

	missing := media.CanvasKey{TenantID: "tenant-c", CanvasID: "canvas-1"}
	if _, err := store.Usage(context.Background(), missing); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Usage(missing canvas) = %v, want ErrNotFound", err)
	}
	runErr(t, store, missing, media.Reserve{DeclaredBytes: 1}, media.ErrNotFound)
	runErr(t, store, mediaA, media.Reserve{}, media.ErrInvalid)
}
