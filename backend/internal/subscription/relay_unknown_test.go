package subscription_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// orderedCuts is a cut store whose pending cuts, oldest first, are the
// current view minus the settled ones. A test changes the view between
// passes to choose which cuts a pass reads and in which order.
type orderedCuts struct {
	mu      sync.Mutex
	view    []subscription.PendingCut
	settled map[int64]bool
}

func newOrderedCuts(view ...subscription.PendingCut) *orderedCuts {
	return &orderedCuts{view: view, settled: map[int64]bool{}}
}

func (s *orderedCuts) PendingCuts(_ context.Context, limit int) ([]subscription.PendingCut, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []subscription.PendingCut
	for _, cut := range s.view {
		if !s.settled[cut.ID] && len(pending) < limit {
			pending = append(pending, cut)
		}
	}
	return pending, nil
}

func (s *orderedCuts) SettleCut(_ context.Context, id int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled[id] = true
	return nil
}

func (s *orderedCuts) setView(view ...subscription.PendingCut) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view = view
}

func (s *orderedCuts) isSettled(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled[id]
}

func deactivationCut(id int64, tenantID string) subscription.PendingCut {
	return subscription.PendingCut{ID: id, Kind: subscription.CutDeactivation, AccessCut: subscription.AccessCut{
		TenantID: tenantID, Version: 2, CorrelationID: cutCorrelation,
	}}
}

func unknownCut(id int64) subscription.PendingCut {
	return subscription.PendingCut{ID: id, Kind: "suspension", AccessCut: subscription.AccessCut{
		TenantID: "tenant-unknown", Version: 2, CorrelationID: cutCorrelation,
	}}
}

// A pass that returns early, because an earlier cut failed or the relay is
// stopping, keeps the unknown cuts already logged: each is logged and
// counted once, however often the passes before it fail.
func TestAnUnknownCutIsLoggedOnceAcrossPassesThatReturnEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unknown := unknownCut(5)
		store := newOrderedCuts(unknown)
		live := newRig(t, store, nil)
		live.start(t)
		requireCounters(t, live.relay, subscription.RelayCounters{Unknown: 1})

		// A cut committed later with a lower ID now leads the batch, and its
		// first two applies fail, so two passes return before the unknown cut.
		failing := deactivationCut(3, "tenant-late")
		live.cutter.mu.Lock()
		live.cutter.script = []error{errors.New("apply failed"), errors.New("apply failed")}
		live.cutter.mu.Unlock()
		store.setView(failing, unknown)
		for range 3 {
			live.relay.Notify()
			synctest.Wait()
			time.Sleep(10 * time.Second)
			synctest.Wait()
		}

		if !store.isSettled(failing.ID) {
			t.Fatal("the failing cut was not applied once its retries succeeded")
		}
		if store.isSettled(unknown.ID) {
			t.Fatal("the unknown cut was settled")
		}
		requireCounters(t, live.relay, subscription.RelayCounters{Applied: 1, Retries: 2, Unknown: 1})
		if found := recordsWith(live.logs(), "access cut kind unknown"); len(found) != 1 {
			t.Fatalf("unknown records = %d, want 1", len(found))
		}
	})
}

// A full batch led by an unknown cut still delivers its known cuts and
// reads the next batch at once, so a backlog drains back to back.
func TestABacklogBehindAnUnknownCutDrainsWithoutWaitingForThePoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backlog := []subscription.PendingCut{unknownCut(1)}
		for id := int64(2); id <= 7; id++ {
			backlog = append(backlog, deactivationCut(id, "tenant-backlog"))
		}
		store := newOrderedCuts(backlog...)
		live := newRig(t, store, func(c *subscription.RelayConfig) {
			c.BatchSize = 2
			c.PollInterval = time.Hour
		})
		started := time.Now()
		live.start(t)

		for _, cut := range backlog[1:] {
			if !store.isSettled(cut.ID) {
				t.Fatalf("cut %d waited for the poll", cut.ID)
			}
		}
		if elapsed := time.Since(started); elapsed != 0 {
			t.Fatalf("the backlog took %v, want no wait", elapsed)
		}
		requireCounters(t, live.relay, subscription.RelayCounters{Applied: 6, Unknown: 1})
	})
}

// A full batch of unknown cuts cannot make progress: the known cut behind
// it waits (the accepted limit), and the stall is counted and logged once
// until a batch delivers again.
func TestAFullBatchOfUnknownCutsIsReportedAsStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		known := deactivationCut(3, "tenant-behind")
		store := newOrderedCuts(unknownCut(1), unknownCut(2), known)
		live := newRig(t, store, func(c *subscription.RelayConfig) {
			c.BatchSize = 2
			c.PollInterval = time.Second
		})
		live.start(t)
		time.Sleep(3500 * time.Millisecond)
		synctest.Wait()

		if store.isSettled(known.ID) {
			t.Fatal("the cut behind a full batch of unknown cuts was applied")
		}
		if got := live.relay.Counters(); got.Stalled < 4 || got.Unknown != 2 || got.Applied != 0 {
			t.Fatalf("Counters = %+v, want at least 4 stalled passes, 2 unknown, none applied", got)
		}
		stalls := recordsWith(live.logs(), "access cut batch holds only unknown kinds")
		if len(stalls) != 1 || stalls[0]["level"] != "WARN" {
			t.Fatalf("stall records = %v, want one WARN record", stalls)
		}

		// Once a batch delivers again, a later stall is logged anew.
		store.setView(known)
		live.relay.Notify()
		synctest.Wait()
		if !store.isSettled(known.ID) {
			t.Fatal("the known cut was not applied once it led a batch")
		}
		store.setView(unknownCut(1), unknownCut(2))
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		if stalls := recordsWith(live.logs(), "access cut batch holds only unknown kinds"); len(stalls) != 2 {
			t.Fatalf("stall records = %d, want 2", len(stalls))
		}
	})
}
