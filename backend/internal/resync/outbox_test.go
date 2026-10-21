package resync_test

import (
	"context"
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

// replicaSink delivers outbox records to one replica, as a client
// connection would receive them.
type replicaSink struct {
	mu      sync.Mutex
	replica *resync.Replica
}

func (s *replicaSink) Deliver(_ context.Context, record outbox.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replica.Receive(record.Event)
}

func (s *replicaSink) state() (resync.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replica.State(), s.replica.NeedsResync()
}

func startPublisher(t *testing.T, events *postgres.OutboxStore, sink outbox.Sink) *outbox.Publisher {
	t.Helper()
	publisher, err := outbox.NewPublisher(outbox.Config{Store: events, Sink: sink, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := publisher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	return publisher
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReplicaFollowsTheOutboxAndResyncsWhileThePublisherIsStopped is the
// retroactive CTRL-CONS-01 proof for TE-008: a replica fed only by the outbox
// publisher equals the committed state; while the publisher is stopped the
// replica learns of later commits, resyncs from a snapshot, and ignores the
// records delivered after the publisher restarts.
func TestReplicaFollowsTheOutboxAndResyncsWhileThePublisherIsStopped(t *testing.T) {
	ctx := context.Background()
	server := pgtest.Start(t)
	store, pool := openPG(t, server)
	events := postgres.NewOutboxStore(pool)
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sink := &replicaSink{replica: resync.NewReplica(key)}
	if err := sink.replica.Adopt(resync.NewSnapshot(pgLoad(t, store, key), "")); err != nil {
		t.Fatalf("Adopt initial snapshot: %v", err)
	}
	committed := func() resync.Snapshot { return resync.NewSnapshot(pgLoad(t, store, key), "") }
	converged := func() bool {
		state, needs := sink.state()
		return !needs && reflect.DeepEqual(state, committed())
	}

	publisher := startPublisher(t, events, sink)
	created := pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindVideo, Content: "clip", Transform: canvas.Transform{X: 1, Y: 2, Width: 30, Height: 40, Rotation: 12.345678901234567}})
	pgApply(t, store, key, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}})
	pgApply(t, store, key, canvas.SetTransform{ObjectID: created.ObjectID, Transform: canvas.Transform{X: -5, Y: 6, Width: 7, Height: 8, Rotation: 90}})
	waitFor(t, "the replica to follow the outbox to revision 3", converged)

	// Stopping the publisher keeps later records pending; a replica that
	// learns of the later commits resyncs (the hub signal is proven in
	// outbox.TestStoppingThePublisherForcesClientsToResync).
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindText, Content: "late", Transform: canvas.Transform{Width: 1, Height: 1}})
	pgApply(t, store, key, canvas.DeleteObject{ObjectID: created.ObjectID})
	pending, err := events.Pending(ctx, key, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending after stop = %d records, %v; want 2", len(pending), err)
	}
	if state, _ := sink.state(); state.Revision() != 3 {
		t.Fatalf("replica revision while stopped = %d, want 3", state.Revision())
	}
	sink.mu.Lock()
	sink.replica.Notice(pgLoad(t, store, key).Revision)
	needs := sink.replica.NeedsResync()
	adoptErr := sink.replica.Adopt(committed())
	sink.mu.Unlock()
	if !needs || adoptErr != nil {
		t.Fatalf("after Notice NeedsResync = %v, Adopt = %v; want true, nil", needs, adoptErr)
	}
	if !converged() {
		t.Fatal("replica differs from the committed state after resync")
	}
	if state, _ := sink.state(); len(state.Canvas.Objects) != 1 || state.Canvas.Objects[0].ID != second.ObjectID {
		t.Fatalf("resynced objects = %+v, want only %s", state.Canvas.Objects, second.ObjectID)
	}

	// A restarted publisher delivers the pending records; the replica already
	// holds them and stays equal to the committed state.
	startPublisher(t, events, sink)
	waitFor(t, "the pending records to settle", func() bool {
		stats, err := events.Stats(ctx, key.TenantID)
		return err == nil && stats.Pending == 0 && stats.Rejected == 0
	})
	if !converged() {
		t.Fatal("replica differs from the committed state after redelivery")
	}
}
