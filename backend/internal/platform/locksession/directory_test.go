package locksession_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/locksession"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// Inside a synctest bubble time is virtual, so every idle deadline is exact,
// and the bubble fails if a goroutine or timer outlives the test.

// lockTTL outlives every idle deadline, so only the directory frees a lock.
const lockTTL = time.Minute

type fixture struct {
	manager   *locking.Manager
	registry  *revocation.Registry
	directory *locksession.Directory
}

func newFixture(t *testing.T, config locksession.Config) fixture {
	t.Helper()
	manager, err := locking.NewManager(locking.Config{
		TTL:       lockTTL,
		Authorize: func(context.Context, locking.Holder, locking.Key) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	config.Manager = manager
	config.Registry = registry
	directory, err := locksession.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := directory.Close(ctx); err != nil {
			t.Errorf("directory Close: %v", err)
		}
		if err := manager.Close(ctx); err != nil {
			t.Errorf("manager Close: %v", err)
		}
		if err := registry.Close(ctx); err != nil {
			t.Errorf("registry Close: %v", err)
		}
	})
	return fixture{manager: manager, registry: registry, directory: directory}
}

func grant(tenant, account, authSession string, version uint64) revocation.Grant {
	return revocation.Grant{TenantID: tenant, AccountID: account, SessionID: authSession, Client: revocation.ClientEditor, Version: version}
}

func (x fixture) open(t *testing.T, g revocation.Grant, name string) string {
	t.Helper()
	id, err := x.directory.Open(context.Background(), g, name)
	if err != nil {
		t.Fatalf("Open(%s/%s): %v", g.TenantID, g.AccountID, err)
	}
	return id
}

// lock acquires object on canvas-1 through the session behind id.
func (x fixture) lock(t *testing.T, id, tenant, account, object string) {
	t.Helper()
	err := x.directory.Use(context.Background(), id, tenant, account, func(s *locking.Session) error {
		result, err := s.Acquire(context.Background(), locking.Request{OperationID: "op-" + object, CanvasID: "canvas-1", ObjectID: object})
		if err == nil && result.Outcome != locking.OutcomeGranted {
			err = errors.New("acquire denied")
		}
		return err
	})
	if err != nil {
		t.Fatalf("lock %s: %v", object, err)
	}
}

// heartbeat is the transport's sign of life: a Use that renews the locks.
func (x fixture) heartbeat(id, tenant, account string) error {
	return x.directory.Use(context.Background(), id, tenant, account, func(s *locking.Session) error {
		_, err := s.Heartbeat(context.Background())
		return err
	})
}

func (x fixture) wantHeld(t *testing.T, tenant, object, sessionID string) {
	t.Helper()
	lease, ok := x.manager.Lookup(locking.Key{TenantID: tenant, CanvasID: "canvas-1", ObjectID: object})
	if !ok {
		t.Fatalf("%s/%s is free, want held by its session", tenant, object)
	}
	if lease.Holder.SessionID != sessionID {
		t.Fatalf("%s/%s held by %+v, want session %s", tenant, object, lease.Holder, sessionID)
	}
}

func (x fixture) wantFree(t *testing.T, tenant, object string) {
	t.Helper()
	if lease, ok := x.manager.Lookup(locking.Key{TenantID: tenant, CanvasID: "canvas-1", ObjectID: object}); ok {
		t.Fatalf("%s/%s held by %+v, want free", tenant, object, lease)
	}
}

func (x fixture) wantGone(t *testing.T, id, tenant, account string) {
	t.Helper()
	called := false
	err := x.directory.Use(context.Background(), id, tenant, account, func(*locking.Session) error {
		called = true
		return nil
	})
	if !errors.Is(err, locksession.ErrGone) || called {
		t.Fatalf("Use(%s): err = %v, called = %v, want ErrGone without calling fn", tenant+"/"+account, err, called)
	}
	if _, ok := x.directory.Name(id); ok {
		t.Fatalf("Name of an ended session is still known")
	}
}

// liveConnections counts the registry leases a tenant cut would close.
func (x fixture) liveConnections(t *testing.T, tenant string, version uint64) int {
	t.Helper()
	report, err := x.registry.Apply(context.Background(), revocation.Cut{
		OperationID: fmt.Sprintf("count-%s-%d", tenant, version), Type: revocation.TypeDeactivation,
		Target: revocation.Target{Kind: revocation.TargetTenant, TenantID: tenant}, Version: version,
	})
	if err != nil {
		t.Fatalf("Apply tenant cut: %v", err)
	}
	return len(report.Closures)
}

var idShape = regexp.MustCompile(`^pls_[0-9a-f]{32}$`)

func TestOpenAndUseRunOnTheSessionOfThatAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		id := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		other := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		if !idShape.MatchString(id) || !idShape.MatchString(other) || id == other {
			t.Fatalf("ids %q and %q, want two distinct pls_ + 32 lowercase hex", id, other)
		}

		var seen *locking.Session
		x.lock(t, id, "tenant-a", "alice", "object-1")
		if err := x.directory.Use(context.Background(), id, "tenant-a", "alice", func(s *locking.Session) error {
			seen = s
			return nil
		}); err != nil || seen == nil {
			t.Fatalf("Use: err = %v, session = %v", err, seen)
		}
		x.wantHeld(t, "tenant-a", "object-1", id)
		lease, _ := x.manager.Lookup(locking.Key{TenantID: "tenant-a", CanvasID: "canvas-1", ObjectID: "object-1"})
		if lease.Holder != (locking.Holder{TenantID: "tenant-a", AccountID: "alice", SessionID: id}) {
			t.Fatalf("holder = %+v, want tenant-a/alice/%s", lease.Holder, id)
		}

		fnErr := errors.New("fn failed")
		if err := x.directory.Use(context.Background(), id, "tenant-a", "alice", func(*locking.Session) error { return fnErr }); err != fnErr {
			t.Fatalf("Use returned %v, want fn's error unchanged", err)
		}
	})
}

func TestUseOfAnotherAccountTenantOrUnknownIDIsGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		id := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		x.lock(t, id, "tenant-a", "alice", "object-1")

		x.wantGoneUse(t, id, "tenant-a", "bob")
		x.wantGoneUse(t, id, "tenant-b", "alice")
		x.wantGoneUse(t, "pls_00000000000000000000000000000000", "tenant-a", "alice")
		x.wantGoneUse(t, "", "tenant-a", "alice")

		// The rejected calls neither ended nor touched the session.
		x.wantHeld(t, "tenant-a", "object-1", id)
		if err := x.heartbeat(id, "tenant-a", "alice"); err != nil {
			t.Fatalf("owner Use after foreign attempts: %v", err)
		}
	})
}

func (x fixture) wantGoneUse(t *testing.T, id, tenant, account string) {
	t.Helper()
	called := false
	err := x.directory.Use(context.Background(), id, tenant, account, func(*locking.Session) error {
		called = true
		return nil
	})
	if !errors.Is(err, locksession.ErrGone) || called {
		t.Fatalf("Use(%q, %s/%s): err = %v, called = %v, want ErrGone without calling fn", id, tenant, account, err, called)
	}
}

func TestSilentSessionEndsExactlyAtIdleAndFreesItsLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		id := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		x.lock(t, id, "tenant-a", "alice", "object-1")

		// A sign of life just before the deadline moves it to Idle after it.
		time.Sleep(locking.DefaultTTL - time.Nanosecond)
		if err := x.heartbeat(id, "tenant-a", "alice"); err != nil {
			t.Fatalf("Use at Idle-1ns: %v", err)
		}
		time.Sleep(locking.DefaultTTL - time.Nanosecond)
		synctest.Wait()
		x.wantHeld(t, "tenant-a", "object-1", id)

		// Exactly Idle after the last sign of life, with no access at all.
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		x.wantFree(t, "tenant-a", "object-1")
		x.wantGone(t, id, "tenant-a", "alice")
		if n := x.liveConnections(t, "tenant-a", 2); n != 0 {
			t.Fatalf("%d registry leases still live after the idle end, want 0", n)
		}
	})
}

// stalledClock never runs its timers, so only the lazy check can end a
// session; it counts the timers still pending.
type stalledClock struct {
	mu      sync.Mutex
	pending int
}

type stalledTimer struct {
	clock   *stalledClock
	stopped bool
}

func (c *stalledClock) Now() time.Time { return time.Now() }

func (c *stalledClock) AfterFunc(time.Duration, func()) locking.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending++
	return &stalledTimer{clock: c}
}

func (t *stalledTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	t.clock.pending--
	return true
}

func (c *stalledClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
}

func TestIdleEndIsExactEvenBeforeItsTimerRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &stalledClock{}
		x := newFixture(t, locksession.Config{Clock: clock})
		id := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		x.lock(t, id, "tenant-a", "alice", "object-1")

		time.Sleep(locking.DefaultTTL - time.Nanosecond)
		if err := x.heartbeat(id, "tenant-a", "alice"); err != nil {
			t.Fatalf("Use at Idle-1ns: %v", err)
		}
		time.Sleep(locking.DefaultTTL - time.Nanosecond)
		if name, ok := x.directory.Name(id); !ok || name != "alice@example.com" {
			t.Fatalf("Name at Idle-1ns = %q, %v, want alive", name, ok)
		}
		x.wantHeld(t, "tenant-a", "object-1", id)

		time.Sleep(time.Nanosecond)
		x.wantGone(t, id, "tenant-a", "alice")
		x.wantFree(t, "tenant-a", "object-1")
		if pending := clock.Pending(); pending != 0 {
			t.Fatalf("%d idle timers pending after the lazy end, want 0", pending)
		}
	})
}

func TestReconnectGetsANewSessionWithoutTheOldLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		g := grant("tenant-a", "alice", "auth-alice", 1)
		old := x.open(t, g, "alice@example.com")
		x.lock(t, old, "tenant-a", "alice", "object-1")
		time.Sleep(locking.DefaultTTL)
		synctest.Wait()

		fresh := x.open(t, g, "alice@example.com")
		if fresh == old {
			t.Fatalf("reconnect reused the id %q", old)
		}
		x.wantGone(t, old, "tenant-a", "alice")
		x.wantFree(t, "tenant-a", "object-1")
		if err := x.directory.Use(context.Background(), fresh, "tenant-a", "alice", func(s *locking.Session) error {
			renewal, err := s.Heartbeat(context.Background())
			if err == nil && (len(renewal.Renewed) != 0 || len(renewal.Lost) != 0) {
				t.Errorf("new session heartbeat = %+v, want no leases", renewal)
			}
			return err
		}); err != nil {
			t.Fatalf("Use of the new session: %v", err)
		}
	})
}

func TestAccessCutFreesTheLocksBeforeApplyReturns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target revocation.Target
		typ    revocation.Type
		// survivor is the account of tenant-a the cut leaves untouched, if any.
		survivor string
	}{
		{"tenant deactivation", revocation.Target{Kind: revocation.TargetTenant, TenantID: "tenant-a"}, revocation.TypeDeactivation, ""},
		{"account revocation", revocation.Target{Kind: revocation.TargetAccount, TenantID: "tenant-a", ID: "alice"}, revocation.TypeRevocation, "bob"},
		{"auth session revocation", revocation.Target{Kind: revocation.TargetSession, TenantID: "tenant-a", ID: "auth-alice"}, revocation.TypeRevocation, "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := newFixture(t, locksession.Config{})
				alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
				bob := x.open(t, grant("tenant-a", "bob", "auth-bob", 1), "bob@example.com")
				carol := x.open(t, grant("tenant-b", "carol", "auth-carol", 1), "carol@example.com")
				x.lock(t, alice, "tenant-a", "alice", "object-1")
				x.lock(t, bob, "tenant-a", "bob", "object-2")
				x.lock(t, carol, "tenant-b", "carol", "object-1")

				if _, err := x.registry.Apply(context.Background(), revocation.Cut{OperationID: "cut-1", Type: tc.typ, Target: tc.target, Version: 2}); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				// No synctest.Wait: the locks are free when Apply returns.
				x.wantFree(t, "tenant-a", "object-1")
				x.wantGone(t, alice, "tenant-a", "alice")
				if tc.survivor == "" {
					x.wantFree(t, "tenant-a", "object-2")
					x.wantGone(t, bob, "tenant-a", "bob")
				} else {
					x.wantHeld(t, "tenant-a", "object-2", bob)
					if err := x.heartbeat(bob, "tenant-a", "bob"); err != nil {
						t.Fatalf("Use of the untouched session: %v", err)
					}
				}
				x.wantHeld(t, "tenant-b", "object-1", carol)
				if err := x.heartbeat(carol, "tenant-b", "carol"); err != nil {
					t.Fatalf("Use in the other tenant: %v", err)
				}
			})
		})
	}
}

func TestGrantReadBeforeACutIsRejectedWithNothingAttached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		if _, err := x.registry.Apply(context.Background(), revocation.Cut{OperationID: "cut-1", Type: revocation.TypeRevocation,
			Target: revocation.Target{Kind: revocation.TargetAccount, TenantID: "tenant-a", ID: "alice"}, Version: 5}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		id, err := x.directory.Open(context.Background(), grant("tenant-a", "alice", "auth-alice", 3), "alice@example.com")
		if !errors.Is(err, revocation.ErrAccessRevoked) || id != "" {
			t.Fatalf("Open with a stale grant = %q, %v, want ErrAccessRevoked", id, err)
		}
		if n := x.liveConnections(t, "tenant-a", 6); n != 0 {
			t.Fatalf("%d registry leases after a rejected Open, want 0", n)
		}

		if _, err := x.directory.Open(context.Background(), revocation.Grant{TenantID: "tenant-a", CredentialID: "cred-1", Client: revocation.ClientOBS, Version: 7}, "obs"); !errors.Is(err, revocation.ErrInvalidGrant) {
			t.Fatalf("Open with an OBS grant: err = %v, want ErrInvalidGrant", err)
		}
		if n := x.liveConnections(t, "tenant-a", 8); n != 0 {
			t.Fatalf("%d registry leases after an OBS Open, want 0", n)
		}
	})
}

func TestOneSessionTooManyForAnAccountEndsItsOldest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{MaxPerAccount: 2})
		g := grant("tenant-a", "alice", "auth-alice", 1)
		first := x.open(t, g, "alice@example.com")
		x.lock(t, first, "tenant-a", "alice", "object-1")
		time.Sleep(time.Second)
		second := x.open(t, g, "alice@example.com")
		x.lock(t, second, "tenant-a", "alice", "object-2")
		// Other accounts and tenants do not count against alice.
		bob := x.open(t, grant("tenant-a", "bob", "auth-bob", 1), "bob@example.com")
		alienAlice := x.open(t, grant("tenant-b", "alice", "auth-alice-b", 1), "alice@example.com")
		// A recent sign of life does not save the oldest session.
		if err := x.heartbeat(first, "tenant-a", "alice"); err != nil {
			t.Fatal(err)
		}

		third := x.open(t, g, "alice@example.com")
		x.wantFree(t, "tenant-a", "object-1")
		x.wantGone(t, first, "tenant-a", "alice")
		x.wantHeld(t, "tenant-a", "object-2", second)
		for _, s := range []struct{ id, tenant, account string }{{second, "tenant-a", "alice"}, {third, "tenant-a", "alice"}, {bob, "tenant-a", "bob"}, {alienAlice, "tenant-b", "alice"}} {
			if err := x.heartbeat(s.id, s.tenant, s.account); err != nil {
				t.Fatalf("Use of a kept session: %v", err)
			}
		}
		if n := x.liveConnections(t, "tenant-a", 2); n != 3 {
			t.Fatalf("%d registry leases in tenant-a, want 3 (the evicted one released)", n)
		}
	})
}

func TestFullDirectoryRejectsOpenWithNothingRegistered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Timers never run: only Open's sweep can end the idle sessions.
		x := newFixture(t, locksession.Config{MaxSessions: 2, Clock: &stalledClock{}})
		alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		bob := x.open(t, grant("tenant-a", "bob", "auth-bob", 1), "bob@example.com")
		x.lock(t, alice, "tenant-a", "alice", "object-1")

		id, err := x.directory.Open(context.Background(), grant("tenant-a", "carol", "auth-carol", 1), "carol@example.com")
		if !errors.Is(err, locksession.ErrFull) || id != "" {
			t.Fatalf("Open when full = %q, %v, want ErrFull", id, err)
		}
		x.wantHeld(t, "tenant-a", "object-1", alice)
		for _, s := range []struct{ id, account string }{{alice, "alice"}, {bob, "bob"}} {
			if err := x.heartbeat(s.id, "tenant-a", s.account); err != nil {
				t.Fatalf("Use of an existing session: %v", err)
			}
		}

		// Idle sessions make room again.
		time.Sleep(locking.DefaultTTL)
		x.open(t, grant("tenant-a", "carol", "auth-carol", 1), "carol@example.com")
		if n := x.liveConnections(t, "tenant-a", 2); n != 1 {
			t.Fatalf("%d registry leases, want only carol's", n)
		}
	})
}

func TestNameIsKnownWhileTheSessionIs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		bob := x.open(t, grant("tenant-a", "bob", "auth-bob", 1), "bob@example.com")
		x.lock(t, alice, "tenant-a", "alice", "object-1")

		lease, _ := x.manager.Lookup(locking.Key{TenantID: "tenant-a", CanvasID: "canvas-1", ObjectID: "object-1"})
		if name, ok := x.directory.Name(lease.Holder.SessionID); !ok || name != "alice@example.com" {
			t.Fatalf("Name(holder) = %q, %v, want alice@example.com", name, ok)
		}
		if name, ok := x.directory.Name(bob); !ok || name != "bob@example.com" {
			t.Fatalf("Name(bob) = %q, %v", name, ok)
		}
		if name, ok := x.directory.Name("pls_unknown"); ok || name != "" {
			t.Fatalf("Name(unknown) = %q, %v, want false", name, ok)
		}
		x.directory.End(context.Background(), alice, "tenant-a", "alice")
		if name, ok := x.directory.Name(alice); ok || name != "" {
			t.Fatalf("Name after End = %q, %v, want false", name, ok)
		}
	})
}

func TestEndDisconnectsOnlyTheOwnersSessionAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		x.lock(t, alice, "tenant-a", "alice", "object-1")

		x.directory.End(context.Background(), alice, "tenant-a", "bob")
		x.directory.End(context.Background(), alice, "tenant-b", "alice")
		x.directory.End(context.Background(), "pls_unknown", "tenant-a", "alice")
		x.wantHeld(t, "tenant-a", "object-1", alice)
		if err := x.heartbeat(alice, "tenant-a", "alice"); err != nil {
			t.Fatalf("Use after foreign End: %v", err)
		}

		x.directory.End(context.Background(), alice, "tenant-a", "alice")
		x.wantFree(t, "tenant-a", "object-1")
		x.wantGone(t, alice, "tenant-a", "alice")
		x.directory.End(context.Background(), alice, "tenant-a", "alice")
		if n := x.liveConnections(t, "tenant-a", 2); n != 0 {
			t.Fatalf("%d registry leases after End, want 0", n)
		}
	})
}

func TestCloseEndsEverySessionAndRejectsNewWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &countingClock{}
		x := newFixture(t, locksession.Config{Clock: clock})
		alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		carol := x.open(t, grant("tenant-b", "carol", "auth-carol", 1), "carol@example.com")
		x.lock(t, alice, "tenant-a", "alice", "object-1")
		x.lock(t, carol, "tenant-b", "carol", "object-1")

		start := time.Now()
		if err := x.directory.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("Close blocked for %v", elapsed)
		}
		x.wantFree(t, "tenant-a", "object-1")
		x.wantFree(t, "tenant-b", "object-1")
		if pending := clock.Pending(); pending != 0 {
			t.Fatalf("%d idle timers pending after Close, want 0", pending)
		}
		if _, ok := x.directory.Name(alice); ok {
			t.Fatalf("Name after Close is still known")
		}
		if id, err := x.directory.Open(context.Background(), grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com"); !errors.Is(err, locksession.ErrClosed) || id != "" {
			t.Fatalf("Open after Close = %q, %v, want ErrClosed", id, err)
		}
		called := false
		if err := x.directory.Use(context.Background(), alice, "tenant-a", "alice", func(*locking.Session) error { called = true; return nil }); !errors.Is(err, locksession.ErrClosed) || called {
			t.Fatalf("Use after Close: err = %v, called = %v, want ErrClosed", err, called)
		}
		x.directory.End(context.Background(), alice, "tenant-a", "alice")
		if err := x.directory.Close(context.Background()); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			if n := x.liveConnections(t, tenant, 2); n != 0 {
				t.Fatalf("%d registry leases in %s after Close, want 0", n, tenant)
			}
		}
	})
}

func TestRegistryShutdownEndsTheSessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newFixture(t, locksession.Config{})
		alice := x.open(t, grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
		x.lock(t, alice, "tenant-a", "alice", "object-1")
		if err := x.registry.Close(context.Background()); err != nil {
			t.Fatalf("registry Close: %v", err)
		}
		x.wantFree(t, "tenant-a", "object-1")
		x.wantGone(t, alice, "tenant-a", "alice")
	})
}

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	manager, err := locking.NewManager(locking.Config{Authorize: func(context.Context, locking.Holder, locking.Key) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]locksession.Config{
		"no manager":            {Registry: registry},
		"no registry":           {Manager: manager},
		"negative idle":         {Manager: manager, Registry: registry, Idle: -time.Second},
		"negative per account":  {Manager: manager, Registry: registry, MaxPerAccount: -1},
		"negative max sessions": {Manager: manager, Registry: registry, MaxSessions: -1},
	} {
		if d, err := locksession.New(config); !errors.Is(err, locksession.ErrInvalidConfig) || d != nil {
			t.Errorf("%s: New = %v, %v, want ErrInvalidConfig", name, d, err)
		}
	}
}

func TestConcurrentOpenUseAndCutsStayBounded(t *testing.T) {
	x := newFixture(t, locksession.Config{MaxPerAccount: 2})
	var wg sync.WaitGroup
	ids := make(chan string, 64)
	for i := range 32 {
		wg.Go(func() {
			id, err := x.directory.Open(context.Background(), grant("tenant-a", "alice", "auth-alice", 1), "alice@example.com")
			if err != nil {
				return
			}
			ids <- id
			_ = x.heartbeat(id, "tenant-a", "alice")
			if i%3 == 0 {
				x.directory.End(context.Background(), id, "tenant-a", "alice")
			}
		})
	}
	wg.Go(func() {
		_, _ = x.registry.Apply(context.Background(), revocation.Cut{OperationID: "cut-1", Type: revocation.TypeRevocation,
			Target: revocation.Target{Kind: revocation.TargetAccount, TenantID: "tenant-a", ID: "bob"}, Version: 2})
	})
	wg.Wait()
	close(ids)
	alive := 0
	for id := range ids {
		if x.heartbeat(id, "tenant-a", "alice") == nil {
			alive++
		}
	}
	if alive > 2 {
		t.Fatalf("%d live sessions for one account, want at most 2", alive)
	}
}
