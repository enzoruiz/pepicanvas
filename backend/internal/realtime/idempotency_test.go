package realtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRetryReturnsSameResultWithSingleEffect(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	original := setCommand(key, "op-1", "obj-1", "a")
	original.CorrelationID = "conn-1"
	first := mustSubmit(t, hub, original)

	// A retry after reconnecting carries a new correlation ID but the same intent.
	retry := original
	retry.CorrelationID = "conn-2"
	for range 3 {
		if got := mustSubmit(t, hub, retry); got != first {
			t.Fatalf("retry result = %+v, want original %+v", got, first)
		}
	}
	events := mustEvents(t, hub, key, 0)
	if len(events) != 1 || events[0].CorrelationID != "conn-1" {
		t.Fatalf("events = %+v, want one effect from the original submission", events)
	}
}

func TestRetryAfterTimeoutReturnsCommittedResult(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	hub := newTestHub(t, Config{Authorize: func(context.Context, Command) error {
		once.Do(func() { <-release })
		return nil
	}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	command := setCommand(key, "op-1", "obj-1", "a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := hub.Submit(ctx, command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit() error = %v, want deadline exceeded", err)
	}
	close(release)

	want := Result{OperationID: "op-1", Outcome: OutcomeAccepted, Sequence: 1}
	if got := mustSubmit(t, hub, command); got != want {
		t.Fatalf("retry after timeout = %+v, want %+v", got, want)
	}
	if events := mustEvents(t, hub, key, 0); len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
}

func TestRetainedResultsSurviveLaterConflicts(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	accepted := mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))
	mustSubmit(t, hub, deleteCommand(key, "op-2", "obj-1"))
	rejected := mustSubmit(t, hub, setCommand(key, "op-3", "obj-1", "b"))

	// The set accepted before the delete keeps its answer; the rejection is stable too.
	if got := mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a")); got != accepted {
		t.Fatalf("retry op-1 = %+v, want %+v", got, accepted)
	}
	if got := mustSubmit(t, hub, setCommand(key, "op-3", "obj-1", "b")); got != rejected || got.Reason != ReasonObjectDeleted {
		t.Fatalf("retry op-3 = %+v, want %+v", got, rejected)
	}
	if events := mustEvents(t, hub, key, 0); len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
}

func TestReusedOperationIDWithDifferentIntentIsRejected(t *testing.T) {
	hub := newTestHub(t, Config{})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))

	for name, command := range map[string]Command{
		"payload": setCommand(key, "op-1", "obj-1", "b"),
		"object":  setCommand(key, "op-1", "obj-2", "a"),
		"kind":    deleteCommand(key, "op-1", "obj-1"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hub.Submit(context.Background(), command); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("Submit() error = %v, want ErrOperationConflict", err)
			}
		})
	}
	events := mustEvents(t, hub, key, 0)
	if len(events) != 1 || string(events[0].Payload) != "a" {
		t.Fatalf("events = %+v, want only the original effect", events)
	}
}

func TestRetentionExpiresAtTTL(t *testing.T) {
	clock := newFakeClock()
	hub := newTestHub(t, Config{Now: clock.Now, Retention: Retention{TTL: time.Minute}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	command := setCommand(key, "op-1", "obj-1", "a")

	first := mustSubmit(t, hub, command)
	clock.Advance(time.Minute - time.Nanosecond)
	if got := mustSubmit(t, hub, command); got != first {
		t.Fatalf("retry before expiry = %+v, want %+v", got, first)
	}
	// Conflicts are detected for as long as the result is retained.
	if _, err := hub.Submit(context.Background(), setCommand(key, "op-1", "obj-1", "b")); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("conflict before expiry error = %v", err)
	}

	clock.Advance(time.Nanosecond)
	after := mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "b"))
	if after.Outcome != OutcomeAccepted || after.Sequence != 2 {
		t.Fatalf("reuse at expiry = %+v, want a new accepted operation with sequence 2", after)
	}
	if events := mustEvents(t, hub, key, 0); len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
}

func TestRetentionIsBoundedByOperationCount(t *testing.T) {
	hub := newTestHub(t, Config{Retention: Retention{MaxOperations: 2}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a"))
	mustSubmit(t, hub, setCommand(key, "op-2", "obj-2", "b"))
	mustSubmit(t, hub, setCommand(key, "op-3", "obj-3", "c"))

	if got := mustSubmit(t, hub, setCommand(key, "op-3", "obj-3", "c")); got.Sequence != 3 {
		t.Fatalf("retry op-3 = %+v, want retained sequence 3", got)
	}
	if got := mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "a")); got.Sequence != 4 {
		t.Fatalf("retry evicted op-1 = %+v, want new sequence 4", got)
	}
}

func TestDeniedOperationsAreNotRetained(t *testing.T) {
	var mu sync.Mutex
	allowed := false
	hub := newTestHub(t, Config{Authorize: func(context.Context, Command) error {
		mu.Lock()
		defer mu.Unlock()
		if !allowed {
			return errDenied
		}
		return nil
	}})
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	command := setCommand(key, "op-1", "obj-1", "a")

	if _, err := hub.Submit(context.Background(), command); !errors.Is(err, errDenied) {
		t.Fatalf("Submit() error = %v, want denial", err)
	}
	mu.Lock()
	allowed = true
	mu.Unlock()
	if got := mustSubmit(t, hub, command); got.Outcome != OutcomeAccepted || got.Sequence != 1 {
		t.Fatalf("Submit after grant = %+v, want accepted sequence 1", got)
	}
}

func TestRetentionConfigMustNotBeNegative(t *testing.T) {
	for name, retention := range map[string]Retention{
		"ttl":   {TTL: -time.Second},
		"count": {MaxOperations: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHub(Config{Authorize: allowAll, Retention: retention}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewHub() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
