package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

// resyncSink is a sink that records when the publisher requires resync and
// whether a delivery ran after it.
type resyncSink struct {
	sink
	mu                 sync.Mutex
	resyncs            int
	deliveredAfterward bool
}

func (s *resyncSink) Deliver(ctx context.Context, record outbox.Record) error {
	s.mu.Lock()
	if s.resyncs > 0 {
		s.deliveredAfterward = true
	}
	s.mu.Unlock()
	return s.sink.Deliver(ctx, record)
}

func (s *resyncSink) RequireResync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncs++
}

func (s *resyncSink) seen() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resyncs, s.deliveredAfterward
}

func TestPublisherCloseRequiresResyncOnceAfterTheLastDelivery(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	out := &resyncSink{sink: sink{deliver: func(context.Context, outbox.Record) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}}}
	store.add(canvasA, 1, "", t0)
	pending := store.add(canvasA, 2, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour})
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- publisher.Close(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	if resyncs, _ := out.seen(); resyncs != 0 {
		t.Fatalf("resync required %d times while a delivery was in flight, want 0", resyncs)
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
	if resyncs, afterward := out.seen(); resyncs != 1 || afterward {
		t.Fatalf("after Close resyncs = %d, delivery afterward = %v; want 1, false", resyncs, afterward)
	}
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if resyncs, _ := out.seen(); resyncs != 1 {
		t.Fatalf("resyncs after a second Close = %d, want 1", resyncs)
	}
	if r := store.get(pending); r.outcome != "" || r.record.Attempts != 0 {
		t.Fatalf("record after Close = %+v, want pending and untouched", r)
	}
}

func TestPublisherCloseRequiresResyncWhenItRunsOutOfTimeOrNeverStarted(t *testing.T) {
	store := &memStore{}
	entered := make(chan struct{})
	var once sync.Once
	out := &resyncSink{sink: sink{deliver: func(ctx context.Context, _ outbox.Record) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}}
	store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, PollInterval: time.Hour, AttemptTimeout: time.Hour})
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := publisher.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if resyncs, _ := out.seen(); resyncs != 1 {
		t.Fatalf("resyncs after an aborted Close = %d, want 1", resyncs)
	}

	idle := &resyncSink{}
	unstarted, err := outbox.NewPublisher(outbox.Config{Store: &memStore{}, Sink: idle})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := unstarted.Close(context.Background()); err != nil {
		t.Fatalf("Close of an unstarted publisher = %v", err)
	}
	if resyncs, _ := idle.seen(); resyncs != 1 {
		t.Fatalf("resyncs after closing an unstarted publisher = %d, want 1", resyncs)
	}
}

func TestHubSinkRequireResyncMarksTheHub(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	sink := outbox.NewHubSink(hub)
	if err := sink.Deliver(context.Background(), record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	key := realtime.Key{TenantID: canvasA.TenantID, CanvasID: canvasA.CanvasID}
	sink.RequireResync()
	if _, err := hub.Events(context.Background(), key, 1); !errors.Is(err, realtime.ErrResyncRequired) {
		t.Fatalf("Events after RequireResync = %v, want ErrResyncRequired", err)
	}
	// An actor without resync support is left alone.
	outbox.NewHubSink(&answers{hub: hub}).RequireResync()
}

// TestStoppingThePublisherForcesClientsToResync is S7: a client following the
// hub learns, once the publisher stops, that its stream no longer follows the
// commits, and recovers the committed state from PostgreSQL.
func TestStoppingThePublisherForcesClientsToResync(t *testing.T) {
	ctx := context.Background()
	server := pgtest.Start(t)
	store, events, _ := openPG(t, server)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	hubKey := realtime.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher := start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	created := mustCommit(t, store, key, canvas.Operation{ID: "op-1"}, canvas.CreateObject{Kind: canvas.KindText, Content: "a", Transform: canvas.Transform{Width: 1, Height: 1}})
	mustCommit(t, store, key, canvas.Operation{ID: "op-2"}, canvas.SetContent{ObjectID: created.ObjectID, Content: "b"})
	eventually(t, "two records delivered", func() bool { return mustStats(t, events, key.TenantID).Pending == 0 })

	client := realtime.NewReplica(hubKey)
	fetch := func(ctx context.Context, after uint64) ([]realtime.Event, error) {
		return hub.Events(ctx, hubKey, after)
	}
	if err := client.CatchUp(ctx, fetch); err != nil || client.State().Sequence() != 2 {
		t.Fatalf("CatchUp before stop = %v at sequence %d, want nil at 2", err, client.State().Sequence())
	}

	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mustCommit(t, store, key, canvas.Operation{ID: "op-3"}, canvas.SetContent{ObjectID: created.ObjectID, Content: "c"})
	if stats := mustStats(t, events, key.TenantID); stats.Pending != 1 {
		t.Fatalf("Pending after stop = %d, want 1", stats.Pending)
	}
	if err := client.CatchUp(ctx, fetch); !errors.Is(err, realtime.ErrResyncRequired) || client.State().Sequence() != 2 {
		t.Fatalf("CatchUp after stop = %v at sequence %d, want ErrResyncRequired at 2", err, client.State().Sequence())
	}
	committed, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snapshot := resync.NewSnapshot(committed, "")
	if snapshot.Revision() != 3 || snapshot.Canvas.Objects[0].Content != "c" {
		t.Fatalf("resync snapshot = revision %d %+v, want revision 3 with content c", snapshot.Revision(), snapshot.Canvas.Objects)
	}
}
