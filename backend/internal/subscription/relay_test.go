package subscription_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription/subscriptiontest"
)

// Relay tests run inside a synctest bubble: time is virtual, so every close
// latency and retry delay is exact, and the bubble fails if the relay leaves
// a goroutine or timer behind.

const cutCorrelation = "0123456789abcdef0123456789abcdef"

var errAborted = errors.New("fake: aborted")

// fakeConn simulates a transport. A negative closeDelay ignores ctx and
// hangs until Abort; hangAbort makes Abort itself outlast the budget.
type fakeConn struct {
	closeDelay time.Duration
	hangAbort  bool

	once    sync.Once
	abortCh chan struct{}

	mu      sync.Mutex
	reason  revocation.Reason
	closes  int
	aborted bool
}

func newConn(closeDelay time.Duration) *fakeConn {
	return &fakeConn{closeDelay: closeDelay, abortCh: make(chan struct{})}
}

func (c *fakeConn) Close(ctx context.Context, reason revocation.Reason) error {
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
		time.Sleep(3 * time.Second)
	}
	c.mu.Lock()
	c.aborted = true
	c.mu.Unlock()
	c.once.Do(func() { close(c.abortCh) })
}

func (c *fakeConn) state() (revocation.Reason, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason, c.closes, c.aborted
}

// call is one Apply the relay made.
type call struct {
	cut         revocation.Cut
	correlation string
	at          time.Time
	report      revocation.Report
	err         error
}

// recordingCutter records every Apply. While script has entries, each call
// answers the next scripted error without reaching inner; a nil entry, or an
// empty script, delegates to inner.
type recordingCutter struct {
	inner subscription.Cutter
	// block makes each call wait for release or the end of ctx.
	block   bool
	release chan struct{}

	mu     sync.Mutex
	script []error
	calls  []call
}

func (c *recordingCutter) Apply(ctx context.Context, cut revocation.Cut) (revocation.Report, error) {
	id, _ := correlation.FromContext(ctx)
	record := call{cut: cut, correlation: id.String(), at: time.Now()}
	if c.block {
		select {
		case <-c.release:
		case <-ctx.Done():
			record.err = ctx.Err()
			c.add(record)
			return revocation.Report{}, ctx.Err()
		}
	}
	c.mu.Lock()
	var scripted error
	delegate := true
	if len(c.script) > 0 {
		scripted, c.script = c.script[0], c.script[1:]
		delegate = scripted == nil
	}
	c.mu.Unlock()
	if delegate && c.inner != nil {
		record.report, record.err = c.inner.Apply(ctx, cut)
	} else {
		record.err = scripted
	}
	c.add(record)
	return record.report, record.err
}

func (c *recordingCutter) add(record call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, record)
}

func (c *recordingCutter) recorded() []call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]call(nil), c.calls...)
}

type logRecord map[string]any

func captureLogs(t *testing.T) (*slog.Logger, func() []logRecord) {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	return logger, func() []logRecord {
		mu.Lock()
		defer mu.Unlock()
		var records []logRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var record logRecord
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			records = append(records, record)
		}
		return records
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// correlate is the bridge cmd/server installs: it carries a canonical
// correlation ID in the context the platform logger reads.
func correlate(ctx context.Context, id string) context.Context {
	if parsed, err := correlation.Parse(id); err == nil {
		return correlation.WithID(ctx, parsed)
	}
	return ctx
}

// rig is one process's live side: a registry and the relay feeding it.
type rig struct {
	registry *revocation.Registry
	cutter   *recordingCutter
	relay    *subscription.CutRelay
	logs     func() []logRecord
}

func newRig(t *testing.T, store subscription.CutStore, configure func(*subscription.RelayConfig)) *rig {
	t.Helper()
	logger, logs := captureLogs(t)
	registry, err := revocation.NewRegistry(revocation.Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	cutter := &recordingCutter{inner: registry, release: make(chan struct{})}
	config := subscription.RelayConfig{Store: store, Cutter: cutter, Logger: logger, Correlate: correlate}
	if configure != nil {
		configure(&config)
	}
	relay, err := subscription.NewCutRelay(config)
	if err != nil {
		t.Fatalf("NewCutRelay: %v", err)
	}
	t.Cleanup(func() {
		if err := relay.Close(context.Background()); err != nil {
			t.Errorf("relay Close: %v", err)
		}
	})
	return &rig{registry: registry, cutter: cutter, relay: relay, logs: logs}
}

// start starts the relay and waits for its startup pass to finish, so later
// commits never race it. It runs inside a synctest bubble.
func (r *rig) start(t *testing.T) {
	t.Helper()
	if err := r.relay.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	synctest.Wait()
}

// streamers seeds the accounts of fixture and activates A and B with a service
// whose committed hook is notify (nil for none).
func streamers(t *testing.T, notify func()) (*subscription.Service, *subscriptiontest.MemoryStore, subscription.Activation, subscription.Activation) {
	t.Helper()
	_, store, c := fixture(t)
	options := []subscription.Option{subscription.WithClock(c.now)}
	if notify != nil {
		options = append(options, subscription.WithCommitted(notify))
	}
	service := subscription.NewService(store, options...)
	a, err := service.Activate(context.Background(), activate("activation-a-0001", "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.Activate(context.Background(), activate("activation-b-0001", "b@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	return service, store, a, b
}

func deactivateA(t *testing.T, service *subscription.Service, op string) subscription.Deactivation {
	t.Helper()
	d, err := service.Deactivate(context.Background(), subscription.DeactivateRequest{
		OperatorID: operator, OperationID: op, Email: "a@example.com", CorrelationID: cutCorrelation,
	})
	if err != nil || !d.Deactivated {
		t.Fatalf("Deactivate = %+v, %v; want a cut", d, err)
	}
	return d
}

func editorGrant(t *testing.T, service *subscription.Service, account access.AccountID, session string) revocation.Grant {
	t.Helper()
	grant, err := service.EditorGrant(context.Background(), account, session)
	if err != nil {
		t.Fatalf("EditorGrant(%s): %v", account, err)
	}
	return grant
}

// obsGrant is an OBS credential grant read at the tenant's access version.
func obsGrant(editor revocation.Grant, credential string) revocation.Grant {
	return revocation.Grant{TenantID: editor.TenantID, CredentialID: credential, Client: revocation.ClientOBS, Version: editor.Version}
}

func register(t *testing.T, registry *revocation.Registry, grant revocation.Grant, conn revocation.Conn) *revocation.Lease {
	t.Helper()
	lease, err := registry.Register(grant, conn)
	if err != nil {
		t.Fatalf("Register(%+v): %v", grant, err)
	}
	return lease
}

func requireCut(t *testing.T, lease *revocation.Lease) {
	t.Helper()
	if err := lease.Err(); !errors.Is(err, revocation.ErrAccessRevoked) {
		t.Fatalf("lease Err = %v, want ErrAccessRevoked", err)
	}
	ran := false
	if err := lease.Guard(func(context.Context) error { ran = true; return nil }); !errors.Is(err, revocation.ErrAccessRevoked) || ran {
		t.Fatalf("Guard = %v (ran %v), want ErrAccessRevoked without running", err, ran)
	}
	if lease.Context().Err() == nil {
		t.Fatal("lease context still live")
	}
}

func requireLive(t *testing.T, lease *revocation.Lease) {
	t.Helper()
	ran := false
	if err := lease.Guard(func(context.Context) error { ran = true; return nil }); err != nil || !ran || lease.Context().Err() != nil {
		t.Fatalf("Guard = %v (ran %v), ctx %v; want a live lease", err, ran, lease.Context().Err())
	}
}

func requireClosedOnce(t *testing.T, conn *fakeConn, aborted bool) {
	t.Helper()
	if reason, closes, gotAborted := conn.state(); reason != revocation.ReasonDeactivated || closes != 1 || gotAborted != aborted {
		t.Fatalf("conn reason=%q closes=%d aborted=%v; want %q once, aborted=%v", reason, closes, gotAborted, revocation.ReasonDeactivated, aborted)
	}
}

func requireUntouched(t *testing.T, conn *fakeConn) {
	t.Helper()
	if reason, closes, aborted := conn.state(); reason != "" || closes != 0 || aborted {
		t.Fatalf("conn reason=%q closes=%d aborted=%v; want untouched", reason, closes, aborted)
	}
}

func requireCounters(t *testing.T, relay *subscription.CutRelay, want subscription.RelayCounters) {
	t.Helper()
	if got := relay.Counters(); got != want {
		t.Fatalf("Counters = %+v, want %+v", got, want)
	}
}

// onlyCut returns the single committed cut.
func onlyCut(t *testing.T, store *subscriptiontest.MemoryStore) subscription.PendingCut {
	t.Helper()
	cuts := store.Cuts()
	if len(cuts) != 1 {
		t.Fatalf("cuts = %+v, want one", cuts)
	}
	return cuts[0]
}

func requirePending(t *testing.T, store *subscriptiontest.MemoryStore, want int) {
	t.Helper()
	pending, err := store.PendingCuts(context.Background(), 100)
	if err != nil || len(pending) != want {
		t.Fatalf("PendingCuts = %+v, %v; want %d", pending, err, want)
	}
}

func wantCut(pending subscription.PendingCut) revocation.Cut {
	return revocation.Cut{
		OperationID:   fmt.Sprintf("cut-%d", pending.ID),
		Type:          revocation.TypeDeactivation,
		Target:        revocation.Target{Kind: revocation.TargetTenant, TenantID: pending.TenantID},
		Version:       pending.Version,
		CorrelationID: pending.CorrelationID,
	}
}

func recordsWith(records []logRecord, msg string) []logRecord {
	var found []logRecord
	for _, record := range records {
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

func TestADeactivationClosesTheTenantsEditorAndOBSLeasesWithinTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store, a, b := streamers(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay

		editorA := editorGrant(t, service, streamerA, "session-a")
		editorB := editorGrant(t, service, streamerB, "session-b")
		editorConnA, obsConnA := newConn(100*time.Millisecond), newConn(-1)
		editorConnB, obsConnB := newConn(0), newConn(0)
		leaseEditorA := register(t, live.registry, editorA, editorConnA)
		leaseOBSA := register(t, live.registry, obsGrant(editorA, "credential-a"), obsConnA)
		leaseEditorB := register(t, live.registry, editorB, editorConnB)
		leaseOBSB := register(t, live.registry, obsGrant(editorB, "credential-b"), obsConnB)
		live.start(t)
		synctest.Wait()

		start := time.Now()
		d := deactivateA(t, service, deactivateOp)
		synctest.Wait()
		// The cut reached the registry at the commit: no virtual time passed.
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("cut applied after %v, want at once", elapsed)
		}
		requireCut(t, leaseEditorA)
		requireCut(t, leaseOBSA)

		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()
		calls := live.cutter.recorded()
		cut := onlyCut(t, store)
		if len(calls) != 1 || calls[0].cut != wantCut(cut) || calls[0].err != nil || calls[0].correlation != cutCorrelation {
			t.Fatalf("calls = %+v, want one clean Apply of %+v carrying the correlation", calls, wantCut(cut))
		}
		if cut.TenantID != a.TenantID || cut.Version != d.Version || cut.CorrelationID != cutCorrelation {
			t.Fatalf("cut = %+v, want A at %d", cut, d.Version)
		}
		report := calls[0].report
		if len(report.Closures) != 2 || report.Exceeded != 0 || report.MaxLatency > revocation.DefaultBudget || report.MaxLatency > 2*time.Second {
			t.Fatalf("report = %+v, want two closes within the budget", report)
		}
		for _, closure := range report.Closures {
			if closure.Latency > revocation.DefaultBudget || closure.Exceeded {
				t.Fatalf("closure %+v exceeds the budget", closure)
			}
			// The editor closes gracefully; the hung OBS conn is aborted.
			if (closure.Client == revocation.ClientOBS) != closure.Aborted {
				t.Fatalf("closure %+v, want only the hung OBS conn aborted", closure)
			}
		}
		requireClosedOnce(t, editorConnA, false)
		requireClosedOnce(t, obsConnA, true)

		// S8: tenant B is untouched and still admits connections.
		requireLive(t, leaseEditorB)
		requireLive(t, leaseOBSB)
		requireUntouched(t, editorConnB)
		requireUntouched(t, obsConnB)
		register(t, live.registry, editorGrant(t, service, streamerB, "session-b2"), newConn(0))
		if tenant := onlyTenant(t, store, streamerB); tenant.ID != b.TenantID || tenant.Status != access.SubscriptionActive || tenant.Version != 1 {
			t.Fatalf("tenant B = %+v, want active at 1", tenant)
		}

		at, settled := store.AppliedAt(cut.ID)
		if !settled || at.Before(start) || at.Sub(start) > revocation.DefaultBudget {
			t.Fatalf("AppliedAt = %v, %v; want settled within the budget of %v", at, settled, start)
		}
		requirePending(t, store, 0)
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1})

		// S13: the relay and the registry log the cut under its correlation,
		// and no record carries an email or a connection secret.
		records := live.logs()
		applied := recordsWith(records, "access cut applied")
		if len(applied) != 1 || applied[0][logging.CorrelationKey] != cutCorrelation || applied[0]["tenant_id"] != a.TenantID ||
			applied[0]["cut_id"] != float64(cut.ID) || applied[0]["version"] != float64(d.Version) || applied[0]["affected"] != float64(2) {
			t.Fatalf("applied records = %v", applied)
		}
		confirmed := recordsWith(records, "access cut confirmed")
		closed := recordsWith(records, "connection closed")
		if len(confirmed) != 1 || len(closed) != 2 {
			t.Fatalf("confirmed=%d closed=%d, want 1 and 2", len(confirmed), len(closed))
		}
		for _, record := range append(confirmed, closed...) {
			if record[logging.CorrelationKey] != cutCorrelation || record["tenant_id"] != a.TenantID {
				t.Fatalf("registry record %v lacks the cut's correlation or tenant", record)
			}
		}
		raw, _ := json.Marshal(records)
		for _, leak := range []string{"a@example.com", "b@example.com", "session-a", "credential-a", string(streamerA)} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("logs contain %q: %s", leak, raw)
			}
		}
	})
}

func TestGrantsReadBeforeTheCutNeverRestoreAccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store, a, _ := streamers(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay
		live.start(t)

		preEditor := editorGrant(t, service, streamerA, "session-a")
		preOBS := obsGrant(preEditor, "credential-a")
		deactivateA(t, service, deactivateOp)
		synctest.Wait()
		requirePending(t, store, 0)

		// An existing tab or OBS source reconnecting with its old grant.
		for _, grant := range []revocation.Grant{preEditor, preOBS} {
			conn := newConn(0)
			if _, err := live.registry.Register(grant, conn); !errors.Is(err, revocation.ErrAccessRevoked) {
				t.Fatalf("Register(pre-cut %s) = %v, want ErrAccessRevoked", grant.Client, err)
			}
			requireUntouched(t, conn)
		}
		// A fresh read is denied while the tenant is inactive.
		if _, err := service.EditorGrant(context.Background(), streamerA, "session-a"); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("EditorGrant while inactive = %v, want ErrAccessDenied", err)
		}

		if again, err := service.Activate(context.Background(), activate("activation-a-0002", "a@example.com")); err != nil || again.TenantID != a.TenantID {
			t.Fatalf("reactivate = %+v, %v", again, err)
		}
		fresh := editorGrant(t, service, streamerA, "session-a")
		if fresh.Version != 2 {
			t.Fatalf("fresh grant = %+v, want version 2", fresh)
		}
		requireLive(t, register(t, live.registry, fresh, newConn(0)))
		requireLive(t, register(t, live.registry, obsGrant(fresh, "credential-a"), newConn(0)))
		for _, grant := range []revocation.Grant{preEditor, preOBS} {
			if _, err := live.registry.Register(grant, newConn(0)); !errors.Is(err, revocation.ErrAccessRevoked) {
				t.Fatalf("Register(pre-cut %s) after reactivation = %v, want ErrAccessRevoked", grant.Client, err)
			}
		}
		// Reactivation writes no cut and wakes nothing.
		if n := len(store.Cuts()); n != 1 {
			t.Fatalf("cuts = %d, want only the deactivation's", n)
		}
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1})
	})
}

func TestACutLeftPendingByACrashIsAppliedAtStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The crashed process committed the deactivation and stopped before
		// any relay applied its cut.
		crashed, store, a, _ := streamers(t, nil)
		pre := editorGrant(t, crashed, streamerA, "session-a")
		d := deactivateA(t, crashed, deactivateOp)
		requirePending(t, store, 1)

		// The new process has an empty registry, so a client reconnecting
		// with its pre-cut grant before the relay starts is admitted...
		restarted := newRig(t, store, nil)
		conn := newConn(50 * time.Millisecond)
		lease := register(t, restarted.registry, pre, conn)
		start := time.Now()
		restarted.start(t)
		synctest.Wait()
		// ...and cut by the startup pass.
		if time.Since(start) != 0 {
			t.Fatal("the pending cut was not applied at Start")
		}
		requireCut(t, lease)
		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()
		requireClosedOnce(t, conn, false)

		cut := onlyCut(t, store)
		if calls := restarted.cutter.recorded(); len(calls) != 1 || calls[0].cut != wantCut(cut) || cut.TenantID != a.TenantID || cut.Version != d.Version {
			t.Fatalf("calls = %+v, want one Apply of the pending cut", calls)
		}
		if _, settled := store.AppliedAt(cut.ID); !settled {
			t.Fatal("the recovered cut was not settled")
		}
		requirePending(t, store, 0)
		requireCounters(t, restarted.relay, subscription.RelayCounters{Applied: 1})
		if _, err := restarted.registry.Register(pre, newConn(0)); !errors.Is(err, revocation.ErrAccessRevoked) {
			t.Fatalf("Register(pre-cut) after recovery = %v, want ErrAccessRevoked", err)
		}
	})
}

func TestACutRefusedByAClosedRegistryIsDeliveredOnceByTheNextProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store, _, _ := streamers(t, func() { relay.Notify() })
		closing := newRig(t, store, nil)
		relay = closing.relay
		if err := closing.registry.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		closing.start(t)
		pre := editorGrant(t, service, streamerA, "session-a")
		deactivateA(t, service, deactivateOp)
		synctest.Wait()

		if calls := closing.cutter.recorded(); len(calls) != 1 || !errors.Is(calls[0].err, revocation.ErrClosed) {
			t.Fatalf("calls = %+v, want one refused Apply", calls)
		}
		requirePending(t, store, 1)
		requireCounters(t, closing.relay, subscription.RelayCounters{Retries: 1})
		if err := closing.relay.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		requirePending(t, store, 1)

		next := newRig(t, store, nil)
		conn := newConn(0)
		lease := register(t, next.registry, pre, conn)
		next.start(t)
		synctest.Wait()
		requireCut(t, lease)
		time.Sleep(revocation.DefaultBudget)
		synctest.Wait()
		requireClosedOnce(t, conn, false)
		requirePending(t, store, 0)
		requireCounters(t, next.relay, subscription.RelayCounters{Applied: 1})
		if calls := next.cutter.recorded(); len(calls) != 1 || calls[0].cut != wantCut(onlyCut(t, store)) {
			t.Fatalf("calls = %+v, want exactly one delivery", calls)
		}
	})
}

func TestRetriesBackOffWithinTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store, _, _ := streamers(t, func() { relay.Notify() })
		live := newRig(t, store, func(c *subscription.RelayConfig) {
			c.MinBackoff = 100 * time.Millisecond
			c.MaxBackoff = 150 * time.Millisecond
		})
		relay = live.relay
		live.cutter.script = []error{revocation.ErrClosed, context.DeadlineExceeded, errors.New("transient")}
		live.start(t)
		synctest.Wait()

		start := time.Now()
		deactivateA(t, service, deactivateOp)
		synctest.Wait()
		requirePending(t, store, 1)
		time.Sleep(time.Second)
		synctest.Wait()

		calls := live.cutter.recorded()
		var offsets []time.Duration
		for _, c := range calls {
			offsets = append(offsets, c.at.Sub(start))
		}
		want := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 400 * time.Millisecond}
		if !reflect.DeepEqual(offsets, want) || calls[3].err != nil {
			t.Fatalf("attempts at %v (last err %v), want %v ending applied", offsets, calls[len(calls)-1].err, want)
		}
		for _, c := range calls {
			if c.cut != calls[0].cut {
				t.Fatalf("retry changed the cut: %+v", calls)
			}
		}
		requirePending(t, store, 0)
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1, Retries: 3})
	})
}

func TestASettleFailureRedeliversWithoutASecondClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var relay *subscription.CutRelay
		service, store, _, _ := streamers(t, func() { relay.Notify() })
		live := newRig(t, store, nil)
		relay = live.relay
		conn := newConn(10 * time.Millisecond)
		register(t, live.registry, editorGrant(t, service, streamerA, "session-a"), conn)
		store.FailOnce("SettleCut", errors.New("store down"))
		live.start(t)
		synctest.Wait()

		deactivateA(t, service, deactivateOp)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		requirePending(t, store, 1)
		requireCounters(t, relay, subscription.RelayCounters{StoreErrors: 1})
		time.Sleep(time.Second)
		synctest.Wait()

		calls := live.cutter.recorded()
		if len(calls) != 2 || calls[0].err != nil || calls[1].err != nil || calls[1].cut != calls[0].cut ||
			!reflect.DeepEqual(calls[1].report, calls[0].report) || len(calls[0].report.Closures) != 1 {
			t.Fatalf("calls = %+v, want one cut and one replay of its answer", calls)
		}
		requireClosedOnce(t, conn, false)
		requirePending(t, store, 0)
		requireCounters(t, relay, subscription.RelayCounters{Applied: 1, StoreErrors: 1})
	})
}

func TestEachApplyOutcomeSettlesOrKeepsTheCut(t *testing.T) {
	cases := []struct {
		name     string
		answer   error
		settled  bool
		counters subscription.RelayCounters
		level    string
		msg      string
	}{
		{"applied", nil, true, subscription.RelayCounters{Applied: 1}, "INFO", "access cut applied"},
		{"over budget", fmt.Errorf("wrapped: %w", revocation.ErrBudgetExceeded), true, subscription.RelayCounters{Applied: 1, Exceeded: 1}, "ERROR", "access cut applied over the close budget"},
		{"invalid", fmt.Errorf("%w: malformed", revocation.ErrInvalidCut), true, subscription.RelayCounters{Rejected: 1}, "ERROR", "access cut rejected"},
		{"operation conflict", revocation.ErrOperationConflict, true, subscription.RelayCounters{Rejected: 1}, "ERROR", "access cut rejected"},
		{"registry closed", revocation.ErrClosed, false, subscription.RelayCounters{Retries: 1}, "WARN", "access cut apply failed"},
		{"timeout", context.DeadlineExceeded, false, subscription.RelayCounters{Retries: 1}, "WARN", "access cut apply failed"},
		{"canceled", context.Canceled, false, subscription.RelayCounters{Retries: 1}, "WARN", "access cut apply failed"},
		{"unknown", errors.New("boom"), false, subscription.RelayCounters{Retries: 1}, "WARN", "access cut apply failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var relay *subscription.CutRelay
				service, store, a, _ := streamers(t, func() { relay.Notify() })
				live := newRig(t, store, nil)
				relay = live.relay
				// The scripted answer never reaches a registry.
				live.cutter.inner = nil
				live.cutter.script = []error{tc.answer}
				if tc.answer == nil {
					live.cutter.inner = live.registry
				}
				live.start(t)
				deactivateA(t, service, deactivateOp)
				synctest.Wait()

				cut := onlyCut(t, store)
				calls := live.cutter.recorded()
				if len(calls) != 1 || calls[0].cut != wantCut(cut) || calls[0].correlation != cutCorrelation {
					t.Fatalf("calls = %+v, want one Apply of %+v", calls, wantCut(cut))
				}
				if _, settled := store.AppliedAt(cut.ID); settled != tc.settled {
					t.Fatalf("settled = %v, want %v", settled, tc.settled)
				}
				requireCounters(t, relay, tc.counters)
				found := recordsWith(live.logs(), tc.msg)
				if len(found) != 1 || found[0]["level"] != tc.level || found[0]["tenant_id"] != a.TenantID ||
					found[0][logging.CorrelationKey] != cutCorrelation || found[0]["cut_id"] != float64(cut.ID) {
					t.Fatalf("%q records = %v, want one %s record of the cut", tc.msg, found, tc.level)
				}
			})
		})
	}
}

func TestAStoreReadFailureIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crashed, store, _, _ := streamers(t, nil)
		deactivateA(t, crashed, deactivateOp)
		store.FailOnce("PendingCuts", errors.New("store down"))
		live := newRig(t, store, nil)
		live.start(t)
		synctest.Wait()
		requirePending(t, store, 1)
		requireCounters(t, live.relay, subscription.RelayCounters{StoreErrors: 1})
		if n := len(recordsWith(live.logs(), "access cut store failed")); n != 1 {
			t.Fatalf("store failure records = %d, want 1", n)
		}

		time.Sleep(subscription.DefaultRelayMinBackoff - time.Millisecond)
		synctest.Wait()
		requirePending(t, store, 1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		requirePending(t, store, 0)
		requireCounters(t, live.relay, subscription.RelayCounters{Applied: 1, StoreErrors: 1})
	})
}

func TestACutCommittedWithoutNotifyIsFoundByPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, store, _, _ := streamers(t, nil)
		live := newRig(t, store, nil)
		live.start(t)
		synctest.Wait()

		deactivateA(t, service, deactivateOp)
		time.Sleep(subscription.DefaultRelayPollInterval - time.Millisecond)
		synctest.Wait()
		requirePending(t, store, 1)
		time.Sleep(time.Millisecond)
		synctest.Wait()
		requirePending(t, store, 0)
		requireCounters(t, live.relay, subscription.RelayCounters{Applied: 1})
	})
}

func TestABacklogLargerThanABatchIsAppliedInOneWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crashed, store, _, _ := streamers(t, nil)
		if _, err := crashed.Activate(context.Background(), activate("activation-s-0001", "stranger@example.com")); err != nil {
			t.Fatal(err)
		}
		for i, email := range []string{"a@example.com", "b@example.com", "stranger@example.com"} {
			if _, err := crashed.Deactivate(context.Background(), deactivate(fmt.Sprintf("deactivation-%05d", i), email)); err != nil {
				t.Fatal(err)
			}
		}
		live := newRig(t, store, func(c *subscription.RelayConfig) { c.BatchSize = 2 })
		live.start(t)
		synctest.Wait()

		cuts := store.Cuts()
		calls := live.cutter.recorded()
		if len(cuts) != 3 || len(calls) != 3 {
			t.Fatalf("cuts=%d calls=%d, want 3 and 3", len(cuts), len(calls))
		}
		for i, c := range calls {
			if c.cut != wantCut(cuts[i]) {
				t.Fatalf("call %d = %+v, want %+v oldest first", i, c.cut, wantCut(cuts[i]))
			}
		}
		requirePending(t, store, 0)
		requireCounters(t, live.relay, subscription.RelayCounters{Applied: 3})
	})
}

func TestOnlyACommitThatWroteACutWakesTheRelay(t *testing.T) {
	var mu sync.Mutex
	wakes := 0
	service, store, _, _ := streamers(t, func() {
		mu.Lock()
		defer mu.Unlock()
		wakes++
	})
	requireWakes := func(want int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if wakes != want {
			t.Fatalf("wakes = %d, want %d", wakes, want)
		}
	}
	ctx := context.Background()
	requireWakes(0) // activations write no cut

	store.FailOnce("commit", errors.New("commit lost"))
	if _, err := service.Deactivate(ctx, deactivate(deactivateOp, "a@example.com")); err == nil {
		t.Fatal("Deactivate with a failed commit succeeded")
	}
	requireWakes(0)

	first, err := service.Deactivate(ctx, deactivate(deactivateOp, "a@example.com"))
	if err != nil || !first.Deactivated {
		t.Fatalf("Deactivate = %+v, %v", first, err)
	}
	requireWakes(1)

	// A replay, a no-op deactivation, and a rejected one wake nothing.
	if replay, err := service.Deactivate(ctx, deactivate(deactivateOp, "a@example.com")); err != nil || replay != first {
		t.Fatalf("replay = %+v, %v; want %+v", replay, err, first)
	}
	if noop, err := service.Deactivate(ctx, deactivate("deactivation-00002", "a@example.com")); err != nil || noop.Deactivated {
		t.Fatalf("second deactivation = %+v, %v; want a no-op", noop, err)
	}
	if _, err := service.Deactivate(ctx, deactivate(deactivateOp, "b@example.com")); !errors.Is(err, subscription.ErrOperationConflict) {
		t.Fatalf("conflict = %v", err)
	}
	if _, err := service.Activate(ctx, activate("activation-a-0002", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	requireWakes(1)
	if n := len(store.Cuts()); n != 1 {
		t.Fatalf("cuts = %d, want 1", n)
	}
}

func TestCloseStopsTheRelay(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, store, _, _ := streamers(t, nil)
			live := newRig(t, store, nil)
			if err := live.relay.Close(context.Background()); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if err := live.relay.Start(); !errors.Is(err, subscription.ErrRelayClosed) {
				t.Fatalf("Start after Close = %v, want ErrRelayClosed", err)
			}
			if live.relay.Running() {
				t.Fatal("a closed relay reports running")
			}
		})
	})
	t.Run("idle", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var relay *subscription.CutRelay
			service, store, _, _ := streamers(t, func() { relay.Notify() })
			live := newRig(t, store, nil)
			relay = live.relay
			live.start(t)
			if !relay.Running() {
				t.Fatal("a started relay is not running")
			}
			if err := relay.Start(); err == nil {
				t.Fatal("a second Start succeeded")
			}
			if err := relay.Close(context.Background()); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if relay.Running() {
				t.Fatal("a closed relay reports running")
			}
			// Notify after Close is a no-op: the cut stays pending.
			deactivateA(t, service, deactivateOp)
			time.Sleep(10 * subscription.DefaultRelayPollInterval)
			synctest.Wait()
			if calls := live.cutter.recorded(); len(calls) != 0 {
				t.Fatalf("calls after Close = %+v", calls)
			}
			requirePending(t, store, 1)
			if err := relay.Start(); !errors.Is(err, subscription.ErrRelayClosed) {
				t.Fatalf("Start after Close = %v, want ErrRelayClosed", err)
			}
			if err := relay.Close(context.Background()); err != nil {
				t.Fatalf("second Close = %v, want idempotent nil", err)
			}
		})
	})
	t.Run("drains the apply in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var relay *subscription.CutRelay
			service, store, _, _ := streamers(t, func() { relay.Notify() })
			live := newRig(t, store, nil)
			relay = live.relay
			conn := newConn(300 * time.Millisecond)
			register(t, live.registry, editorGrant(t, service, streamerA, "session-a"), conn)
			live.start(t)
			deactivateA(t, service, deactivateOp)
			synctest.Wait()

			start := time.Now()
			if err := relay.Close(context.Background()); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if waited := time.Since(start); waited != 300*time.Millisecond {
				t.Fatalf("Close returned after %v, want the 300ms close in flight", waited)
			}
			requireClosedOnce(t, conn, false)
			requirePending(t, store, 0)
			requireCounters(t, relay, subscription.RelayCounters{Applied: 1})
		})
	})
	t.Run("deadline cancels the apply in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var relay *subscription.CutRelay
			service, store, _, _ := streamers(t, func() { relay.Notify() })
			live := newRig(t, store, nil)
			relay = live.relay
			live.cutter.block = true
			live.start(t)
			deactivateA(t, service, deactivateOp)
			synctest.Wait()

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := relay.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Close = %v, want DeadlineExceeded", err)
			}
			if waited := time.Since(start); waited != 200*time.Millisecond {
				t.Fatalf("Close returned after %v, want the 200ms deadline", waited)
			}
			if calls := live.cutter.recorded(); len(calls) != 1 || !errors.Is(calls[0].err, context.Canceled) {
				t.Fatalf("calls = %+v, want the apply in flight canceled", calls)
			}
			requirePending(t, store, 1)
			requireCounters(t, relay, subscription.RelayCounters{})
		})
	})
}

func TestNewCutRelayRejectsAConfigThatCannotRun(t *testing.T) {
	store := subscriptiontest.NewMemoryStore()
	cutter := &recordingCutter{}
	for name, config := range map[string]subscription.RelayConfig{
		"no store":           {Cutter: cutter},
		"no cutter":          {Store: store},
		"negative poll":      {Store: store, Cutter: cutter, PollInterval: -1},
		"negative timeout":   {Store: store, Cutter: cutter, AttemptTimeout: -1},
		"negative backoff":   {Store: store, Cutter: cutter, MinBackoff: -1},
		"negative batch":     {Store: store, Cutter: cutter, BatchSize: -1},
		"inverted backoffs":  {Store: store, Cutter: cutter, MinBackoff: time.Second, MaxBackoff: time.Millisecond},
		"negative max retry": {Store: store, Cutter: cutter, MaxBackoff: -1},
	} {
		if _, err := subscription.NewCutRelay(config); !errors.Is(err, subscription.ErrInvalidRelayConfig) {
			t.Errorf("%s: NewCutRelay = %v, want ErrInvalidRelayConfig", name, err)
		}
	}
	if _, err := subscription.NewCutRelay(subscription.RelayConfig{Store: store, Cutter: cutter}); err != nil {
		t.Fatalf("defaults: NewCutRelay = %v", err)
	}
}
