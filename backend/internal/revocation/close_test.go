package revocation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
)

func TestCloseShutsDownConnectionsWithRetryableReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		fast := newConn(100 * time.Millisecond)
		hung := newConn(-1)
		leases := []*Lease{
			register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), fast),
			register(t, registry, obsGrant("tenant-b", "cred-1", 5), hung),
		}

		start := time.Now()
		closed := make(chan error, 1)
		go func() { closed <- registry.Close(context.Background()) }()
		synctest.Wait()

		if _, err := registry.Register(editorGrant("tenant-a", "acct-2", "sess-2", 5), newConn(0)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Register during shutdown error = %v, want ErrClosed", err)
		}
		if _, err := registry.Apply(context.Background(), deactivation("op-1", "tenant-a", 10)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Apply during shutdown error = %v, want ErrClosed", err)
		}
		for _, lease := range leases {
			if err := lease.Guard(func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
				t.Fatalf("Guard during shutdown error = %v, want ErrClosed", err)
			}
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > DefaultBudget {
			t.Fatalf("Close took %v, want <= %v", elapsed, DefaultBudget)
		}
		for _, conn := range []*fakeConn{fast, hung} {
			reason, closes, _ := conn.state()
			if reason != ReasonShutdown || !reason.Retryable() || closes != 1 {
				t.Fatalf("reason=%q closes=%d, want retryable shutdown once", reason, closes)
			}
		}
		if _, _, aborted := hung.state(); !aborted {
			t.Fatal("hung connection was not aborted within the budget")
		}
		if err := registry.Close(context.Background()); err != nil {
			t.Fatalf("second Close() error = %v, want idempotent nil", err)
		}
	})
}

func TestCloseWaitsForCutsInProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := newConn(time.Second)
		register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), conn)

		applied := make(chan error, 1)
		go func() {
			_, err := registry.Apply(context.Background(), accountRevocation("op-1", "tenant-a", "acct-1", 10))
			applied <- err
		}()
		synctest.Wait()

		start := time.Now()
		if err := registry.Close(context.Background()); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("Close returned after %v, want the 1s cut in progress", waited)
		}
		if err := <-applied; err != nil {
			t.Fatalf("Apply error = %v", err)
		}
		if reason, closes, _ := conn.state(); reason != ReasonRevoked || closes != 1 {
			t.Fatalf("reason=%q closes=%d, want the cut's reason once", reason, closes)
		}
	})
}

func TestCloseDeadlineAbortsRemainingConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conns := []*fakeConn{newConn(time.Second), newConn(-1)}
		register(t, registry, editorGrant("tenant-a", "acct-1", "sess-1", 5), conns[0])
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conns[1])

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := registry.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close() error = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 200*time.Millisecond {
			t.Fatalf("Close returned after %v, want the 200ms deadline", elapsed)
		}
		for _, conn := range conns {
			if _, _, aborted := conn.state(); !aborted {
				t.Fatal("connection still open after the Close deadline")
			}
		}
	})
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

func TestCutConfirmationAndEveryClosureShareTheCorrelationID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, records := captureLogs(t)
		registry, err := NewRegistry(Config{Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		// Identifier-shaped decoys: none may reach the logs.
		decoys := []string{"pcs_sessiondecoy4f1c", "pco_credentialdecoy9a2b", "acct-decoy-7e11"}
		register(t, registry, editorGrant("tenant-a", decoys[2], decoys[0], 5), newConn(100*time.Millisecond))
		register(t, registry, editorGrant("tenant-a", "acct-2", "sess-2", 5), newConn(-1))
		register(t, registry, obsGrant("tenant-a", decoys[1], 5), newConn(0))

		id, err := correlation.New()
		if err != nil {
			t.Fatal(err)
		}
		ctx := correlation.WithID(context.Background(), id)
		if _, err := registry.Apply(ctx, deactivation("op-1", "tenant-a", 10)); err != nil {
			t.Fatal(err)
		}

		var confirmed, closed int
		for _, record := range records() {
			if record[logging.CorrelationKey] != id.String() {
				t.Fatalf("record %v lacks correlation %s", record, id)
			}
			switch record["msg"] {
			case "access cut confirmed":
				confirmed++
				if record["operation_id"] != "op-1" || record["type"] != "deactivation" || record["tenant_id"] != "tenant-a" {
					t.Fatalf("confirmation record = %v", record)
				}
			case "connection closed":
				closed++
				if record["reason"] != string(ReasonDeactivated) || record["latency_ms"] == nil {
					t.Fatalf("closure record = %v", record)
				}
			}
		}
		if confirmed != 1 || closed != 3 {
			t.Fatalf("confirmed=%d closed=%d, want 1 and 3", confirmed, closed)
		}
		raw, _ := json.Marshal(records())
		for _, decoy := range decoys {
			if strings.Contains(string(raw), decoy) {
				t.Fatalf("logs contain decoy %q", decoy)
			}
		}
	})
}

func TestBudgetExcessIsLoggedAsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, records := captureLogs(t)
		registry, err := NewRegistry(Config{Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		conn := newConn(-1)
		conn.hangAbort = true
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)
		if _, err := registry.Apply(context.Background(), rotation("op-1", "tenant-a", "cred-1", 10)); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("Apply error = %v, want ErrBudgetExceeded", err)
		}
		for _, record := range records() {
			if record["msg"] == "access cut exceeded close budget" && record["level"] == "ERROR" && record["exceeded"] == float64(1) {
				return
			}
		}
		t.Fatalf("no budget error record in %v", records())
	})
}

// slowAbortConn honors the Conn contract but its Close returns only some
// time after Abort, like a transport flushing its socket.
type slowAbortConn struct {
	abort    chan struct{}
	once     sync.Once
	returned atomic.Bool
}

func (c *slowAbortConn) Close(context.Context, Reason) error {
	<-c.abort
	time.Sleep(50 * time.Millisecond)
	c.returned.Store(true)
	return errAborted
}

func (c *slowAbortConn) Abort() { c.once.Do(func() { close(c.abort) }) }

func TestCloseAndCutWaitForAbortedTransportClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newRegistry(t)
		conn := &slowAbortConn{abort: make(chan struct{})}
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)
		if err := registry.Close(context.Background()); err != nil || !conn.returned.Load() {
			t.Fatalf("Close() error = %v, transport Close returned = %v; want nil and true", err, conn.returned.Load())
		}

		registry = newRegistry(t)
		conn = &slowAbortConn{abort: make(chan struct{})}
		register(t, registry, obsGrant("tenant-a", "cred-1", 5), conn)
		report := mustApply(t, registry, rotation("op-1", "tenant-a", "cred-1", 10))
		if !conn.returned.Load() {
			t.Fatal("Apply returned before the transport Close")
		}
		if got, want := report.MaxLatency, DefaultBudget*3/4+50*time.Millisecond; got != want {
			t.Fatalf("MaxLatency = %v, want %v including the transport drain", got, want)
		}
	})
}
