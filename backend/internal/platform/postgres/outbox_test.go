package postgres_test

import (
	"context"
	"errors"
	"reflect"
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
