// Package tracingtest records exported spans in memory, so adapter tests can
// read the spans a traced call produced.
package tracingtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// Recorder is an exporter that keeps every exported span.
type Recorder struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
}

// Export keeps spans.
func (r *Recorder) Export(_ context.Context, spans []telemetry.SpanData) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

// Harness is a tracer whose spans end up in a Recorder.
type Harness struct {
	Tracer   *telemetry.Tracer
	recorder *Recorder
}

// Start returns a harness whose tracer is closed when the test ends.
func Start(t testing.TB) *Harness {
	t.Helper()
	recorder := &Recorder{}
	tracer, err := telemetry.New(telemetry.Config{Exporter: recorder})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = tracer.Close(context.Background()) })
	return &Harness{Tracer: tracer, recorder: recorder}
}

// Context returns ctx with the harness tracer.
func (h *Harness) Context(ctx context.Context) context.Context {
	return telemetry.WithTracer(ctx, h.Tracer)
}

// Spans closes the tracer, so every ended span is exported, and returns the
// exported spans in export order. Later spans are dropped.
func (h *Harness) Spans(t testing.TB) []telemetry.SpanData {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Tracer.Close(ctx); err != nil {
		t.Fatalf("tracer Close: %v", err)
	}
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	return append([]telemetry.SpanData(nil), h.recorder.spans...)
}

// Named returns the spans named name.
func Named(spans []telemetry.SpanData, name telemetry.SpanName) []telemetry.SpanData {
	var named []telemetry.SpanData
	for _, span := range spans {
		if span.Name == string(name) {
			named = append(named, span)
		}
	}
	return named
}
