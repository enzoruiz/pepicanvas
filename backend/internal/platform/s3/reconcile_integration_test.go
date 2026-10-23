package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
)

var (
	canvasA = media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}
	canvasB = media.CanvasKey{TenantID: "tenant-b", CanvasID: "canvas-1"}
)

// late is a reconciler clock past the orphan grace of every object written
// by the test, so age never keeps an orphan.
func late() time.Time { return time.Now().Add(2 * time.Hour) }

// waitLimit bounds every wait on another goroutine.
const waitLimit = 2 * time.Minute

// crashed is the panic value of a simulated process crash. A panic unwinds
// Uploads without running its abort, like a process that stops between two
// stages.
type crashed string

// crashStore crashes right after a command commits, before its caller sees
// the result.
type crashStore struct {
	media.Store
	crashAfter func(media.Command) bool
}

func (s *crashStore) Apply(ctx context.Context, key media.CanvasKey, operation media.Operation, command media.Command) (media.Result, error) {
	result, err := s.Store.Apply(ctx, key, operation, command)
	if err == nil && s.crashAfter != nil && s.crashAfter(command) {
		panic(crashed(fmt.Sprintf("after %T committed", command)))
	}
	return result, err
}

// crashStorage crashes before or after a storage call and remembers the last
// key and owner written.
type crashStorage struct {
	*s3.Storage
	beforePut, afterPut, afterDelete bool
	lastKey                          string
	lastOwner                        media.Owner
}

func (s *crashStorage) Put(ctx context.Context, key string, owner media.Owner, body io.Reader, size int64) error {
	s.lastKey, s.lastOwner = key, owner
	if s.beforePut {
		panic(crashed("before Put"))
	}
	if err := s.Storage.Put(ctx, key, owner, body, size); err != nil {
		return err
	}
	if s.afterPut {
		panic(crashed("after Put"))
	}
	return nil
}

func (s *crashStorage) Delete(ctx context.Context, key string) error {
	if err := s.Storage.Delete(ctx, key); err != nil {
		return err
	}
	if s.afterDelete {
		panic(crashed("after Delete"))
	}
	return nil
}

// blockingStorage blocks the first Delete until its context ends.
type blockingStorage struct {
	*s3.Storage
	started chan struct{}
	once    sync.Once
}

func (s *blockingStorage) Delete(ctx context.Context, key string) error {
	blocked := false
	s.once.Do(func() { blocked = true })
	if !blocked {
		return s.Storage.Delete(ctx, key)
	}
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

// asset is an upload the test tracks until convergence.
type asset struct {
	key        media.CanvasKey
	id         string
	storageKey string
	data       []byte
}

// reconcileHarness is media.Uploads and media.Reconciler over PostgreSQL and
// MinIO, with crash points between the upload stages.
type reconcileHarness struct {
	store    *postgres.MediaStore
	storage  *s3.Storage
	server   *s3test.Server
	crashes  *crashStore
	writes   *crashStorage
	uploads  *media.Uploads
	inspect  func(ctx context.Context, n int64) (media.Inspection, error)
	offset   atomic.Int64 // store clock offset, in nanoseconds
	canvases []media.CanvasKey
	ready    map[string]asset // by storage key
	gone     []asset
	kept     map[string]bool // objects without an asset that must stay
	seq      int
}

func newReconcileHarness(t *testing.T, canvases ...media.CanvasKey) *reconcileHarness {
	t.Helper()
	ctx := context.Background()
	h := &reconcileHarness{canvases: canvases, ready: map[string]asset{}, kept: map[string]bool{}, inspect: validImage}
	h.server = s3test.Start(t)
	pool, err := postgres.Open(ctx, pgtest.Start(t).DSN(t))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, key := range canvases {
		if _, err := postgres.NewCanvasStore(pool).Create(ctx, canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}); err != nil {
			t.Fatalf("create canvas: %v", err)
		}
	}
	h.store = postgres.NewMediaStore(pool, postgres.WithMediaClock(h.now))
	h.storage = open(t, h.server)
	h.crashes = &crashStore{Store: h.store}
	h.writes = &crashStorage{Storage: h.storage}
	h.uploads = media.NewUploads(h.crashes, h.writes, fakeInspector{outcome: func(ctx context.Context, n int64) (media.Inspection, error) {
		return h.inspect(ctx, n)
	}})
	return h
}

// now is the store clock: real time moved forward by expireReservations.
func (h *reconcileHarness) now() time.Time {
	return time.Now().Add(time.Duration(h.offset.Load()))
}

// expireReservations moves the store clock past the reservation TTL.
func (h *reconcileHarness) expireReservations() {
	h.offset.Add(int64(postgres.DefaultReservationTTL + time.Second))
}

func (h *reconcileHarness) reconciler(t *testing.T, now func() time.Time, storage media.ObjectStore, batch int) *media.Reconciler {
	t.Helper()
	r, err := media.NewReconciler(media.ReconcilerConfig{Store: h.store, Storage: storage, Now: now, Batch: batch})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func (h *reconcileHarness) request(key media.CanvasKey, operationID, referenceID string, data []byte, replace bool) media.UploadRequest {
	return media.UploadRequest{
		Key:           key,
		Operation:     media.Operation{ID: operationID, CorrelationID: "0123456789abcdef0123456789abcdef"},
		DeclaredBytes: int64(len(data)),
		ReferenceID:   referenceID,
		Replace:       replace,
		Body:          bytes.NewReader(data),
	}
}

func (h *reconcileHarness) next() string {
	h.seq++
	return fmt.Sprintf("upload-%d", h.seq)
}

// upload stores a ready asset under referenceID.
func (h *reconcileHarness) upload(t *testing.T, key media.CanvasKey, operationID, referenceID string, replace bool) asset {
	t.Helper()
	data := random(t, 4096+h.seq)
	result, err := h.uploads.Upload(context.Background(), h.request(key, operationID, referenceID, data, replace))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ready, err := h.store.Ready(context.Background(), key, result.AssetID)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	a := asset{key: key, id: result.AssetID, storageKey: ready.StorageKey, data: data}
	h.ready[a.storageKey] = a
	return a
}

// crash runs an upload that must stop at the armed crash point, disarms
// every crash point, and returns the asset it reserved. The asset is tracked
// as never served.
func (h *reconcileHarness) crash(t *testing.T, ctx context.Context, key media.CanvasKey, operationID, referenceID string) asset {
	t.Helper()
	data := random(t, 4096+h.seq)
	defer func() {
		h.crashes.crashAfter = nil
		h.writes.beforePut, h.writes.afterPut, h.writes.afterDelete = false, false, false
		h.inspect = validImage
	}()
	crashes(t, func() {
		_, _ = h.uploads.Upload(ctx, h.request(key, operationID, referenceID, data, false))
	})
	a := asset{key: key, id: h.writes.lastOwner.AssetID, storageKey: h.writes.lastKey, data: data}
	if a.id == "" || h.writes.lastOwner.Key != key {
		t.Fatalf("the crashed upload never reached storage: owner %+v", h.writes.lastOwner)
	}
	h.gone = append(h.gone, a)
	return a
}

// forget marks a ready asset as released.
func (h *reconcileHarness) forget(a asset) {
	delete(h.ready, a.storageKey)
	h.gone = append(h.gone, a)
}

func crashes(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		switch r := recover().(type) {
		case crashed:
		case nil:
			t.Fatal("the upload finished; want a crash")
		default:
			panic(r)
		}
	}()
	f()
}

func abandonCommitted(command media.Command) bool {
	_, ok := command.(media.Abandon)
	return ok
}

// counts are the per-item outcomes of one pass.
type counts struct {
	Purged, OrphansDeleted, Unowned, Young, Claimed, MissingReady int
}

func countsOf(report media.Report) counts {
	return counts{report.Purged, report.OrphansDeleted, report.Unowned, report.Young, report.Claimed, report.MissingReady}
}

// pass runs one complete pass without failures and checks its counts.
func pass(t *testing.T, r *media.Reconciler, want counts) {
	t.Helper()
	report, err := r.Run(context.Background(), "")
	if err != nil || !report.Complete || report.Failures != 0 {
		t.Fatalf("Run = %+v, %v; want a complete pass without failures", report, err)
	}
	if got := countsOf(report); got != want {
		t.Fatalf("Run counts = %+v, want %+v", got, want)
	}
}

func (h *reconcileHarness) inventory(t *testing.T, tenantID string) map[media.State]int64 {
	t.Helper()
	entries, err := h.store.Inventory(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	assets := map[media.State]int64{}
	for _, entry := range entries {
		assets[entry.State] = entry.Assets
	}
	return assets
}

// wantInventory checks the assets per state of a tenant. Without a ready
// entry, want takes the tracked ready assets of the tenant.
func (h *reconcileHarness) wantInventory(t *testing.T, tenantID string, want map[media.State]int64) {
	t.Helper()
	if _, ok := want[media.StateReady]; !ok {
		for _, a := range h.ready {
			if a.key.TenantID == tenantID {
				want[media.StateReady]++
			}
		}
	}
	got := h.inventory(t, tenantID)
	for _, state := range media.InventoryStates {
		if got[state] != want[state] {
			t.Fatalf("inventory of %s = %v, want %v", tenantID, got, want)
		}
	}
}

func (h *reconcileHarness) wantNotServed(t *testing.T, a asset) {
	t.Helper()
	if _, body, err := h.uploads.Open(context.Background(), a.key, a.id); !errors.Is(err, media.ErrNotFound) || body != nil {
		t.Fatalf("Open(%s) = %v, %v; want ErrNotFound", a.id, body, err)
	}
}

func (h *reconcileHarness) wantServed(t *testing.T, a asset) {
	t.Helper()
	_, body, err := h.uploads.Open(context.Background(), a.key, a.id)
	if err != nil {
		t.Fatalf("Open(ready %s): %v", a.id, err)
	}
	defer body.Close()
	served, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(served, a.data) {
		t.Fatalf("served %d bytes (%v), want the %d bytes uploaded", len(served), err, len(a.data))
	}
}

// converged checks that nothing is left to reconcile: no garbage, the bucket
// holds exactly the ready assets and the kept objects, the accounting counts
// only ready assets, ready assets are served byte-exact and nothing else is,
// and a second pass has no effect and observes steady.
func (h *reconcileHarness) converged(t *testing.T, r *media.Reconciler, steady counts) {
	t.Helper()
	ctx := context.Background()
	if garbage, err := h.store.Collect(ctx, 100); err != nil || len(garbage) != 0 {
		t.Fatalf("Collect = %d items, %v; want none", len(garbage), err)
	}
	var want []string
	for key := range h.ready {
		want = append(want, key)
	}
	for key := range h.kept {
		want = append(want, key)
	}
	got := h.server.Keys(t)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("bucket keys = %v, want %v", got, want)
	}
	readyAssets := map[string]int64{}
	for _, key := range h.canvases {
		used := int64(0)
		for _, a := range h.ready {
			if a.key == key {
				used += int64(len(a.data))
			}
		}
		usage, err := h.store.Usage(ctx, key)
		if want := (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: used}); err != nil || usage != want {
			t.Fatalf("Usage(%v) = %+v, %v; want %+v", key, usage, err, want)
		}
	}
	for _, a := range h.ready {
		h.wantServed(t, a)
		readyAssets[a.key.TenantID]++
	}
	for _, a := range h.gone {
		h.wantNotServed(t, a)
	}
	for _, key := range h.canvases {
		h.wantInventory(t, key.TenantID, map[media.State]int64{media.StateReady: readyAssets[key.TenantID]})
	}
	steady.Purged, steady.OrphansDeleted = 0, 0
	pass(t, r, steady)
}

// TestReconcileConvergesAfterEveryInterruption interrupts every upload, abort,
// release, and replacement transition before and after its S3 write, and
// reconciles each one to convergence (TE-013 S3, S4, S6; TE-006 retroactive
// proof, S9).
func TestReconcileConvergesAfterEveryInterruption(t *testing.T) {
	h := newReconcileHarness(t, canvasA)
	r := h.reconciler(t, late, h.storage, 0)
	ctx := context.Background()
	h.upload(t, canvasA, h.next(), "kept-reference", false)

	t.Run("1 reserve committed, crash before Put (before S3)", func(t *testing.T) {
		h.writes.beforePut = true
		a := h.crash(t, ctx, canvasA, h.next(), "ref-1")
		if h.server.Exists(t, a.storageKey) {
			t.Fatal("an object was stored before Put")
		}
		// Before the reservation expires the asset stays pending and unused.
		pass(t, r, counts{})
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StatePending: 1})
		if usage := h.mustUsage(t, canvasA); usage.ReservedBytes != int64(len(a.data)) {
			t.Fatalf("reserved = %d, want %d", usage.ReservedBytes, len(a.data))
		}
		h.wantNotServed(t, a)
		h.expireReservations()
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("2 Put done, crash before inspection (after S3)", func(t *testing.T) {
		h.writes.afterPut = true
		a := h.crash(t, ctx, canvasA, h.next(), "ref-2")
		// The pending asset holds its object: it is kept and never served.
		pass(t, r, counts{})
		if !h.server.Exists(t, a.storageKey) {
			t.Fatal("the held object of a pending asset was deleted")
		}
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StatePending: 1})
		h.wantNotServed(t, a)
		h.expireReservations()
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("3 Complete committed, response lost (after S3)", func(t *testing.T) {
		h.crashes.crashAfter = func(command media.Command) bool { _, ok := command.(media.Complete); return ok }
		a := h.crash(t, ctx, canvasA, h.next(), "ref-3")
		h.gone = h.gone[:len(h.gone)-1]
		h.ready[a.storageKey] = a
		h.wantServed(t, a)
		pass(t, r, counts{})
		h.converged(t, r, counts{})
	})

	t.Run("4 Abandon failed committed, crash before Delete (after S3)", func(t *testing.T) {
		h.inspect = func(context.Context, int64) (media.Inspection, error) {
			return media.Inspection{}, errors.New("decoder failed")
		}
		h.crashes.crashAfter = abandonCommitted
		a := h.crash(t, ctx, canvasA, h.next(), "ref-4")
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		if !h.server.Exists(t, a.storageKey) {
			t.Fatal("the abandoned object is already gone")
		}
		h.wantNotServed(t, a)
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("4 Abandon rejected committed, crash before Delete (after S3)", func(t *testing.T) {
		h.inspect = func(context.Context, int64) (media.Inspection, error) {
			return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
		}
		h.crashes.crashAfter = abandonCommitted
		a := h.crash(t, ctx, canvasA, h.next(), "ref-4r")
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateQuarantined: 1})
		h.wantNotServed(t, a)
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("5 Delete done, crash before Purge (after S3)", func(t *testing.T) {
		h.inspect = func(context.Context, int64) (media.Inspection, error) {
			return media.Inspection{}, errors.New("decoder failed")
		}
		h.writes.afterDelete = true
		a := h.crash(t, ctx, canvasA, h.next(), "ref-5")
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		if h.server.Exists(t, a.storageKey) {
			t.Fatal("the deleted object still exists")
		}
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("6 Detach of the last reference, before Delete (before S3)", func(t *testing.T) {
		a := h.upload(t, canvasA, h.next(), "ref-6", false)
		if _, err := h.store.Apply(ctx, canvasA, media.Operation{ID: "detach-6"}, media.Detach{ReferenceID: "ref-6"}); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		h.forget(a)
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		h.wantNotServed(t, a)
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("7 successful replacement, before Delete of the old bytes (before S3)", func(t *testing.T) {
		old := h.upload(t, canvasA, h.next(), "ref-7", false)
		replacement := h.upload(t, canvasA, h.next(), "ref-7", true)
		h.forget(old)
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		h.wantNotServed(t, old)
		h.wantServed(t, replacement)
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("8 restored orphan with an owner and no row (after S3)", func(t *testing.T) {
		key := "restored/orphan-8"
		owner := media.Owner{Key: canvasA, AssetID: "restored-asset-8"}
		if err := h.storage.Put(ctx, key, owner, bytes.NewReader([]byte("orphan")), 6); err != nil {
			t.Fatalf("Put: %v", err)
		}
		h.kept[key] = true
		// Within the grace the orphan is young and kept.
		young := h.reconciler(t, time.Now, h.storage, 0)
		pass(t, young, counts{Young: 1})
		h.converged(t, young, counts{Young: 1})
		// Past the grace its owner is proven absent: it is deleted.
		delete(h.kept, key)
		pass(t, r, counts{OrphansDeleted: 1})
		h.converged(t, r, counts{})
	})

	t.Run("8 object naming an existing asset is claimed (after S3)", func(t *testing.T) {
		var owner asset
		for _, a := range h.ready {
			owner = a
			break
		}
		key := "restored/claimed-8"
		if err := h.storage.Put(ctx, key, media.Owner{Key: owner.key, AssetID: owner.id}, bytes.NewReader([]byte("copy")), 4); err != nil {
			t.Fatalf("Put: %v", err)
		}
		h.kept[key] = true
		defer h.removeKept(t, key)
		pass(t, r, counts{Claimed: 1})
		h.converged(t, r, counts{Claimed: 1})
	})

	t.Run("9 unowned object (after S3)", func(t *testing.T) {
		key := "foreign/unowned-9"
		if _, err := h.server.Client.PutObject(ctx, s3test.Bucket, key, bytes.NewReader([]byte("foreign")), 7, minio.PutObjectOptions{}); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		h.kept[key] = true
		defer h.removeKept(t, key)
		pass(t, r, counts{Unowned: 1})
		h.converged(t, r, counts{Unowned: 1})
	})

	t.Run("10 ready asset whose object vanished (after S3)", func(t *testing.T) {
		a := h.upload(t, canvasA, h.next(), "ref-10", false)
		h.removeRaw(t, a.storageKey)
		pass(t, r, counts{MissingReady: 1})
		if got, err := h.store.Ready(ctx, a.key, a.id); err != nil || got.State != media.StateReady {
			t.Fatalf("Ready = %+v, %v; want the row still ready", got, err)
		}
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{})
		h.wantNotServed(t, a)
		// Restoring the bytes serves the asset again.
		if err := h.storage.Put(ctx, a.storageKey, media.Owner{Key: a.key, AssetID: a.id}, bytes.NewReader(a.data), int64(len(a.data))); err != nil {
			t.Fatalf("restore Put: %v", err)
		}
		h.converged(t, r, counts{})
	})
}

// TestReconcileConvergesAfterInspectionInterruptions interrupts the
// inspection stage: a rejection, an inspection timeout, a request cancelled
// during inspection, each crashed before its bytes are deleted, and a crash
// during inspection (TE-005 retroactive proof, S9; S6).
func TestReconcileConvergesAfterInspectionInterruptions(t *testing.T) {
	h := newReconcileHarness(t, canvasA)
	r := h.reconciler(t, late, h.storage, 0)
	h.upload(t, canvasA, h.next(), "kept-reference", false)

	abandoned := func(t *testing.T, operationID string, want media.AbandonReason) {
		t.Helper()
		abandon, err := h.store.Abandoned(context.Background(), canvasA, operationID+"/abandon")
		if err != nil || abandon.Reason != want {
			t.Fatalf("Abandoned = %+v, %v; want reason %s", abandon, err, want)
		}
	}

	t.Run("rejected by inspection", func(t *testing.T) {
		h.inspect = func(context.Context, int64) (media.Inspection, error) {
			return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
		}
		h.crashes.crashAfter = abandonCommitted
		op := h.next()
		h.crash(t, context.Background(), canvasA, op, "rejected")
		abandoned(t, op, media.AbandonRejected)
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateQuarantined: 1})
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("inspection times out", func(t *testing.T) {
		h.inspect = func(ctx context.Context, _ int64) (media.Inspection, error) {
			deadline, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			<-deadline.Done()
			return media.Inspection{}, deadline.Err()
		}
		h.crashes.crashAfter = abandonCommitted
		op := h.next()
		h.crash(t, context.Background(), canvasA, op, "timed-out")
		abandoned(t, op, media.AbandonFailed)
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("request cancelled during inspection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.inspect = func(inspectCtx context.Context, _ int64) (media.Inspection, error) {
			cancel()
			return media.Inspection{}, inspectCtx.Err()
		}
		h.crashes.crashAfter = abandonCommitted
		op := h.next()
		h.crash(t, ctx, canvasA, op, "cancelled")
		abandoned(t, op, media.AbandonCancelled)
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})

	t.Run("crash during inspection", func(t *testing.T) {
		h.inspect = func(context.Context, int64) (media.Inspection, error) { panic(crashed("during inspection")) }
		a := h.crash(t, context.Background(), canvasA, h.next(), "crashed")
		pass(t, r, counts{})
		if !h.server.Exists(t, a.storageKey) {
			t.Fatal("the held object of a pending asset was deleted")
		}
		h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StatePending: 1})
		h.expireReservations()
		pass(t, r, counts{Purged: 1})
		h.converged(t, r, counts{})
	})
}

// garbage leaves n deleting assets with their objects, half abandoned before
// Delete and half released by Detach.
func (h *reconcileHarness) garbage(t *testing.T, key media.CanvasKey, n int) {
	t.Helper()
	for i := range n {
		if i%2 == 0 {
			h.inspect = func(context.Context, int64) (media.Inspection, error) {
				return media.Inspection{}, errors.New("decoder failed")
			}
			h.crashes.crashAfter = abandonCommitted
			h.crash(t, context.Background(), key, h.next(), fmt.Sprintf("garbage-%d", i))
			continue
		}
		op, ref := h.next(), fmt.Sprintf("garbage-%d", i)
		a := h.upload(t, key, op, ref, false)
		if _, err := h.store.Apply(context.Background(), key, media.Operation{ID: "detach-" + op}, media.Detach{ReferenceID: ref}); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		h.forget(a)
	}
}

// TestReconcilersConcurrentlyConverge runs two reconcilers over the same
// store and bucket on the same garbage (S4, CTRL-IDEM-01).
func TestReconcilersConcurrentlyConverge(t *testing.T) {
	h := newReconcileHarness(t, canvasA)
	h.upload(t, canvasA, h.next(), "kept-1", false)
	h.upload(t, canvasA, h.next(), "kept-2", false)
	const n = 10
	h.garbage(t, canvasA, n)
	first := h.reconciler(t, late, h.storage, 3)
	second := h.reconciler(t, late, h.storage, 3)

	type outcome struct {
		report media.Report
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, r := range []*media.Reconciler{first, second} {
		go func() {
			<-start
			report, err := r.Run(context.Background(), "")
			results <- outcome{report, err}
		}()
	}
	close(start)
	purged := 0
	for range 2 {
		select {
		case got := <-results:
			if got.err != nil || !got.report.Complete || got.report.Failures != 0 {
				t.Fatalf("concurrent Run = %+v, %v; want a complete pass", got.report, got.err)
			}
			if got.report.Purged > n || got.report.OrphansDeleted != 0 {
				t.Fatalf("concurrent Run counts = %+v", countsOf(got.report))
			}
			purged += got.report.Purged
		case <-time.After(waitLimit):
			t.Fatal("a concurrent Run did not return")
		}
	}
	// Each asset is purged by one pass or both; Purge of a purged asset is a
	// no-op, so a pass may count an asset the other one already purged.
	if purged < n || purged > 2*n {
		t.Fatalf("purged %d in total, want %d to %d", purged, n, 2*n)
	}
	t.Logf("the concurrent passes counted %d purges for %d assets", purged, n)
	h.converged(t, first, counts{})
	h.converged(t, second, counts{})
}

// TestReconcileResumesAfterCancelledRun cancels a pass while it deletes and
// shows the next pass converges (S3, S4).
func TestReconcileResumesAfterCancelledRun(t *testing.T) {
	h := newReconcileHarness(t, canvasA)
	h.upload(t, canvasA, h.next(), "kept", false)
	const n = 3
	h.garbage(t, canvasA, n)
	blocking := &blockingStorage{Storage: h.storage, started: make(chan struct{})}
	r := h.reconciler(t, late, blocking, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		report media.Report
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := r.Run(ctx, "")
		done <- outcome{report, err}
	}()
	select {
	case <-blocking.started:
	case <-time.After(waitLimit):
		t.Fatal("the pass never reached Delete")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.report.Complete || got.report.Purged != 0 {
			t.Fatalf("cancelled Run = %+v, %v; want context.Canceled, incomplete, nothing purged", got.report, got.err)
		}
	case <-time.After(waitLimit):
		t.Fatal("the cancelled Run did not return")
	}
	// Every item is still pending reconciliation with its bytes.
	h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: n})
	if got := len(h.server.Keys(t)); got != n+1 {
		t.Fatalf("bucket holds %d objects, want %d", got, n+1)
	}
	pass(t, r, counts{Purged: n})
	h.converged(t, r, counts{})
}

// TestReconcileHomonymousTenants interrupts uploads of two tenants with the
// same canvas, operation, and reference IDs and reconciles both without
// reading, attaching, or deleting across tenants (S8).
func TestReconcileHomonymousTenants(t *testing.T) {
	h := newReconcileHarness(t, canvasA, canvasB)
	r := h.reconciler(t, late, h.storage, 2)
	ctx := context.Background()

	readyA := h.upload(t, canvasA, "u1", "r1", false)
	readyB := h.upload(t, canvasB, "u1", "r1", false)
	// A: abandoned before Delete. B: crashed before Put.
	h.inspect = func(context.Context, int64) (media.Inspection, error) {
		return media.Inspection{}, errors.New("decoder failed")
	}
	h.crashes.crashAfter = abandonCommitted
	h.crash(t, ctx, canvasA, "u2", "r2")
	h.writes.beforePut = true
	h.crash(t, ctx, canvasB, "u2", "r2")
	// A keeps r3 ready; B releases its own r3.
	keptA := h.upload(t, canvasA, "u3", "r3", false)
	releasedB := h.upload(t, canvasB, "u3", "r3", false)
	if _, err := h.store.Apply(ctx, canvasB, media.Operation{ID: "detach-r3"}, media.Detach{ReferenceID: "r3"}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	h.forget(releasedB)
	// An orphan that names B with A's asset ID is B's orphan; an object that
	// names A's asset is claimed by A.
	if err := h.storage.Put(ctx, "restored/b-orphan", media.Owner{Key: canvasB, AssetID: readyA.id}, bytes.NewReader([]byte("b")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.storage.Put(ctx, "restored/a-claimed", media.Owner{Key: canvasA, AssetID: readyA.id}, bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h.kept["restored/a-claimed"] = true

	h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
	h.wantInventory(t, canvasB.TenantID, map[media.State]int64{media.StatePending: 1, media.StateDeleting: 1})
	h.expireReservations()
	pass(t, r, counts{Purged: 3, OrphansDeleted: 1, Claimed: 1})
	h.converged(t, r, counts{Claimed: 1})

	for _, cross := range []struct {
		a     asset
		other media.CanvasKey
	}{{readyA, canvasB}, {keptA, canvasB}, {readyB, canvasA}} {
		if _, err := h.store.Ready(ctx, cross.other, cross.a.id); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("Ready under the other tenant = %v, want ErrNotFound", err)
		}
		h.wantNotServed(t, asset{key: cross.other, id: cross.a.id})
		if _, err := h.store.Apply(ctx, cross.other, media.Operation{ID: "attach-" + cross.a.id}, media.Attach{AssetID: cross.a.id, ReferenceID: "cross"}); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("Attach under the other tenant = %v, want ErrNotFound", err)
		}
	}
	h.converged(t, r, counts{Claimed: 1})
}

// TestReconcilerLoopOverS3 runs the periodic loop over the real stack: a
// triggered pass converges, Close returns, and later work stays pending for
// the next reconciler (S2, S7).
func TestReconcilerLoopOverS3(t *testing.T) {
	h := newReconcileHarness(t, canvasA)
	h.upload(t, canvasA, h.next(), "kept", false)
	r, err := media.NewReconciler(media.ReconcilerConfig{Store: h.store, Storage: h.storage, Now: late, Interval: time.Hour})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "the first pass", func() bool { _, ok := r.Last(); return ok })

	h.garbage(t, canvasA, 1)
	r.Trigger()
	eventually(t, "the triggered pass", func() bool {
		garbage, err := h.store.Collect(context.Background(), 10)
		return err == nil && len(garbage) == 0
	})
	h.converged(t, r, counts{})

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	h.garbage(t, canvasA, 1)
	r.Trigger()
	if r.Running() {
		t.Fatal("the reconciler still runs after Close")
	}
	if _, err := r.Run(context.Background(), ""); !errors.Is(err, media.ErrReconcilerClosed) {
		t.Fatalf("Run after Close = %v, want ErrReconcilerClosed", err)
	}
	// The work left after Close stays pending with its bytes.
	h.wantInventory(t, canvasA.TenantID, map[media.State]int64{media.StateDeleting: 1})
	if got := len(h.server.Keys(t)); got != 2 {
		t.Fatalf("bucket holds %d objects, want 2", got)
	}
	next := h.reconciler(t, late, h.storage, 0)
	pass(t, next, counts{Purged: 1})
	h.converged(t, next, counts{})
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, waitLimit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *reconcileHarness) mustUsage(t *testing.T, key media.CanvasKey) media.Usage {
	t.Helper()
	usage, err := h.store.Usage(context.Background(), key)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	return usage
}

// removeKept removes a kept object, so later passes no longer observe it.
func (h *reconcileHarness) removeKept(t *testing.T, key string) {
	t.Helper()
	h.removeRaw(t, key)
	delete(h.kept, key)
}

func (h *reconcileHarness) removeRaw(t *testing.T, key string) {
	t.Helper()
	if err := h.server.Client.RemoveObject(context.Background(), s3test.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("RemoveObject: %v", err)
	}
}
