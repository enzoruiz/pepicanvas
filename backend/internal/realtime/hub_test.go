package realtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var errDenied = errors.New("denied")

func allowAll(context.Context, Command) error { return nil }

func newTestHub(t *testing.T, cfg Config) *Hub {
	t.Helper()
	if cfg.Authorize == nil {
		cfg.Authorize = allowAll
	}
	hub, err := NewHub(cfg)
	if err != nil {
		t.Fatalf("NewHub() error = %v", err)
	}
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	return hub
}

func setCommand(key Key, operationID, objectID, payload string) Command {
	return Command{Key: key, OperationID: operationID, ObjectID: objectID, Kind: KindSet, Payload: []byte(payload)}
}

func deleteCommand(key Key, operationID, objectID string) Command {
	return Command{Key: key, OperationID: operationID, ObjectID: objectID, Kind: KindDelete}
}

func mustSubmit(t *testing.T, hub *Hub, command Command) Result {
	t.Helper()
	result, err := hub.Submit(context.Background(), command)
	if err != nil {
		t.Fatalf("Submit(%s) error = %v", command.OperationID, err)
	}
	return result
}

func mustEvents(t *testing.T, hub *Hub, key Key, after uint64) []Event {
	t.Helper()
	events, err := hub.Events(context.Background(), key, after)
	if err != nil {
		t.Fatalf("Events(%v, %d) error = %v", key, after, err)
	}
	return events
}

func TestNewHubRequiresAuthorizer(t *testing.T) {
	if _, err := NewHub(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewHub(Config{}) error = %v, want ErrInvalidConfig", err)
	}
}

func TestAcceptedCommandsReceiveGaplessSequence(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	first := mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))
	deleted := mustSubmit(t, hub, deleteCommand(key, "op-2", "obj-1"))
	rejected := mustSubmit(t, hub, setCommand(key, "op-3", "obj-1", "b"))
	second := mustSubmit(t, hub, setCommand(key, "op-4", "obj-2", "c"))

	got := []Result{first, deleted, rejected, second}
	want := []Result{
		{OperationID: "op-1", Outcome: OutcomeAccepted, Sequence: 1},
		{OperationID: "op-2", Outcome: OutcomeAccepted, Sequence: 2},
		{OperationID: "op-3", Outcome: OutcomeRejected, Reason: ReasonObjectDeleted},
		{OperationID: "op-4", Outcome: OutcomeAccepted, Sequence: 3},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	events := mustEvents(t, hub, key, 0)
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.Key != key {
			t.Fatalf("event %d = %+v, want sequence %d for %v", i, event, i+1, key)
		}
	}
}

func TestConcurrentCommandsLastAcceptedWins(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	const writers = 32
	results := make([]Result, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = mustSubmit(t, hub, setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", fmt.Sprintf("v%d", i)))
		}()
	}
	wg.Wait()

	last := results[0]
	seen := map[uint64]bool{}
	for _, result := range results {
		if result.Outcome != OutcomeAccepted || seen[result.Sequence] {
			t.Fatalf("result = %+v, want unique accepted sequence", result)
		}
		seen[result.Sequence] = true
		if result.Sequence > last.Sequence {
			last = result
		}
	}
	if last.Sequence != writers {
		t.Fatalf("highest sequence = %d, want %d", last.Sequence, writers)
	}

	state := NewState()
	for _, event := range mustEvents(t, hub, key, 0) {
		state.Apply(event)
	}
	object, ok := state.Object("obj-1")
	wantPayload := "v" + strings.TrimPrefix(last.OperationID, "op-")
	if !ok || object.Deleted || string(object.Payload) != wantPayload {
		t.Fatalf("Object(obj-1) = %+v, %v; want payload %q from last accepted %s", object, ok, wantPayload, last.OperationID)
	}
}

func TestDeleteWinsOverLaterCommands(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	// A delete that arrives before the create still wins.
	mustSubmit(t, hub, deleteCommand(key, "op-1", "obj-early"))
	for _, command := range []Command{
		setCommand(key, "op-2", "obj-early", "late-create"),
		deleteCommand(key, "op-3", "obj-early"),
	} {
		result := mustSubmit(t, hub, command)
		if result.Outcome != OutcomeRejected || result.Reason != ReasonObjectDeleted || result.Sequence != 0 {
			t.Fatalf("Submit(%s) = %+v, want rejected object_deleted without sequence", command.OperationID, result)
		}
	}

	mustSubmit(t, hub, setCommand(key, "op-4", "obj-1", "a"))
	mustSubmit(t, hub, deleteCommand(key, "op-5", "obj-1"))
	if result := mustSubmit(t, hub, setCommand(key, "op-6", "obj-1", "b")); result.Outcome != OutcomeRejected {
		t.Fatalf("set after delete = %+v, want rejected", result)
	}

	state := NewState()
	for _, event := range mustEvents(t, hub, key, 0) {
		state.Apply(event)
	}
	for _, objectID := range []string{"obj-early", "obj-1"} {
		if object, ok := state.Object(objectID); !ok || !object.Deleted || object.Payload != nil {
			t.Fatalf("Object(%s) = %+v, %v; want tombstone", objectID, object, ok)
		}
	}
	if got := len(mustEvents(t, hub, key, 0)); got != 3 {
		t.Fatalf("len(events) = %d, want 3 accepted", got)
	}
}

func TestTenantsWithSameCanvasIDAreIsolated(t *testing.T) {
	hub := newTestHub(t, Config{})
	keyA := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	keyB := Key{TenantID: "tenant-b", CanvasID: "canvas-1"}

	var wg sync.WaitGroup
	for _, key := range []Key{keyA, keyB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				mustSubmit(t, hub, setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", key.TenantID))
			}
		}()
	}
	wg.Wait()

	// The same operation and object IDs in another tenant never collide.
	mustSubmit(t, hub, deleteCommand(keyA, "op-delete", "obj-1"))
	if result := mustSubmit(t, hub, setCommand(keyB, "op-after", "obj-1", "tenant-b")); result.Outcome != OutcomeAccepted || result.Sequence != 21 {
		t.Fatalf("tenant-b after tenant-a delete = %+v, want accepted sequence 21", result)
	}

	for _, key := range []Key{keyA, keyB} {
		events := mustEvents(t, hub, key, 0)
		if len(events) != 21 {
			t.Fatalf("len(events %s) = %d, want 21", key.TenantID, len(events))
		}
		for i, event := range events {
			if event.Key != key || event.Sequence != uint64(i+1) {
				t.Fatalf("event %d for %s = %+v", i, key.TenantID, event)
			}
			if event.Kind == KindSet && string(event.Payload) != key.TenantID {
				t.Fatalf("event %d for %s carries payload %q", i, key.TenantID, event.Payload)
			}
		}
	}
}

func TestAuthorizationIsCheckedBeforeAcceptance(t *testing.T) {
	var mu sync.Mutex
	allowed := true
	hub := newTestHub(t, Config{Authorize: func(_ context.Context, command Command) error {
		mu.Lock()
		defer mu.Unlock()
		if !allowed || command.Key.TenantID != "tenant-a" {
			return errDenied
		}
		return nil
	}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))
	mu.Lock()
	allowed = false
	mu.Unlock()

	if _, err := hub.Submit(context.Background(), setCommand(key, "op-2", "obj-1", "b")); !errors.Is(err, errDenied) {
		t.Fatalf("Submit after revocation error = %v, want denial", err)
	}
	foreign := Key{TenantID: "tenant-b", CanvasID: "canvas-1"}
	if _, err := hub.Submit(context.Background(), setCommand(foreign, "op-1", "obj-1", "x")); !errors.Is(err, errDenied) {
		t.Fatalf("Submit foreign tenant error = %v, want denial", err)
	}

	if events := mustEvents(t, hub, key, 0); len(events) != 1 {
		t.Fatalf("len(events) = %d, want only the authorized command", len(events))
	}
	if events := mustEvents(t, hub, foreign, 0); len(events) != 0 {
		t.Fatalf("foreign events = %+v, want none", events)
	}
}

func TestCorrelationIDIsCarriedToResultAndEvent(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	command := setCommand(key, "op-1", "obj-1", "a")
	command.CorrelationID = "corr-123"
	result := mustSubmit(t, hub, command)
	if result.CorrelationID != "corr-123" {
		t.Fatalf("result correlation = %q, want corr-123", result.CorrelationID)
	}
	events := mustEvents(t, hub, key, 0)
	if len(events) != 1 || events[0].CorrelationID != "corr-123" || events[0].OperationID != "op-1" {
		t.Fatalf("events = %+v, want correlation corr-123 for op-1", events)
	}
}

func TestInvalidCommandsAreRejectedBeforeAcceptance(t *testing.T) {
	var authorized atomic.Int32
	hub := newTestHub(t, Config{Authorize: func(context.Context, Command) error {
		authorized.Add(1)
		return nil
	}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	long := strings.Repeat("x", MaxIdentifierBytes+1)

	tests := map[string]func(*Command){
		"missing tenant":             func(c *Command) { c.Key.TenantID = "" },
		"missing canvas":             func(c *Command) { c.Key.CanvasID = "" },
		"missing operation":          func(c *Command) { c.OperationID = "" },
		"missing object":             func(c *Command) { c.ObjectID = "" },
		"long operation":             func(c *Command) { c.OperationID = long },
		"long object":                func(c *Command) { c.ObjectID = long },
		"unknown kind":               func(c *Command) { c.Kind = "move" },
		"delete with payload":        func(c *Command) { c.Kind = KindDelete },
		"oversized payload":          func(c *Command) { c.Payload = make([]byte, MaxPayloadBytes+1) },
		"long correlation":           func(c *Command) { c.CorrelationID = long },
		"non-printable correlation":  func(c *Command) { c.CorrelationID = "corr\n1" },
		"non-printable operation id": func(c *Command) { c.OperationID = "op\x001" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			command := setCommand(key, "op-1", "obj-1", "a")
			mutate(&command)
			if _, err := hub.Submit(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("Submit() error = %v, want ErrInvalidCommand", err)
			}
		})
	}
	if got := authorized.Load(); got != 0 {
		t.Fatalf("authorizer called %d times, want 0 for invalid commands", got)
	}
	if events := mustEvents(t, hub, key, 0); len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}
