package locking_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
)

// recordingStore counts every Apply and answers with a fixed result.
type recordingStore struct {
	mu     sync.Mutex
	calls  []canvas.Mutation
	keys   []canvas.Key
	result canvas.Result
	err    error
}

func (s *recordingStore) Apply(_ context.Context, key canvas.Key, _ canvas.Operation, mutation canvas.Mutation) (canvas.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, mutation)
	s.keys = append(s.keys, key)
	return s.result, s.err
}

func (s *recordingStore) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

var canvasKey = canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

func operation(id string) canvas.Operation { return canvas.Operation{ID: id} }

func protectedMutations(object string) map[string]canvas.Mutation {
	return map[string]canvas.Mutation{
		"move/resize/rotate": canvas.SetTransform{ObjectID: object, Transform: canvas.Transform{X: 1, Y: 2, Width: 10, Height: 10, Rotation: 90}},
		"edit text":          canvas.SetContent{ObjectID: object, Content: "hello"},
		"delete":             canvas.DeleteObject{ObjectID: object},
	}
}

func wantRejected(t *testing.T, store *recordingStore, before int, err error, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
	if calls := store.Calls(); calls != before {
		t.Fatalf("%s: store called %d times, want %d (no commit)", what, calls, before)
	}
}

func TestProtectedObjectClassifiesMutations(t *testing.T) {
	for name, mutation := range protectedMutations("object-1") {
		if object, protected := locking.ProtectedObject(mutation); !protected || object != "object-1" {
			t.Fatalf("%s: ProtectedObject = %q, %v; want object-1, true", name, object, protected)
		}
	}
	for name, mutation := range map[string]canvas.Mutation{
		"create":     canvas.CreateObject{Kind: canvas.KindText, Content: "x", Transform: canvas.Transform{Width: 1, Height: 1}},
		"dimensions": canvas.SetDimensions{Dimensions: canvas.DefaultDimensions},
		"nil":        nil,
	} {
		if object, protected := locking.ProtectedObject(mutation); protected || object != "" {
			t.Fatalf("%s: ProtectedObject = %q, %v; want unprotected", name, object, protected)
		}
	}
}

func TestProtectedMutationsCommitOnlyWhileHoldingTheLease(t *testing.T) {
	for name, mutation := range protectedMutations("object-1") {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := newManager(t, locking.Config{})
				alice := holder("tenant-a", "alice", "s-alice")
				sa := attach(t, manager, context.Background(), alice)
				sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
				store := &recordingStore{result: canvas.Result{Revision: 7, ObjectID: "object-1"}}
				ctx := context.Background()

				_, err := locking.ApplyLocked(ctx, sa, store, canvasKey, operation("m-1"), mutation)
				wantRejected(t, store, 0, err, locking.ErrNotHolder, "without the lease")
				_, err = locking.ApplyLocked(ctx, nil, store, canvasKey, operation("m-2"), mutation)
				wantRejected(t, store, 0, err, locking.ErrNotHolder, "without a session")

				acquire(t, sb, request("op-b", "canvas-1", "object-1"))
				_, err = locking.ApplyLocked(ctx, sa, store, canvasKey, operation("m-3"), mutation)
				wantRejected(t, store, 0, err, locking.ErrNotHolder, "while another session holds it")
				if _, err := sb.Release(ctx, release("op-b2", "canvas-1", "object-1", locking.ReasonFinished)); err != nil {
					t.Fatalf("Release: %v", err)
				}

				acquire(t, sa, request("op-a", "canvas-1", "object-1"))
				got, err := locking.ApplyLocked(ctx, sa, store, canvasKey, operation("m-4"), mutation)
				if err != nil || got != store.result || store.Calls() != 1 {
					t.Fatalf("holder commit = %+v, %v with %d store calls; want %+v, nil, 1", got, err, store.Calls(), store.result)
				}
				if store.calls[0] != mutation || store.keys[0] != canvasKey {
					t.Fatalf("store got %+v on %v, want %+v on %v", store.calls[0], store.keys[0], mutation, canvasKey)
				}
				wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), alice)

				// S4: another participant edits a different object meanwhile.
				acquire(t, sb, request("op-b3", "canvas-1", "object-2"))
				other := protectedMutations("object-2")[name]
				if _, err := locking.ApplyLocked(ctx, sb, store, canvasKey, operation("m-5"), other); err != nil || store.Calls() != 2 {
					t.Fatalf("commit on a different object: err = %v, calls = %d; want nil, 2", err, store.Calls())
				}
				_, err = locking.ApplyLocked(ctx, sb, store, canvasKey, operation("m-6"), mutation)
				wantRejected(t, store, 2, err, locking.ErrNotHolder, "other participant on the locked object")

				if _, err := sa.Release(ctx, release("op-a2", "canvas-1", "object-1", locking.ReasonCancelled)); err != nil {
					t.Fatalf("Release: %v", err)
				}
				_, err = locking.ApplyLocked(ctx, sa, store, canvasKey, operation("m-7"), mutation)
				wantRejected(t, store, 2, err, locking.ErrNotHolder, "after the holder released")

				acquire(t, sa, request("op-a3", "canvas-1", "object-1"))
				time.Sleep(locking.DefaultTTL)
				_, err = locking.ApplyLocked(ctx, sa, store, canvasKey, operation("m-8"), mutation)
				wantRejected(t, store, 2, err, locking.ErrNotHolder, "after the lease expired")
				wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
			})
		})
	}
}

func TestStoreErrorsReturnUnchanged(t *testing.T) {
	manager := newManager(t, locking.Config{})
	sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
	acquire(t, sa, request("op-a", "canvas-1", "object-1"))
	store := &recordingStore{err: canvas.ErrNotFound}
	if _, err := locking.ApplyLocked(context.Background(), sa, store, canvasKey, operation("m-1"), canvas.DeleteObject{ObjectID: "object-1"}); err != canvas.ErrNotFound || store.Calls() != 1 {
		t.Fatalf("ApplyLocked: err = %v, calls = %d; want the store error unchanged, 1", err, store.Calls())
	}
}

func TestUnprotectedMutationsCommitWithoutALock(t *testing.T) {
	manager := newManager(t, locking.Config{})
	alice := holder("tenant-a", "alice", "s-alice")
	sa := attach(t, manager, context.Background(), alice)
	sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
	acquire(t, sa, request("op-a", "canvas-1", "object-1"))
	store := &recordingStore{result: canvas.Result{Revision: 3, ObjectID: "object-9", Z: 4}}

	mutations := []canvas.Mutation{
		canvas.CreateObject{Kind: canvas.KindText, Content: "x", Transform: canvas.Transform{Width: 1, Height: 1}},
		canvas.SetDimensions{Dimensions: canvas.DefaultDimensions},
	}
	for i, mutation := range mutations {
		for _, session := range []*locking.Session{nil, sb} {
			before := store.Calls()
			got, err := locking.ApplyLocked(context.Background(), session, store, canvasKey, operation(fmt.Sprint("m-", i)), mutation)
			if err != nil || got != store.result || store.Calls() != before+1 {
				t.Fatalf("%T with session %v = %+v, %v; want %+v committed once", mutation, session != nil, got, err, store.result)
			}
		}
	}
	wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), alice)
}

func TestInvalidMutationsNeverReachTheStore(t *testing.T) {
	manager := newManager(t, locking.Config{})
	sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
	store := &recordingStore{}
	// Pointer forms are not mutations; classifying one as unprotected must
	// not let it skip the lock.
	mutations := []canvas.Mutation{
		&canvas.SetTransform{ObjectID: "object-1", Transform: canvas.Transform{Width: 1, Height: 1}},
		&canvas.SetContent{ObjectID: "object-1", Content: "x"},
		&canvas.DeleteObject{ObjectID: "object-1"},
		canvas.DeleteObject{},
		nil,
	}
	for _, mutation := range mutations {
		for _, session := range []*locking.Session{nil, sa} {
			if _, err := locking.ApplyLocked(context.Background(), session, store, canvasKey, operation("op-1"), mutation); !errors.Is(err, canvas.ErrInvalid) {
				t.Fatalf("ApplyLocked(%#v): err = %v, want canvas.ErrInvalid", mutation, err)
			}
		}
	}
	if calls := store.Calls(); calls != 0 {
		t.Fatalf("store called %d times, want 0", calls)
	}
}

func TestInflightCommitKeepsTheObjectFromOtherSessions(t *testing.T) {
	endings := map[string]struct {
		end    func(t *testing.T, sa *locking.Session)
		reason locking.Reason
	}{
		"expiry": {func(t *testing.T, sa *locking.Session) {
			time.Sleep(locking.DefaultTTL - time.Second)
			synctest.Wait()
		}, locking.ReasonExpired},
		"release": {func(t *testing.T, sa *locking.Session) {
			if _, err := sa.Release(context.Background(), release("op-r", "canvas-1", "object-1", locking.ReasonFinished)); err != nil {
				t.Fatalf("Release during commit: %v", err)
			}
		}, locking.ReasonFinished},
		"detach": {func(t *testing.T, sa *locking.Session) { sa.Detach() }, locking.ReasonDetached},
	}
	for name, ending := range endings {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				manager := newManager(t, locking.Config{})
				alice := holder("tenant-a", "alice", "s-alice")
				bob := holder("tenant-a", "bob", "s-bob")
				sa := attach(t, manager, context.Background(), alice)
				sb := attach(t, manager, context.Background(), bob)
				k := key("tenant-a", "canvas-1", "object-1")
				granted := acquire(t, sa, request("op-a", "canvas-1", "object-1"))

				started := make(chan struct{})
				unblock := make(chan struct{})
				commitErr := errors.New("commit outcome")
				returned := make(chan error, 1)
				go func() {
					returned <- sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error {
						close(started)
						<-unblock
						return commitErr
					})
				}()
				<-started
				time.Sleep(time.Second)
				ending.end(t, sa)

				// The lease ended exactly; the fence keeps A the only effective holder.
				if lease, ok := manager.Lookup(k); !ok || lease.Holder != alice || !lease.Expires.Equal(granted.Lease.Expires) {
					t.Fatalf("Lookup during fenced commit = %+v, %v; want A's lease %+v", lease, ok, granted.Lease)
				}
				if leases := manager.Leases("tenant-a", "canvas-1"); len(leases) != 1 || leases[0].Holder != alice || leases[0].Key != k {
					t.Fatalf("Leases during fenced commit = %+v, want only A on %v", leases, k)
				}
				denied := acquire(t, sb, request("op-b1", "canvas-1", "object-1"))
				if denied.Outcome != locking.OutcomeDenied || denied.Lease.Holder != alice || denied.Lease.Key != k {
					t.Fatalf("Acquire by B during A's commit = %+v, want denied naming A", denied)
				}
				if name != "detach" {
					if err := sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error {
						t.Fatal("second commit ran after the lease ended")
						return nil
					}); !errors.Is(err, locking.ErrNotHolder) {
						t.Fatalf("Protect after the lease ended: err = %v, want ErrNotHolder", err)
					}
					renewal, err := sa.Heartbeat(context.Background())
					wantLost := locking.Loss{Key: k, Reason: ending.reason}
					if name == "release" {
						if err != nil || len(renewal.Lost) != 0 || len(renewal.Renewed) != 0 {
							t.Fatalf("Heartbeat after release = %+v, %v; want nothing renewed or lost", renewal, err)
						}
					} else if err != nil || len(renewal.Renewed) != 0 || len(renewal.Lost) != 1 || renewal.Lost[0] != wantLost {
						t.Fatalf("Heartbeat after the lease ended = %+v, %v; want lost %+v", renewal, err, wantLost)
					}
				}

				close(unblock)
				if err := <-returned; err != commitErr {
					t.Fatalf("Protect = %v, want the commit error unchanged", err)
				}
				wantFree(t, manager, k)
				if leases := manager.Leases("tenant-a", "canvas-1"); len(leases) != 0 {
					t.Fatalf("Leases after the commit = %+v, want none", leases)
				}
				if retried := acquire(t, sb, request("op-b1", "canvas-1", "object-1")); retried != denied {
					t.Fatalf("retried denied Acquire = %+v, want the retained %+v", retried, denied)
				}
				if got := acquire(t, sb, request("op-b2", "canvas-1", "object-1")); got.Outcome != locking.OutcomeGranted || got.Lease.Holder != bob {
					t.Fatalf("Acquire by B after the commit = %+v, want granted", got)
				}
			})
		})
	}
}

func TestFencedHolderFollowsTheNormalRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		sa := attach(t, manager, context.Background(), alice)
		acquire(t, sa, request("op-a", "canvas-1", "object-1"))
		unblock := make(chan struct{})
		started := make(chan struct{})
		returned := make(chan error, 1)
		go func() {
			returned <- sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error {
				close(started)
				<-unblock
				return nil
			})
		}()
		<-started
		time.Sleep(locking.DefaultTTL)
		synctest.Wait()

		again := acquire(t, sa, request("op-a2", "canvas-1", "object-1"))
		if again.Outcome != locking.OutcomeGranted || again.Lease.Holder != alice || !again.Lease.Expires.Equal(time.Now().Add(locking.DefaultTTL)) {
			t.Fatalf("A re-acquiring its fenced object = %+v, want granted until now+5s", again)
		}
		close(unblock)
		if err := <-returned; err != nil {
			t.Fatalf("Protect: %v", err)
		}
		wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), alice)
	})
}

func TestProtectDoesNotRenewTheLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		granted := acquire(t, sa, request("op-a", "canvas-1", "object-1"))
		time.Sleep(time.Second)
		if err := sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error { return nil }); err != nil {
			t.Fatalf("Protect: %v", err)
		}
		if lease, ok := manager.Lookup(key("tenant-a", "canvas-1", "object-1")); !ok || !lease.Expires.Equal(granted.Lease.Expires) {
			t.Fatalf("Lookup after Protect = %+v, %v; want the original deadline %v", lease, ok, granted.Lease.Expires)
		}
		time.Sleep(locking.DefaultTTL - time.Second)
		synctest.Wait()
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
	})
}

func TestConcurrentProtectedCommitsHaveOneEffectiveHolder(t *testing.T) {
	// A 1ms TTL makes leases end during commits, so the fence is exercised.
	manager := newManager(t, locking.Config{TTL: time.Millisecond})
	const contenders = 16
	const rounds = 50
	k := key("tenant-a", "canvas-1", "object-1")
	var active, commits atomic.Int32
	var wg sync.WaitGroup
	for i := range contenders {
		h := holder("tenant-a", fmt.Sprint("acct-", i), fmt.Sprint("sess-", i))
		session := attach(t, manager, context.Background(), h)
		wg.Go(func() {
			for round := range rounds {
				result, err := session.Acquire(context.Background(), request(fmt.Sprint("op-", round), "canvas-1", "object-1"))
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if result.Outcome != locking.OutcomeGranted {
					continue
				}
				err = session.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error {
					if n := active.Add(1); n != 1 {
						t.Errorf("%d commits on one object at once", n)
					}
					defer active.Add(-1)
					commits.Add(1)
					if lease, ok := manager.Lookup(k); !ok || lease.Holder != h {
						t.Errorf("commit of %v ran while the visible holder was %+v, %v", h, lease, ok)
					}
					if round%2 == 0 {
						time.Sleep(2 * time.Millisecond)
					}
					return nil
				})
				if err != nil && !errors.Is(err, locking.ErrNotHolder) {
					t.Errorf("Protect: %v", err)
				}
			}
		})
	}
	wg.Wait()
	if commits.Load() == 0 {
		t.Fatal("no commit ran")
	}
}

func TestLocksAreScopedToTheirTenant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		alice := holder("tenant-a", "alice", "s-alice")
		bob := holder("tenant-a", "bob", "s-bob")
		carol := holder("tenant-b", "carol", "s-carol")
		sa := attach(t, manager, context.Background(), alice)
		sb := attach(t, manager, context.Background(), bob)
		sc := attach(t, manager, context.Background(), carol)
		acquire(t, sa, request("op-1", "canvas-1", "object-2"))
		acquire(t, sb, request("op-2", "canvas-1", "object-1"))
		acquire(t, sa, request("op-3", "canvas-2", "object-1"))
		acquire(t, sc, request("op-4", "canvas-1", "object-1"))
		time.Sleep(time.Second)
		acquire(t, sa, request("op-5", "canvas-1", "object-3"))

		store := &recordingStore{}
		otherTenant := canvas.Key{TenantID: "tenant-b", CanvasID: "canvas-1"}
		_, err := locking.ApplyLocked(context.Background(), sb, store, otherTenant, operation("m-1"), canvas.DeleteObject{ObjectID: "object-1"})
		wantRejected(t, store, 0, err, locking.ErrNotHolder, "key of another tenant")
		wantHolder(t, manager, key("tenant-b", "canvas-1", "object-1"), carol)

		leaseKeys := func(leases []locking.Lease) []string {
			var keys []string
			for _, lease := range leases {
				keys = append(keys, lease.Key.TenantID+"/"+lease.Key.CanvasID+"/"+lease.Key.ObjectID+"="+lease.Holder.AccountID)
			}
			return keys
		}
		want := []string{"tenant-a/canvas-1/object-1=bob", "tenant-a/canvas-1/object-2=alice", "tenant-a/canvas-1/object-3=alice"}
		if got := leaseKeys(manager.Leases("tenant-a", "canvas-1")); !slices.Equal(got, want) {
			t.Fatalf("Leases(tenant-a, canvas-1) = %v, want %v", got, want)
		}
		if got := leaseKeys(manager.Leases("tenant-b", "canvas-1")); !slices.Equal(got, []string{"tenant-b/canvas-1/object-1=carol"}) {
			t.Fatalf("Leases(tenant-b, canvas-1) = %v, want only carol's", got)
		}
		if got := manager.Leases("tenant-c", "canvas-1"); len(got) != 0 {
			t.Fatalf("Leases(tenant-c) = %v, want none", got)
		}

		time.Sleep(locking.DefaultTTL - time.Second)
		want = []string{"tenant-a/canvas-1/object-3=alice"}
		if got := leaseKeys(manager.Leases("tenant-a", "canvas-1")); !slices.Equal(got, want) {
			t.Fatalf("Leases at the first deadline = %v, want %v", got, want)
		}
	})
}

func TestProtectAfterDetachAndDuringClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &countingClock{}
		manager := newManager(t, locking.Config{Clock: clock})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		sd := attach(t, manager, context.Background(), holder("tenant-a", "dave", "s-dave"))
		acquire(t, sa, request("op-a", "canvas-1", "object-1"))
		acquire(t, sd, request("op-d", "canvas-1", "object-2"))
		sd.Detach()
		ran := false
		if err := sd.Protect(context.Background(), "canvas-1", "object-2", func(context.Context) error { ran = true; return nil }); !errors.Is(err, locking.ErrDetached) || ran {
			t.Fatalf("Protect after Detach: err = %v, ran = %v; want ErrDetached without a commit", err, ran)
		}

		started := make(chan struct{})
		var finished atomic.Bool
		returned := make(chan error, 1)
		go func() {
			returned <- sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error {
				close(started)
				time.Sleep(100 * time.Millisecond)
				finished.Store(true)
				return nil
			})
		}()
		<-started
		start := time.Now()
		if err := manager.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond || !finished.Load() {
			t.Fatalf("Close returned after %v (commit finished %v), want after the 100ms commit", elapsed, finished.Load())
		}
		if err := <-returned; err != nil {
			t.Fatalf("in-flight Protect: %v", err)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
		if leases := manager.Leases("tenant-a", "canvas-1"); len(leases) != 0 {
			t.Fatalf("Leases after Close = %+v, want none", leases)
		}
		if err := sa.Protect(context.Background(), "canvas-1", "object-1", func(context.Context) error { ran = true; return nil }); !errors.Is(err, locking.ErrClosed) || ran {
			t.Fatalf("Protect after Close: err = %v, ran = %v; want ErrClosed without a commit", err, ran)
		}
		if pending := clock.Pending(); pending != 0 {
			t.Fatalf("%d timers still pending after Close", pending)
		}
	})
}

func TestCloseDeadlineCancelsAnInflightCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newManager(t, locking.Config{})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		acquire(t, sa, request("op-a", "canvas-1", "object-1"))
		started := make(chan struct{})
		returned := make(chan error, 1)
		go func() {
			returned <- sa.Protect(context.Background(), "canvas-1", "object-1", func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close: err = %v, want DeadlineExceeded", err)
		}
		if err := <-returned; !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight Protect: err = %v, want the commit's cancellation", err)
		}
	})
}

func TestProtectRejectsInvalidInputWithoutEffect(t *testing.T) {
	manager := newManager(t, locking.Config{})
	alice := holder("tenant-a", "alice", "s-alice")
	sa := attach(t, manager, context.Background(), alice)
	acquire(t, sa, request("op-a", "canvas-1", "object-1"))
	ran := false
	commit := func(context.Context) error { ran = true; return nil }
	for _, ids := range [][2]string{{"", "object-1"}, {"canvas-1", ""}, {"canvas 1", "object-1"}, {"canvas-1", string(make([]byte, locking.MaxIdentifierBytes+1))}} {
		if err := sa.Protect(context.Background(), ids[0], ids[1], commit); !errors.Is(err, locking.ErrInvalidRequest) || ran {
			t.Fatalf("Protect(%q, %q): err = %v, ran = %v; want ErrInvalidRequest without a commit", ids[0], ids[1], err, ran)
		}
	}
	if err := sa.Protect(context.Background(), "canvas-1", "object-1", nil); !errors.Is(err, locking.ErrInvalidRequest) {
		t.Fatalf("Protect with a nil commit: err = %v, want ErrInvalidRequest", err)
	}
	store := &recordingStore{}
	_, err := locking.ApplyLocked(context.Background(), sa, store, canvasKey, operation("m-1"), canvas.SetContent{ObjectID: "", Content: "x"})
	wantRejected(t, store, 0, err, canvas.ErrInvalid, "protected mutation without an object")
	wantHolder(t, manager, key("tenant-a", "canvas-1", "object-1"), alice)
	if leases := manager.Leases("tenant-a", "canvas-1"); len(leases) != 1 {
		t.Fatalf("Leases = %+v, want only the original lease", leases)
	}
}
