package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// usageRecords returns the pending media usage records of a canvas, oldest
// first, read through the outbox store.
func usageRecords(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) []outbox.Record {
	t.Helper()
	pending, err := postgres.NewOutboxStore(pool).Pending(context.Background(), canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}, postgres.MaxOutboxPage)
	if err != nil {
		t.Fatalf("Pending(%+v): %v", key, err)
	}
	var records []outbox.Record
	for _, record := range pending {
		if record.Malformed != nil {
			t.Fatalf("record %d malformed: %v", record.ID, record.Malformed)
		}
		if record.Usage != nil {
			records = append(records, record)
		}
	}
	return records
}

// storedUsageVersion is the committed usage version of a canvas.
func storedUsageVersion(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(context.Background(), `SELECT version FROM media_usage WHERE tenant_id = $1 AND canvas_id = $2`,
		key.TenantID, key.CanvasID).Scan(&version); err != nil {
		t.Fatalf("read usage version: %v", err)
	}
	return version
}

// commitCounter counts the commits the store reports per canvas.
type commitCounter struct {
	mu    sync.Mutex
	count map[media.CanvasKey]int
}

func (c *commitCounter) record(key media.CanvasKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == nil {
		c.count = map[media.CanvasKey]int{}
	}
	c.count[key]++
}

func (c *commitCounter) of(key media.CanvasKey) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[key]
}

// usageLedger checks that each step wrote the expected media usage records.
type usageLedger struct {
	t       *testing.T
	pool    *pgxpool.Pool
	key     media.CanvasKey
	commits *commitCounter
	seen    int
}

// wrote asserts that exactly one new record holds usage with the next
// version and correlation, and that the commit hook ran once for it.
func (l *usageLedger) wrote(step string, usage media.Usage, correlationID string) {
	l.t.Helper()
	records := usageRecords(l.t, l.pool, l.key)
	if len(records) != l.seen+1 {
		l.t.Fatalf("%s: %d usage records, want %d", step, len(records), l.seen+1)
	}
	l.seen++
	last := records[len(records)-1]
	want := outbox.MediaUsage{Version: int64(l.seen), QuotaBytes: usage.QuotaBytes, UsedBytes: usage.UsedBytes, ReservedBytes: usage.ReservedBytes}
	if *last.Usage != want || last.Event.CorrelationID != correlationID || last.Event.Mutation != nil || last.Event.Result != (canvas.Result{}) {
		l.t.Fatalf("%s: record %+v usage %+v, want %+v with correlation %q", step, last, *last.Usage, want, correlationID)
	}
	if got := storedUsageVersion(l.t, l.pool, l.key); got != want.Version {
		l.t.Fatalf("%s: stored usage version %d, want %d", step, got, want.Version)
	}
	if got := l.commits.of(l.key); got != l.seen {
		l.t.Fatalf("%s: commit hook ran %d times, want %d", step, got, l.seen)
	}
}

// none asserts that no record was written since the last step.
func (l *usageLedger) none(step string) {
	l.t.Helper()
	if got := len(usageRecords(l.t, l.pool, l.key)); got != l.seen {
		l.t.Fatalf("%s: %d usage records, want %d", step, got, l.seen)
	}
	if got := storedUsageVersion(l.t, l.pool, l.key); got != int64(l.seen) {
		l.t.Fatalf("%s: stored usage version %d, want %d", step, got, l.seen)
	}
	if got := l.commits.of(l.key); got != l.seen {
		l.t.Fatalf("%s: commit hook ran %d times, want %d", step, got, l.seen)
	}
}

func applyAs(t *testing.T, store media.Store, key media.CanvasKey, operation media.Operation, command media.Command) media.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, operation, command)
	if err != nil {
		t.Fatalf("Apply(%#v): %v", command, err)
	}
	return result
}

func TestEveryUsageChangeWritesOneMediaUsageRecord(t *testing.T) {
	commits := &commitCounter{}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA}, postgres.WithMediaCommitted(commits.record))
	ledger := &usageLedger{t: t, pool: pool, key: mediaA, commits: commits}
	step := func(name string, command media.Command) media.Result {
		t.Helper()
		operation := nextMediaOperation()
		operation.CorrelationID = "corr-" + operation.ID
		result := applyAs(t, store, mediaA, operation, command)
		ledger.wrote(name, result.Usage, operation.CorrelationID)
		return result
	}

	reserved := step("reserve", media.Reserve{DeclaredBytes: 100})
	step("complete", media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindImage})
	run(t, store, mediaA, media.Attach{AssetID: reserved.AssetID, ReferenceID: "ref-2"})
	ledger.none("attach")
	run(t, store, mediaA, media.Detach{ReferenceID: "ref-2"})
	ledger.none("detach of a reference that is not the last")
	step("detach of the last reference", media.Detach{ReferenceID: "ref-1"})
	abandoned := step("reserve again", media.Reserve{DeclaredBytes: 50})
	step("abandon", media.Abandon{AssetID: abandoned.AssetID, Reason: media.AbandonFailed})

	old := step("reserve the original", media.Reserve{DeclaredBytes: 10})
	step("complete the original", media.Complete{AssetID: old.AssetID, ReferenceID: "ref-3", Kind: media.KindImage})
	replacement := step("reserve the replacement", media.Reserve{DeclaredBytes: 20, ReplacesReference: "ref-3"})
	done := step("complete the replacement", media.Complete{AssetID: replacement.AssetID, ReferenceID: "ref-3", Kind: media.KindImage})
	if want := (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 20}); done.Usage != want {
		t.Fatalf("usage after the replacement = %+v, want %+v", done.Usage, want)
	}
	usageOf(t, store, mediaA)
	ledger.none("usage read without expiry")

	// The stored record names no asset, reference, size, or storage key.
	rows, err := pool.Query(context.Background(), `SELECT revision IS NULL, object_id, z, mutation FROM canvas_outbox WHERE kind = 'media_usage'`)
	if err != nil {
		t.Fatalf("read usage records: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var noRevision bool
		var objectID string
		var z int64
		var payload []byte
		if err := rows.Scan(&noRevision, &objectID, &z, &payload); err != nil {
			t.Fatalf("scan usage record: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("usage payload %s: %v", payload, err)
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		if !noRevision || objectID != "" || z != 0 || !reflect.DeepEqual(names, []string{"quota_bytes", "reserved_bytes", "used_bytes", "version"}) {
			t.Fatalf("usage record revision-less %v object %q z %d fields %v", noRevision, objectID, z, names)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read usage records: %v", err)
	}
}

func TestUnchangedUsageWritesNoMediaUsageRecord(t *testing.T) {
	commits := &commitCounter{}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA}, postgres.WithMediaCommitted(commits.record))
	ledger := &usageLedger{t: t, pool: pool, key: mediaA, commits: commits}
	ctx := context.Background()

	operation := nextMediaOperation()
	reserved := applyAs(t, store, mediaA, operation, media.Reserve{DeclaredBytes: 100})
	ledger.wrote("reserve", reserved.Usage, operation.CorrelationID)

	if replayed := applyAs(t, store, mediaA, operation, media.Reserve{DeclaredBytes: 100}); replayed != reserved {
		t.Fatalf("replay = %+v, want %+v", replayed, reserved)
	}
	ledger.none("replay")
	if _, err := store.Apply(ctx, mediaA, operation, media.Reserve{DeclaredBytes: 7}); !errors.Is(err, media.ErrOperationConflict) {
		t.Fatalf("conflict = %v", err)
	}
	ledger.none("conflict")
	runErr(t, store, mediaA, media.Reserve{DeclaredBytes: media.QuotaBytes}, media.ErrQuotaExceeded)
	ledger.none("quota exceeded")
	runErr(t, store, mediaA, media.Complete{AssetID: "0123456789abcdef0123456789abcdef", ReferenceID: "ref", Kind: media.KindImage}, media.ErrNotFound)
	ledger.none("unknown asset")
	runErr(t, store, mediaA, media.Detach{ReferenceID: "missing"}, media.ErrNotFound)
	ledger.none("unknown reference")

	failed := errors.New("injected failure before commit")
	postgres.SetMediaBeforeCommit(store, func(context.Context) error { return failed })
	if _, err := store.Apply(ctx, mediaA, nextMediaOperation(), media.Reserve{DeclaredBytes: 5}); !errors.Is(err, failed) {
		t.Fatalf("Apply with a failure before commit = %v", err)
	}
	if _, err := store.Apply(ctx, mediaA, nextMediaOperation(), media.Abandon{AssetID: reserved.AssetID, Reason: media.AbandonFailed}); !errors.Is(err, failed) {
		t.Fatalf("Abandon with a failure before commit = %v", err)
	}
	postgres.SetMediaBeforeCommit(store, nil)
	ledger.none("failure before commit")
	wantUsage(t, store, pool, mediaA, 0, 100)
	ledger.none("usage read")
}

func TestInactiveTenantCommandsWriteNoMediaUsageRecord(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, _, key := mediaStreamer(t, s, "media-outbox@example.com")
	commits := &commitCounter{}
	store := postgres.NewMediaStore(s.pool, postgres.WithMediaCommitted(commits.record))
	ledger := &usageLedger{t: t, pool: s.pool, key: key, commits: commits}

	operation := nextMediaOperation()
	reserved := applyAs(t, store, key, operation, media.Reserve{DeclaredBytes: 300})
	ledger.wrote("reserve", reserved.Usage, operation.CorrelationID)
	mustDeactivate(t, s, operator, "op-deactivate-media-outbox", "media-outbox@example.com")
	runErr(t, store, key, media.Reserve{DeclaredBytes: 1}, media.ErrInactive)
	ledger.none("reserve of an inactive tenant")

	// Abandon is always accepted, so the released reservation is published.
	abandon := nextMediaOperation()
	released := applyAs(t, store, key, abandon, media.Abandon{AssetID: reserved.AssetID, Reason: media.AbandonCancelled})
	ledger.wrote("abandon of an inactive tenant", released.Usage, abandon.CorrelationID)
}

func TestReservationExpiryWritesOneMediaUsageRecord(t *testing.T) {
	const ttl = 30 * time.Minute
	clock := &testClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	commits := &commitCounter{}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA},
		postgres.WithMediaClock(clock.Now), postgres.WithReservationTTL(ttl), postgres.WithMediaCommitted(commits.record))
	ledger := &usageLedger{t: t, pool: pool, key: mediaA, commits: commits}
	reserve := func(step string, bytes int64) media.Result {
		t.Helper()
		operation := nextMediaOperation()
		result := applyAs(t, store, mediaA, operation, media.Reserve{DeclaredBytes: bytes})
		ledger.wrote(step, result.Usage, operation.CorrelationID)
		return result
	}
	expireAll := func() { clock.Set(clock.Now().Add(ttl)) }

	// Expiry inside Usage has no correlation.
	reserve("reserve before the usage read", 100)
	expireAll()
	ledger.wrote("usage read", usageOf(t, store, mediaA), "")
	usageOf(t, store, mediaA)
	ledger.none("second usage read")

	// Expiry inside Collect.
	reserve("reserve before collect", 40)
	expireAll()
	if _, err := store.Collect(context.Background(), 10); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	ledger.wrote("collect", media.Usage{QuotaBytes: media.QuotaBytes}, "")

	// Expiry inside Apply commits with the command: one record.
	reserve("reserve before apply", 60)
	expireAll()
	reserve("reserve after expiry", 25)
	if got := usageOf(t, store, mediaA); got.ReservedBytes != 25 {
		t.Fatalf("usage after expiry and reserve = %+v, want 25 reserved", got)
	}
	ledger.none("usage read after apply")
}

func TestConcurrentReservationsWriteIncreasingUsageVersions(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	upload(t, store, mediaA, media.QuotaBytes-1000, "object-0")
	before := len(usageRecords(t, pool, mediaA))

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			_, err := store.Apply(context.Background(), mediaA, nextMediaOperation(), media.Reserve{DeclaredBytes: 300})
			switch {
			case err == nil:
				accepted.Add(1)
			case !errors.Is(err, media.ErrQuotaExceeded):
				t.Errorf("concurrent Reserve: %v", err)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("%d reservations accepted, want 3", accepted.Load())
	}

	records := usageRecords(t, pool, mediaA)
	if len(records) != before+3 {
		t.Fatalf("%d usage records, want %d", len(records), before+3)
	}
	for i, record := range records {
		if record.Usage.Version != int64(i+1) {
			t.Fatalf("record %d (id %d) version %d, want %d", i, record.ID, record.Usage.Version, i+1)
		}
		if i > 0 && record.Usage.ReservedBytes <= records[i-1].Usage.ReservedBytes && record.Usage.UsedBytes == records[i-1].Usage.UsedBytes {
			t.Fatalf("record %d usage %+v does not follow %+v", i, *record.Usage, *records[i-1].Usage)
		}
	}
	final := usageOf(t, store, mediaA)
	last := *records[len(records)-1].Usage
	if last != (outbox.MediaUsage{Version: int64(len(records)), QuotaBytes: final.QuotaBytes, UsedBytes: final.UsedBytes, ReservedBytes: final.ReservedBytes}) {
		t.Fatalf("last record %+v, want the final usage %+v", last, final)
	}
}
