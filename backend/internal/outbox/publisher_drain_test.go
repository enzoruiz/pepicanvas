package outbox_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
)

// Pending returns the undelivered records of one canvas, oldest first.
func (s *memStore) Pending(_ context.Context, key canvas.Key, limit int) ([]outbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []outbox.Record
	for _, r := range s.records {
		if r.record.Event.Key == key && r.outcome == "" && len(pending) < limit {
			pending = append(pending, r.record)
		}
	}
	return pending, nil
}

func (s *memStore) setFailWrite(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWrite = n
}

func TestPublisherDrainsACanvasBacklogInFewPasses(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	const backlog = 250
	want := make([]int64, 0, backlog)
	for revision := int64(1); revision <= backlog; revision++ {
		store.add(canvasA, revision, "", t0)
		want = append(want, revision)
	}
	other := store.add(canvasB, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, BatchSize: 100})
	eventually(t, "the backlog delivered", func() bool { return publisher.Counters().Delivered == backlog+1 })
	if got := out.calls(canvasA); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want revisions 1..%d once each in order", got, backlog)
	}
	if r := store.get(other); r.outcome != outbox.OutcomeDelivered {
		t.Fatalf("other canvas record = %+v, want delivered", r)
	}
	// Each pass drains up to BatchSize records of a canvas: three passes and
	// one that finds nothing, instead of one pass per record.
	if reads := store.headReads(); reads > 5 {
		t.Fatalf("Heads read %d times for a backlog of %d, want at most 5", reads, backlog)
	}
}

func TestPublisherDrainStopsAtTheFirstFailureOfTheCanvas(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	out := &sink{deliver: func(_ context.Context, r outbox.Record) error {
		if r.Event.Result.Revision == 3 {
			return errors.New("busy")
		}
		return nil
	}}
	var ids []int64
	for revision := int64(1); revision <= 5; revision++ {
		ids = append(ids, store.add(canvasA, revision, "", t0))
	}
	start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, MinBackoff: time.Hour, MaxBackoff: time.Hour})
	eventually(t, "revision 3 retried", func() bool { return store.get(ids[2]).record.Attempts == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := out.calls(canvasA); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("calls = %v, want 1, 2, 3 and nothing after the failure", got)
	}
	for _, id := range ids[3:] {
		if r := store.get(id); r.outcome != "" || r.record.Attempts != 0 {
			t.Fatalf("record %d = %+v, want pending and untouched", id, r)
		}
	}
}

func TestPublisherBacksOffWhenTheStoreCannotRecordAFailure(t *testing.T) {
	store, now := &memStore{failWrite: 1000}, &clock{now: t0}
	out := &sink{deliver: func(context.Context, outbox.Record) error { return errors.New("busy") }}
	id := store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	calls := func() int { return len(out.calls(canvasA)) }
	eventually(t, "the first attempt", func() bool { return calls() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := calls(); got != 1 {
		t.Fatalf("attempts before the backoff elapsed = %d, want 1", got)
	}
	now.Advance(10 * time.Millisecond)
	publisher.Notify()
	eventually(t, "the second attempt", func() bool { return calls() == 2 })
	// The backoff doubles while the store keeps failing.
	now.Advance(10 * time.Millisecond)
	publisher.Notify()
	time.Sleep(30 * time.Millisecond)
	if got := calls(); got != 2 {
		t.Fatalf("attempts before the doubled backoff elapsed = %d, want 2", got)
	}
	now.Advance(10 * time.Millisecond)
	publisher.Notify()
	eventually(t, "the third attempt", func() bool { return calls() == 3 })
	if c := publisher.Counters(); c.StoreErrors != 3 || c.Retries != 0 {
		t.Fatalf("Counters = %+v, want 3 store errors and no recorded retry", c)
	}

	// Once the store recovers, the failure is recorded as usual.
	store.setFailWrite(0)
	now.Advance(40 * time.Millisecond)
	publisher.Notify()
	eventually(t, "a recorded retry", func() bool { return store.get(id).record.Attempts == 1 })
}

func TestPublisherBacksOffWhenTheStoreCannotSettleADelivery(t *testing.T) {
	store, out, now := &memStore{failWrite: 1}, &sink{}, &clock{now: t0}
	id := store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	eventually(t, "the first delivery", func() bool { return len(out.calls(canvasA)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := len(out.calls(canvasA)); got != 1 {
		t.Fatalf("deliveries before the backoff elapsed = %d, want 1", got)
	}
	now.Advance(10 * time.Millisecond)
	publisher.Notify()
	eventually(t, "settled", func() bool { return store.get(id).outcome == outbox.OutcomeDelivered })
	if got := len(out.calls(canvasA)); got != 2 {
		t.Fatalf("deliveries = %d, want 2", got)
	}
}
