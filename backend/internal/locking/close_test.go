package locking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
)

// countingClock is the system clock that tracks timers still pending.
type countingClock struct {
	mu      sync.Mutex
	pending int
}

type countedTimer struct {
	clock *countingClock
	timer *time.Timer
	once  sync.Once
}

func (c *countingClock) Now() time.Time { return time.Now() }

func (c *countingClock) AfterFunc(d time.Duration, f func()) locking.Timer {
	c.mu.Lock()
	c.pending++
	c.mu.Unlock()
	t := &countedTimer{clock: c}
	t.timer = time.AfterFunc(d, func() {
		t.done()
		f()
	})
	return t
}

func (t *countedTimer) Stop() bool {
	stopped := t.timer.Stop()
	if stopped {
		t.done()
	}
	return stopped
}

func (t *countedTimer) done() {
	t.once.Do(func() {
		t.clock.mu.Lock()
		t.clock.pending--
		t.clock.mu.Unlock()
	})
}

func (c *countingClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending
}

func TestCloseDrainsInflightWorkReleasesEveryLeaseAndRejectsNewWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &countingClock{}
		slow := make(chan struct{})
		manager := newManager(t, locking.Config{Clock: clock, Authorize: func(ctx context.Context, _ locking.Holder, k locking.Key) error {
			if k.ObjectID == "slow" {
				close(slow)
				select {
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}})
		conn, disconnect := context.WithCancel(context.Background())
		defer disconnect()
		sa := attach(t, manager, conn, holder("tenant-a", "alice", "s-alice"))
		sb := attach(t, manager, context.Background(), holder("tenant-b", "bob", "s-bob"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		acquire(t, sb, request("op-2", "canvas-1", "object-1"))

		inflight := make(chan error, 1)
		go func() {
			_, err := sa.Acquire(context.Background(), request("op-3", "canvas-1", "slow"))
			inflight <- err
		}()
		<-slow
		start := time.Now()
		if err := manager.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
			t.Fatalf("drain took %v, want the 100ms in-flight authorization", elapsed)
		}
		if err := <-inflight; !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("in-flight Acquire: err = %v, want ErrClosed", err)
		}
		for _, k := range []locking.Key{key("tenant-a", "canvas-1", "object-1"), key("tenant-b", "canvas-1", "object-1"), key("tenant-a", "canvas-1", "slow")} {
			wantFree(t, manager, k)
		}
		if pending := clock.Pending(); pending != 0 {
			t.Fatalf("%d expiry timers still pending after Close", pending)
		}

		if _, err := manager.Attach(context.Background(), holder("tenant-a", "carol", "s-carol")); !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("Attach after Close: err = %v, want ErrClosed", err)
		}
		if _, err := sa.Acquire(context.Background(), request("op-4", "canvas-1", "object-1")); !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("Acquire after Close: err = %v, want ErrClosed", err)
		}
		if _, err := sa.Heartbeat(context.Background()); !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("Heartbeat after Close: err = %v, want ErrClosed", err)
		}
		if _, err := sb.Release(context.Background(), release("op-5", "canvas-1", "object-1", locking.ReasonFinished)); !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("Release after Close: err = %v, want ErrClosed", err)
		}
		if err := manager.Close(context.Background()); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		disconnect()
		sa.Detach()
	})
}

func TestCloseDeadlineCancelsInflightAuthorization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := make(chan struct{})
		manager := newManager(t, locking.Config{Authorize: func(ctx context.Context, _ locking.Holder, _ locking.Key) error {
			close(blocked)
			<-ctx.Done()
			return ctx.Err()
		}})
		sa := attach(t, manager, context.Background(), holder("tenant-a", "alice", "s-alice"))
		inflight := make(chan error, 1)
		go func() {
			_, err := sa.Acquire(context.Background(), request("op-1", "canvas-1", "object-1"))
			inflight <- err
		}()
		<-blocked

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close: err = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 200*time.Millisecond {
			t.Fatalf("Close returned after %v, want the 200ms deadline", elapsed)
		}
		if err := <-inflight; !errors.Is(err, locking.ErrClosed) {
			t.Fatalf("in-flight Acquire: err = %v, want ErrClosed", err)
		}
		wantFree(t, manager, key("tenant-a", "canvas-1", "object-1"))
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

func correlated(t *testing.T) (context.Context, string) {
	t.Helper()
	id, err := correlation.New()
	if err != nil {
		t.Fatal(err)
	}
	return correlation.WithID(context.Background(), id), id.String()
}

func TestLeaseRecordsCarryTheCorrelationOfTheOperationThatSetThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, records := captureLogs(t)
		manager := newManager(t, locking.Config{Logger: logger})
		// Identifier-shaped decoys: none may reach the logs.
		decoys := []string{"pcs_sessiondecoy4f1c", "acct-decoy-7e11"}
		sa := attach(t, manager, context.Background(), holder("tenant-a", decoys[1], decoys[0]))
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))

		acquireCtx, acquireID := correlated(t)
		if _, err := sa.Acquire(acquireCtx, request("op-1", "canvas-1", "object-1")); err != nil {
			t.Fatal(err)
		}
		deniedCtx, deniedID := correlated(t)
		if _, err := sb.Acquire(deniedCtx, request("op-2", "canvas-1", "object-1")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(locking.DefaultTTL)
		synctest.Wait()

		releaseAcquireCtx, releaseAcquireID := correlated(t)
		if _, err := sb.Acquire(releaseAcquireCtx, request("op-3", "canvas-1", "object-1")); err != nil {
			t.Fatal(err)
		}
		releaseCtx, releaseID := correlated(t)
		if _, err := sb.Release(releaseCtx, release("op-4", "canvas-1", "object-1", locking.ReasonCancelled)); err != nil {
			t.Fatal(err)
		}

		type line struct{ msg, outcome, reason, correlation string }
		want := []line{
			{"lease acquire", "granted", "", acquireID},
			{"lease acquire", "denied", "", deniedID},
			{"lease ended", "", "expired", acquireID},
			{"lease acquire", "granted", "", releaseAcquireID},
			{"lease ended", "", "cancelled", releaseID},
		}
		got := records()
		if len(got) != len(want) {
			t.Fatalf("got %d records %v, want %d", len(got), got, len(want))
		}
		for i, record := range got {
			str := func(name string) string { s, _ := record[name].(string); return s }
			if (line{str("msg"), str("outcome"), str("reason"), str(logging.CorrelationKey)}) != want[i] ||
				str("tenant_id") != "tenant-a" || str("canvas_id") != "canvas-1" || str("object_id") != "object-1" {
				t.Fatalf("record %d = %v, want %+v on tenant-a/canvas-1/object-1", i, record, want[i])
			}
		}
		raw, _ := json.Marshal(got)
		for _, decoy := range decoys {
			if strings.Contains(string(raw), decoy) {
				t.Fatalf("logs leak holder identifier %q: %s", decoy, raw)
			}
		}
	})
}

func TestDetachAndCloseRecordWhyEachLeaseEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, records := captureLogs(t)
		manager := newManager(t, locking.Config{Logger: logger})
		connCtx, connID := correlated(t)
		conn, disconnect := context.WithCancel(connCtx)
		sa := attach(t, manager, conn, holder("tenant-a", "alice", "s-alice"))
		sb := attach(t, manager, context.Background(), holder("tenant-a", "bob", "s-bob"))
		acquire(t, sa, request("op-1", "canvas-1", "object-1"))
		acquire(t, sb, request("op-2", "canvas-1", "object-2"))

		disconnect()
		synctest.Wait()
		closeCtx, closeID := correlated(t)
		if err := manager.Close(closeCtx); err != nil {
			t.Fatal(err)
		}

		var ended []string
		for _, record := range records() {
			if record["msg"] != "lease ended" {
				continue
			}
			ended = append(ended, record["object_id"].(string)+":"+record["reason"].(string)+":"+record[logging.CorrelationKey].(string))
		}
		want := []string{"object-1:detached:" + connID, "object-2:shutdown:" + closeID}
		if strings.Join(ended, ",") != strings.Join(want, ",") {
			t.Fatalf("ended records = %v, want %v", ended, want)
		}
	})
}
