package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

var createText = canvas.CreateObject{Kind: canvas.KindText, Content: "once", Transform: canvas.Transform{X: 3, Y: 4, Width: 50, Height: 20, Rotation: 7.5}}

// clock is a settable time source for retention tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// operationRows counts the retained operations of one canvas.
func operationRows(t *testing.T, pool *pgxpool.Pool, key canvas.Key) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM canvas_operations WHERE tenant_id = $1 AND canvas_id = $2", key.TenantID, key.CanvasID).Scan(&rows); err != nil {
		t.Fatalf("count operations: %v", err)
	}
	return rows
}

func mustApply(t *testing.T, store canvas.Store, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) canvas.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, operation, mutation)
	if err != nil {
		t.Fatalf("Apply(%s, %T): %v", operation.ID, mutation, err)
	}
	return result
}

func TestFailureBeforeCommitLeavesNoTraceAndRetryAppliesOnce(t *testing.T) {
	injected := errors.New("injected fault")
	faults := map[string]struct {
		hook  func(context.Context) error
		ctx   func() (context.Context, context.CancelFunc)
		check func(error) bool
	}{
		"hook error": {
			hook:  func(context.Context) error { return injected },
			ctx:   func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			check: func(err error) bool { return errors.Is(err, injected) },
		},
		"context deadline": {
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
	for name, fault := range faults {
		t.Run(name, func(t *testing.T) {
			store, pool := openStore(t, pgtest.Start(t))
			mustCreate(t, store, tenantA)
			want := mustLoad(t, store, tenantA)
			operation := nextOperation(t)

			postgres.SetBeforeCommit(store, fault.hook)
			ctx, cancel := fault.ctx()
			_, err := store.Apply(ctx, tenantA, operation, createText)
			cancel()
			if !fault.check(err) {
				t.Fatalf("Apply with fault before commit: error = %v", err)
			}
			if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, want) {
				t.Fatalf("failed commit changed the canvas:\n got %+v\nwant %+v", got, want)
			}
			if rows := operationRows(t, pool, tenantA); rows != 0 {
				t.Fatalf("failed commit retained %d operations, want 0", rows)
			}

			postgres.SetBeforeCommit(store, nil)
			first := mustApply(t, store, tenantA, operation, createText)
			second := mustApply(t, store, tenantA, operation, createText)
			got := mustLoad(t, store, tenantA)
			if first != second || got.Revision != want.Revision+1 || len(got.Objects) != 1 || got.Objects[0].ID != first.ObjectID {
				t.Fatalf("retry after fault: first %+v, second %+v, canvas %+v; want one effect", first, second, got)
			}
		})
	}
}

func TestLostAnswerAfterCommitReplaysTheSameResultAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	store, pool := openStore(t, server)
	mustCreate(t, store, tenantA)
	text := apply(t, store, tenantA, createText)

	retries := []struct {
		name     string
		mutation canvas.Mutation
	}{
		{"dimensions", canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}}},
		{"create", canvas.CreateObject{Kind: canvas.KindImage, Content: "asset-1", Transform: canvas.Transform{Width: 8, Height: 8}}},
		{"content", canvas.SetContent{ObjectID: text.ObjectID, Content: "edited"}},
		{"transform", canvas.SetTransform{ObjectID: text.ObjectID, Transform: canvas.Transform{X: 1, Width: 9, Height: 9}}},
		{"delete", canvas.DeleteObject{ObjectID: text.ObjectID}},
	}
	for _, retry := range retries {
		operation := nextOperation(t)
		// The committed answer is lost on the way back; the client only
		// keeps it here to compare with the replay.
		lost := mustApply(t, store, tenantA, operation, retry.mutation)
		want := mustLoad(t, store, tenantA)
		retained := operationRows(t, pool, tenantA)

		server.Restart(t)
		store, pool = openStore(t, server)

		// The retry arrives over a new connection with a new correlation ID.
		reconnected := canvas.Operation{ID: operation.ID, CorrelationID: "fedcba9876543210fedcba9876543210"}
		if got := mustApply(t, store, tenantA, reconnected, retry.mutation); got != lost {
			t.Fatalf("%s retry after restart = %+v, want the committed %+v", retry.name, got, lost)
		}
		if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s retry produced a second effect:\n got %+v\nwant %+v", retry.name, got, want)
		}
		if rows := operationRows(t, pool, tenantA); rows != retained {
			t.Fatalf("%s retry retained %d operations, want %d", retry.name, rows, retained)
		}
	}
}

func TestConcurrentDuplicatesProduceOneEffect(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	operation := nextOperation(t)

	var wg sync.WaitGroup
	results := make([]canvas.Result, 16)
	for i := range results {
		wg.Go(func() {
			result, err := store.Apply(context.Background(), tenantA, operation, createText)
			if err != nil {
				t.Errorf("concurrent duplicate Apply: %v", err)
			}
			results[i] = result
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	got := mustLoad(t, store, tenantA)
	if got.Revision != 1 || len(got.Objects) != 1 || operationRows(t, pool, tenantA) != 1 {
		t.Fatalf("duplicates produced canvas %+v, want one object at revision 1 and one operation", got)
	}
	for i, result := range results {
		if result != results[0] || result.ObjectID != got.Objects[0].ID {
			t.Fatalf("duplicate %d result = %+v, want %+v for object %s", i, result, results[0], got.Objects[0].ID)
		}
	}
}

func TestReusedOperationIDWithADifferentMutationIsRejected(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	operation := nextOperation(t)
	created := mustApply(t, store, tenantA, operation, createText)
	want := mustLoad(t, store, tenantA)

	for _, mutation := range []canvas.Mutation{
		canvas.CreateObject{Kind: createText.Kind, Content: "twice", Transform: createText.Transform},
		canvas.SetContent{ObjectID: created.ObjectID, Content: "once"},
		canvas.DeleteObject{ObjectID: created.ObjectID},
	} {
		if _, err := store.Apply(context.Background(), tenantA, operation, mutation); !errors.Is(err, canvas.ErrOperationConflict) {
			t.Fatalf("%T under a used operation ID: error = %v, want ErrOperationConflict", mutation, err)
		}
	}
	if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicting mutations changed the canvas:\n got %+v\nwant %+v", got, want)
	}
	if rows := operationRows(t, pool, tenantA); rows != 1 {
		t.Fatalf("conflicts retained %d operations, want 1", rows)
	}
	if replay := mustApply(t, store, tenantA, operation, createText); replay != created {
		t.Fatalf("original retry after conflicts = %+v, want %+v", replay, created)
	}

	// Operation IDs are scoped to their canvas.
	tenantB := canvas.Key{TenantID: "tenant-b", CanvasID: tenantA.CanvasID}
	mustCreate(t, store, tenantB)
	if other := mustApply(t, store, tenantB, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}}); other.Revision != 1 {
		t.Fatalf("same operation ID in another tenant = %+v, want its own first revision", other)
	}
}

func TestRejectedMutationsAreNotRetained(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	operation := nextOperation(t)

	if _, err := store.Apply(context.Background(), tenantA, operation, canvas.DeleteObject{ObjectID: "missing"}); !errors.Is(err, canvas.ErrNotFound) {
		t.Fatalf("delete of a missing object: error = %v, want ErrNotFound", err)
	}
	if _, err := store.Apply(context.Background(), tenantA, operation, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1, Height: 1}}); !errors.Is(err, canvas.ErrInvalid) {
		t.Fatalf("invalid dimensions: error = %v, want ErrInvalid", err)
	}
	if rows := operationRows(t, pool, tenantA); rows != 0 {
		t.Fatalf("rejected mutations retained %d operations, want 0", rows)
	}
	if result := mustApply(t, store, tenantA, operation, createText); result.Revision != 1 {
		t.Fatalf("first accepted mutation under the rejected ID = %+v, want revision 1", result)
	}
}

func TestRetentionExpiresOperations(t *testing.T) {
	const ttl = time.Minute
	start := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	now := &clock{now: start}
	store, pool := openStore(t, pgtest.Start(t), postgres.WithClock(now.Now), postgres.WithRetention(ttl))
	mustCreate(t, store, tenantA)
	reused, other := nextOperation(t), nextOperation(t)
	first := mustApply(t, store, tenantA, reused, createText)
	mustApply(t, store, tenantA, other, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}})

	now.Set(start.Add(ttl - time.Microsecond))
	if replay := mustApply(t, store, tenantA, reused, createText); replay != first {
		t.Fatalf("retry just before expiry = %+v, want %+v", replay, first)
	}
	if rows := operationRows(t, pool, tenantA); rows != 2 {
		t.Fatalf("operations before expiry = %d, want 2", rows)
	}

	now.Set(start.Add(ttl))
	again := mustApply(t, store, tenantA, reused, createText)
	got := mustLoad(t, store, tenantA)
	if again.Revision != 3 || again.ObjectID == first.ObjectID || len(got.Objects) != 2 {
		t.Fatalf("expired operation ID = %+v with canvas %+v, want a new effect at revision 3", again, got)
	}
	var acceptedAt time.Time
	if err := pool.QueryRow(context.Background(), "SELECT accepted_at FROM canvas_operations WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3", tenantA.TenantID, tenantA.CanvasID, reused.ID).Scan(&acceptedAt); err != nil || !acceptedAt.Equal(start.Add(ttl)) {
		t.Fatalf("reused operation accepted_at = %v (err %v), want %v", acceptedAt, err, start.Add(ttl))
	}
	if rows := operationRows(t, pool, tenantA); rows != 1 {
		t.Fatalf("operations after expiry = %d, want only the new one", rows)
	}
}
