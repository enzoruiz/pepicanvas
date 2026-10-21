package realtime

import (
	"context"
	"errors"
	"testing"
)

func TestRequireResyncAnswersEveryReplayAtOrBelowTheMark(t *testing.T) {
	hub := newTestHub(t, Config{})
	busy := Key{TenantID: "tenant-a", CanvasID: "busy"}
	other := Key{TenantID: "tenant-b", CanvasID: "busy"}
	idle := Key{TenantID: "tenant-a", CanvasID: "idle"}
	mustSubmit(t, hub, setCommand(busy, "op-1", "obj-1", `"a"`))
	mustSubmit(t, hub, setCommand(busy, "op-2", "obj-1", `"b"`))
	mustSubmit(t, hub, setCommand(other, "op-1", "obj-1", `"c"`))

	hub.RequireResync()

	marks := []struct {
		key   Key
		after uint64
	}{{busy, 0}, {busy, 1}, {busy, 2}, {other, 0}, {other, 1}, {idle, 0}}
	for _, mark := range marks {
		if events, err := hub.Events(context.Background(), mark.key, mark.after); !errors.Is(err, ErrResyncRequired) || events != nil {
			t.Fatalf("Events(%v, %d) = %+v, %v; want nil, ErrResyncRequired", mark.key, mark.after, events, err)
		}
	}

	// The hub keeps accepting commands; replay resumes after the mark.
	if result := mustSubmit(t, hub, setCommand(busy, "op-3", "obj-1", `"d"`)); result.Outcome != OutcomeAccepted || result.Sequence != 3 {
		t.Fatalf("Submit after the mark = %+v, want accepted with sequence 3", result)
	}
	if events := mustEvents(t, hub, busy, 3); len(events) != 0 {
		t.Fatalf("Events(busy, 3) = %+v, want none", events)
	}
	mustSubmit(t, hub, setCommand(busy, "op-4", "obj-1", `"e"`))
	if events := mustEvents(t, hub, busy, 3); len(events) != 1 || events[0].Sequence != 4 {
		t.Fatalf("Events(busy, 3) = %+v, want only sequence 4", events)
	}
	if _, err := hub.Events(context.Background(), busy, 2); !errors.Is(err, ErrResyncRequired) {
		t.Fatalf("Events(busy, 2) after new events = %v, want ErrResyncRequired", err)
	}
	// A canvas first used after the mark replays from its first event.
	mustSubmit(t, hub, setCommand(idle, "op-1", "obj-1", `"f"`))
	if _, err := hub.Events(context.Background(), idle, 0); !errors.Is(err, ErrResyncRequired) {
		t.Fatalf("Events(idle, 0) = %v, want ErrResyncRequired", err)
	}
	if events := mustEvents(t, hub, idle, 1); len(events) != 0 {
		t.Fatalf("Events(idle, 1) = %+v, want none", events)
	}

	// A replica catching up from before the mark keeps its state and resyncs.
	replica := NewReplica(busy)
	if err := replica.CatchUp(context.Background(), func(ctx context.Context, after uint64) ([]Event, error) {
		return hub.Events(ctx, busy, after)
	}); !errors.Is(err, ErrResyncRequired) || replica.State().Sequence() != 0 {
		t.Fatalf("CatchUp = %v at sequence %d, want ErrResyncRequired at 0", err, replica.State().Sequence())
	}
}

func TestEventsWithoutRequireResyncReplayEverything(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "main"}
	if events, err := hub.Events(context.Background(), key, 0); err != nil || events != nil {
		t.Fatalf("Events of an unused canvas = %+v, %v; want nil, nil", events, err)
	}
	mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", `"a"`))
	if events := mustEvents(t, hub, key, 0); len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("Events(0) = %+v, want sequence 1", events)
	}
}
