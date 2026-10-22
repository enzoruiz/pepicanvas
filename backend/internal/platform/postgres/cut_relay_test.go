package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// signalingCuts reports the relay's store calls on buffered channels, so the
// test waits for a pass instead of sleeping. A full channel drops the signal
// rather than block the relay.
type signalingCuts struct {
	*postgres.SubscriptionStore
	reads   chan struct{}
	settles chan error
}

func (s signalingCuts) PendingCuts(ctx context.Context, limit int) ([]subscription.PendingCut, error) {
	cuts, err := s.SubscriptionStore.PendingCuts(ctx, limit)
	select {
	case s.reads <- struct{}{}:
	default:
	}
	return cuts, err
}

func (s signalingCuts) SettleCut(ctx context.Context, id int64, at time.Time) error {
	err := s.SubscriptionStore.SettleCut(ctx, id, at)
	select {
	case s.settles <- err:
	default:
	}
	return err
}

// countingCutter counts the cuts that reach the registry.
type countingCutter struct {
	registry *revocation.Registry
	mu       sync.Mutex
	cuts     []revocation.Cut
}

func (c *countingCutter) Apply(ctx context.Context, cut revocation.Cut) (revocation.Report, error) {
	c.mu.Lock()
	c.cuts = append(c.cuts, cut)
	c.mu.Unlock()
	return c.registry.Apply(ctx, cut)
}

func (c *countingCutter) applied() []revocation.Cut {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]revocation.Cut(nil), c.cuts...)
}

// closingConn records how the registry closed it.
type closingConn struct {
	mu      sync.Mutex
	reasons []revocation.Reason
}

func (c *closingConn) Close(_ context.Context, reason revocation.Reason) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, reason)
	return nil
}

func (c *closingConn) Abort() {}

func (c *closingConn) closed() []revocation.Reason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]revocation.Reason(nil), c.reasons...)
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func startRelay(t *testing.T, store signalingCuts, cutter subscription.Cutter) *subscription.CutRelay {
	t.Helper()
	relay, err := subscription.NewCutRelay(subscription.RelayConfig{Store: store, Cutter: cutter})
	if err != nil {
		t.Fatalf("NewCutRelay: %v", err)
	}
	if err := relay.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := relay.Close(ctx); err != nil {
			t.Errorf("relay Close: %v", err)
		}
	})
	return relay
}

// The process commits a deactivation and stops before any relay applied its
// cut; the next process applies it once at start (CTRL-CONS-01).
func TestRelayAppliesACutLeftPendingByACrashOnce(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	crashed := openSubscriptions(t, server, clock)
	operator, a, _ := streamer(t, crashed, "alice@example.com")
	pre, err := crashed.svc.EditorGrant(context.Background(), a.AccountID, "session-alice")
	if err != nil {
		t.Fatalf("EditorGrant: %v", err)
	}
	d := mustDeactivate(t, crashed, operator, "op-crash-deactivate1", "alice@example.com")
	crashed.pool.Close()

	restarted := openSubscriptions(t, server, clock)
	if rows := countDeactivationRows(t, restarted.pool); rows != (deactivationRows{1, 1, 1}) {
		t.Fatalf("rows after the crash = %+v, want the committed record and one pending cut", rows)
	}
	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// A client reconnecting with its pre-cut grant before the relay runs.
	conn := &closingConn{}
	lease, err := registry.Register(pre, conn)
	if err != nil {
		t.Fatalf("Register(pre-cut) before the relay: %v", err)
	}
	cutter := &countingCutter{registry: registry}
	store := signalingCuts{SubscriptionStore: restarted.store, reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	relay := startRelay(t, store, cutter)
	receive(t, store.reads, "the startup pass")
	if err := receive(t, store.settles, "the settle"); err != nil {
		t.Fatalf("SettleCut: %v", err)
	}

	cuts := cutter.applied()
	if len(cuts) != 1 || cuts[0].Type != revocation.TypeDeactivation || cuts[0].Target != (revocation.Target{Kind: revocation.TargetTenant, TenantID: a.TenantID}) ||
		cuts[0].Version != d.Version || cuts[0].CorrelationID != deactivationCorrelation {
		t.Fatalf("cuts = %+v, want one deactivation of A at %d", cuts, d.Version)
	}
	if !errors.Is(lease.Err(), revocation.ErrAccessRevoked) {
		t.Fatalf("lease Err = %v, want ErrAccessRevoked", lease.Err())
	}
	if reasons := conn.closed(); len(reasons) != 1 || reasons[0] != revocation.ReasonDeactivated {
		t.Fatalf("conn closed with %v, want subscription_inactive once", reasons)
	}
	if n := count(t, restarted.pool, "SELECT count(*) FROM access_cuts WHERE tenant_id = $1 AND version = $2 AND applied_at IS NOT NULL",
		a.TenantID, int64(d.Version)); n != 1 {
		t.Fatal("the recovered cut has no applied_at")
	}
	if rows := countDeactivationRows(t, restarted.pool); rows != (deactivationRows{1, 1, 0}) {
		t.Fatalf("rows = %+v, want the cut settled", rows)
	}
	if _, err := registry.Register(pre, &closingConn{}); !errors.Is(err, revocation.ErrAccessRevoked) {
		t.Fatalf("Register(pre-cut) after recovery = %v, want ErrAccessRevoked", err)
	}

	// Another pass finds nothing pending and applies nothing more.
	relay.Notify()
	receive(t, store.reads, "a second pass")
	if err := relay.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := signalingCuts{SubscriptionStore: postgres.NewSubscriptionStore(restarted.pool), reads: make(chan struct{}, 16), settles: make(chan error, 16)}
	startRelay(t, again, cutter)
	receive(t, again.reads, "the next startup pass")
	if n := len(cutter.applied()); n != 1 {
		t.Fatalf("cuts applied = %d, want still 1", n)
	}
	if reasons := conn.closed(); len(reasons) != 1 {
		t.Fatalf("conn closed %d times, want once", len(reasons))
	}
	if len(store.settles) != 0 || len(again.settles) != 0 {
		t.Fatal("a later pass settled a cut")
	}
}
