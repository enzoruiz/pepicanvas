package revocation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Inside a synctest bubble time is virtual, so every measured close latency
// is exact, and the bubble fails if a goroutine or timer outlives the test.

var errAborted = errors.New("fake: aborted")

// fakeConn simulates a transport. A negative closeDelay ignores ctx and hangs
// until Abort; hangAbort makes Abort itself take longer than the budget.
type fakeConn struct {
	closeDelay time.Duration
	hangAbort  bool

	once    sync.Once
	abortCh chan struct{}

	mu      sync.Mutex
	reason  Reason
	closes  int
	aborted bool
}

func newConn(closeDelay time.Duration) *fakeConn {
	return &fakeConn{closeDelay: closeDelay, abortCh: make(chan struct{})}
}

func (c *fakeConn) Close(ctx context.Context, reason Reason) error {
	c.mu.Lock()
	c.reason = reason
	c.closes++
	c.mu.Unlock()
	if c.closeDelay < 0 {
		<-c.abortCh
		return errAborted
	}
	select {
	case <-time.After(c.closeDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeConn) Abort() {
	if c.hangAbort {
		time.Sleep(time.Second)
	}
	c.mu.Lock()
	c.aborted = true
	c.mu.Unlock()
	c.once.Do(func() { close(c.abortCh) })
}

func (c *fakeConn) state() (Reason, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason, c.closes, c.aborted
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry(Config{})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func editorGrant(tenant, account, session string, version uint64) Grant {
	return Grant{TenantID: tenant, AccountID: account, SessionID: session, Client: ClientEditor, Version: version}
}

func obsGrant(tenant, credential string, version uint64) Grant {
	return Grant{TenantID: tenant, CredentialID: credential, Client: ClientOBS, Version: version}
}

func register(t *testing.T, registry *Registry, grant Grant, conn Conn) *Lease {
	t.Helper()
	lease, err := registry.Register(grant, conn)
	if err != nil {
		t.Fatalf("Register(%+v) error = %v", grant, err)
	}
	return lease
}

func deactivation(op, tenant string, version uint64) Cut {
	return Cut{OperationID: op, Type: TypeDeactivation, Target: Target{Kind: TargetTenant, TenantID: tenant}, Version: version}
}

func accountRevocation(op, tenant, account string, version uint64) Cut {
	return Cut{OperationID: op, Type: TypeRevocation, Target: Target{Kind: TargetAccount, TenantID: tenant, ID: account}, Version: version}
}

func sessionRevocation(op, tenant, session string, version uint64) Cut {
	return Cut{OperationID: op, Type: TypeRevocation, Target: Target{Kind: TargetSession, TenantID: tenant, ID: session}, Version: version}
}

func rotation(op, tenant, credential string, version uint64) Cut {
	return Cut{OperationID: op, Type: TypeRotation, Target: Target{Kind: TargetCredential, TenantID: tenant, ID: credential}, Version: version}
}

func mustApply(t *testing.T, registry *Registry, cut Cut) Report {
	t.Helper()
	report, err := registry.Apply(context.Background(), cut)
	if err != nil {
		t.Fatalf("Apply(%+v) error = %v", cut, err)
	}
	return report
}

func TestEveryCutTypeClosesAffectedEditorAndOBSConnections(t *testing.T) {
	cases := []struct {
		name   string
		cut    Cut
		reason Reason
		hit    []int
	}{
		{"deactivation", deactivation("op-1", "tenant-a", 10), ReasonDeactivated, []int{0, 1, 2}},
		{"account revocation", accountRevocation("op-1", "tenant-a", "acct-1", 10), ReasonRevoked, []int{0}},
		{"session revocation", sessionRevocation("op-1", "tenant-a", "sess-2", 10), ReasonRevoked, []int{1}},
		{"credential rotation", rotation("op-1", "tenant-a", "cred-1", 10), ReasonRotated, []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				registry := newRegistry(t)
				grants := []Grant{
					editorGrant("tenant-a", "acct-1", "sess-1", 5),
					editorGrant("tenant-a", "acct-2", "sess-2", 5),
					obsGrant("tenant-a", "cred-1", 5),
				}
				conns := make([]*fakeConn, len(grants))
				leases := make([]*Lease, len(grants))
				for i, grant := range grants {
					conns[i] = newConn(100 * time.Millisecond)
					leases[i] = register(t, registry, grant, conns[i])
				}

				report := mustApply(t, registry, tc.cut)

				if len(report.Closures) != len(tc.hit) {
					t.Fatalf("closures = %+v, want %d", report.Closures, len(tc.hit))
				}
				for _, closure := range report.Closures {
					if closure.Latency != 100*time.Millisecond || closure.Aborted {
						t.Fatalf("closure = %+v, want graceful close in 100ms", closure)
					}
				}
				hit := map[int]bool{}
				for _, i := range tc.hit {
					hit[i] = true
				}
				for i, conn := range conns {
					reason, closes, _ := conn.state()
					if hit[i] {
						if reason != tc.reason || closes != 1 {
							t.Fatalf("conn %d reason=%q closes=%d, want %q once", i, reason, closes, tc.reason)
						}
						if err := leases[i].Err(); !errors.Is(err, ErrAccessRevoked) {
							t.Fatalf("lease %d Err() = %v, want ErrAccessRevoked", i, err)
						}
						continue
					}
					if closes != 0 || leases[i].Err() != nil {
						t.Fatalf("unaffected conn %d closes=%d err=%v", i, closes, leases[i].Err())
					}
				}
			})
		})
	}
}

func TestCutAbortsConnectionsThatDoNotCloseWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		slow := newConn(5 * time.Second)
		hung := newConn(-1)
		register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), slow)
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), hung)

		start := time.Now()
		report := mustApply(t, registry, deactivation("op-1", "tenant-a", 10))

		if elapsed := time.Since(start); elapsed > DefaultBudget {
			t.Fatalf("Apply took %v, want <= %v", elapsed, DefaultBudget)
		}
		for _, closure := range report.Closures {
			if !closure.Aborted || closure.Latency > DefaultBudget {
				t.Fatalf("closure = %+v, want aborted within %v", closure, DefaultBudget)
			}
		}
		for _, conn := range []*fakeConn{slow, hung} {
			if _, _, aborted := conn.state(); !aborted {
				t.Fatal("connection was not aborted")
			}
		}
		if report.MaxLatency > DefaultBudget {
			t.Fatalf("MaxLatency = %v, want <= %v", report.MaxLatency, DefaultBudget)
		}
	})
}

func TestCutReportsBudgetExcessAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := newConn(-1)
		conn.hangAbort = true
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)

		report, err := registry.Apply(context.Background(), rotation("op-1", "tenant-a", "cred-1", 10))

		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("Apply error = %v, want ErrBudgetExceeded", err)
		}
		if report.Exceeded != 1 || report.MaxLatency <= DefaultBudget {
			t.Fatalf("report = %+v, want one closure over budget", report)
		}
	})
}

func TestCutRejectsOperationsBeforeTransportCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := newConn(time.Second)
		lease := register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), conn)

		ran := 0
		if err := lease.Guard(func(context.Context) error { ran++; return nil }); err != nil {
			t.Fatalf("Guard before cut error = %v", err)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))
		}()
		synctest.Wait()

		if _, closes, _ := conn.state(); closes != 1 {
			t.Fatalf("closes = %d, want close in progress", closes)
		}
		for _, op := range []string{"read", "write", "command"} {
			if err := lease.Guard(func(context.Context) error { ran++; return nil }); !errors.Is(err, ErrAccessRevoked) {
				t.Fatalf("%s after cut error = %v, want ErrAccessRevoked", op, err)
			}
		}
		if ran != 1 {
			t.Fatalf("callbacks ran %d times, want only the one before the cut", ran)
		}
		if lease.Context().Err() == nil {
			t.Fatal("lease context still live after cut")
		}
		<-done
	})
}

func TestUnconfirmedOrMalformedCutIsRejectedWithoutEffect(t *testing.T) {
	registry := newRegistry(t)
	conn := newConn(0)
	lease := register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), conn)

	cases := map[string]Cut{
		"unconfirmed":          deactivation("op-1", "tenant-a", 0),
		"missing operation":    deactivation("", "tenant-a", 10),
		"missing tenant":       deactivation("op-1", "", 10),
		"rotation of account":  {OperationID: "op-1", Type: TypeRotation, Target: Target{Kind: TargetAccount, TenantID: "tenant-a", ID: "acct-1"}, Version: 10},
		"revocation of tenant": {OperationID: "op-1", Type: TypeRevocation, Target: Target{Kind: TargetTenant, TenantID: "tenant-a"}, Version: 10},
		"missing target id":    accountRevocation("op-1", "tenant-a", "", 10),
		"tenant with id":       {OperationID: "op-1", Type: TypeDeactivation, Target: Target{Kind: TargetTenant, TenantID: "tenant-a", ID: "x"}, Version: 10},
		"unknown type":         {OperationID: "op-1", Type: "erase", Target: Target{Kind: TargetTenant, TenantID: "tenant-a"}, Version: 10},
		"malformed identifier": accountRevocation("op 1", "tenant-a", "acct-1", 10),
	}
	for name, cut := range cases {
		if _, err := registry.Apply(context.Background(), cut); !errors.Is(err, ErrInvalidCut) {
			t.Fatalf("%s: Apply error = %v, want ErrInvalidCut", name, err)
		}
	}
	if _, err := registry.Apply(context.Background(), deactivation("op-1", "tenant-a", 0)); !errors.Is(err, ErrUnconfirmedCut) {
		t.Fatalf("unconfirmed Apply error = %v, want ErrUnconfirmedCut", err)
	}
	if _, closes, _ := conn.state(); closes != 0 || lease.Err() != nil {
		t.Fatalf("rejected cuts had effect: closes=%d err=%v", closes, lease.Err())
	}
	if _, err := registry.Register(editorGrant("tenant-a", "acct-1", "sess-1", 5), newConn(0)); err != nil {
		t.Fatalf("rejected cuts left history: Register error = %v", err)
	}
}

func TestCutsApplyInConfirmationOrderNotArrivalOrder(t *testing.T) {
	registry := newRegistry(t)
	conn := newConn(0)
	lease := register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 20), conn)

	// A cut confirmed before the grant was read arrives late: no effect.
	if report := mustApply(t, registry, accountRevocation("op-old", "tenant-a", "acct-1", 15)); len(report.Closures) != 0 {
		t.Fatalf("older cut closed %d connections", len(report.Closures))
	}
	if lease.Err() != nil {
		t.Fatalf("older cut revoked a newer grant: %v", lease.Err())
	}

	if report := mustApply(t, registry, accountRevocation("op-new", "tenant-a", "acct-1", 21)); len(report.Closures) != 1 {
		t.Fatalf("newer cut closed %d connections, want 1", len(report.Closures))
	}

	// The later-confirmed cut wins even when an earlier one arrives after it.
	mustApply(t, registry, accountRevocation("op-late", "tenant-a", "acct-1", 18))
	if _, err := registry.Register(editorGrant("tenant-a", "acct-1", "sess-1", 19), newConn(0)); !errors.Is(err, ErrAccessRevoked) {
		t.Fatalf("grant between cuts: Register error = %v, want ErrAccessRevoked", err)
	}
}

func TestRegisterAfterCutWithStaleGrantIsRejected(t *testing.T) {
	registry := newRegistry(t)
	mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))
	mustApply(t, registry, rotation("op-2", "tenant-a", "cred-1", 11))
	mustApply(t, registry, sessionRevocation("op-3", "tenant-a", "sess-9", 12))
	mustApply(t, registry, deactivation("op-4", "tenant-b", 13))

	rejected := []Grant{
		editorGrant("tenant-a", "acct-1", "sess-1", 9),  // reconnect with a cached grant
		obsGrant("tenant-a", "cred-1", 10),              // old OBS credential
		editorGrant("tenant-a", "acct-2", "sess-9", 11), // revoked session
		editorGrant("tenant-b", "acct-1", "sess-1", 12), // deactivated tenant
		obsGrant("tenant-b", "cred-1", 12),
	}
	for _, grant := range rejected {
		conn := newConn(0)
		if _, err := registry.Register(grant, conn); !errors.Is(err, ErrAccessRevoked) {
			t.Fatalf("Register(%+v) error = %v, want ErrAccessRevoked", grant, err)
		}
		if _, closes, _ := conn.state(); closes != 0 {
			t.Fatal("rejected registration touched the connection")
		}
	}

	accepted := []Grant{
		editorGrant("tenant-a", "acct-1", "sess-1", 10), // re-admitted after the cut
		obsGrant("tenant-a", "cred-2", 5),               // rotated credential is a new ID
		editorGrant("tenant-a", "acct-2", "sess-2", 5),
		editorGrant("tenant-b", "acct-1", "sess-1", 13), // reactivated tenant
	}
	for _, grant := range accepted {
		register(t, registry, grant, newConn(0))
	}
}

func TestGrantOlderThanRetainedHistoryIsStale(t *testing.T) {
	registry, err := NewRegistry(Config{History: 2})
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, registry, accountRevocation("op-1", "tenant-a", "acct-1", 10))
	mustApply(t, registry, accountRevocation("op-2", "tenant-a", "acct-2", 11))
	mustApply(t, registry, accountRevocation("op-3", "tenant-a", "acct-3", 12))

	if _, err := registry.Register(editorGrant("tenant-a", "acct-1", "sess-1", 9), newConn(0)); !errors.Is(err, ErrStaleGrant) {
		t.Fatalf("grant before evicted cut: Register error = %v, want ErrStaleGrant", err)
	}
	register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 10), newConn(0))
	// Another tenant's history never makes this tenant's grants stale.
	register(t, registry, editorGrant("tenant-b", "acct-1", "sess-1", 1), newConn(0))
}

func TestInvalidGrantIsRejected(t *testing.T) {
	registry := newRegistry(t)
	cases := map[string]Grant{
		"missing tenant":      editorGrant("", "acct-1", "sess-1", 5),
		"unconfirmed":         editorGrant("tenant-a", "acct-1", "sess-1", 0),
		"editor no session":   editorGrant("tenant-a", "acct-1", "", 5),
		"editor no account":   editorGrant("tenant-a", "", "sess-1", 5),
		"obs no credential":   obsGrant("tenant-a", "", 5),
		"unknown client":      {TenantID: "tenant-a", AccountID: "acct-1", SessionID: "sess-1", Client: "cli", Version: 5},
		"malformed tenant id": editorGrant("tenant a", "acct-1", "sess-1", 5),
	}
	for name, grant := range cases {
		if _, err := registry.Register(grant, newConn(0)); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("%s: Register error = %v, want ErrInvalidGrant", name, err)
		}
	}
	if _, err := registry.Register(editorGrant("tenant-a", "acct-1", "sess-1", 5), nil); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("nil conn: Register error = %v, want ErrInvalidGrant", err)
	}
}

func TestReleasedLeaseIsNotClosedByLaterCut(t *testing.T) {
	registry := newRegistry(t)
	conn := newConn(0)
	lease := register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), conn)
	lease.Release()

	if report := mustApply(t, registry, deactivation("op-1", "tenant-a", 10)); len(report.Closures) != 0 {
		t.Fatalf("released lease was closed: %+v", report.Closures)
	}
	if lease.Context().Err() == nil {
		t.Fatal("released lease context still live")
	}
	if err := lease.Guard(func(context.Context) error { return nil }); err == nil {
		t.Fatal("Guard on released lease succeeded")
	}
}

// TestConcurrentCutsInTwoTenants applies every cut type concurrently in two
// tenants with fast, slow, and hung transports. Any close over the budget,
// any cross-tenant effect, or any surviving affected connection fails.
func TestConcurrentCutsInTwoTenants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		delays := []time.Duration{0, 300 * time.Millisecond, 5 * time.Second, -1}

		type conn struct {
			grant Grant
			conn  *fakeConn
			lease *Lease
		}
		var conns []conn
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			for _, delay := range delays {
				for _, grant := range []Grant{
					editorGrant(tenant, "acct-1", "sess-1", 5),
					editorGrant(tenant, "acct-2", "sess-2", 5),
					obsGrant(tenant, "cred-1", 5),
					obsGrant(tenant, "cred-2", 5),
				} {
					fake := newConn(delay)
					conns = append(conns, conn{grant: grant, conn: fake, lease: register(t, registry, grant, fake)})
				}
			}
		}

		cuts := []Cut{
			accountRevocation("op-a1", "tenant-a", "acct-1", 10),
			sessionRevocation("op-a2", "tenant-a", "sess-2", 11),
			rotation("op-a3", "tenant-a", "cred-1", 12),
			deactivation("op-b1", "tenant-b", 13),
		}
		reports := make([]Report, len(cuts))
		errs := make([]error, len(cuts))
		var wg sync.WaitGroup
		start := time.Now()
		for i, cut := range cuts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reports[i], errs[i] = registry.Apply(context.Background(), cut)
			}()
		}
		wg.Wait()
		if elapsed := time.Since(start); elapsed > DefaultBudget {
			t.Fatalf("concurrent cuts took %v, want <= %v", elapsed, DefaultBudget)
		}

		closed := 0
		for i, report := range reports {
			if errs[i] != nil {
				t.Fatalf("Apply(%s) error = %v", cuts[i].OperationID, errs[i])
			}
			if report.Exceeded != 0 || report.MaxLatency > DefaultBudget {
				t.Fatalf("Apply(%s) report = %+v exceeds %v", cuts[i].OperationID, report, DefaultBudget)
			}
			closed += len(report.Closures)
		}

		affected := func(grant Grant) bool {
			if grant.TenantID == "tenant-b" {
				return true
			}
			return grant.AccountID == "acct-1" || grant.SessionID == "sess-2" || grant.CredentialID == "cred-1"
		}
		want := 0
		for _, c := range conns {
			_, closes, _ := c.conn.state()
			if affected(c.grant) {
				want++
				if closes != 1 || !errors.Is(c.lease.Err(), ErrAccessRevoked) {
					t.Fatalf("affected %+v closes=%d err=%v", c.grant, closes, c.lease.Err())
				}
				continue
			}
			if closes != 0 || c.lease.Err() != nil {
				t.Fatalf("unaffected %+v closes=%d err=%v", c.grant, closes, c.lease.Err())
			}
		}
		if closed != want {
			t.Fatalf("reports closed %d connections, want %d", closed, want)
		}
	})
}

// TestEveryCutTypeStaysInsideItsTenant applies each cut type in one tenant
// while the other tenant holds grants with the same identifiers.
func TestEveryCutTypeStaysInsideItsTenant(t *testing.T) {
	cuts := []func(tenant string) Cut{
		func(tenant string) Cut { return accountRevocation("op-1", tenant, "acct-1", 10) },
		func(tenant string) Cut { return sessionRevocation("op-1", tenant, "sess-1", 10) },
		func(tenant string) Cut { return rotation("op-1", tenant, "cred-1", 10) },
		func(tenant string) Cut { return deactivation("op-1", tenant, 10) },
	}
	for _, cutIn := range cuts {
		for _, cutTenant := range []string{"tenant-a", "tenant-b"} {
			registry := newRegistry(t)
			other := map[string]string{"tenant-a": "tenant-b", "tenant-b": "tenant-a"}[cutTenant]
			editor, obs := newConn(0), newConn(0)
			editorLease := register(t, registry, editorGrant(other, "acct-1", "sess-1", 5), editor)
			obsLease := register(t, registry, obsGrant(other, "cred-1", 5), obs)

			cut := cutIn(cutTenant)
			if report := mustApply(t, registry, cut); len(report.Closures) != 0 {
				t.Fatalf("%s in %s closed %d connections of %s", cut.Type, cutTenant, len(report.Closures), other)
			}
			for _, conn := range []*fakeConn{editor, obs} {
				if _, closes, _ := conn.state(); closes != 0 {
					t.Fatalf("%s in %s closed a connection of %s", cut.Type, cutTenant, other)
				}
			}
			if editorLease.Err() != nil || obsLease.Err() != nil {
				t.Fatalf("%s in %s ended a lease of %s", cut.Type, cutTenant, other)
			}
			register(t, registry, editorGrant(other, "acct-1", "sess-1", 5), newConn(0))
			register(t, registry, obsGrant(other, "cred-1", 5), newConn(0))
		}
	}
}
