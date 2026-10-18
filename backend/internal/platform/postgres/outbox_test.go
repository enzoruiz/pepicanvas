package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

// committedKeys records the commit hook calls of a store.
type committedKeys struct {
	mu   sync.Mutex
	keys []canvas.Key
}

func (c *committedKeys) record(key canvas.Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, key)
}

func (c *committedKeys) list() []canvas.Key {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]canvas.Key(nil), c.keys...)
}

func mustPending(t *testing.T, store *postgres.OutboxStore, key canvas.Key) []outbox.Record {
	t.Helper()
	records, err := store.Pending(context.Background(), key, 100)
	if err != nil {
		t.Fatalf("Pending(%v): %v", key, err)
	}
	return records
}

func TestApplyWritesOneOutboxEventPerAcceptedMutation(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	hooks := &committedKeys{}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(func() time.Time { return at }), postgres.WithCommitted(hooks.record))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	created := mustApply(t, store, key, canvas.Operation{ID: "op-1", CorrelationID: "corr-1"}, createText)
	steps := []struct {
		operation canvas.Operation
		mutation  canvas.Mutation
	}{
		{canvas.Operation{ID: "op-2", CorrelationID: "corr-2"}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}}},
		{canvas.Operation{ID: "op-3"}, canvas.SetContent{ObjectID: created.ObjectID, Content: "twice"}},
		{canvas.Operation{ID: "op-4", CorrelationID: "corr-4"}, canvas.SetTransform{ObjectID: created.ObjectID, Transform: canvas.Transform{X: -9, Y: 8, Width: 7, Height: 6, Rotation: 12.345678901234567}}},
		{canvas.Operation{ID: "op-5", CorrelationID: "corr-5"}, canvas.DeleteObject{ObjectID: created.ObjectID}},
	}
	want := []resync.Event{{Key: key, Mutation: createText, Result: created, CorrelationID: "corr-1"}}
	for _, step := range steps {
		result := mustApply(t, store, key, step.operation, step.mutation)
		want = append(want, resync.Event{Key: key, Mutation: step.mutation, Result: result, CorrelationID: step.operation.CorrelationID})
	}

	records := mustPending(t, events, key)
	if len(records) != len(want) {
		t.Fatalf("Pending returned %d records, want %d", len(records), len(want))
	}
	for i, record := range records {
		if !reflect.DeepEqual(record.Event, want[i]) {
			t.Fatalf("record %d event = %#v, want %#v", i, record.Event, want[i])
		}
		if record.Event.Result.Revision != int64(i+1) {
			t.Fatalf("record %d revision = %d, want %d", i, record.Event.Result.Revision, i+1)
		}
		if i > 0 && record.ID <= records[i-1].ID {
			t.Fatalf("record IDs not increasing: %d after %d", record.ID, records[i-1].ID)
		}
		if !record.CreatedAt.Equal(at) || record.Attempts != 0 || !record.NextAttempt.Equal(at) {
			t.Fatalf("record %d bookkeeping = created %v attempts %d next %v; want %v, 0, %v", i, record.CreatedAt, record.Attempts, record.NextAttempt, at, at)
		}
	}
	if got := hooks.list(); len(got) != len(want) {
		t.Fatalf("commit hook ran %d times, want %d", len(got), len(want))
	}
	for _, got := range hooks.list() {
		if got != key {
			t.Fatalf("commit hook key = %v, want %v", got, key)
		}
	}
}

func TestOutboxIgnoresReplaysConflictsAndRejectedMutations(t *testing.T) {
	ctx := context.Background()
	hooks := &committedKeys{}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithCommitted(hooks.record))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := mustApply(t, store, key, canvas.Operation{ID: "op-1"}, createText)

	if replay := mustApply(t, store, key, canvas.Operation{ID: "op-1", CorrelationID: "retry"}, createText); replay != first {
		t.Fatalf("replay result = %+v, want %+v", replay, first)
	}
	if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-1"}, canvas.SetDimensions{Dimensions: canvas.DefaultDimensions}); !errors.Is(err, canvas.ErrOperationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-2"}, canvas.DeleteObject{ObjectID: "missing"}); !errors.Is(err, canvas.ErrNotFound) {
		t.Fatalf("missing object error = %v", err)
	}
	if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-3"}, canvas.SetDimensions{}); !errors.Is(err, canvas.ErrInvalid) {
		t.Fatalf("invalid mutation error = %v", err)
	}

	records := mustPending(t, events, key)
	if len(records) != 1 || records[0].Event.Result != first || records[0].Event.CorrelationID != "" {
		t.Fatalf("Pending = %#v, want only the first event without correlation", records)
	}
	if got := len(hooks.list()); got != 1 {
		t.Fatalf("commit hook ran %d times, want 1", got)
	}
}

func TestFailureBeforeCommitLeavesNoOutboxEvent(t *testing.T) {
	ctx := context.Background()
	hooks := &committedKeys{}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithCommitted(hooks.record))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	injected := errors.New("injected fault")
	postgres.SetBeforeCommit(store, func(context.Context) error { return injected })
	if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-1"}, createText); !errors.Is(err, injected) {
		t.Fatalf("Apply with fault error = %v, want injected", err)
	}
	if records := mustPending(t, events, key); len(records) != 0 {
		t.Fatalf("Pending after a failed commit = %#v, want none", records)
	}
	loaded, err := store.Load(ctx, key)
	if err != nil || loaded.Revision != 0 || len(loaded.Objects) != 0 {
		t.Fatalf("canvas after a failed commit = %+v, %v; want revision 0 and no objects", loaded, err)
	}
	if got := len(hooks.list()); got != 0 {
		t.Fatalf("commit hook ran %d times after a failed commit", got)
	}

	postgres.SetBeforeCommit(store, nil)
	result := mustApply(t, store, key, canvas.Operation{ID: "op-1"}, createText)
	records := mustPending(t, events, key)
	if len(records) != 1 || records[0].Event.Result != result {
		t.Fatalf("Pending after retry = %#v, want one event with %+v", records, result)
	}
}

func TestOutboxPendingIsScopedToTheCanvasKey(t *testing.T) {
	ctx := context.Background()
	store, pool := openStore(t, pgtest.Start(t))
	events := postgres.NewOutboxStore(pool)
	a := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	b := canvas.Key{TenantID: "tenant-b", CanvasID: "main"}
	for _, key := range []canvas.Key{a, b} {
		if _, err := store.Create(ctx, key); err != nil {
			t.Fatalf("Create(%v): %v", key, err)
		}
	}
	var wg sync.WaitGroup
	for _, key := range []canvas.Key{a, b} {
		for i := range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-" + string(rune('a'+i))}, createText); err != nil {
					t.Errorf("Apply(%v): %v", key, err)
				}
			}()
		}
	}
	wg.Wait()
	for _, key := range []canvas.Key{a, b} {
		records := mustPending(t, events, key)
		if len(records) != 5 {
			t.Fatalf("Pending(%v) returned %d records, want 5", key, len(records))
		}
		for i, record := range records {
			if record.Event.Key != key || record.Event.Result.Revision != int64(i+1) {
				t.Fatalf("Pending(%v) record %d = key %v revision %d", key, i, record.Event.Key, record.Event.Result.Revision)
			}
		}
	}
	if records := mustPending(t, events, canvas.Key{TenantID: "tenant-c", CanvasID: "main"}); len(records) != 0 {
		t.Fatalf("Pending for an unknown tenant = %#v, want none", records)
	}
	if _, err := events.Pending(ctx, canvas.Key{TenantID: "bad tenant", CanvasID: "main"}, 10); !errors.Is(err, canvas.ErrInvalid) {
		t.Fatalf("Pending with a malformed key error = %v, want ErrInvalid", err)
	}
	if records, err := events.Pending(ctx, a, 2); err != nil || len(records) != 2 || records[1].Event.Result.Revision != 2 {
		t.Fatalf("Pending limit 2 = %#v, %v; want the two oldest", records, err)
	}
}

func mustHeads(t *testing.T, store *postgres.OutboxStore, limit int) []outbox.Record {
	t.Helper()
	records, err := store.Heads(context.Background(), limit)
	if err != nil {
		t.Fatalf("Heads(%d): %v", limit, err)
	}
	return records
}

func mustStats(t *testing.T, store *postgres.OutboxStore, tenantID string) outbox.Stats {
	t.Helper()
	stats, err := store.Stats(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Stats(%q): %v", tenantID, err)
	}
	stats.OldestPending = stats.OldestPending.UTC()
	return stats
}

func mustAllStats(t *testing.T, store *postgres.OutboxStore) outbox.Stats {
	t.Helper()
	stats, err := store.AllStats(context.Background())
	if err != nil {
		t.Fatalf("AllStats: %v", err)
	}
	stats.OldestPending = stats.OldestPending.UTC()
	return stats
}

// headIdentity is the canvas and revision of a record.
type headIdentity struct {
	key      canvas.Key
	revision int64
}

func identities(records []outbox.Record) []headIdentity {
	out := make([]headIdentity, 0, len(records))
	for _, record := range records {
		out = append(out, headIdentity{record.Event.Key, record.Event.Result.Revision})
	}
	return out
}

// utc returns records with their times in UTC, so equal instants compare equal.
func utc(records []outbox.Record) []outbox.Record {
	out := make([]outbox.Record, 0, len(records))
	for _, record := range records {
		record.CreatedAt, record.NextAttempt = record.CreatedAt.UTC(), record.NextAttempt.UTC()
		out = append(out, record)
	}
	return out
}

func sameRecords(t *testing.T, label string, got, want []outbox.Record) {
	t.Helper()
	if !reflect.DeepEqual(utc(got), utc(want)) {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
}

func TestOutboxHeadsReturnsTheEarliestDueHeadOfEachCanvas(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	now := &clock{now: t0}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(now.Now))
	events := postgres.NewOutboxStore(pool)
	a1 := canvas.Key{TenantID: "tenant-a", CanvasID: "one"}
	a2 := canvas.Key{TenantID: "tenant-a", CanvasID: "two"}
	a3 := canvas.Key{TenantID: "tenant-a", CanvasID: "three"}
	b1 := canvas.Key{TenantID: "tenant-b", CanvasID: "one"}
	for _, key := range []canvas.Key{a1, a2, a3, b1} {
		mustCreate(t, store, key)
	}
	// Records are created one second apart: a1 r1, a1 r2, a2 r1, a3 r1, b1 r1.
	for i, key := range []canvas.Key{a1, a1, a2, a3, b1} {
		now.Set(t0.Add(time.Duration(i) * time.Second))
		mustApply(t, store, key, canvas.Operation{ID: "op-" + string(rune('a'+i))}, createText)
	}
	headOf := func(key canvas.Key) outbox.Record {
		return mustPending(t, events, key)[0]
	}

	all := mustHeads(t, events, 10)
	sameRecords(t, "Heads(10)", all, []outbox.Record{headOf(a1), headOf(a2), headOf(a3), headOf(b1)})
	if got := identities(all)[0]; got != (headIdentity{a1, 1}) {
		t.Fatalf("first head = %v, want a1 revision 1", got)
	}
	sameRecords(t, "Heads(0) clamps to one", mustHeads(t, events, 0), all[:1])

	// The head of a1 backs off: the other canvases come first and a1's later
	// record stays hidden behind its head.
	if err := events.Retry(ctx, all[0].ID, 1, t0.Add(time.Minute), "boom"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	backedOff := all[0]
	backedOff.Attempts, backedOff.NextAttempt, backedOff.LastError = 1, t0.Add(time.Minute), "boom"
	sameRecords(t, "Heads(2) after backoff", mustHeads(t, events, 2), []outbox.Record{headOf(a2), headOf(a3)})
	sameRecords(t, "Heads(10) after backoff", mustHeads(t, events, 10), []outbox.Record{headOf(a2), headOf(a3), headOf(b1), backedOff})

	// Settling a head exposes the next record of its canvas.
	if err := events.Settle(ctx, backedOff.ID, outbox.OutcomeDelivered, t0.Add(time.Hour), ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	heads := mustHeads(t, events, 10)
	if got, want := identities(heads), []headIdentity{{a1, 2}, {a2, 1}, {a3, 1}, {b1, 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Heads after settling a1 r1 = %v, want %v", got, want)
	}
	sameRecords(t, "a1 head after settle", heads[:1], []outbox.Record{headOf(a1)})
}

func TestOutboxSettleIsFinalAndRejectsUnknownOutcomes(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(func() time.Time { return t0 }))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	mustCreate(t, store, key)
	for _, id := range []string{"op-1", "op-2", "op-3"} {
		mustApply(t, store, key, canvas.Operation{ID: id}, createText)
	}
	before := mustPending(t, events, key)
	initial := outbox.Stats{Pending: 3, OldestPending: t0}

	for _, outcome := range []outbox.Outcome{"", "pending", "DELIVERED"} {
		if err := events.Settle(ctx, before[0].ID, outcome, t0, "x"); err == nil {
			t.Fatalf("Settle with outcome %q succeeded, want an error", outcome)
		}
	}
	sameRecords(t, "Pending after rejected outcomes", mustPending(t, events, key), before)
	if got := mustStats(t, events, key.TenantID); got != initial {
		t.Fatalf("Stats after rejected outcomes = %+v, want %+v", got, initial)
	}

	if err := events.Settle(ctx, before[0].ID, outbox.OutcomeDelivered, t0.Add(time.Second), ""); err != nil {
		t.Fatalf("Settle delivered: %v", err)
	}
	sameRecords(t, "Pending after delivery", mustPending(t, events, key), before[1:])
	afterDelivery := outbox.Stats{Pending: 2, OldestPending: t0}
	if got := mustStats(t, events, key.TenantID); got != afterDelivery {
		t.Fatalf("Stats after delivery = %+v, want %+v", got, afterDelivery)
	}

	// A settled record ignores later settles and retries.
	if err := events.Settle(ctx, before[0].ID, outbox.OutcomeRejected, t0.Add(2*time.Second), "late"); err != nil {
		t.Fatalf("Settle of a settled record: %v", err)
	}
	if err := events.Retry(ctx, before[0].ID, 4, t0.Add(time.Hour), "late"); err != nil {
		t.Fatalf("Retry of a settled record: %v", err)
	}
	sameRecords(t, "Pending after settling a settled record", mustPending(t, events, key), before[1:])
	if got := mustStats(t, events, key.TenantID); got != afterDelivery {
		t.Fatalf("Stats after settling a settled record = %+v, want %+v", got, afterDelivery)
	}

	if err := events.Settle(ctx, before[1].ID, outbox.OutcomeRejected, t0.Add(3*time.Second), "undeliverable"); err != nil {
		t.Fatalf("Settle rejected: %v", err)
	}
	sameRecords(t, "Pending after rejection", mustPending(t, events, key), before[2:])
	if got, want := mustStats(t, events, key.TenantID), (outbox.Stats{Pending: 1, OldestPending: t0, Rejected: 1}); got != want {
		t.Fatalf("Stats after rejection = %+v, want %+v", got, want)
	}
	if err := events.Settle(ctx, 1<<40, outbox.OutcomeDelivered, t0, ""); err != nil {
		t.Fatalf("Settle of an unknown record: %v", err)
	}
}

func TestOutboxRetryRecordsFailuresOfPendingRecords(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(func() time.Time { return t0 }))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	mustCreate(t, store, key)
	mustApply(t, store, key, canvas.Operation{ID: "op-1"}, createText)
	mustApply(t, store, key, canvas.Operation{ID: "op-2"}, createText)
	before := mustPending(t, events, key)

	if err := events.Retry(ctx, before[0].ID, 1, t0.Add(time.Second), "first"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if err := events.Retry(ctx, before[0].ID, 3, t0.Add(4*time.Second), "third"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if err := events.Retry(ctx, before[1].ID, 2, t0.Add(2*time.Second), "second"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	want := append([]outbox.Record(nil), before...)
	want[0].Attempts, want[0].NextAttempt, want[0].LastError = 3, t0.Add(4*time.Second), "third"
	want[1].Attempts, want[1].NextAttempt, want[1].LastError = 2, t0.Add(2*time.Second), "second"
	sameRecords(t, "Pending after retries", mustPending(t, events, key), want)
	if got, wantStats := mustStats(t, events, key.TenantID), (outbox.Stats{Pending: 2, OldestPending: t0, Retrying: 2, PendingAttempts: 5}); got != wantStats {
		t.Fatalf("Stats after retries = %+v, want %+v", got, wantStats)
	}

	if err := events.Retry(ctx, before[0].ID, -1, t0, "negative"); err == nil {
		t.Fatal("Retry with negative attempts succeeded, want an error")
	}
	sameRecords(t, "Pending after a rejected retry", mustPending(t, events, key), want)
}

func TestOutboxPurgeDeletesOnlyOldSettledRecordsInBatches(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(func() time.Time { return t0 }))
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	mustCreate(t, store, key)
	for _, id := range []string{"op-1", "op-2", "op-3"} {
		mustApply(t, store, key, canvas.Operation{ID: id}, createText)
	}
	records := mustPending(t, events, key)
	// The rejected record settles first, so the oldest-first purge removes it first.
	if err := events.Settle(ctx, records[1].ID, outbox.OutcomeRejected, t0.Add(time.Second), "bad"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := events.Settle(ctx, records[0].ID, outbox.OutcomeDelivered, t0.Add(2*time.Second), ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	pendingOnly := outbox.Stats{Pending: 1, OldestPending: t0}

	purge := func(cutoff time.Time, limit, want int) {
		t.Helper()
		if got, err := events.Purge(ctx, cutoff, limit); err != nil || got != want {
			t.Fatalf("Purge(%v, %d) = %d, %v; want %d", cutoff, limit, got, err, want)
		}
	}
	purge(t0.Add(time.Second-time.Nanosecond), 10, 0)
	if got, want := mustStats(t, events, key.TenantID), (outbox.Stats{Pending: 1, OldestPending: t0, Rejected: 1}); got != want {
		t.Fatalf("Stats before the cutoff = %+v, want %+v", got, want)
	}
	purge(t0.Add(2*time.Second), 1, 1)
	if got := mustStats(t, events, key.TenantID); got != pendingOnly {
		t.Fatalf("Stats after purging the oldest = %+v, want %+v (the rejected record first)", got, pendingOnly)
	}
	purge(t0.Add(2*time.Second), 1, 1)
	purge(t0.Add(2*time.Second), 1, 0)
	purge(t0.Add(24*time.Hour), 10, 0)
	sameRecords(t, "Pending after purges", mustPending(t, events, key), records[2:])
	if got := mustStats(t, events, key.TenantID); got != pendingOnly {
		t.Fatalf("Stats after purges = %+v, want %+v", got, pendingOnly)
	}
}

func TestOutboxStatsRejectMalformedTenants(t *testing.T) {
	ctx := context.Background()
	_, pool := openStore(t, pgtest.Start(t))
	events := postgres.NewOutboxStore(pool)
	for _, tenant := range []string{"", "bad tenant", "tenant\n", strings.Repeat("x", canvas.MaxIdentifierBytes+1)} {
		if stats, err := events.Stats(ctx, tenant); !errors.Is(err, canvas.ErrInvalid) || stats != (outbox.Stats{}) {
			t.Fatalf("Stats(%q) = %+v, %v; want zero stats and ErrInvalid", tenant, stats, err)
		}
	}
	if got := mustStats(t, events, "tenant-unknown"); got != (outbox.Stats{}) {
		t.Fatalf("Stats of an unknown tenant = %+v, want zero", got)
	}
	if got := mustAllStats(t, events); got != (outbox.Stats{}) {
		t.Fatalf("AllStats of an empty outbox = %+v, want zero", got)
	}
}

func TestOutboxStatsAndPendingAreScopedToOneTenant(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	now := &clock{now: t0}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(now.Now))
	events := postgres.NewOutboxStore(pool)
	a := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	b := canvas.Key{TenantID: "tenant-b", CanvasID: "main"}
	mustCreate(t, store, a)
	mustCreate(t, store, b)
	// tenant-b writes first, so its records are the oldest overall.
	now.Set(t0.Add(-time.Hour))
	mustApply(t, store, b, canvas.Operation{ID: "op-early"}, createText)
	now.Set(t0)

	counts := map[canvas.Key]int{a: 5, b: 3}
	var wg sync.WaitGroup
	for key, n := range counts {
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-" + string(rune('a'+i))}, createText); err != nil {
					t.Errorf("Apply(%v): %v", key, err)
				}
			}()
		}
	}
	wg.Wait()

	pendingA, pendingB := mustPending(t, events, a), mustPending(t, events, b)
	if len(pendingA) != 5 || len(pendingB) != 4 {
		t.Fatalf("Pending counts = %d, %d; want 5, 4", len(pendingA), len(pendingB))
	}
	for key, records := range map[canvas.Key][]outbox.Record{a: pendingA, b: pendingB} {
		for _, record := range records {
			if record.Event.Key != key {
				t.Fatalf("Pending(%v) returned a record of %v", key, record.Event.Key)
			}
		}
	}

	// Activity in one tenant never shows in the other's stats.
	if err := events.Retry(ctx, pendingA[0].ID, 2, t0.Add(time.Minute), "a failed"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if err := events.Settle(ctx, pendingB[1].ID, outbox.OutcomeRejected, t0, "b rejected"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	wantA := outbox.Stats{Pending: 5, OldestPending: t0, Retrying: 1, PendingAttempts: 2}
	wantB := outbox.Stats{Pending: 3, OldestPending: t0.Add(-time.Hour), Rejected: 1}
	if got := mustStats(t, events, a.TenantID); got != wantA {
		t.Fatalf("Stats(tenant-a) = %+v, want %+v", got, wantA)
	}
	if got := mustStats(t, events, b.TenantID); got != wantB {
		t.Fatalf("Stats(tenant-b) = %+v, want %+v", got, wantB)
	}
	wantAll := outbox.Stats{Pending: 8, OldestPending: t0.Add(-time.Hour), Retrying: 1, PendingAttempts: 2, Rejected: 1}
	if got := mustAllStats(t, events); got != wantAll {
		t.Fatalf("AllStats = %+v, want %+v", got, wantAll)
	}
	if got := len(mustPending(t, events, b)); got != 3 {
		t.Fatalf("Pending(tenant-b) after the rejection = %d records, want 3", got)
	}
}
