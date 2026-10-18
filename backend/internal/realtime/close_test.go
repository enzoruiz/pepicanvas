package realtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Inside a synctest bubble time is virtual, so the measured drain is exact,
// and the bubble fails if any goroutine or timer outlives the test.

func TestCloseDrainsQueuedWorkAndRejectsNewWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, err := NewHub(Config{Authorize: func(context.Context, Command) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

		results := make([]Result, 4)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = submitAsync(t, hub, setCommand(key, fmt.Sprintf("op-%d", i), fmt.Sprintf("obj-%d", i), "v"))
			}()
		}
		synctest.Wait()

		start := time.Now()
		closed := make(chan error, 1)
		go func() { closed <- hub.Close(context.Background()) }()
		synctest.Wait()

		if _, err := hub.Submit(context.Background(), setCommand(key, "op-late", "obj-late", "v")); !errors.Is(err, ErrClosed) {
			t.Fatalf("Submit during drain error = %v, want ErrClosed", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if drained := time.Since(start); drained != 400*time.Millisecond {
			t.Fatalf("drain took %v, want 400ms for four queued commands", drained)
		}
		wg.Wait()

		for _, result := range results {
			if result.Outcome != OutcomeAccepted {
				t.Fatalf("queued result = %+v, want accepted", result)
			}
		}
		if events := mustEvents(t, hub, key, 0); len(events) != 4 {
			t.Fatalf("len(events) = %d, want 4 drained commands", len(events))
		}
	})
}

func TestCloseDeadlineAbortsWithoutPartialCommits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, err := NewHub(Config{Authorize: func(ctx context.Context, _ Command) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		if err != nil {
			t.Fatal(err)
		}
		key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

		errs := make([]error, 3)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = hub.Submit(context.Background(), setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", "v"))
			}()
		}
		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		if err := hub.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close() error = %v, want deadline exceeded", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("Close waited %v, want the 1s deadline", waited)
		}
		wg.Wait()

		for i, err := range errs {
			if err == nil {
				t.Fatalf("Submit %d succeeded after abort", i)
			}
		}
		if events := mustEvents(t, hub, key, 0); len(events) != 0 {
			t.Fatalf("events = %+v, want no partial commits", events)
		}
	})
}

func TestCloseIsIdempotentAndCreatesNoActors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, err := NewHub(Config{Authorize: allowAll})
		if err != nil {
			t.Fatal(err)
		}
		key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
		mustSubmit(t, hub, setCommand(key, "op-1", "obj-1", "v"))
		for range 2 {
			if err := hub.Close(context.Background()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		}
		other := Key{TenantID: "tenant-a", CanvasID: "canvas-2"}
		if _, err := hub.Submit(context.Background(), setCommand(other, "op-1", "obj-1", "v")); !errors.Is(err, ErrClosed) {
			t.Fatalf("Submit after Close error = %v, want ErrClosed", err)
		}
		if events := mustEvents(t, hub, other, 0); len(events) != 0 {
			t.Fatalf("closed hub created an actor: %+v", events)
		}
		// Accepted history stays readable for catch-up after Close.
		if events := mustEvents(t, hub, key, 0); len(events) != 1 {
			t.Fatalf("len(events) = %d, want 1", len(events))
		}
	})
}

func TestSubmitRacingCloseIsAcceptedOrRejected(t *testing.T) {
	hub, err := NewHub(Config{Authorize: allowAll, QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	key := Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

	results := make(chan Result, 200)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := hub.Submit(context.Background(), setCommand(key, fmt.Sprintf("op-%d", i), "obj-1", "v"))
			switch {
			case err == nil:
				results <- result
			case !errors.Is(err, ErrClosed):
				t.Errorf("Submit() error = %v, want nil or ErrClosed", err)
			}
		}()
	}
	if err := hub.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wg.Wait()
	close(results)

	accepted := map[uint64]bool{}
	for result := range results {
		accepted[result.Sequence] = true
	}
	events := mustEvents(t, hub, key, 0)
	if len(events) != len(accepted) {
		t.Fatalf("len(events) = %d, accepted results = %d", len(events), len(accepted))
	}
	for _, event := range events {
		if !accepted[event.Sequence] {
			t.Fatalf("event %d has no accepted result", event.Sequence)
		}
	}
}
