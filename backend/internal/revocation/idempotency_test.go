package revocation

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestRetriedCutClosesOnceAndReturnsTheSameReport(t *testing.T) {
	cases := map[string]Cut{
		"revocation":   accountRevocation("op-1", "tenant-a", "acct-1", 10),
		"deactivation": deactivation("op-1", "tenant-a", 10),
		"rotation":     rotation("op-1", "tenant-a", "cred-1", 10),
	}
	for name, cut := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				registry := newRegistry(t)
				editor := newConn(200 * time.Millisecond)
				obs := newConn(200 * time.Millisecond)
				register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), editor)
				register(t, registry, obsGrant("tenant-a", "cred-1", 5), obs)

				first := mustApply(t, registry, cut)
				// A connection admitted after the cut must not be closed by a retry.
				late := newConn(0)
				register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 10), late)
				retry := cut
				retry.CorrelationID = "retry-over-new-connection"
				second := mustApply(t, registry, retry)

				if !reflect.DeepEqual(first, second) {
					t.Fatalf("retry report = %+v, want %+v", second, first)
				}
				for _, conn := range []*fakeConn{editor, obs, late} {
					if _, closes, _ := conn.state(); closes > 1 {
						t.Fatalf("connection closed %d times, want at most once", closes)
					}
				}
				if _, closes, _ := late.state(); closes != 0 {
					t.Fatal("retry closed a connection admitted after the cut")
				}
			})
		})
	}
}

func TestConcurrentRetriesProduceOneCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := newConn(time.Second)
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)

		cut := rotation("op-1", "tenant-a", "cred-1", 10)
		reports := make([]Report, 5)
		var wg sync.WaitGroup
		for i := range reports {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reports[i] = mustApply(t, registry, cut)
			}()
		}
		wg.Wait()

		if _, closes, _ := conn.state(); closes != 1 {
			t.Fatalf("closes = %d, want 1", closes)
		}
		for _, report := range reports {
			if !reflect.DeepEqual(report, reports[0]) || len(report.Closures) != 1 {
				t.Fatalf("report = %+v, want %+v with one closure", report, reports[0])
			}
		}
	})
}

func TestRetryReturnsTheSameBudgetFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := newConn(-1)
		conn.hangAbort = true
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)
		cut := rotation("op-1", "tenant-a", "cred-1", 10)

		first, err := registry.Apply(context.Background(), cut)
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("first Apply error = %v, want ErrBudgetExceeded", err)
		}
		second, err := registry.Apply(context.Background(), cut)
		if !errors.Is(err, ErrBudgetExceeded) || !reflect.DeepEqual(first, second) {
			t.Fatalf("retry = %+v, %v; want %+v, ErrBudgetExceeded", second, err, first)
		}
	})
}

func TestOperationIDReusedWithADifferentCutIsRejected(t *testing.T) {
	registry := newRegistry(t)
	mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))
	conn := newConn(0)
	lease := register(t, registry, editorGrant("tenant-a", "acct-2", "sess-2", 5), conn)

	conflicts := []Cut{
		accountRevocation("op-1", "tenant-a", "acct-2", 10),
		accountRevocation("op-1", "tenant-a", "acct-1", 11),
		sessionRevocation("op-1", "tenant-a", "acct-1", 10),
		deactivation("op-1", "tenant-a", 10),
	}
	for _, cut := range conflicts {
		if _, err := registry.Apply(context.Background(), cut); !errors.Is(err, ErrOperationConflict) {
			t.Fatalf("Apply(%+v) error = %v, want ErrOperationConflict", cut, err)
		}
	}
	if _, closes, _ := conn.state(); closes != 0 || lease.Err() != nil {
		t.Fatalf("conflicting cuts had effect: closes=%d err=%v", closes, lease.Err())
	}
	register(t, registry, editorGrant("tenant-a", "acct-2", "sess-2", 6), newConn(0))

	// The same operation ID in another tenant is a different operation.
	mustApply(t, registry, deactivation("op-1", "tenant-b", 10))
}

func TestRetainedCutResultExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	registry, err := NewRegistry(Config{Retention: Retention{TTL: time.Minute}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))

	now = now.Add(59 * time.Second)
	if _, err := registry.Apply(context.Background(), accountRevocation("op-1", "tenant-a", "acct-1", 11)); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("before expiry: Apply error = %v, want ErrOperationConflict", err)
	}
	now = now.Add(time.Second)
	if _, err := registry.Apply(context.Background(), accountRevocation("op-1", "tenant-a", "acct-1", 11)); err != nil {
		t.Fatalf("after expiry: Apply error = %v, want a new operation", err)
	}
}

func TestRetentionIsBoundedByCount(t *testing.T) {
	registry, err := NewRegistry(Config{Retention: Retention{MaxOperations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))
	mustApply(t, registry, accountRevocation("op-2", "tenant-a", "acct-1", 11))
	if _, err := registry.Apply(context.Background(), accountRevocation("op-1", "tenant-a", "acct-1", 12)); err != nil {
		t.Fatalf("evicted op-1: Apply error = %v, want a new operation", err)
	}
	if _, err := registry.Apply(context.Background(), accountRevocation("op-1", "tenant-a", "acct-1", 13)); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("retained op-1: Apply error = %v, want ErrOperationConflict", err)
	}
}

func TestInvalidRetentionIsRejected(t *testing.T) {
	for name, config := range map[string]Config{
		"negative ttl":   {Retention: Retention{TTL: -time.Second}},
		"negative count": {Retention: Retention{MaxOperations: -1}},
		"ttl below budget": {
			Budget:    2 * time.Second,
			Retention: Retention{TTL: time.Second},
		},
	} {
		if _, err := NewRegistry(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s: NewRegistry error = %v, want ErrInvalidConfig", name, err)
		}
	}
}
