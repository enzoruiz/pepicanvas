package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

var inventoryStart = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func inventoryOf(t *testing.T, store *postgres.MediaStore, tenantID string) []media.StateInventory {
	t.Helper()
	inventory, err := store.Inventory(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Inventory(%s): %v", tenantID, err)
	}
	return inventory
}

// emptyInventory is the inventory of a tenant without assets.
func emptyInventory() []media.StateInventory {
	return []media.StateInventory{
		{State: media.StatePending}, {State: media.StateReady}, {State: media.StateQuarantined}, {State: media.StateDeleting},
	}
}

func wantInventory(t *testing.T, store *postgres.MediaStore, tenantID string, want []media.StateInventory) {
	t.Helper()
	if got := inventoryOf(t, store, tenantID); !reflect.DeepEqual(got, want) {
		t.Fatalf("Inventory(%s) =\n%+v\nwant\n%+v", tenantID, got, want)
	}
}

// stateAt returns the inventory entry of state.
func stateAt(t *testing.T, store *postgres.MediaStore, tenantID string, state media.State) media.StateInventory {
	t.Helper()
	for _, entry := range inventoryOf(t, store, tenantID) {
		if entry.State == state {
			return entry
		}
	}
	t.Fatalf("Inventory(%s) has no %s entry", tenantID, state)
	return media.StateInventory{}
}

func wantState(t *testing.T, store *postgres.MediaStore, tenantID string, want media.StateInventory) {
	t.Helper()
	if got := stateAt(t, store, tenantID, want.State); got != want {
		t.Fatalf("Inventory(%s)[%s] = %+v, want %+v", tenantID, want.State, got, want)
	}
}

// populate leaves one canvas with ready assets of readyBytes, one pending, one
// quarantined, and one deleting asset, and returns the storage key of every
// asset it created.
func populate(t *testing.T, store *postgres.MediaStore, key media.CanvasKey, readyBytes []int64, pending, quarantined, deleting int64) []string {
	t.Helper()
	var keys []string
	for i, bytes := range readyBytes {
		reserved := run(t, store, key, media.Reserve{DeclaredBytes: bytes})
		run(t, store, key, media.Complete{AssetID: reserved.AssetID, ReferenceID: fmt.Sprintf("%s-ref-%d", key.CanvasID, i), Kind: media.KindImage})
		keys = append(keys, reserved.StorageKey)
	}
	keys = append(keys, run(t, store, key, media.Reserve{DeclaredBytes: pending}).StorageKey)
	rejected := run(t, store, key, media.Reserve{DeclaredBytes: quarantined})
	run(t, store, key, media.Abandon{AssetID: rejected.AssetID, Reason: media.AbandonRejected, Rejection: media.RejectionUnsupportedKind})
	failed := run(t, store, key, media.Reserve{DeclaredBytes: deleting})
	run(t, store, key, media.Abandon{AssetID: failed.AssetID, Reason: media.AbandonFailed})
	return append(keys, rejected.StorageKey, failed.StorageKey)
}

func TestMediaInventoryCountsAndBytesPerState(t *testing.T) {
	first := media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}
	second := media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-2"}
	empty := media.CanvasKey{TenantID: "tenant-e", CanvasID: "canvas-1"}
	clock := &testClock{now: inventoryStart}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{first, second, empty}, postgres.WithMediaClock(clock.Now))

	wantInventory(t, store, first.TenantID, emptyInventory())
	populate(t, store, first, []int64{100, 200}, 50, 30, 20)
	populate(t, store, second, []int64{400}, 5, 3, 2)

	// Every asset of the tenant counts, across all its canvases, and an
	// asset that just entered its state is under an hour old.
	wantInventory(t, store, first.TenantID, []media.StateInventory{
		{State: media.StatePending, Assets: 2, Bytes: 55, ByAge: [3]int64{2, 0, 0}},
		{State: media.StateReady, Assets: 3, Bytes: 700, ByAge: [3]int64{3, 0, 0}},
		{State: media.StateQuarantined, Assets: 2, Bytes: 33, ByAge: [3]int64{2, 0, 0}},
		{State: media.StateDeleting, Assets: 2, Bytes: 22, ByAge: [3]int64{2, 0, 0}},
	})
	// A tenant with canvases but no assets, and an unknown tenant, read as
	// four zeroed entries.
	wantInventory(t, store, empty.TenantID, emptyInventory())
	wantInventory(t, store, "tenant-unknown", emptyInventory())
}

func TestMediaInventoryAgeBuckets(t *testing.T) {
	clock := &testClock{now: inventoryStart}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA},
		postgres.WithMediaClock(clock.Now), postgres.WithMediaRetention(48*time.Hour))
	upload(t, store, mediaA, 100, "object-1")

	at := func(age time.Duration, byAge [3]int64, oldest time.Duration) {
		t.Helper()
		clock.Set(inventoryStart.Add(age))
		wantState(t, store, mediaA.TenantID, media.StateInventory{State: media.StateReady, Assets: byAge[0] + byAge[1] + byAge[2], Bytes: 100 * (byAge[0] + byAge[1] + byAge[2]), ByAge: byAge, Oldest: oldest})
	}
	at(0, [3]int64{1, 0, 0}, 0)
	at(time.Hour-time.Microsecond, [3]int64{1, 0, 0}, time.Hour-time.Microsecond)
	// An age of exactly one hour is no longer under an hour.
	at(time.Hour, [3]int64{0, 1, 0}, time.Hour)
	upload(t, store, mediaA, 100, "object-2")
	at(24*time.Hour-time.Microsecond, [3]int64{0, 2, 0}, 24*time.Hour-time.Microsecond)
	// An age of exactly one day is a day or more; the second asset entered
	// ready an hour later.
	at(24*time.Hour, [3]int64{0, 1, 1}, 24*time.Hour)
	at(25*time.Hour, [3]int64{0, 0, 2}, 25*time.Hour)
}

func TestMediaInventoryTransitionsResetAge(t *testing.T) {
	key := func(tenant string) media.CanvasKey { return media.CanvasKey{TenantID: tenant, CanvasID: "canvas-1"} }
	complete, reject, fail, expire, detach, replace := key("t-complete"), key("t-reject"), key("t-fail"), key("t-expire"), key("t-detach"), key("t-replace")
	clock := &testClock{now: inventoryStart}
	ttl := 30 * time.Minute
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{complete, reject, fail, expire, detach, replace},
		postgres.WithMediaClock(clock.Now), postgres.WithReservationTTL(ttl), postgres.WithMediaRetention(48*time.Hour))

	completing := run(t, store, complete, media.Reserve{DeclaredBytes: 10})
	rejecting := run(t, store, reject, media.Reserve{DeclaredBytes: 10})
	failing := run(t, store, fail, media.Reserve{DeclaredBytes: 10})
	expiring := run(t, store, expire, media.Reserve{DeclaredBytes: 10})
	upload(t, store, detach, 10, "object-1")
	upload(t, store, replace, 10, "object-1")
	replacing := run(t, store, replace, media.Reserve{DeclaredBytes: 20, ReplacesReference: "object-1"})

	clock.Set(inventoryStart.Add(10 * time.Minute))
	tenMinutes := [3]int64{1, 0, 0}
	for _, tenant := range []media.CanvasKey{complete, reject, fail, expire} {
		wantState(t, store, tenant.TenantID, media.StateInventory{State: media.StatePending, Assets: 1, Bytes: 10, ByAge: tenMinutes, Oldest: 10 * time.Minute})
	}
	wantState(t, store, detach.TenantID, media.StateInventory{State: media.StateReady, Assets: 1, Bytes: 10, ByAge: tenMinutes, Oldest: 10 * time.Minute})

	run(t, store, complete, media.Complete{AssetID: completing.AssetID, ReferenceID: "object-1", Kind: media.KindImage})
	run(t, store, reject, media.Abandon{AssetID: rejecting.AssetID, Reason: media.AbandonRejected, Rejection: media.RejectionUnsupportedKind})
	run(t, store, fail, media.Abandon{AssetID: failing.AssetID, Reason: media.AbandonFailed})
	run(t, store, detach, media.Detach{ReferenceID: "object-1"})
	run(t, store, replace, media.Complete{AssetID: replacing.AssetID, ReferenceID: "object-1", Kind: media.KindImage})

	fresh := func(state media.State, bytes int64) media.StateInventory {
		return media.StateInventory{State: state, Assets: 1, Bytes: bytes, ByAge: [3]int64{1, 0, 0}}
	}
	wantInventory(t, store, complete.TenantID, []media.StateInventory{{State: media.StatePending}, fresh(media.StateReady, 10), {State: media.StateQuarantined}, {State: media.StateDeleting}})
	wantInventory(t, store, reject.TenantID, []media.StateInventory{{State: media.StatePending}, {State: media.StateReady}, fresh(media.StateQuarantined, 10), {State: media.StateDeleting}})
	wantInventory(t, store, fail.TenantID, []media.StateInventory{{State: media.StatePending}, {State: media.StateReady}, {State: media.StateQuarantined}, fresh(media.StateDeleting, 10)})
	wantInventory(t, store, detach.TenantID, []media.StateInventory{{State: media.StatePending}, {State: media.StateReady}, {State: media.StateQuarantined}, fresh(media.StateDeleting, 10)})
	// The replaced asset lost its last reference and is deleting from now.
	wantInventory(t, store, replace.TenantID, []media.StateInventory{{State: media.StatePending}, fresh(media.StateReady, 20), {State: media.StateQuarantined}, fresh(media.StateDeleting, 10)})

	// The reservation expires when Collect runs, not when its TTL elapsed.
	clock.Set(inventoryStart.Add(45 * time.Minute))
	wantState(t, store, expire.TenantID, media.StateInventory{State: media.StatePending, Assets: 1, Bytes: 10, ByAge: [3]int64{1, 0, 0}, Oldest: 45 * time.Minute})
	if _, err := store.Collect(context.Background(), 10); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantInventory(t, store, expire.TenantID, []media.StateInventory{{State: media.StatePending}, {State: media.StateReady}, {State: media.StateQuarantined}, fresh(media.StateDeleting, 10)})
	if state := assetState(t, pool, expire, expiring.AssetID); state != string(media.StateDeleting) {
		t.Fatalf("expired reservation state = %s, want deleting", state)
	}
	// Assets that did not change keep the age of their last transition.
	wantState(t, store, complete.TenantID, media.StateInventory{State: media.StateReady, Assets: 1, Bytes: 10, ByAge: [3]int64{1, 0, 0}, Oldest: 35 * time.Minute})
}

// storedAssets returns the ready assets of every canvas through ReadyPage with
// the given page size.
func storedAssets(t *testing.T, store *postgres.MediaStore, limit int) []media.StoredAsset {
	t.Helper()
	var all []media.StoredAsset
	var after media.Owner
	for {
		page, err := store.ReadyPage(context.Background(), after, limit)
		if err != nil {
			t.Fatalf("ReadyPage(%+v, %d): %v", after, limit, err)
		}
		if len(page) > limit {
			t.Fatalf("ReadyPage(%+v, %d) returned %d assets", after, limit, len(page))
		}
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].Owner
	}
}

func TestMediaScansIsolateTenantsWithEqualCanvasIDs(t *testing.T) {
	clock := &testClock{now: inventoryStart}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA, mediaB}, postgres.WithMediaClock(clock.Now))
	ctx := context.Background()

	ready := func(key media.CanvasKey, bytes int64, reference string) media.StoredAsset {
		reserved := run(t, store, key, media.Reserve{DeclaredBytes: bytes})
		run(t, store, key, media.Complete{AssetID: reserved.AssetID, ReferenceID: reference, Kind: media.KindImage})
		return media.StoredAsset{Owner: media.Owner{Key: key, AssetID: reserved.AssetID}, StorageKey: reserved.StorageKey}
	}
	readyA := []media.StoredAsset{ready(mediaA, 100, "object-1"), ready(mediaA, 200, "object-2")}
	readyB := []media.StoredAsset{ready(mediaB, 7, "object-1"), ready(mediaB, 8, "object-2")}
	keys := populate(t, store, mediaA, nil, 50, 30, 20)
	keys = append(keys, populate(t, store, mediaB, nil, 5, 3, 2)...)

	wantInventory(t, store, mediaA.TenantID, []media.StateInventory{
		{State: media.StatePending, Assets: 1, Bytes: 50, ByAge: [3]int64{1, 0, 0}},
		{State: media.StateReady, Assets: 2, Bytes: 300, ByAge: [3]int64{2, 0, 0}},
		{State: media.StateQuarantined, Assets: 1, Bytes: 30, ByAge: [3]int64{1, 0, 0}},
		{State: media.StateDeleting, Assets: 1, Bytes: 20, ByAge: [3]int64{1, 0, 0}},
	})
	wantInventory(t, store, mediaB.TenantID, []media.StateInventory{
		{State: media.StatePending, Assets: 1, Bytes: 5, ByAge: [3]int64{1, 0, 0}},
		{State: media.StateReady, Assets: 2, Bytes: 15, ByAge: [3]int64{2, 0, 0}},
		{State: media.StateQuarantined, Assets: 1, Bytes: 3, ByAge: [3]int64{1, 0, 0}},
		{State: media.StateDeleting, Assets: 1, Bytes: 2, ByAge: [3]int64{1, 0, 0}},
	})

	// The operator scan sees the keys of both tenants and nothing else.
	for _, asset := range slices.Concat(readyA, readyB) {
		keys = append(keys, asset.StorageKey)
	}
	held, err := store.HeldKeys(ctx, append(slices.Clone(keys), "assets/unknown"))
	if err != nil {
		t.Fatalf("HeldKeys: %v", err)
	}
	want := map[string]bool{}
	for _, key := range keys {
		want[key] = true
	}
	if !reflect.DeepEqual(held, want) {
		t.Fatalf("HeldKeys = %v, want %v", held, want)
	}

	// An asset ID exists only under the tenant that owns it.
	exists := func(owner media.Owner, want bool) {
		t.Helper()
		got, err := store.AssetExists(ctx, owner)
		if err != nil || got != want {
			t.Fatalf("AssetExists(%+v) = %v, %v; want %v", owner, got, err, want)
		}
	}
	exists(readyA[0].Owner, true)
	exists(media.Owner{Key: mediaB, AssetID: readyA[0].Owner.AssetID}, false)
	exists(media.Owner{Key: media.CanvasKey{TenantID: mediaA.TenantID, CanvasID: "canvas-2"}, AssetID: readyA[0].Owner.AssetID}, false)

	// Ready pages cover both tenants in (tenant, canvas, asset) order and
	// skip every asset that is not ready.
	all := slices.Concat(readyA, readyB)
	slices.SortFunc(all, func(a, b media.StoredAsset) int {
		return strings.Compare(a.Owner.Key.TenantID+"/"+a.Owner.AssetID, b.Owner.Key.TenantID+"/"+b.Owner.AssetID)
	})
	for _, limit := range []int{1, 2, 10} {
		if got := storedAssets(t, store, limit); !reflect.DeepEqual(got, all) {
			t.Fatalf("ReadyPage with limit %d =\n%+v\nwant\n%+v", limit, got, all)
		}
	}
	page, err := store.ReadyPage(ctx, all[1].Owner, 2)
	if err != nil || !reflect.DeepEqual(page, all[2:4]) {
		t.Fatalf("ReadyPage(after second) = %+v, %v; want %+v", page, err, all[2:4])
	}
}

func TestMediaHeldKeysForgetPurgedAssets(t *testing.T) {
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	ctx := context.Background()

	ready := upload(t, store, mediaA, 100, "object-1")
	readyKey := mustReady(t, store, mediaA, ready).StorageKey
	rejected := run(t, store, mediaA, media.Reserve{DeclaredBytes: 30})
	run(t, store, mediaA, media.Abandon{AssetID: rejected.AssetID, Reason: media.AbandonRejected, Rejection: media.RejectionUnsupportedKind})

	held, err := store.HeldKeys(ctx, []string{readyKey, rejected.StorageKey, "assets/unknown"})
	if err != nil || !reflect.DeepEqual(held, map[string]bool{readyKey: true, rejected.StorageKey: true}) {
		t.Fatalf("HeldKeys before purge = %v, %v", held, err)
	}
	exists, err := store.AssetExists(ctx, media.Owner{Key: mediaA, AssetID: rejected.AssetID})
	if err != nil || !exists {
		t.Fatalf("AssetExists(quarantined) = %v, %v; want true", exists, err)
	}
	if err := store.Purge(ctx, mediaA, rejected.AssetID); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	held, err = store.HeldKeys(ctx, []string{readyKey, rejected.StorageKey})
	if err != nil || !reflect.DeepEqual(held, map[string]bool{readyKey: true}) {
		t.Fatalf("HeldKeys after purge = %v, %v", held, err)
	}
	exists, err = store.AssetExists(ctx, media.Owner{Key: mediaA, AssetID: rejected.AssetID})
	if err != nil || exists {
		t.Fatalf("AssetExists(purged) = %v, %v; want false", exists, err)
	}
	held, err = store.HeldKeys(ctx, nil)
	if err != nil || held == nil || len(held) != 0 {
		t.Fatalf("HeldKeys(nil) = %v, %v; want an empty map", held, err)
	}
	wantState(t, store, mediaA.TenantID, media.StateInventory{State: media.StateQuarantined})
}

func mustReady(t *testing.T, store *postgres.MediaStore, key media.CanvasKey, assetID string) media.Asset {
	t.Helper()
	asset, err := store.Ready(context.Background(), key, assetID)
	if err != nil {
		t.Fatalf("Ready(%s): %v", assetID, err)
	}
	return asset
}

func TestMediaScansRejectInvalidInput(t *testing.T) {
	clock := &testClock{now: inventoryStart}
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA}, postgres.WithMediaClock(clock.Now))
	ctx := context.Background()
	upload(t, store, mediaA, 100, "object-1")

	invalid := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, media.ErrInvalid) {
			t.Fatalf("%s = %v, want ErrInvalid", name, err)
		}
	}
	for _, tenant := range []string{"", "tenant a", strings.Repeat("t", media.MaxIdentifierBytes+1)} {
		_, err := store.Inventory(ctx, tenant)
		invalid(fmt.Sprintf("Inventory(%q)", tenant), err)
	}
	for _, owner := range []media.Owner{{}, {Key: mediaA}, {Key: media.CanvasKey{TenantID: "tenant-a"}, AssetID: "asset"}, {Key: mediaA, AssetID: "bad asset"}} {
		_, err := store.AssetExists(ctx, owner)
		invalid(fmt.Sprintf("AssetExists(%+v)", owner), err)
	}
	for _, limit := range []int{0, -1} {
		_, err := store.ReadyPage(ctx, media.Owner{}, limit)
		invalid(fmt.Sprintf("ReadyPage(limit %d)", limit), err)
	}
	_, err := store.ReadyPage(ctx, media.Owner{Key: mediaA}, 1)
	invalid("ReadyPage(partial cursor)", err)

	keys := make([]string, 1001)
	for i := range keys {
		keys[i] = fmt.Sprintf("assets/%d", i)
	}
	_, err = store.HeldKeys(ctx, keys)
	invalid("HeldKeys(1001 keys)", err)
	_, err = store.HeldKeys(ctx, []string{"assets/1", ""})
	invalid("HeldKeys(empty key)", err)
	_, err = store.HeldKeys(ctx, []string{strings.Repeat("k", 257)})
	invalid("HeldKeys(257-byte key)", err)
	if held, err := store.HeldKeys(ctx, append(keys[:999], strings.Repeat("k", 256))); err != nil || len(held) != 0 {
		t.Fatalf("HeldKeys(1000 keys, 256-byte key) = %v, %v; want an empty map", held, err)
	}

	// Rejected scans change nothing.
	wantState(t, store, mediaA.TenantID, media.StateInventory{State: media.StateReady, Assets: 1, Bytes: 100, ByAge: [3]int64{1, 0, 0}})
}

func TestMediaReconciliationMigrationBackfillsStateChange(t *testing.T) {
	clock := &testClock{now: inventoryStart}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA}, postgres.WithMediaClock(clock.Now))
	ctx := context.Background()
	reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 10})
	provider := migrationProvider(t, pool)

	// The test exercises 00007 alone, so later migrations never change what
	// it observes. 00010 refuses to roll back while the reservation's usage
	// record exists, so it is cleared first.
	exec(t, pool, "DELETE FROM canvas_outbox WHERE kind = 'media_usage'")
	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("DownTo(6): %v", err)
	}
	var column bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'media_assets' AND column_name = 'state_changed_at')`).Scan(&column); err != nil || column {
		t.Fatalf("state_changed_at after down = %v, %v; want absent", column, err)
	}
	exec(t, pool, "UPDATE media_assets SET reserved_at = $1", inventoryStart.Add(-2*time.Hour))
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("UpTo(7): %v", err)
	}
	var changedAt time.Time
	var nullable string
	err := pool.QueryRow(ctx, `SELECT a.state_changed_at, c.is_nullable FROM media_assets a, information_schema.columns c
		WHERE a.asset_id = $1 AND c.table_name = 'media_assets' AND c.column_name = 'state_changed_at'`, reserved.AssetID).Scan(&changedAt, &nullable)
	if err != nil || !changedAt.Equal(inventoryStart.Add(-2*time.Hour)) || nullable != "NO" {
		t.Fatalf("backfilled state_changed_at = %v (nullable %s), %v; want the reservation time, not null", changedAt, nullable, err)
	}
	wantState(t, store, mediaA.TenantID, media.StateInventory{State: media.StatePending, Assets: 1, Bytes: 10, ByAge: [3]int64{0, 1, 0}, Oldest: 2 * time.Hour})
}
