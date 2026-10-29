package tracing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

// outageMutations is the number of committed resizes of each outage; each
// ends three spans: postgres, outbox.deliver and realtime.submit.
const outageMutations = 12

// outageBudget bounds committing and delivering every resize, however long
// the exporter is unavailable; a healthy run takes well under a second.
const outageBudget = 3 * time.Second

// blockingExporter never finishes an export before its context ends.
type blockingExporter struct{ calls chan struct{} }

func (e *blockingExporter) Export(ctx context.Context, _ []telemetry.SpanData) error {
	select {
	case e.calls <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// failingExporter fails every export at once.
type failingExporter struct{}

func (failingExporter) Export(context.Context, []telemetry.SpanData) error {
	return errors.New("collector unavailable")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// outageEnv is the cmd/server composition of the publisher over real
// PostgreSQL: the traced sink and actor deliver committed resizes to the hub.
type outageEnv struct {
	key       canvas.Key
	canvases  *postgres.CanvasStore
	publisher *outbox.Publisher
	hub       *realtime.Hub
	logs      *lockedBuffer
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	server := pgtest.Start(t)
	ctx := context.Background()
	pool, err := postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func startOutage(t *testing.T, pool *pgxpool.Pool, tenant string, tracer *telemetry.Tracer, logs *lockedBuffer) *outageEnv {
	t.Helper()
	ctx := context.Background()
	key := canvas.Key{TenantID: tenant, CanvasID: "main"}
	if _, err := pool.Exec(ctx, "INSERT INTO tenants (tenant_id, subscription_status) VALUES ($1, 'active')", tenant); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(logs, nil)))
	hub, err := realtime.NewHub(realtime.Config{Authorize: func(context.Context, realtime.Command) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := outbox.NewPublisher(outbox.Config{
		Store:        postgres.NewOutboxStore(pool),
		Sink:         tracing.Sink(outbox.NewHubSink(tracing.Actor(hub, tracer)), tracer),
		Logger:       logger,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	canvases := postgres.NewCanvasStore(pool, postgres.WithCommitted(func(canvas.Key) { publisher.Notify() }))
	if _, err := canvases.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := publisher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = publisher.Close(ctx)
		_ = hub.Close(ctx)
	})
	return &outageEnv{key: key, canvases: canvases, publisher: publisher, hub: hub, logs: logs}
}

// resizeAll commits outageMutations resizes, each in its own traced request
// context, and returns their correlations in commit order.
func (e *outageEnv) resizeAll(t *testing.T, tracer *telemetry.Tracer) []correlation.ID {
	t.Helper()
	ids := make([]correlation.ID, 0, outageMutations)
	for i := range outageMutations {
		id, err := correlation.New()
		if err != nil {
			t.Fatal(err)
		}
		ctx := telemetry.WithTracer(correlation.WithID(context.Background(), id), tracer)
		mutation := canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1000 + i, Height: 600}}
		operation := canvas.Operation{ID: fmt.Sprintf("op-outage-size-%02d", i), CorrelationID: id.String()}
		if _, err := e.canvases.Apply(ctx, e.key, operation, mutation); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// awaitHub waits until the hub accepted every resize committed since began,
// within outageBudget.
func (e *outageEnv) awaitHub(t *testing.T, ids []correlation.ID, began time.Time) {
	t.Helper()
	for {
		events, err := e.hub.Events(context.Background(), realtime.Key{TenantID: e.key.TenantID, CanvasID: e.key.CanvasID}, 0)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) == len(ids) {
			for i, event := range events {
				if event.CorrelationID != ids[i].String() {
					t.Fatalf("hub event %d correlation = %q, want %s", i, event.CorrelationID, ids[i])
				}
			}
			if took := time.Since(began); took > outageBudget {
				t.Fatalf("committing and delivering %d resizes took %v, over %v", len(ids), took, outageBudget)
			}
			return
		}
		if time.Since(began) > outageBudget {
			t.Fatalf("hub accepted %d of %d committed resizes within %v\n%s", len(events), len(ids), outageBudget, e.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// close stops the publisher, then the hub, as cmd/server does.
func (e *outageEnv) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.publisher.Close(ctx); err != nil {
		t.Errorf("publisher Close: %v", err)
	}
	if err := e.hub.Close(ctx); err != nil {
		t.Errorf("hub Close: %v", err)
	}
}

// checkCorrelatedLogs asserts that each resize's delivery is logged with
// its correlation.
func checkCorrelatedLogs(t *testing.T, logs string, ids []correlation.ID) {
	t.Helper()
	delivered := map[string]int{}
	for _, line := range strings.Split(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "outbox record delivered" {
			id, _ := record["correlation_id"].(string)
			delivered[id]++
		}
	}
	for _, id := range ids {
		if delivered[id.String()] != 1 {
			t.Errorf("delivery logs of %s = %d, want 1\n%s", id, delivered[id.String()], logs)
		}
	}
}

// S9, S13, L11b: over real PostgreSQL, an exporter that never answers neither
// delays nor stops the delivery of committed resizes; spans beyond the
// bounded queue are dropped and counted instead of held, the deliveries stay
// logged with their correlations, publisher and hub close cleanly, and
// closing the tracer gives up at its deadline and accounts for every span.
// An exporter that fails at once costs the same: the product continues and
// every span is counted failed.
func TestOutageOfTheExporterNeverBlocksDelivery(t *testing.T) {
	pool := migratedPool(t)

	t.Run("blocked exporter", func(t *testing.T) {
		const queue, batch = 4, 1
		logs := &lockedBuffer{}
		exporter := &blockingExporter{calls: make(chan struct{}, 1)}
		tracer, err := telemetry.New(telemetry.Config{
			Exporter: exporter, QueueSize: queue, BatchSize: batch,
			Interval: 10 * time.Millisecond, ExportTimeout: time.Hour,
			Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		env := startOutage(t, pool, "tenant-blocked", tracer, logs)
		began := time.Now()
		ids := env.resizeAll(t, tracer)
		env.awaitHub(t, ids, began)
		select {
		case <-exporter.calls:
		case <-time.After(time.Second):
			t.Fatal("the exporter was never called")
		}
		env.close(t)

		const ended = 3 * outageMutations
		stats := tracer.Stats()
		if stats.Exported != 0 || stats.Dropped < ended-queue-batch {
			t.Fatalf("stats while blocked = %+v, want nothing exported and at least %d dropped", stats, ended-queue-batch)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		closing := time.Now()
		err = tracer.Close(ctx)
		if took := time.Since(closing); !errors.Is(err, context.DeadlineExceeded) || took > time.Second {
			t.Fatalf("tracer Close = %v after %v, want the deadline error within 1s", err, took)
		}
		stats = tracer.Stats()
		if stats.Exported != 0 || stats.Failed != batch || stats.Dropped+stats.Failed+stats.Discarded != ended {
			t.Fatalf("stats after Close = %+v, want %d in flight failed and all %d spans accounted for", stats, batch, ended)
		}
		checkCorrelatedLogs(t, logs.String(), ids)
	})

	t.Run("failing exporter", func(t *testing.T) {
		logs := &lockedBuffer{}
		tracer, err := telemetry.New(telemetry.Config{
			Exporter: failingExporter{}, QueueSize: 8, BatchSize: 2,
			Interval: 10 * time.Millisecond,
			Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		env := startOutage(t, pool, "tenant-failing", tracer, logs)
		began := time.Now()
		ids := env.resizeAll(t, tracer)
		env.awaitHub(t, ids, began)
		env.close(t)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := tracer.Close(ctx); err != nil {
			t.Fatalf("tracer Close = %v, want nil", err)
		}
		const ended = 3 * outageMutations
		stats := tracer.Stats()
		if stats.Exported != 0 || stats.Discarded != 0 || stats.Failed == 0 || stats.Failed+stats.Dropped != ended {
			t.Fatalf("stats = %+v, want every one of %d spans failed or dropped", stats, ended)
		}
		text := logs.String()
		if !strings.Contains(text, `"msg":"telemetry export failed"`) || strings.Contains(text, "collector unavailable") {
			t.Fatalf("export failure logs = %s, want counts without the error text", text)
		}
		checkCorrelatedLogs(t, text, ids)
	})
}
