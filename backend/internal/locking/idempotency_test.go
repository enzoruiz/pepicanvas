package locking_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
)

func TestAcquireRetryAfterReconnectReturnsTheSameAnswerOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		conn, disconnect := context.WithCancel(context.Background())
		sa := attach(t, manager, conn, alice)
		first := acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		time.Sleep(time.Second)
		if retry := acquire(t, sa, request("op-1", "canvas-1", "object-1")); retry != first {
			t.Fatalf("retry before timeout = %+v, want %+v", retry, first)
		}
		if lease, _ := manager.Lookup(key("tenant-a", "canvas-1", "object-1")); !lease.Expires.Equal(first.Lease.Expires) {
			t.Fatalf("retry renewed the lease to %v, want the single effect expiring at %v", lease.Expires, first.Lease.Expires)
		}

		disconnect()
		synctest.Wait()
		reconnected := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice-2"))
		if retry := acquire(t, reconnected, request("op-1", "canvas-1", "object-1")); retry != first {
			t.Fatalf("retry after reconnect = %+v, want %+v", retry, first)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
	})
}

func TestReleaseRetryReturnsTheSameAnswerOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		r := release("op-2", "canvas-1", "object-1", locking.ReasonFinished)
		first, err := sa.Release(context.Background(), r)
		if err != nil {
			t.Fatalf("Release: %v", err)
		}
		acquire(t, sb, request("op-3", "canvas-1", "object-1"))

		retry, err := sa.Release(context.Background(), r)
		if err != nil || retry != first {
			t.Fatalf("Release retry = %+v, %v; want %+v", retry, err, first)
		}
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), holder("tenant-a", "bob", "s-bob"))
	})
}

func TestDeniedAcquireIsRetainedLikeAnyAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		denied := acquire(t, sb, request("op-2", "canvas-1", "object-1"))
		sa.Detach()

		if retry := acquire(t, sb, request("op-2", "canvas-1", "object-1")); retry != denied {
			t.Fatalf("denied retry = %+v, want %+v", retry, denied)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
		if fresh := acquire(t, sb, request("op-3", "canvas-1", "object-1")); fresh.Outcome != locking.OutcomeGranted {
			t.Fatalf("new operation = %+v, want granted", fresh)
		}
	})
}

func TestReusedOperationIDWithADifferentIntentIsRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		sa := attach(t, manager, context.Background(), alice)
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		for name, call := range map[string]func() error{
			"other object": func() error {
				_, err := sa.Acquire(context.Background(), request("op-1", "canvas-1", "object-2"))
				return err
			},
			"other canvas": func() error {
				_, err := sa.Acquire(context.Background(), request("op-1", "canvas-2", "object-1"))
				return err
			},
			"release": func() error {
				_, err := sa.Release(context.Background(), release("op-1", "canvas-1", "object-1", locking.ReasonFinished))
				return err
			},
		} {
			if err := call(); !errors.Is(err, locking.ErrOperationConflict) {
				t.Fatalf("%s: err = %v, want ErrOperationConflict", name, err)
			}
		}
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), alice)
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-2"))
		wantFree(t, manager, key("tenant-a", "canvas-2", "object-1"))

		acquire(t, sa, request("op-2", "canvas-1", "object-3"))
		if _, err := sa.Release(context.Background(), release("op-3", "canvas-1", "object-3", locking.ReasonFinished)); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if _, err := sa.Release(context.Background(), release("op-3", "canvas-1", "object-3", locking.ReasonCancelled)); !errors.Is(err, locking.ErrOperationConflict) {
			t.Fatalf("release with another reason: err = %v, want ErrOperationConflict", err)
		}
	})
}

func TestOperationIDsAreScopedToTheAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
		sc := attach(t, manager, context.Background(), holder("tenant-b", "alice", "s-alice-b"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		if got := acquire(t, sb, request("op-1", "canvas-1", "object-2")); got.Outcome != locking.OutcomeGranted {
			t.Fatalf("other account reusing op-1 = %+v, want its own grant", got)
		}
		if got := acquire(t, sc, request("op-1", "canvas-1", "object-1")); got.Outcome != locking.OutcomeGranted || got.Lease.Holder.TenantID != "tenant-b" {
			t.Fatalf("other tenant reusing op-1 = %+v, want its own grant", got)
		}
	})
}

func TestRetentionExpiresAtItsTTLAndCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{Retention: locking.Retention{TTL: time.Minute, MaxOperations: 2}})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		first := acquire(t, sa, request("op-1", "canvas-1", "object-1"))

		time.Sleep(time.Minute - time.Nanosecond)
		if retry := acquire(t, sa, request("op-1", "canvas-1", "object-1")); retry != first {
			t.Fatalf("retry inside retention = %+v, want %+v", retry, first)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		renewed := acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		if renewed.Outcome != locking.OutcomeGranted || !renewed.Lease.Expires.Equal(time.Now().Add(locking.DefaultTTL)) {
			t.Fatalf("op-1 after retention = %+v, want a new operation", renewed)
		}
		if _, err := sa.Acquire(context.Background(), request("op-1", "canvas-1", "object-9")); !errors.Is(err, locking.ErrOperationConflict) {
			t.Fatalf("op-1 restarted retention: err = %v, want ErrOperationConflict", err)
		}

		acquire(t, sa, request("op-2", "canvas-1", "object-2"))
		acquire(t, sa, request("op-3", "canvas-1", "object-3"))
		if got := acquire(t, sa, request("op-1", "canvas-1", "object-9")); got.Outcome != locking.OutcomeGranted || got.Lease.Key.ObjectID != "object-9" {
			t.Fatalf("op-1 after count eviction = %+v, want a new operation on object-9", got)
		}
	})
}

func TestUnauthorizedAndInvalidRequestsAreNotRetained(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		denied := errors.New("denied")
		allowed := false
		manager := newManager(t, locking.Config{Authorize: func(context.Context, locking.Holder, locking.Key) error {
			if !allowed {
				return denied
			}
			return nil
		}})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		if _, err := sa.Acquire(context.Background(), request("op-1", "canvas-1", "object-1")); !errors.Is(err, denied) {
			t.Fatalf("unauthorized Acquire: err = %v, want denied", err)
		}
		if _, err := sa.Release(context.Background(), release("op-2", "canvas-1", "object-1", locking.ReasonFinished)); !errors.Is(err, locking.ErrNotHolder) {
			t.Fatalf("Release: err = %v, want ErrNotHolder", err)
		}
		allowed = true
		if got := acquire(t, sa, request("op-1", "canvas-1", "object-1")); got.Outcome != locking.OutcomeGranted {
			t.Fatalf("authorized retry = %+v, want granted", got)
		}
		if _, err := sa.Release(context.Background(), release("op-2", "canvas-1", "object-1", locking.ReasonFinished)); err != nil {
			t.Fatalf("Release retry: %v", err)
		}
	})
}

func TestNewManagerRejectsNegativeRetention(t *testing.T) {
	for _, retention := range []locking.Retention{{TTL: -time.Second}, {MaxOperations: -1}} {
		if _, err := locking.NewManager(locking.Config{Authorize: allowAll, Retention: retention}); !errors.Is(err, locking.ErrInvalidConfig) {
			t.Fatalf("Retention %+v: err = %v, want ErrInvalidConfig", retention, err)
		}
	}
}
