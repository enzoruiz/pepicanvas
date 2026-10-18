package realtime

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
)

func fetcher(hub *Hub, key Key) Fetcher {
	return func(ctx context.Context, after uint64) ([]Event, error) {
		return hub.Events(ctx, key, after)
	}
}

func authoritative(t *testing.T, hub *Hub, key Key) *State {
	t.Helper()
	state := NewState()
	for _, event := range mustEvents(t, hub, key, 0) {
		state.Apply(event)
	}
	return state
}

func TestReplicaAppliesEventsInSequenceOrder(t *testing.T) {
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	replica := NewReplica(key)
	set := func(sequence uint64, payload string) Event {
		return Event{Key: key, Sequence: sequence, OperationID: fmt.Sprintf("op-%d", sequence), ObjectID: "obj-1", Kind: KindSet, Payload: []byte(payload)}
	}

	// Sequence 2 arrives first and waits for 1; duplicates are ignored.
	for _, event := range []Event{set(2, "second"), set(2, "second"), set(1, "first"), set(1, "first")} {
		if err := replica.Receive(event); err != nil {
			t.Fatalf("Receive(%d) error = %v", event.Sequence, err)
		}
	}
	object, _ := replica.State().Object("obj-1")
	if replica.State().Sequence() != 2 || string(object.Payload) != "second" {
		t.Fatalf("replica = sequence %d payload %q, want 2 and second", replica.State().Sequence(), object.Payload)
	}
	if replica.Waiting() {
		t.Fatal("Waiting() = true after gap was filled")
	}
	if err := replica.Receive(set(4, "fourth")); err != nil || !replica.Waiting() {
		t.Fatalf("Receive(4) error = %v, Waiting() = %v; want buffered gap", err, replica.Waiting())
	}
}

func TestReplicaRejectsForeignAndInvalidEvents(t *testing.T) {
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	replica := NewReplica(key)
	for name, event := range map[string]Event{
		"foreign tenant": {Key: Key{TenantID: "tenant-b", CanvasID: "canvas-1"}, Sequence: 1, ObjectID: "obj-1", Kind: KindSet},
		"foreign canvas": {Key: Key{TenantID: "tenant-a", CanvasID: "canvas-2"}, Sequence: 1, ObjectID: "obj-1", Kind: KindSet},
	} {
		if err := replica.Receive(event); !errors.Is(err, ErrForeignEvent) {
			t.Fatalf("%s: Receive() error = %v, want ErrForeignEvent", name, err)
		}
	}
	if err := replica.Receive(Event{Key: key, Sequence: 0, ObjectID: "obj-1", Kind: KindSet}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero sequence error = %v, want ErrInvalidEvent", err)
	}
	if replica.State().Sequence() != 0 || len(replica.State().Objects()) != 0 || replica.Waiting() {
		t.Fatal("rejected events changed replica state")
	}
}

func TestReplicaCatchUpRecoversLostEvents(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	for i := range 5 {
		mustSubmit(t, hub, setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", fmt.Sprintf("v%d", i)))
	}
	events := mustEvents(t, hub, key, 0)

	replica := NewReplica(key)
	// Events 2 and 3 are lost and 5, the tail, never arrives.
	for _, event := range []Event{events[0], events[3]} {
		if err := replica.Receive(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := replica.CatchUp(context.Background(), fetcher(hub, key)); err != nil {
		t.Fatalf("CatchUp() error = %v", err)
	}
	if want := authoritative(t, hub, key); !reflect.DeepEqual(replica.State().Objects(), want.Objects()) || replica.State().Sequence() != 5 {
		t.Fatalf("replica objects = %+v at %d, want %+v at 5", replica.State().Objects(), replica.State().Sequence(), want.Objects())
	}
}

func TestReplicaCatchUpRequiresResyncPastRetention(t *testing.T) {
	hub := newTestHub(t, Config{LogSize: 4})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	for i := range 10 {
		mustSubmit(t, hub, setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", "v"))
	}

	replica := NewReplica(key)
	if err := replica.CatchUp(context.Background(), fetcher(hub, key)); !errors.Is(err, ErrResyncRequired) {
		t.Fatalf("CatchUp() error = %v, want ErrResyncRequired", err)
	}
	if replica.State().Sequence() != 0 || len(replica.State().Objects()) != 0 {
		t.Fatal("failed catch-up changed replica state")
	}
	// A cursor at the retained boundary still replays.
	if events, err := hub.Events(context.Background(), key, 6); err != nil || len(events) != 4 || events[0].Sequence != 7 {
		t.Fatalf("Events(after 6) = %d events, %v; want sequences 7..10", len(events), err)
	}
}

// faultyDelivery duplicates, reorders, delays, and drops events. Delayed
// events are returned separately so they arrive after the replica caught up.
func faultyDelivery(rng *rand.Rand, events []Event) (now, late []Event) {
	for _, event := range events {
		switch roll := rng.IntN(10); {
		case roll < 2: // lost
		case roll < 4:
			now = append(now, event, event)
		case roll < 5:
			late = append(late, event)
		default:
			now = append(now, event)
		}
	}
	rng.Shuffle(len(now), func(i, j int) { now[i], now[j] = now[j], now[i] })
	return now, late
}

func TestReplicasConvergeUnderInjectedFaultsAcrossTenants(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			hub := newTestHub(t, Config{})
			keys := []Key{{TenantID: "tenant-a", CanvasID: "canvas-1"}, {TenantID: "tenant-b", CanvasID: "canvas-1"}}

			// Three clients per tenant edit the same objects at the same time,
			// retry some operations, and delete some objects.
			var wg sync.WaitGroup
			for _, key := range keys {
				for client := range 3 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(seed, uint64(client)))
						for i := range 25 {
							objectID := fmt.Sprintf("obj-%d", rng.IntN(4))
							command := setCommand(key, fmt.Sprintf("c%d-op-%d", client, i), objectID, fmt.Sprintf("%s-c%d-%d", key.TenantID, client, i))
							if rng.IntN(12) == 0 {
								command = deleteCommand(key, command.OperationID, objectID)
							}
							first := submitAsync(t, hub, command)
							if rng.IntN(3) == 0 {
								if retry := submitAsync(t, hub, command); retry != first {
									t.Errorf("retry %s = %+v, want %+v", command.OperationID, retry, first)
								}
							}
						}
					}()
				}
			}
			wg.Wait()
			if t.Failed() {
				t.FailNow()
			}

			rng := rand.New(rand.NewPCG(seed, 99))
			for _, key := range keys {
				want := authoritative(t, hub, key)
				for _, other := range want.Objects() {
					if !other.Deleted && string(other.Payload[:len(key.TenantID)]) != key.TenantID {
						t.Fatalf("%s state holds foreign payload %q", key.TenantID, other.Payload)
					}
				}
				events := mustEvents(t, hub, key, 0)
				for replicaID := range 3 {
					replica := NewReplica(key)
					now, late := faultyDelivery(rng, events)
					for _, event := range now {
						if err := replica.Receive(event); err != nil {
							t.Fatalf("Receive() error = %v", err)
						}
					}
					if err := replica.CatchUp(context.Background(), fetcher(hub, key)); err != nil {
						t.Fatalf("CatchUp() error = %v", err)
					}
					for _, event := range late {
						if err := replica.Receive(event); err != nil {
							t.Fatalf("late Receive() error = %v", err)
						}
					}
					if !reflect.DeepEqual(replica.State().Objects(), want.Objects()) || replica.State().Sequence() != want.Sequence() {
						t.Fatalf("replica %d of %s diverged at sequence %d, want %d", replicaID, key.TenantID, replica.State().Sequence(), want.Sequence())
					}
					// Events of the other tenant never enter this replica.
					foreign := keys[0]
					if foreign == key {
						foreign = keys[1]
					}
					for _, event := range mustEvents(t, hub, foreign, 0) {
						if err := replica.Receive(event); !errors.Is(err, ErrForeignEvent) {
							t.Fatalf("foreign Receive() error = %v, want ErrForeignEvent", err)
						}
					}
					if !reflect.DeepEqual(replica.State().Objects(), want.Objects()) {
						t.Fatalf("foreign events changed replica %d of %s", replicaID, key.TenantID)
					}
				}
			}
		})
	}
}
