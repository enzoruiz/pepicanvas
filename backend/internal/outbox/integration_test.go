package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

// openPG connects one process to the current server address and migrates it.
func openPG(t *testing.T, server *pgtest.Server, options ...postgres.CanvasStoreOption) (*postgres.CanvasStore, *postgres.OutboxStore, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return postgres.NewCanvasStore(pool, options...), postgres.NewOutboxStore(pool), pool
}

func mustCommit(t *testing.T, store canvas.Store, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) canvas.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, operation, mutation)
	if err != nil {
		t.Fatalf("Apply(%v, %T): %v", key, mutation, err)
	}
	return result
}

// commitEveryKind commits one mutation of each kind with correlation IDs.
func commitEveryKind(t *testing.T, store canvas.Store, key canvas.Key) {
	t.Helper()
	if _, err := store.Create(context.Background(), key); err != nil {
		t.Fatalf("Create(%v): %v", key, err)
	}
	created := mustCommit(t, store, key, canvas.Operation{ID: "op-1", CorrelationID: "corr-1"},
		canvas.CreateObject{Kind: canvas.KindText, Content: "hi", Transform: canvas.Transform{Width: 10, Height: 10}})
	mustCommit(t, store, key, canvas.Operation{ID: "op-2", CorrelationID: "corr-2"}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}})
	mustCommit(t, store, key, canvas.Operation{ID: "op-3", CorrelationID: "corr-3"}, canvas.SetContent{ObjectID: created.ObjectID, Content: "twice"})
	mustCommit(t, store, key, canvas.Operation{ID: "op-4", CorrelationID: "corr-4"}, canvas.SetTransform{ObjectID: created.ObjectID, Transform: canvas.Transform{X: 3, Width: 5, Height: 5}})
	mustCommit(t, store, key, canvas.Operation{ID: "op-5", CorrelationID: "corr-5"}, canvas.DeleteObject{ObjectID: created.ObjectID})
}

func mustStats(t *testing.T, events *postgres.OutboxStore, tenantID string) outbox.Stats {
	t.Helper()
	stats, err := events.Stats(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Stats(%s): %v", tenantID, err)
	}
	return stats
}

func mustPendingRecords(t *testing.T, events *postgres.OutboxStore, key canvas.Key) []outbox.Record {
	t.Helper()
	records, err := events.Pending(context.Background(), key, 100)
	if err != nil {
		t.Fatalf("Pending(%v): %v", key, err)
	}
	return records
}

// payloadRevision returns the revision a set event carries in its payload,
// or 0 for a delete, which carries no payload.
func payloadRevision(t *testing.T, event realtime.Event) int64 {
	t.Helper()
	if event.Kind == realtime.KindDelete {
		return 0
	}
	var payload struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("event %d payload %s: %v", event.Sequence, event.Payload, err)
	}
	return payload.Revision
}

// assertDeliveredInOrder checks that events are exactly revisions first..last
// in revision order, each once: operation IDs name increasing outbox records,
// and set events carry their revision.
func assertDeliveredInOrder(t *testing.T, events []realtime.Event, first, last int64) {
	t.Helper()
	if want := int(last - first + 1); len(events) != want {
		t.Fatalf("hub has %d events, want %d: %+v", len(events), want, events)
	}
	var previous int64
	for i, event := range events {
		revision := first + int64(i)
		var id int64
		if _, err := fmt.Sscanf(event.OperationID, "outbox-%d", &id); err != nil || id <= previous {
			t.Fatalf("event %d operation %s, want an outbox record after %d", i, event.OperationID, previous)
		}
		previous = id
		if got := payloadRevision(t, event); event.Kind == realtime.KindSet && got != revision {
			t.Fatalf("event %d payload revision %d, want %d", i, got, revision)
		}
	}
}

func allowAll(context.Context, realtime.Command) error { return nil }

// submitOnly is an actor without resync support.
type submitOnly struct{ hub *realtime.Hub }

func (a submitOnly) Submit(ctx context.Context, command realtime.Command) (realtime.Result, error) {
	return a.hub.Submit(ctx, command)
}

func TestCrashBetweenCommitAndPublishIsDeliveredAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	now := &clock{now: t0}
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}

	// The crashed process committed every mutation and published none.
	store, events, pool := openPG(t, server, postgres.WithClock(now.Now))
	crashed := newHub(t, realtime.Config{Authorize: allowAll})
	commitEveryKind(t, store, key)
	if got := hubEvents(t, crashed, key); len(got) != 0 {
		t.Fatalf("hub confirmed %+v before publication", got)
	}
	now.Advance(5 * time.Second)
	stats := mustStats(t, events, key.TenantID)
	if stats.Pending != 5 || !stats.OldestPending.Equal(t0) || stats.Age(now.Now()) != 5*time.Second {
		t.Fatalf("Stats before restart = %+v age %v, want 5 pending since %v, age 5s", stats, stats.Age(now.Now()), t0)
	}
	pool.Close()

	server.Restart(t)
	_, events, _ = openPG(t, server)
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher := start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), Now: now.Now, PollInterval: 10 * time.Millisecond})
	// The publisher settles a record before it counts it, so wait for both.
	eventually(t, "every record to settle and be counted", func() bool {
		return mustStats(t, events, key.TenantID).Pending == 0 && publisher.Counters().Delivered == 5
	})

	delivered := hubEvents(t, hub, key)
	assertDeliveredInOrder(t, delivered, 1, 5)
	for i, event := range delivered {
		if want := fmt.Sprintf("corr-%d", i+1); event.CorrelationID != want {
			t.Fatalf("event %d correlation = %q, want %q", i, event.CorrelationID, want)
		}
	}
	if counters := publisher.Counters(); counters.Delivered != 5 || counters.Rejected != 0 {
		t.Fatalf("Counters = %+v, want 5 delivered", counters)
	}
	if stats := mustStats(t, events, key.TenantID); stats.Pending != 0 || stats.Age(now.Now()) != 0 {
		t.Fatalf("Stats after delivery = %+v", stats)
	}
	if pending := mustPendingRecords(t, events, key); len(pending) != 0 {
		t.Fatalf("Pending after delivery = %+v", pending)
	}
	if got := hubEvents(t, crashed, key); len(got) != 0 {
		t.Fatalf("crashed hub confirmed %+v", got)
	}
}

func TestCrashDuringPublishDeliversTheRestAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	store, events, pool := openPG(t, server)
	if _, err := store.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	created := mustCommit(t, store, key, canvas.Operation{ID: "op-1"}, canvas.CreateObject{Kind: canvas.KindText, Content: "0", Transform: canvas.Transform{Width: 1, Height: 1}})
	for i := 2; i <= 6; i++ {
		mustCommit(t, store, key, canvas.Operation{ID: fmt.Sprintf("op-%d", i)}, canvas.SetContent{ObjectID: created.ObjectID, Content: fmt.Sprint(i)})
	}

	// The first process stops while the delivery of revision 4 hangs.
	blocked := make(chan struct{})
	first := newHub(t, realtime.Config{Authorize: func(ctx context.Context, command realtime.Command) error {
		if command.OperationID == "outbox-4" { // revision 4 of the only canvas
			close(blocked)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}})
	// submitOnly hides RequireResync, so the crashed hub's log stays readable.
	publisher := start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(submitOnly{first}), PollInterval: 10 * time.Millisecond, AttemptTimeout: time.Hour})
	<-blocked
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := publisher.Close(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}
	assertDeliveredInOrder(t, hubEvents(t, first, key), 1, 3)
	pending := mustPendingRecords(t, events, key)
	if len(pending) != 3 || pending[0].Event.Result.Revision != 4 || pending[0].Attempts != 0 {
		t.Fatalf("Pending after the crash = %+v, want revisions 4..6 untouched", pending)
	}
	pool.Close()

	server.Restart(t)
	_, events, _ = openPG(t, server)
	second := newHub(t, realtime.Config{Authorize: allowAll})
	start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(second), PollInterval: 10 * time.Millisecond})
	eventually(t, "the remaining records to settle", func() bool { return mustStats(t, events, key.TenantID).Pending == 0 })
	assertDeliveredInOrder(t, hubEvents(t, second, key), 4, 6)
}

func TestFailureBeforeCommitPublishesNothing(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	var publisher *outbox.Publisher
	store, events, pool := openPG(t, server, postgres.WithCommitted(func(canvas.Key) { publisher.Notify() }))
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher = start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A deferred constraint trigger fails the transaction at COMMIT, after
	// the mutation and its outbox record were written.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_outbox_commit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_outbox_commit AFTER INSERT ON canvas_outbox
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_outbox_commit();`); err != nil {
		t.Fatalf("install commit fault: %v", err)
	}
	lost := canvas.CreateObject{Kind: canvas.KindText, Content: "lost", Transform: canvas.Transform{Width: 1, Height: 1}}
	if _, err := store.Apply(ctx, key, canvas.Operation{ID: "op-1"}, lost); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("Apply with a commit fault = %v, want the injected failure", err)
	}
	if pending := mustPendingRecords(t, events, key); len(pending) != 0 {
		t.Fatalf("Pending after a failed commit = %+v", pending)
	}
	if stats := mustStats(t, events, key.TenantID); stats.Pending != 0 {
		t.Fatalf("Stats after a failed commit = %+v", stats)
	}
	if loaded, err := store.Load(ctx, key); err != nil || loaded.Revision != 0 || len(loaded.Objects) != 0 {
		t.Fatalf("canvas after a failed commit = %+v, %v", loaded, err)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_outbox_commit ON canvas_outbox`); err != nil {
		t.Fatalf("remove commit fault: %v", err)
	}
	kept := mustCommit(t, store, key, canvas.Operation{ID: "op-2"}, canvas.CreateObject{Kind: canvas.KindText, Content: "kept", Transform: canvas.Transform{Width: 1, Height: 1}})
	eventually(t, "the committed record to settle", func() bool { return mustStats(t, events, key.TenantID).Pending == 0 })
	// The publisher delivers in revision order, so only the committed record
	// can have reached the hub.
	delivered := hubEvents(t, hub, key)
	assertDeliveredInOrder(t, delivered, 1, 1)
	if delivered[0].ObjectID != kept.ObjectID {
		t.Fatalf("hub event object = %s, want the committed %s", delivered[0].ObjectID, kept.ObjectID)
	}
}

// firstHeads signals when the publisher's first read of pending records ended.
type firstHeads struct {
	*postgres.OutboxStore
	once sync.Once
	done chan struct{}
}

func (s *firstHeads) Heads(ctx context.Context, limit int) ([]outbox.Record, error) {
	defer s.once.Do(func() { close(s.done) })
	return s.OutboxStore.Heads(ctx, limit)
}

func TestConcurrentTenantsReceiveOnlyTheirOwnEvents(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	var publisher *outbox.Publisher
	store, events, _ := openPG(t, server, postgres.WithCommitted(func(canvas.Key) { publisher.Notify() }))
	keys := []canvas.Key{canvasA, canvasB}
	for _, key := range keys {
		if _, err := store.Create(ctx, key); err != nil {
			t.Fatalf("Create(%v): %v", key, err)
		}
	}
	commands := &seen{}
	hub := newHub(t, realtime.Config{Authorize: commands.authorize})
	observed := &firstHeads{OutboxStore: events, done: make(chan struct{})}
	// An hour of polling: every delivery after the first pass is woken by a commit.
	publisher = start(t, outbox.Config{Store: observed, Sink: outbox.NewHubSink(hub), PollInterval: time.Hour})
	<-observed.done

	const perTenant = 10
	var mu sync.Mutex
	objects := map[string]canvas.Key{}
	var wg sync.WaitGroup
	for _, key := range keys {
		for i := range perTenant {
			wg.Go(func() {
				operation := canvas.Operation{ID: fmt.Sprintf("op-%d", i), CorrelationID: fmt.Sprintf("%s-%d", key.TenantID, i)}
				result, err := store.Apply(ctx, key, operation, canvas.CreateObject{Kind: canvas.KindText, Content: "x", Transform: canvas.Transform{Width: 1, Height: 1}})
				if err != nil {
					t.Errorf("Apply(%v): %v", key, err)
					return
				}
				mu.Lock()
				objects[result.ObjectID] = key
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	eventually(t, "both tenants to drain", func() bool {
		return len(hubEvents(t, hub, canvasA)) == perTenant && len(hubEvents(t, hub, canvasB)) == perTenant
	})

	for _, key := range keys {
		delivered := hubEvents(t, hub, key)
		assertDeliveredInOrder(t, delivered, 1, perTenant)
		for _, event := range delivered {
			if objects[event.ObjectID] != key || !strings.HasPrefix(event.CorrelationID, key.TenantID+"-") {
				t.Fatalf("%v received event %+v of %v", key, event, objects[event.ObjectID])
			}
		}
	}
	for _, command := range commands.list() {
		key := canvas.Key{TenantID: command.Key.TenantID, CanvasID: command.Key.CanvasID}
		if objects[command.ObjectID] != key || !strings.HasPrefix(command.CorrelationID, key.TenantID+"-") {
			t.Fatalf("authorizer saw %+v under the key of another record (%v)", command, objects[command.ObjectID])
		}
	}
	if got := len(commands.list()); got != 2*perTenant {
		t.Fatalf("authorizer saw %d commands, want %d", got, 2*perTenant)
	}

	// Stopped publisher: each tenant counts only its own pending records.
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := range 2 {
		mustCommit(t, store, canvasA, canvas.Operation{ID: fmt.Sprintf("late-%d", i)}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280 + i, Height: 720}})
	}
	mustCommit(t, store, canvasB, canvas.Operation{ID: "late-0"}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}})
	if a, b := mustStats(t, events, canvasA.TenantID), mustStats(t, events, canvasB.TenantID); a.Pending != 2 || b.Pending != 1 {
		t.Fatalf("Stats = %s %+v, %s %+v; want 2 and 1 pending", canvasA.TenantID, a, canvasB.TenantID, b)
	}
	if pending := mustPendingRecords(t, events, canvasB); len(pending) != 1 || pending[0].Event.Key != canvasB {
		t.Fatalf("Pending(%v) = %+v", canvasB, pending)
	}
}
