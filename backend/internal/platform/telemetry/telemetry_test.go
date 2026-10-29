package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

const (
	traceHex = "4bf92f3577b34da6a3ce929d0e0e4736"
	spanHex  = "00f067aa0ba902b7"
)

func TestParseTraceparentAcceptsOnlyTheCanonicalForm(t *testing.T) {
	sc, err := telemetry.ParseTraceparent("00-" + traceHex + "-" + spanHex + "-01")
	if err != nil {
		t.Fatalf("ParseTraceparent(valid) = %v", err)
	}
	if sc.TraceID.String() != traceHex || string(sc.SpanID) != spanHex {
		t.Fatalf("ParseTraceparent = %+v", sc)
	}
	if got := sc.Traceparent(); got != "00-"+traceHex+"-"+spanHex+"-01" {
		t.Fatalf("Traceparent() = %q", got)
	}
	for _, bad := range []string{
		"",
		"00-" + strings.ToUpper(traceHex) + "-" + spanHex + "-01",
		"ff-" + traceHex + "-" + spanHex + "-01",
		"01-" + traceHex + "-" + spanHex + "-01",
		"00-00000000000000000000000000000000-" + spanHex + "-01",
		"00-" + traceHex + "-0000000000000000-01",
		"00-" + traceHex[1:] + "-" + spanHex + "-01",
		"00-" + traceHex + "-" + spanHex + "-0",
		"00-" + traceHex + "-" + spanHex + "-01-extra",
		"00-" + traceHex + "-" + spanHex + "-zz",
		"00_" + traceHex + "-" + spanHex + "-01",
	} {
		if _, err := telemetry.ParseTraceparent(bad); !errors.Is(err, telemetry.ErrInvalidTraceparent) {
			t.Errorf("ParseTraceparent(%q) = %v, want ErrInvalidTraceparent", bad, err)
		}
	}
}

// recorder is an exporter that keeps every exported span.
type recorder struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
	calls int
}

func (r *recorder) Export(_ context.Context, spans []telemetry.SpanData) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *recorder) all() []telemetry.SpanData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.SpanData(nil), r.spans...)
}

func newTracer(t *testing.T, exporter telemetry.Exporter, mutate ...func(*telemetry.Config)) *telemetry.Tracer {
	t.Helper()
	config := telemetry.Config{Exporter: exporter, Logger: slog.New(slog.DiscardHandler)}
	for _, m := range mutate {
		m(&config)
	}
	tracer, err := telemetry.New(config)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return tracer
}

func closeTracer(t *testing.T, tracer *telemetry.Tracer) {
	t.Helper()
	if err := tracer.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestStartUsesTheCorrelationIDAsTraceID(t *testing.T) {
	rec := &recorder{}
	tracer := newTracer(t, rec)
	id, _ := correlation.New()
	ctx := telemetry.WithTracer(correlation.WithID(context.Background(), id), tracer)

	ctx, root := telemetry.Start(ctx, telemetry.SpanHTTPRequest)
	_, child := telemetry.Start(ctx, telemetry.SpanPostgres)
	child.End(nil)
	root.End(nil)
	closeTracer(t, tracer)

	spans := rec.all()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(spans))
	}
	childData, rootData := spans[0], spans[1]
	if rootData.TraceID != id.String() || childData.TraceID != id.String() {
		t.Fatalf("trace IDs = %q, %q, want %q", rootData.TraceID, childData.TraceID, id)
	}
	if rootData.ParentSpanID != "" {
		t.Fatalf("root parent = %q, want none", rootData.ParentSpanID)
	}
	if childData.ParentSpanID != rootData.SpanID || childData.SpanID == rootData.SpanID {
		t.Fatalf("child = %+v, root = %+v", childData, rootData)
	}
	if rootData.Name != "http.request" || childData.Name != "postgres" || rootData.Status != "ok" {
		t.Fatalf("names/status = %q %q %q", rootData.Name, childData.Name, rootData.Status)
	}
	if rootData.End.Before(rootData.Start) {
		t.Fatalf("root ends before it starts: %+v", rootData)
	}
}

func TestStartWithoutCorrelationAssignsOneToTheContext(t *testing.T) {
	// Without any tracer the context still gains a correlation, so local
	// logs stay correlated when export is disabled.
	ctx, span := telemetry.Start(context.Background(), telemetry.SpanOutboxDeliver)
	id, ok := correlation.FromContext(ctx)
	if !ok {
		t.Fatal("no correlation ID in the span context")
	}
	sc, ok := telemetry.SpanContextFrom(ctx)
	if !ok || sc.TraceID != id {
		t.Fatalf("span context = %+v, %v; want trace %q", sc, ok, id)
	}
	span.End(errors.New("boom"))
	span.End(nil) // a second End is ignored
}

func TestResumeContinuesAStoredCorrelation(t *testing.T) {
	rec := &recorder{}
	tracer := newTracer(t, rec)
	ctx := telemetry.WithTracer(context.Background(), tracer)
	ctx = telemetry.Resume(ctx, traceHex)
	_, span := telemetry.Start(ctx, telemetry.SpanOutboxDeliver)
	span.End(nil)
	// A malformed stored value starts a fresh trace instead of failing.
	other, span2 := telemetry.Start(telemetry.Resume(telemetry.WithTracer(context.Background(), tracer), "not-hex"), telemetry.SpanOutboxDeliver)
	span2.End(nil)
	closeTracer(t, tracer)
	spans := rec.all()
	if spans[0].TraceID != traceHex {
		t.Fatalf("resumed trace = %q, want %q", spans[0].TraceID, traceHex)
	}
	if id, _ := correlation.FromContext(other); id.String() == traceHex || spans[1].TraceID != id.String() {
		t.Fatalf("malformed resume trace = %q, ctx %q", spans[1].TraceID, id)
	}
}

func TestAttributesAreClosedRedactedAndBounded(t *testing.T) {
	rec := &recorder{}
	tracer := newTracer(t, rec)
	ctx := telemetry.WithTracer(context.Background(), tracer)
	decoy := "pcs_" + strings.Repeat("A", 43)
	_, span := telemetry.Start(ctx, telemetry.SpanName("tenant-"+decoy),
		telemetry.Attr("route", "GET /api/x?token="+decoy),
		telemetry.Attr("tenant_id", "t1"),
		telemetry.Attr("operation", strings.Repeat("o", 200)),
	)
	span.SetAttr("outcome", decoy)
	span.SetAttr("status", "500")
	span.End(errors.New("failure with " + decoy))
	closeTracer(t, tracer)

	got := rec.all()[0]
	if got.Name != "unknown" {
		t.Fatalf("Name = %q, want unknown", got.Name)
	}
	if _, ok := got.Attributes["tenant_id"]; ok {
		t.Fatalf("attribute outside the closed set kept: %v", got.Attributes)
	}
	if len(got.Attributes["operation"]) != telemetry.MaxAttributeBytes {
		t.Fatalf("operation length = %d, want %d", len(got.Attributes["operation"]), telemetry.MaxAttributeBytes)
	}
	if got.Status != "error" || got.Attributes["status"] != "500" {
		t.Fatalf("status = %q attrs = %v", got.Status, got.Attributes)
	}
	var all bytes.Buffer
	all.WriteString(got.Name)
	for k, v := range got.Attributes {
		all.WriteString(k + "=" + v)
	}
	if strings.Contains(all.String(), decoy) || strings.Contains(all.String(), "failure") {
		t.Fatalf("span leaks a secret or error text: %+v", got)
	}
}

// stuck is an exporter that blocks until its context ends.
type stuck struct {
	entered chan struct{}
	once    sync.Once
}

func (s *stuck) Export(ctx context.Context, _ []telemetry.SpanData) error {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func TestAStuckExporterNeverBlocksSpansAndDropsBeyondTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exporter := &stuck{entered: make(chan struct{})}
		tracer := newTracer(t, exporter, func(c *telemetry.Config) {
			c.QueueSize = 4
			c.BatchSize = 2
			c.ExportTimeout = time.Hour
		})
		ctx := telemetry.WithTracer(context.Background(), tracer)
		for range 2 {
			_, span := telemetry.Start(ctx, telemetry.SpanS3)
			span.End(nil)
		}
		<-exporter.entered // the worker holds the first batch
		for range 10 {
			_, span := telemetry.Start(ctx, telemetry.SpanS3)
			span.End(nil) // must return at once
		}
		if got := tracer.Stats(); got.Dropped != 6 {
			t.Fatalf("Stats = %+v, want 6 dropped (queue of 4)", got)
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := tracer.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want DeadlineExceeded", err)
		}
		got := tracer.Stats()
		if got.Failed != 2 || got.Discarded != 4 || got.Exported != 0 {
			t.Fatalf("Stats after Close = %+v, want 2 failed, 4 discarded", got)
		}
	})
}

// failing is an exporter that always fails.
type failing struct{}

func (failing) Export(context.Context, []telemetry.SpanData) error {
	return errors.New("collector down")
}

func TestExportFailuresAreCountedAndLoggedWithoutContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		tracer := newTracer(t, failing{}, func(c *telemetry.Config) {
			c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		})
		ctx := telemetry.WithTracer(context.Background(), tracer)
		for range 3 {
			_, span := telemetry.Start(ctx, telemetry.SpanSubprocess, telemetry.Attr("operation", "ffprobe"))
			span.End(nil)
		}
		closeTracer(t, tracer)
		if got := tracer.Stats(); got.Failed != 3 || got.Exported != 0 {
			t.Fatalf("Stats = %+v, want 3 failed", got)
		}
		if !strings.Contains(logs.String(), "telemetry export failed") || strings.Contains(logs.String(), "ffprobe") {
			t.Fatalf("logs = %s", logs.String())
		}
	})
}

func TestCloseDrainsQueuedSpansAndRejectsLaterOnes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		tracer := newTracer(t, rec, func(c *telemetry.Config) { c.Interval = time.Hour })
		ctx := telemetry.WithTracer(context.Background(), tracer)
		for range 5 {
			_, span := telemetry.Start(ctx, telemetry.SpanRealtimeSubmit)
			span.End(nil)
		}
		closeTracer(t, tracer)
		if got := len(rec.all()); got != 5 {
			t.Fatalf("exported %d spans before Close returned, want 5", got)
		}
		_, late := telemetry.Start(ctx, telemetry.SpanRealtimeSubmit)
		late.End(nil)
		if got := tracer.Stats(); got.Exported != 5 || got.Dropped != 1 {
			t.Fatalf("Stats = %+v, want 5 exported, 1 dropped", got)
		}
		closeTracer(t, tracer) // idempotent
	})
}

func TestTheWorkerFlushesOnItsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		tracer := newTracer(t, rec, func(c *telemetry.Config) { c.Interval = time.Second; c.BatchSize = 100 })
		ctx := telemetry.WithTracer(context.Background(), tracer)
		_, span := telemetry.Start(ctx, telemetry.SpanHTTPRequest)
		span.End(nil)
		time.Sleep(time.Second)
		synctest.Wait()
		if got := len(rec.all()); got != 1 {
			t.Fatalf("exported %d spans after one interval, want 1", got)
		}
		closeTracer(t, tracer)
	})
}

func TestNewRejectsAMissingExporter(t *testing.T) {
	if _, err := telemetry.New(telemetry.Config{}); !errors.Is(err, telemetry.ErrInvalidConfig) {
		t.Fatalf("New(no exporter) = %v, want ErrInvalidConfig", err)
	}
	if _, err := telemetry.New(telemetry.Config{Exporter: failing{}, QueueSize: -1}); !errors.Is(err, telemetry.ErrInvalidConfig) {
		t.Fatalf("New(negative queue) = %v, want ErrInvalidConfig", err)
	}
}
