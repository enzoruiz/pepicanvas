package realtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func usageCommand(key Key, operationID, payload string) Command {
	return Command{Key: key, OperationID: operationID, ObjectID: "media_usage", Kind: KindUsage, Payload: []byte(payload)}
}

func TestUsageCommandIsAnEventThatChangesNoObject(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))
	usage := mustSubmit(t, hub, usageCommand(key, "outbox-7", `{"version":1}`))
	if want := (Result{OperationID: "outbox-7", Outcome: OutcomeAccepted, Sequence: 2}); usage != want {
		t.Fatalf("usage result = %+v, want %+v", usage, want)
	}
	mustSubmit(t, hub, setCommand(key, "op-2", "obj-1", "b"))

	events := mustEvents(t, hub, key, 0)
	if len(events) != 3 || events[1].Kind != KindUsage || string(events[1].Payload) != `{"version":1}` || events[1].Sequence != 2 {
		t.Fatalf("events = %+v, want the usage event at sequence 2", events)
	}
	state := authoritative(t, hub, key)
	want := map[string]Object{"obj-1": {Payload: []byte("b"), Sequence: 3}}
	if got := state.Objects(); !reflect.DeepEqual(got, want) || state.Sequence() != 3 {
		t.Fatalf("state = %+v at %d, want %+v at 3", got, state.Sequence(), want)
	}

	// A replica follows the usage event without changing its objects.
	replica := NewReplica(key)
	if err := replica.CatchUp(context.Background(), fetcher(hub, key)); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if got := replica.State().Objects(); !reflect.DeepEqual(got, want) || replica.State().Sequence() != 3 || replica.Waiting() {
		t.Fatalf("replica = %+v at %d, want %+v at 3", got, replica.State().Sequence(), want)
	}
}

func TestUsageCommandIsNeverRejectedByADeletedObject(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	mustSubmit(t, hub, deleteCommand(key, "op-1", "media_usage"))
	if got := mustSubmit(t, hub, usageCommand(key, "outbox-1", `{}`)); got.Outcome != OutcomeAccepted || got.Sequence != 2 {
		t.Fatalf("usage after a delete of its object ID = %+v, want accepted at 2", got)
	}
}

func TestUsageRedeliveryAnswersTheSameResultWithOneEffect(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	first := mustSubmit(t, hub, usageCommand(key, "outbox-3", `{"version":2}`))
	again := mustSubmit(t, hub, usageCommand(key, "outbox-3", `{"version":2}`))
	if first != again {
		t.Fatalf("redelivery = %+v, want %+v", again, first)
	}
	if events := mustEvents(t, hub, key, 0); len(events) != 1 {
		t.Fatalf("events after redelivery = %+v, want one", events)
	}
	if _, err := hub.Submit(context.Background(), usageCommand(key, "outbox-3", `{"version":3}`)); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("another usage under the same operation = %v, want ErrOperationConflict", err)
	}
}

func TestUsageCommandValidation(t *testing.T) {
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	hub := newTestHub(t, Config{})
	for name, command := range map[string]Command{
		"no payload":    usageCommand(key, "outbox-1", ""),
		"over the size": usageCommand(key, "outbox-1", strings.Repeat("x", MaxPayloadBytes+1)),
	} {
		if _, err := hub.Submit(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("%s: Submit = %v, want ErrInvalidCommand", name, err)
		}
	}
}
