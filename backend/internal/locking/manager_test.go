package locking_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

func allowAll(context.Context, locking.Holder, locking.Key) error { return nil }

func newManager(t *testing.T, config locking.Config) *locking.Manager {
	t.Helper()
	if config.Authorize == nil {
		config.Authorize = allowAll
	}
	manager, err := locking.NewManager(config)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func holder(tenant, account, session string) locking.Holder {
	return locking.Holder{TenantID: tenant, AccountID: account, SessionID: session}
}

func attach(t *testing.T, manager *locking.Manager, ctx context.Context, h locking.Holder) *locking.Session {
	t.Helper()
	session, err := manager.Attach(ctx, h)
	if err != nil {
		t.Fatalf("Attach(%v): %v", h, err)
	}
	return session
}

func request(operation, canvas, object string) locking.Request {
	return locking.Request{OperationID: operation, CanvasID: canvas, ObjectID: object}
}

func release(operation, canvas, object string, reason locking.Reason) locking.Request {
	r := request(operation, canvas, object)
	r.Reason = reason
	return r
}

func acquire(t *testing.T, session *locking.Session, r locking.Request) locking.Result {
	t.Helper()
	result, err := session.Acquire(context.Background(), r)
	if err != nil {
		t.Fatalf("Acquire(%v): %v", r, err)
	}
	return result
}

func key(tenant, canvas, object string) locking.Key {
	return locking.Key{TenantID: tenant, CanvasID: canvas, ObjectID: object}
}

func wantHolder(t *testing.T, manager *locking.Manager, k locking.Key, want locking.Holder) {
	t.Helper()
	lease, ok := manager.Lookup(k)
	if !ok {
		t.Fatalf("Lookup(%v): free, want held by %v", k, want)
	}
	if lease.Holder != want || lease.Key != k {
		t.Fatalf("Lookup(%v) = %+v, want holder %v", k, lease, want)
	}
}

func wantFree(t *testing.T, manager *locking.Manager, k locking.Key) {
	t.Helper()
	if lease, ok := manager.Lookup(k); ok {
		t.Fatalf("Lookup(%v) = %+v, want free", k, lease)
	}
}

func TestAcquireGrantsOneHolderAndShowsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		bob := holder("tenant-a", "bob", "s-bob")
		sa := attach(t, manager, context.Background(), alice)
		sb := attach(t, manager, context.Background(), bob)
		k := key("tenant-a", "canvas-1", "object-1")
		start := time.Now()

		got := acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		want := locking.Result{
			OperationID: "op-1",
			Outcome:     locking.OutcomeGranted,
			Lease:       locking.Lease{Key: k, Holder: alice, Expires: start.Add(locking.DefaultTTL)},
		}
		if got != want {
			t.Fatalf("Acquire = %+v, want %+v", got, want)
		}

		denied := acquire(t, sb, request("op-2", "canvas-1", "object-1"))
		if denied.Outcome != locking.OutcomeDenied || denied.Lease.Holder != alice {
			t.Fatalf("contended Acquire = %+v, want denied naming %v", denied, alice)
		}
		wantHolder(t, manager, k, alice)

		again := acquire(t, sa, request("op-3", "canvas-1", "object-1"))
		if again.Outcome != locking.OutcomeGranted || again.Lease.Holder != alice {
			t.Fatalf("holder re-acquire = %+v, want granted", again)
		}
	})
}

func TestConcurrentAcquireGrantsExactlyOne(t *testing.T) {
	manager := newManager(t, locking.Config{})
	const contenders = 32
	sessions := make([]*locking.Session, contenders)
	for i := range sessions {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		sessions[i] = attach(t, manager, context.Background(), holder("tenant-a", "acct-"+id, "sess-"+id))
	}
	results := make([]locking.Result, contenders)
	var wg sync.WaitGroup
	for i, session := range sessions {
		wg.Go(func() { results[i] = acquire(t, session, request("op", "canvas-1", "object-1")) })
	}
	wg.Wait()

	var winner locking.Holder
	granted := 0
	for _, result := range results {
		if result.Outcome == locking.OutcomeGranted {
			granted++
			winner = result.Lease.Holder
		}
	}
	if granted != 1 {
		t.Fatalf("granted %d leases, want exactly 1", granted)
	}
	for _, result := range results {
		if result.Lease.Holder != winner {
			t.Fatalf("result %+v names %v, want every answer to name winner %v", result, result.Lease.Holder, winner)
		}
	}
	wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), winner)
}

func TestLeaseExpiresExactlyFiveSecondsAfterLastAcceptedHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		sa := attach(t, manager, context.Background(), alice)
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
		k := key("tenant-a", "canvas-1", "object-1")
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		time.Sleep(4 * time.Second)
		renewal, err := sa.Heartbeat(context.Background())
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if len(renewal.Renewed) != 1 || !renewal.Renewed[0].Expires.Equal(time.Now().Add(5*time.Second)) || len(renewal.Lost) != 0 {
			t.Fatalf("Heartbeat = %+v, want one lease renewed to now+5s", renewal)
		}

		time.Sleep(5*time.Second - time.Nanosecond)
		synctest.Wait()
		wantHolder(t, manager, k, alice)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		wantFree(t, manager, k)
		if got := acquire(t, sb, request("op-2", "canvas-1", "object-1")); got.Outcome != locking.OutcomeGranted {
			t.Fatalf("Acquire after expiry = %+v, want granted", got)
		}

		renewal, err = sa.Heartbeat(context.Background())
		if err != nil {
			t.Fatalf("late Heartbeat: %v", err)
		}
		wantLost := []locking.Loss{{Key: k, Reason: locking.ReasonExpired}}
		if len(renewal.Renewed) != 0 || len(renewal.Lost) != 1 || renewal.Lost[0] != wantLost[0] {
			t.Fatalf("late Heartbeat = %+v, want lost %+v", renewal, wantLost)
		}
	})
}

func TestHeartbeatAtTheDeadlineIsNotAccepted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		k := key("tenant-a", "canvas-1", "object-1")
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		time.Sleep(locking.DefaultTTL)
		renewal, err := sa.Heartbeat(context.Background())
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if len(renewal.Renewed) != 0 || len(renewal.Lost) != 1 || renewal.Lost[0].Reason != locking.ReasonExpired {
			t.Fatalf("Heartbeat at deadline = %+v, want the lease lost as expired", renewal)
		}
		wantFree(t, manager, k)
	})
}

func TestFinishAndCancelReleaseAtOnce(t *testing.T) {
	for _, reason := range []locking.Reason{locking.ReasonFinished, locking.ReasonCancelled} {
		t.Run(string(reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := newManager(t, locking.Config{})
				alice := holder("tenant-a", "alice", "s-alice")
				sa := attach(t, manager, context.Background(), alice)
				sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
				k := key("tenant-a", "canvas-1", "object-1")
				acquire(t, sa, request("op-1", "canvas-1", "object-1"))

				if _, err := sb.Release(context.Background(), release("op-x", "canvas-1", "object-1", reason)); !errors.Is(err, locking.ErrNotHolder) {
					t.Fatalf("Release by non-holder: err = %v, want ErrNotHolder", err)
				}
				wantHolder(t, manager, k, alice)

				got, err := sa.Release(context.Background(), release("op-2", "canvas-1", "object-1", reason))
				if err != nil {
					t.Fatalf("Release: %v", err)
				}
				want := locking.Result{OperationID: "op-2", Outcome: locking.OutcomeReleased, Reason: reason,
					Lease: locking.Lease{Key: k, Holder: alice, Expires: time.Now().Add(locking.DefaultTTL)}}
				if got != want {
					t.Fatalf("Release = %+v, want %+v", got, want)
				}
				wantFree(t, manager, k)
				if got := acquire(t, sb, request("op-3", "canvas-1", "object-1")); got.Outcome != locking.OutcomeGranted {
					t.Fatalf("Acquire after release = %+v, want granted", got)
				}
				renewal, err := sa.Heartbeat(context.Background())
				if err != nil || len(renewal.Renewed) != 0 || len(renewal.Lost) != 0 {
					t.Fatalf("Heartbeat after release = %+v, %v; want nothing renewed or lost", renewal, err)
				}
			})
		})
	}
}

func TestReleaseRejectsAnUnknownReason(t *testing.T) {
	manager := newManager(t, locking.Config{})
	sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
	acquire(t, sa, request("op-1", "canvas-1", "object-1"))
	for _, reason := range []locking.Reason{"", locking.ReasonExpired, "other"} {
		if _, err := sa.Release(context.Background(), release("op-2", "canvas-1", "object-1", reason)); !errors.Is(err, locking.ErrInvalidRequest) {
			t.Fatalf("Release with reason %q: err = %v, want ErrInvalidRequest", reason, err)
		}
	}
	wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), holder("tenant-a", "alice", "s-alice"))
}

func TestDisconnectReleasesEveryLeaseAndReconnectDoesNotRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		conn, disconnect := context.WithCancel(context.Background())
		sa := attach(t, manager, conn, alice)
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		acquire(t, sa, request("op-2", "canvas-1", "object-2"))
		start := time.Now()

		disconnect()
		synctest.Wait()
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("release took %v, want immediate", elapsed)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-2"))
		if _, err := sa.Heartbeat(context.Background()); !errors.Is(err, locking.ErrDetached) {
			t.Fatalf("Heartbeat after disconnect: err = %v, want ErrDetached", err)
		}
		if _, err := sa.Acquire(context.Background(), request("op-3", "canvas-1", "object-1")); !errors.Is(err, locking.ErrDetached) {
			t.Fatalf("Acquire after disconnect: err = %v, want ErrDetached", err)
		}

		reconnected := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice-2"))
		renewal, err := reconnected.Heartbeat(context.Background())
		if err != nil || len(renewal.Renewed) != 0 || len(renewal.Lost) != 0 {
			t.Fatalf("Heartbeat after reconnect = %+v, %v; want no restored lease", renewal, err)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))

		if _, err := manager.Attach(conn, holder("tenant-a", "alice", "s-alice-3")); !errors.Is(err, locking.ErrDetached) {
			t.Fatalf("Attach with an ended connection: err = %v, want ErrDetached", err)
		}
	})
}

func TestDetachReleasesEveryLease(t *testing.T) {
	manager := newManager(t, locking.Config{})
	sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
	acquire(t, sa, request("op-1", "canvas-1", "object-1"))
	sa.Detach()
	sa.Detach()
	wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
	if _, err := sa.Release(context.Background(), release("op-2", "canvas-1", "object-1", locking.ReasonFinished)); !errors.Is(err, locking.ErrDetached) {
		t.Fatalf("Release after Detach: err = %v, want ErrDetached", err)
	}
}

type quietConn struct{}

func (quietConn) Close(context.Context, revocation.Reason) error { return nil }
func (quietConn) Abort()                                         {}

func TestAccessCutReleasesLeasesBeforeTheCutReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry, err := revocation.NewRegistry(revocation.Config{})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		grant := revocation.Grant{TenantID: "tenant-a", AccountID: "alice", SessionID: "s-alice", Client: revocation.ClientEditor, Version: 1}
		connLease, err := registry.Register(grant, quietConn{})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, connLease.Context(), holder("tenant-a", "alice", "s-alice"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		start := time.Now()

		cut := revocation.Cut{OperationID: "cut-1", Type: revocation.TypeRevocation,
			Target: revocation.Target{Kind: revocation.TargetSession, TenantID: "tenant-a", ID: "s-alice"}, Version: 2}
		if _, err := registry.Apply(context.Background(), cut); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		synctest.Wait()
		if elapsed := time.Since(start); elapsed >= revocation.DefaultBudget {
			t.Fatalf("release took %v, want inside the %v cut budget", elapsed, revocation.DefaultBudget)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
		if _, err := sa.Heartbeat(context.Background()); !errors.Is(err, locking.ErrDetached) {
			t.Fatalf("Heartbeat after cut: err = %v, want ErrDetached", err)
		}
	})
}

func TestAuthorizationIsReevaluatedAndFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		denied := errors.New("denied")
		var mu sync.Mutex
		allowed := true
		manager := newManager(t, locking.Config{Authorize: func(_ context.Context, h locking.Holder, k locking.Key) error {
			mu.Lock()
			defer mu.Unlock()
			if !allowed || h.TenantID != k.TenantID {
				return denied
			}
			return nil
		}})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		k := key("tenant-a", "canvas-1", "object-1")
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		mu.Lock()
		allowed = false
		mu.Unlock()
		if _, err := sa.Acquire(context.Background(), request("op-2", "canvas-1", "object-2")); !errors.Is(err, denied) {
			t.Fatalf("unauthorized Acquire: err = %v, want the authorizer error", err)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-2"))

		renewal, err := sa.Heartbeat(context.Background())
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if len(renewal.Renewed) != 0 || len(renewal.Lost) != 1 || renewal.Lost[0] != (locking.Loss{Key: k, Reason: locking.ReasonUnauthorized}) {
			t.Fatalf("unauthorized Heartbeat = %+v, want the lease lost as unauthorized", renewal)
		}
		wantFree(t, manager, k)
	})
}

func TestNewManagerRequiresAnAuthorizer(t *testing.T) {
	for name, config := range map[string]locking.Config{
		"no authorizer": {},
		"negative ttl":  {Authorize: allowAll, TTL: -time.Second},
	} {
		if _, err := locking.NewManager(config); !errors.Is(err, locking.ErrInvalidConfig) {
			t.Fatalf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
}

func TestInvalidInputHasNoEffect(t *testing.T) {
	manager := newManager(t, locking.Config{})
	for _, h := range []locking.Holder{
		{AccountID: "alice", SessionID: "s"},
		{TenantID: "tenant-a", SessionID: "s"},
		{TenantID: "tenant-a", AccountID: "alice"},
		{TenantID: "tenant a", AccountID: "alice", SessionID: "s"},
	} {
		if _, err := manager.Attach(context.Background(), h); !errors.Is(err, locking.ErrInvalidRequest) {
			t.Fatalf("Attach(%+v): err = %v, want ErrInvalidRequest", h, err)
		}
	}
	sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
	for _, r := range []locking.Request{
		request("", "canvas-1", "object-1"),
		request("op", "", "object-1"),
		request("op", "canvas-1", ""),
		request("op", "canvas-1", string(make([]byte, locking.MaxIdentifierBytes+1))),
	} {
		if _, err := sa.Acquire(context.Background(), r); !errors.Is(err, locking.ErrInvalidRequest) {
			t.Fatalf("Acquire(%+v): err = %v, want ErrInvalidRequest", r, err)
		}
	}
	wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
}

func TestTenantsAndObjectsAreIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		aliceA := holder("tenant-a", "alice", "s-alice")
		bobA := holder("tenant-a", "bob", "s-bob")
		carolB := holder("tenant-b", "carol", "s-carol")
		sa := attach(t, manager, context.Background(), aliceA)
		sb := attach(t, manager, context.Background(), bobA)
		sc := attach(t, manager, context.Background(), carolB)

		for _, step := range []struct {
			session *locking.Session
			object  string
		}{{sa, "object-1"}, {sb, "object-2"}, {sc, "object-1"}} {
			if got := acquire(t, step.session, request("op-"+step.object, "canvas-1", step.object)); got.Outcome != locking.OutcomeGranted {
				t.Fatalf("Acquire %s = %+v, want granted", step.object, got)
			}
		}
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), aliceA)
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-2"), bobA)
		wantHolder(t, manager, key("tenant-b", "canvas-1", "object-1"), carolB)

		if _, err := sc.Release(context.Background(), release("op-r", "canvas-1", "object-2", locking.ReasonFinished)); !errors.Is(err, locking.ErrNotHolder) {
			t.Fatalf("cross-tenant Release: err = %v, want ErrNotHolder", err)
		}

		time.Sleep(3 * time.Second)
		if _, err := sb.Heartbeat(context.Background()); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
		wantFree(t, manager, key("tenant-b", "canvas-1", "object-1"))
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-2"), bobA)
	})
}
