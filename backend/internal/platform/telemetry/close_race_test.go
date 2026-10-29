package telemetry_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// countingExporter accepts every batch immediately.
type countingExporter struct{ spans atomic.Uint64 }

func (e *countingExporter) Export(_ context.Context, spans []telemetry.SpanData) error {
	e.spans.Add(uint64(len(spans)))
	return nil
}

// S9, S10: spans ended while Close runs are each counted exactly once, as
// exported, dropped, failed or discarded; none is lost or counted twice.
func TestCloseRacingEnqueueAccountsForEverySpan(t *testing.T) {
	const rounds, goroutines, perGoroutine = 50, 8, 200
	for round := 0; round < rounds; round++ {
		exporter := &countingExporter{}
		tracer, err := telemetry.New(telemetry.Config{Exporter: exporter, QueueSize: 64, BatchSize: 16})
		if err != nil {
			t.Fatal(err)
		}
		ctx := telemetry.WithTracer(context.Background(), tracer)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < perGoroutine; i++ {
					_, span := telemetry.Start(ctx, telemetry.SpanPostgres)
					span.End(nil)
				}
			}()
		}
		close(start)
		if err := tracer.Close(context.Background()); err != nil {
			t.Fatalf("round %d: Close() = %v", round, err)
		}
		wg.Wait()
		stats := tracer.Stats()
		total := stats.Exported + stats.Dropped + stats.Failed + stats.Discarded
		if total != goroutines*perGoroutine || stats.Exported != exporter.spans.Load() {
			t.Fatalf("round %d: stats = %+v (total %d), exporter saw %d, want total %d",
				round, stats, total, exporter.spans.Load(), goroutines*perGoroutine)
		}
	}
}
