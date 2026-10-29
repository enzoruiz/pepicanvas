package media_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
)

const runCorrelation = "reconcile-0123456789abcdef"

// testClock is a controllable clock shared by the store, the storage, and the
// reconciler.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Now().Truncate(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newReconciler(t *testing.T, config media.ReconcilerConfig) *media.Reconciler {
	t.Helper()
	reconciler, err := media.NewReconciler(config)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	t.Cleanup(func() { _ = reconciler.Close(context.Background()) })
	return reconciler
}

// counts strips the identity and times of a report, leaving its effects.
func counts(report media.Report) media.Report {
	report.CorrelationID, report.StartedAt, report.FinishedAt = "", time.Time{}, time.Time{}
	return report
}

func wantReport(t *testing.T, got media.Report, want media.Report) {
	t.Helper()
	if counts(got) != want {
		t.Fatalf("report = %+v, want counts %+v", counts(got), want)
	}
}

func inventory(t *testing.T, store media.Store, tenantID string) map[media.State]int64 {
	t.Helper()
	entries, err := store.Inventory(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Inventory(%s): %v", tenantID, err)
	}
	assets := map[media.State]int64{}
	for _, entry := range entries {
		assets[entry.State] = entry.Assets
	}
	return assets
}

func wantInventory(t *testing.T, store media.Store, tenantID string, want map[media.State]int64) {
	t.Helper()
	got := inventory(t, store, tenantID)
	for _, state := range media.InventoryStates {
		if got[state] != want[state] {
			t.Fatalf("Inventory(%s) = %v, want %v", tenantID, got, want)
		}
	}
}

// isolate leaves one asset quarantined (rejected) or deleting (failed) with
// its bytes stored, because deleting them failed during cleanup.
func (f *fixture) isolate(t *testing.T, rejected bool, reference string) media.Result {
	t.Helper()
	if rejected {
		f.inspector.reject(dimensionsRejection(t))
	} else {
		f.storage.fail("open", failWith(errStorage))
	}
	f.storage.fail("delete", failWith(errDeleteA))
	req := f.request("hello", reference)
	if _, err := f.uploads.Upload(context.Background(), req); err == nil {
		t.Fatalf("Upload(%s) succeeded, want a failure", reference)
	}
	f.inspector.set(nil)
	f.storage.fail("open", nil)
	f.storage.fail("delete", nil)
	reserved := f.reservation(t, req)
	f.wantIsolated(t, reserved)
	if _, ok := f.storage.object(reserved.StorageKey); !ok {
		t.Fatalf("storage lost %s", reserved.StorageKey)
	}
	return reserved
}

// NewReconciler rejects a configuration that cannot run, and Run an invalid
// correlation ID.
func TestReconcilerValidatesConfigAndCorrelation(t *testing.T) {
	f := startServer(t).fixture(t)
	invalid := []media.ReconcilerConfig{
		{Storage: f.storage},
		{Store: f.store},
		{Store: f.store, Storage: f.storage, Grace: -time.Second},
		{Store: f.store, Storage: f.storage, Batch: -1},
		{Store: f.store, Storage: f.storage, Batch: media.MaxScanKeys + 1},
		{Store: f.store, Storage: f.storage, Interval: -time.Second},
	}
	for i, config := range invalid {
		if _, err := media.NewReconciler(config); !errors.Is(err, media.ErrInvalid) {
			t.Fatalf("NewReconciler(case %d) = %v, want ErrInvalid", i, err)
		}
	}
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage})
	for _, id := range []string{"has space", strings.Repeat("x", media.MaxIdentifierBytes+1), "tab\t"} {
		if report, err := r.Run(context.Background(), id); !errors.Is(err, media.ErrInvalid) || report != (media.Report{}) {
			t.Fatalf("Run(%q) = %+v, %v; want an empty report and ErrInvalid", id, report, err)
		}
	}
	if _, ok := r.Last(); ok {
		t.Fatal("Last reports a pass although none ran")
	}
	// An empty correlation ID takes a generated one.
	report, err := r.Run(context.Background(), "")
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(report.CorrelationID) {
		t.Fatalf("Run(\"\") = %+v, %v; want a generated 32-hex correlation ID", report, err)
	}
	wantReport(t, report, media.Report{Complete: true})
}

// S3, S4, CTRL-IDEM-01: quarantined and deleting assets converge to purged,
// a failed delete is counted and retried by the next Run, each object is
// deleted exactly once, and a Run on converged state has no effect.
func TestReconcilerGarbageConverges(t *testing.T) {
	f := startServer(t).fixture(t)
	ctx := context.Background()
	isolated := []media.Result{f.isolate(t, true, "ref-q"), f.isolate(t, false, "ref-d1"), f.isolate(t, false, "ref-d2")}
	wantInventory(t, f.store, f.key.TenantID, map[media.State]int64{media.StateQuarantined: 1, media.StateDeleting: 2})
	clock := newTestClock()
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage, Now: clock.Now, Batch: 2})

	failing := isolated[1].StorageKey
	f.storage.fail("delete", func(key string) error {
		if key == failing {
			return errDeleteB
		}
		return nil
	})
	report, err := r.Run(ctx, runCorrelation)
	if !errors.Is(err, errDeleteB) {
		t.Fatalf("Run = %v, want the delete failure", err)
	}
	wantReport(t, report, media.Report{Purged: 2, Failures: 1, Complete: true})
	if report.CorrelationID != runCorrelation || !report.StartedAt.Equal(clock.Now()) || !report.FinishedAt.Equal(clock.Now()) {
		t.Fatalf("report identity = %q %v %v, want %q at %v", report.CorrelationID, report.StartedAt, report.FinishedAt, runCorrelation, clock.Now())
	}
	if last, ok := r.Last(); !ok || last != report {
		t.Fatalf("Last = %+v, %v; want the Run report", last, ok)
	}
	// The report names counts only: no asset, object, or tenant.
	printed := fmt.Sprintf("%+v %v %#v", report, report, report)
	for _, private := range []string{isolated[0].AssetID, isolated[0].StorageKey, failing, isolated[1].AssetID, f.key.TenantID, f.key.CanvasID} {
		if strings.Contains(printed, private) {
			t.Fatalf("report %s exposes %q", printed, private)
		}
	}
	f.wantGone(t, isolated[0])
	f.wantGone(t, isolated[2])
	f.wantIsolated(t, isolated[1])
	if _, ok := f.storage.object(failing); !ok {
		t.Fatalf("storage lost %s although Delete failed", failing)
	}
	wantInventory(t, f.store, f.key.TenantID, map[media.State]int64{media.StateDeleting: 1})

	f.storage.fail("delete", nil)
	report, err = r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	wantReport(t, report, media.Report{Purged: 1, Complete: true})
	for _, reserved := range isolated {
		f.wantGone(t, reserved)
		if n := f.storage.deleteCount(reserved.StorageKey); n != 1 {
			t.Fatalf("%s deleted %d times, want exactly once", reserved.StorageKey, n)
		}
	}
	wantInventory(t, f.store, f.key.TenantID, nil)
	f.wantUsage(t, 0, 0)

	report, err = r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("third Run: %v", err)
	}
	wantReport(t, report, media.Report{Complete: true})
	for _, reserved := range isolated {
		if n := f.storage.deleteCount(reserved.StorageKey); n != 1 {
			t.Fatalf("%s deleted %d times after a converged Run, want once", reserved.StorageKey, n)
		}
	}
}

// S3, S6: an upload that crashed after Put (pending row and object, never
// completed) is neither served nor touched before its reservation expires;
// afterwards a Run expires it, deletes its bytes, and releases its quota.
func TestReconcilerExpiresUploadCrashedAfterPut(t *testing.T) {
	clock := newTestClock()
	f := startServer(t, postgres.WithMediaClock(clock.Now)).fixture(t)
	f.storage.now = clock.Now
	ctx := context.Background()
	reserved, err := f.store.Apply(ctx, f.key, media.Operation{ID: "crashed/reserve"}, media.Reserve{DeclaredBytes: 5})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	owner := media.Owner{Key: f.key, AssetID: reserved.AssetID}
	if err := f.storage.Put(ctx, reserved.StorageKey, owner, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage, Now: clock.Now})

	clock.advance(postgres.DefaultReservationTTL - time.Minute)
	report, err := r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("Run before the TTL: %v", err)
	}
	wantReport(t, report, media.Report{Complete: true})
	f.wantNotServed(t, reserved.AssetID)
	f.wantUsage(t, 0, 5)
	wantInventory(t, f.store, f.key.TenantID, map[media.State]int64{media.StatePending: 1})
	if _, ok := f.storage.object(reserved.StorageKey); !ok || f.storage.deleteCount(reserved.StorageKey) != 0 {
		t.Fatalf("storage touched %s before the TTL", reserved.StorageKey)
	}

	clock.advance(2 * time.Minute)
	report, err = r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("Run after the TTL: %v", err)
	}
	wantReport(t, report, media.Report{Purged: 1, Complete: true})
	f.wantGone(t, reserved)
	f.wantUsage(t, 0, 0)
	wantInventory(t, f.store, f.key.TenantID, nil)
}

// S5: an object no row holds is deleted only when its metadata names an
// owner whose asset does not exist and it is older than the grace period.
// Young, unowned, and claimed objects are kept, and a held key never goes.
func TestReconcilerDeletesOnlyProvenOrphans(t *testing.T) {
	clock := newTestClock()
	f := startServer(t).fixture(t)
	f.storage.now = clock.Now
	ctx := context.Background()
	ready := f.mustUpload(t, f.request("hello", "ref-1"))
	readyAsset := f.wantServed(t, ready.AssetID, "hello")
	old := clock.Now().Add(-media.DefaultOrphanGrace - time.Second)
	f.storage.relabel(readyAsset.StorageKey, media.Owner{Key: f.key, AssetID: "someone-else"}, old)

	orphan := func(asset string) *media.Owner { return &media.Owner{Key: f.key, AssetID: asset} }
	f.storage.plant("orphan-old", orphan("gone-1"), old)
	f.storage.plant("orphan-old-2", orphan("gone-2"), old)
	f.storage.plant("orphan-young", orphan("gone-3"), clock.Now().Add(-media.DefaultOrphanGrace+time.Second))
	f.storage.plant("unowned", nil, old)
	f.storage.plant("invalid-owner", &media.Owner{Key: media.CanvasKey{TenantID: "has space", CanvasID: "c"}, AssetID: "a"}, old)
	f.storage.plant("claimed", orphan(ready.AssetID), old)

	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage, Now: clock.Now, Batch: 2})
	report, err := r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, report, media.Report{OrphansDeleted: 2, Unowned: 2, Young: 1, Claimed: 1, Complete: true})
	for _, key := range []string{"orphan-old", "orphan-old-2"} {
		if _, ok := f.storage.object(key); ok || f.storage.deleteCount(key) != 1 {
			t.Fatalf("orphan %s kept or deleted %d times, want deleted once", key, f.storage.deleteCount(key))
		}
	}
	for _, key := range []string{"orphan-young", "unowned", "invalid-owner", "claimed", readyAsset.StorageKey} {
		if _, ok := f.storage.object(key); !ok || f.storage.deleteCount(key) != 0 {
			t.Fatalf("%s was deleted, want kept", key)
		}
	}
	f.wantServed(t, ready.AssetID, "hello")
	wantInventory(t, f.store, f.key.TenantID, map[media.State]int64{media.StateReady: 1})

	// Once the grace period passes, the young orphan goes too.
	clock.advance(2 * time.Second)
	report, err = r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	wantReport(t, report, media.Report{OrphansDeleted: 1, Unowned: 2, Claimed: 1, Complete: true})
	if _, ok := f.storage.object("orphan-young"); ok {
		t.Fatal("orphan-young kept after the grace period")
	}
}

// S8: two tenants with the same canvas ID reconcile without cross reads,
// associations, or deletions.
func TestReconcilerIsolatesHomonymousTenants(t *testing.T) {
	srv := startServer(t)
	clock := newTestClock()
	bucket := newFakeStorage()
	bucket.now = clock.Now
	canvasID := fmt.Sprintf("canvas-%d", canvasSeq.Add(1))
	a := srv.fixtureWith(t, media.CanvasKey{TenantID: "tenant-a", CanvasID: canvasID}, bucket)
	b := srv.fixtureWith(t, media.CanvasKey{TenantID: "tenant-b", CanvasID: canvasID}, bucket)
	ctx := context.Background()
	readyB := b.mustUpload(t, b.request("bravo", "ref-1"))
	keptB := b.mustUpload(t, b.request("kilo", "ref-2"))
	keptAsset := b.wantServed(t, keptB.AssetID, "kilo")
	old := clock.Now().Add(-media.DefaultOrphanGrace - time.Second)
	// B's held object names tenant A: the row holding the key wins.
	bucket.relabel(keptAsset.StorageKey, media.Owner{Key: a.key, AssetID: keptB.AssetID}, old)
	// Orphans of A, one naming the asset ID B uses on the same canvas ID.
	bucket.plant("orphan-a", &media.Owner{Key: a.key, AssetID: "gone"}, old)
	bucket.plant("orphan-a-homonym", &media.Owner{Key: a.key, AssetID: readyB.AssetID}, old)
	inventoryB := inventory(t, srv.store, "tenant-b")

	r := newReconciler(t, media.ReconcilerConfig{Store: srv.store, Storage: bucket, Now: clock.Now})
	report, err := r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, report, media.Report{OrphansDeleted: 2, Complete: true})
	for _, key := range []string{"orphan-a", "orphan-a-homonym"} {
		if _, ok := bucket.object(key); ok {
			t.Fatalf("orphan %s of tenant A kept", key)
		}
	}
	if bucket.deleteCount(keptAsset.StorageKey) != 0 {
		t.Fatal("the object held by tenant B was deleted")
	}
	b.wantServed(t, readyB.AssetID, "bravo")
	b.wantServed(t, keptB.AssetID, "kilo")
	a.wantNotServed(t, readyB.AssetID)
	if got := inventory(t, srv.store, "tenant-b"); fmt.Sprint(got) != fmt.Sprint(inventoryB) {
		t.Fatalf("Inventory(tenant-b) = %v, want unchanged %v", got, inventoryB)
	}
	wantInventory(t, srv.store, "tenant-a", nil)
}

// S3: a ready asset whose bytes are missing is reported, never mutated, and
// never served.
func TestReconcilerReportsReadyAssetWithoutBytes(t *testing.T) {
	f := startServer(t).fixture(t)
	ctx := context.Background()
	lost := f.mustUpload(t, f.request("hello", "ref-1"))
	kept := f.mustUpload(t, f.request("world", "ref-2"))
	lostAsset := f.wantServed(t, lost.AssetID, "hello")
	f.storage.lose(lostAsset.StorageKey)

	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage, Batch: 1})
	report, err := r.Run(ctx, runCorrelation)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, report, media.Report{MissingReady: 1, Complete: true})
	if asset, err := f.store.Ready(ctx, f.key, lost.AssetID); err != nil || asset != lostAsset {
		t.Fatalf("Ready = %+v, %v; want the unchanged ready asset", asset, err)
	}
	if _, _, err := f.uploads.Open(ctx, f.key, lost.AssetID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Open of the lost asset = %v, want ErrNotFound", err)
	}
	f.wantServed(t, kept.AssetID, "world")
	f.wantUsage(t, 10, 0)
	wantInventory(t, f.store, f.key.TenantID, map[media.State]int64{media.StateReady: 2})
}

// S4, S6: a Run interrupted while deleting returns the context error with an
// incomplete report and leaves the garbage persisted; the next Run converges.
func TestReconcilerResumesAfterInterruption(t *testing.T) {
	f := startServer(t).fixture(t)
	reserved := f.isolate(t, false, "ref-1")
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage})

	f.storage.block("delete")
	ctx := cancelWhenStarted(t, f.storage.started)
	report, err := r.Run(ctx, runCorrelation)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	wantReport(t, report, media.Report{})
	f.wantIsolated(t, reserved)
	if _, ok := f.storage.object(reserved.StorageKey); !ok {
		t.Fatalf("storage lost %s", reserved.StorageKey)
	}

	f.storage.unblock("delete")
	report, err = r.Run(context.Background(), runCorrelation)
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	wantReport(t, report, media.Report{Purged: 1, Complete: true})
	f.wantGone(t, reserved)
	if n := f.storage.deleteCount(reserved.StorageKey); n != 1 {
		t.Fatalf("%s deleted %d times, want once", reserved.StorageKey, n)
	}
}

// waitFor polls cond until it holds or a deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// S2, S7: Start runs a pass at once, Trigger runs another, and Close waits for
// the pass in flight, is idempotent, and blocks later use.
func TestReconcilerLoopRunsAndCloses(t *testing.T) {
	f := startServer(t).fixture(t)
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage, Interval: time.Hour})
	r.Trigger() // not running: no effect
	if r.Running() {
		t.Fatal("Running before Start")
	}
	first := f.isolate(t, false, "ref-1")
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Start(); err == nil || errors.Is(err, media.ErrReconcilerClosed) {
		t.Fatalf("second Start = %v, want an already-started error", err)
	}
	if !r.Running() {
		t.Fatal("not Running after Start")
	}
	waitFor(t, "the first pass", func() bool { _, ok := r.Last(); return ok })
	firstReport, _ := r.Last()
	wantReport(t, firstReport, media.Report{Purged: 1, Complete: true})
	f.wantGone(t, first)

	second := f.isolate(t, false, "ref-2")
	release := f.storage.hold("delete")
	defer release()
	r.Trigger()
	waitStarted(t, f.storage.started)
	closed := make(chan error, 1)
	go func() { closed <- r.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v before the pass in flight finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	if r.Running() {
		t.Fatal("Running after Close started")
	}
	release()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.wantGone(t, second)
	last, _ := r.Last()
	if last.CorrelationID == firstReport.CorrelationID {
		t.Fatal("Trigger did not run a pass with its own correlation ID")
	}
	wantReport(t, last, media.Report{Purged: 1, Complete: true})

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := r.Run(context.Background(), runCorrelation); !errors.Is(err, media.ErrReconcilerClosed) {
		t.Fatalf("Run after Close = %v, want ErrReconcilerClosed", err)
	}
	if err := r.Start(); !errors.Is(err, media.ErrReconcilerClosed) {
		t.Fatalf("Start after Close = %v, want ErrReconcilerClosed", err)
	}
	r.Trigger()
}

// S7, CTRL-SHUTDOWN-01: Close whose context ends first cancels the pass in
// flight, returns the context error, keeps pending work persisted and not
// served, and leaves no goroutine behind.
func TestReconcilerCloseCancelsWithoutBlindDeletion(t *testing.T) {
	f := startServer(t).fixture(t)
	reserved := f.isolate(t, true, "ref-1")
	base := runtime.NumGoroutine()
	r, err := media.NewReconciler(media.ReconcilerConfig{Store: f.store, Storage: f.storage, Interval: time.Hour})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	f.storage.block("delete")
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStarted(t, f.storage.started)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want context.DeadlineExceeded", err)
	}
	if last, ok := r.Last(); !ok || last.Complete || last.Purged != 0 {
		t.Fatalf("Last = %+v, %v; want the cancelled, incomplete pass", last, ok)
	}
	f.storage.unblock("delete")
	f.wantIsolated(t, reserved)
	if _, ok := f.storage.object(reserved.StorageKey); !ok || f.storage.deleteCount(reserved.StorageKey) != 0 {
		t.Fatalf("storage lost %s on Close", reserved.StorageKey)
	}
	if _, err := r.Run(context.Background(), runCorrelation); !errors.Is(err, media.ErrReconcilerClosed) {
		t.Fatalf("Run after Close = %v, want ErrReconcilerClosed", err)
	}
	waitFor(t, "the reconciler goroutines to exit", func() bool { return runtime.NumGoroutine() <= base })
}

// A Run waits for the pass in flight, so passes never overlap.
func TestReconcilerSerializesPasses(t *testing.T) {
	f := startServer(t).fixture(t)
	reserved := f.isolate(t, false, "ref-1")
	r := newReconciler(t, media.ReconcilerConfig{Store: f.store, Storage: f.storage})
	release := f.storage.hold("delete")
	defer release()
	firstDone := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), "first")
		firstDone <- err
	}()
	waitStarted(t, f.storage.started)
	// A second Run waits for the first, honoring its context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if report, err := r.Run(ctx, "second"); !errors.Is(err, context.DeadlineExceeded) || report != (media.Report{}) {
		t.Fatalf("overlapping Run = %+v, %v; want an empty report and context.DeadlineExceeded", report, err)
	}
	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	f.wantGone(t, reserved)
	if last, _ := r.Last(); last.CorrelationID != "first" {
		t.Fatalf("Last = %+v, want the first Run", last)
	}
}

// waitStarted waits for a blocked or held storage call to start, failing the
// test instead of hanging when none does.
func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a storage call to start")
	}
}

// passMark is the context key the test Correlate hook stores a pass's
// correlation ID under.
type passMark struct{}

// markRecorder records, per store or storage method, the correlation ID each
// call's context carried under passMark ("" when none).
type markRecorder struct {
	mu    sync.Mutex
	calls map[string][]string
}

func (m *markRecorder) record(ctx context.Context, method string) {
	mark, _ := ctx.Value(passMark{}).(string)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string][]string{}
	}
	m.calls[method] = append(m.calls[method], mark)
}

// take returns the recorded calls and forgets them.
func (m *markRecorder) take() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := m.calls
	m.calls = nil
	return calls
}

// markedStore records the contexts of the store calls a reconciler makes.
type markedStore struct {
	media.Store
	marks *markRecorder
}

func (s markedStore) Collect(ctx context.Context, limit int) ([]media.Garbage, error) {
	s.marks.record(ctx, "Store.Collect")
	return s.Store.Collect(ctx, limit)
}

func (s markedStore) Purge(ctx context.Context, key media.CanvasKey, assetID string) error {
	s.marks.record(ctx, "Store.Purge")
	return s.Store.Purge(ctx, key, assetID)
}

func (s markedStore) HeldKeys(ctx context.Context, storageKeys []string) (map[string]bool, error) {
	s.marks.record(ctx, "Store.HeldKeys")
	return s.Store.HeldKeys(ctx, storageKeys)
}

func (s markedStore) AssetExists(ctx context.Context, owner media.Owner) (bool, error) {
	s.marks.record(ctx, "Store.AssetExists")
	return s.Store.AssetExists(ctx, owner)
}

func (s markedStore) ReadyPage(ctx context.Context, after media.Owner, limit int) ([]media.StoredAsset, error) {
	s.marks.record(ctx, "Store.ReadyPage")
	return s.Store.ReadyPage(ctx, after, limit)
}

// markedStorage records the contexts of the storage calls a reconciler makes.
type markedStorage struct {
	media.ObjectStore
	marks *markRecorder
}

func (s markedStorage) Delete(ctx context.Context, key string) error {
	s.marks.record(ctx, "Storage.Delete")
	return s.ObjectStore.Delete(ctx, key)
}

func (s markedStorage) Stat(ctx context.Context, key string) (media.Object, error) {
	s.marks.record(ctx, "Storage.Stat")
	return s.ObjectStore.Stat(ctx, key)
}

func (s markedStorage) List(ctx context.Context, after string, limit int) ([]media.Object, error) {
	s.marks.record(ctx, "Storage.List")
	return s.ObjectStore.List(ctx, after, limit)
}

// wantMarked fails unless every reconciler store and storage method was
// called, each time with a context carrying want.
func wantMarked(t *testing.T, calls map[string][]string, want string) {
	t.Helper()
	for _, method := range []string{
		"Store.Collect", "Store.Purge", "Store.HeldKeys", "Store.AssetExists", "Store.ReadyPage",
		"Storage.Delete", "Storage.Stat", "Storage.List",
	} {
		marks := calls[method]
		if len(marks) == 0 {
			t.Errorf("%s not called by the pass", method)
		}
		for i, mark := range marks {
			if mark != want {
				t.Errorf("%s call %d carried correlation %q, want the pass's %q", method, i, mark, want)
			}
		}
	}
}

// TE-015 S1, S5, L12: every store and storage call of a pass, whether Run or
// periodic, gets a context derived from Correlate with the pass's
// correlation ID.
func TestReconcilerCorrelatesEveryCallOfAPass(t *testing.T) {
	f := startServer(t).fixture(t)
	ready := f.mustUpload(t, f.request("hello", "ref-ready"))
	marks := &markRecorder{}
	var hooked []string
	var hookedMu sync.Mutex
	r := newReconciler(t, media.ReconcilerConfig{
		Store:    markedStore{Store: f.store, marks: marks},
		Storage:  markedStorage{ObjectStore: f.storage, marks: marks},
		Interval: time.Hour,
		Correlate: func(ctx context.Context, correlationID string) context.Context {
			hookedMu.Lock()
			hooked = append(hooked, correlationID)
			hookedMu.Unlock()
			return context.WithValue(ctx, passMark{}, correlationID)
		},
	})
	// Garbage, a proven orphan, and a ready asset make the pass call every
	// store and storage method.
	stage := func(n int) {
		f.isolate(t, false, fmt.Sprintf("ref-garbage-%d", n))
		f.storage.plant(fmt.Sprintf("orphan-%d", n), &media.Owner{Key: f.key, AssetID: fmt.Sprintf("gone-%d", n)},
			time.Now().Add(-media.DefaultOrphanGrace-time.Minute))
		marks.take()
	}

	stage(1)
	report, err := r.Run(context.Background(), runCorrelation)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReport(t, report, media.Report{Purged: 1, OrphansDeleted: 1, Complete: true})
	wantMarked(t, marks.take(), runCorrelation)

	stage(2)
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the periodic pass", func() bool { last, ok := r.Last(); return ok && last.CorrelationID != runCorrelation })
	last, _ := r.Last()
	wantReport(t, last, media.Report{Purged: 1, OrphansDeleted: 1, Complete: true})
	wantMarked(t, marks.take(), last.CorrelationID)
	hookedMu.Lock()
	defer hookedMu.Unlock()
	if fmt.Sprint(hooked) != fmt.Sprint([]string{runCorrelation, last.CorrelationID}) {
		t.Fatalf("Correlate called with %v, want once per pass with its correlation ID", hooked)
	}
	f.wantServed(t, ready.AssetID, "hello")
}
