package locksession_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/locksession"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
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

type logRecord map[string]any

func captureLogs(t *testing.T) (*slog.Logger, func() ([]logRecord, string)) {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	return logger, func() ([]logRecord, string) {
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
		return records, buf.String()
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

func TestRecordsCarryTheReasonTenantAndCorrelationButNoIdentifiers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, records := captureLogs(t)
		x := newFixture(t, locksession.Config{MaxPerAccount: 1, Logger: logger})
		// Identifier-shaped decoys: none may reach the logs.
		account, authSession, name := "acct-decoy-7e11", "authdecoy5a2b", "decoy.name@example.com"
		g := grant("tenant-a", account, authSession, 1)
		var ids []string
		open := func(ctx context.Context) string {
			id, err := x.directory.Open(ctx, g, name)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			ids = append(ids, id)
			return id
		}

		openCtx, openID := correlated(t)
		open(openCtx)
		evictCtx, evictID := correlated(t)
		second := open(evictCtx) // evicts first
		endCtx, endID := correlated(t)
		x.directory.End(endCtx, second, "tenant-a", account)

		idleOpenCtx, idleOpenID := correlated(t)
		third := open(idleOpenCtx)
		useCtx, useID := correlated(t)
		if err := x.directory.Use(useCtx, third, "tenant-a", account, func(*locking.Session) error { return nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(locking.DefaultTTL)
		synctest.Wait()

		revokeOpenCtx, revokeOpenID := correlated(t)
		open(revokeOpenCtx)
		cutCtx, _ := correlated(t)
		if _, err := x.registry.Apply(cutCtx, revocation.Cut{OperationID: "cut-1", Type: revocation.TypeRevocation,
			Target: revocation.Target{Kind: revocation.TargetAccount, TenantID: "tenant-a", ID: account}, Version: 2}); err != nil {
			t.Fatal(err)
		}

		shutdownOpenCtx, shutdownOpenID := correlated(t)
		if _, err := x.directory.Open(shutdownOpenCtx, grant("tenant-b", "bob", "auth-bob", 1), "bob@example.com"); err != nil {
			t.Fatal(err)
		}
		closeCtx, closeID := correlated(t)
		if err := x.directory.Close(closeCtx); err != nil {
			t.Fatal(err)
		}

		type line struct{ msg, reason, tenant, correlation string }
		want := []line{
			{"lock session opened", "", "tenant-a", openID},
			{"lock session ended", "evicted", "tenant-a", evictID},
			{"lock session opened", "", "tenant-a", evictID},
			{"lock session ended", "ended", "tenant-a", endID},
			{"lock session opened", "", "tenant-a", idleOpenID},
			{"lock session ended", "idle", "tenant-a", useID},
			{"lock session opened", "", "tenant-a", revokeOpenID},
			// The registry hands Conn.Close no request values, so a cut
			// logs the correlation of the session's last operation; the
			// registry's own records carry the cut's.
			{"lock session ended", "revoked", "tenant-a", revokeOpenID},
			{"lock session opened", "", "tenant-b", shutdownOpenID},
			{"lock session ended", "shutdown", "tenant-b", closeID},
		}
		var got []line
		parsed, raw := records()
		for _, record := range parsed {
			if !strings.HasPrefix(record["msg"].(string), "lock session") {
				continue
			}
			str := func(name string) string { s, _ := record[name].(string); return s }
			got = append(got, line{str("msg"), str("reason"), str("tenant_id"), str(logging.CorrelationKey)})
			for key := range record {
				switch key {
				case "time", "level", "msg", "reason", "tenant_id", logging.CorrelationKey:
				default:
					t.Errorf("record %v carries unexpected attribute %q", record, key)
				}
			}
		}
		if len(got) != len(want) {
			t.Fatalf("got records %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
			}
		}
		for _, decoy := range append(ids, account, authSession, name) {
			if strings.Contains(raw, decoy) {
				t.Fatalf("logs leak %q: %s", decoy, raw)
			}
		}
	})
}
