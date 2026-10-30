package postgres_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing/tracingtest"
)

const traceID = correlation.ID("fedcba9876543210fedcba9876543210")

// tracedContext is a request context: a correlation, the harness tracer, and
// a current parent span.
func tracedContext(h *tracingtest.Harness) (context.Context, telemetry.SpanContext) {
	ctx, _ := telemetry.Start(h.Context(correlation.WithID(context.Background(), traceID)), telemetry.SpanHTTPRequest)
	parent, _ := telemetry.SpanContextFrom(ctx)
	return ctx, parent
}

func wantPostgresSpans(t *testing.T, spans []telemetry.SpanData, parent telemetry.SpanContext, want []map[string]string, statuses []string) {
	t.Helper()
	got := tracingtest.Named(spans, telemetry.SpanPostgres)
	if len(got) != len(want) {
		t.Fatalf("postgres spans = %+v, want %d", got, len(want))
	}
	for i, span := range got {
		if span.TraceID != traceID.String() || span.ParentSpanID != string(parent.SpanID) || span.Status != statuses[i] {
			t.Fatalf("span %d = %+v, want trace %s, parent %s, status %s", i, span, traceID, parent.SpanID, statuses[i])
		}
		if !maps.Equal(span.Attributes, want[i]) {
			t.Fatalf("span %d attributes = %v, want %v", i, span.Attributes, want[i])
		}
	}
}

// S1, L9: CanvasStore.Apply runs in a postgres span of the request's trace,
// whose outcome tells a commit from a replay and records no error text.
func TestCanvasApplyRecordsAPostgresSpan(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	h := tracingtest.Start(t)
	ctx, parent := tracedContext(h)

	operation := nextOperation(t)
	resize := canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}}
	first, err := store.Apply(ctx, tenantA, operation, resize)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if again, err := store.Apply(ctx, tenantA, operation, resize); err != nil || again != first {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
	missing := canvas.Key{TenantID: "tenant-a", CanvasID: "missing"}
	if _, err := store.Apply(ctx, missing, nextOperation(t), resize); !errors.Is(err, canvas.ErrNotFound) {
		t.Fatalf("Apply on a missing canvas = %v, want ErrNotFound", err)
	}

	wantPostgresSpans(t, h.Spans(t), parent, []map[string]string{
		{"operation": "canvas.apply", "outcome": "committed"},
		{"operation": "canvas.apply", "outcome": "replayed"},
		{"operation": "canvas.apply", "outcome": "error"},
	}, []string{"ok", "ok", "error"})
}

// S1, L9: MediaStore.Apply runs in a postgres span of the request's trace.
func TestMediaApplyRecordsAPostgresSpan(t *testing.T) {
	store, _ := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	h := tracingtest.Start(t)
	ctx, parent := tracedContext(h)

	operation := nextMediaOperation()
	reserve := media.Reserve{DeclaredBytes: 10}
	first, err := store.Apply(ctx, mediaA, operation, reserve)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if again, err := store.Apply(ctx, mediaA, operation, reserve); err != nil || again.AssetID != first.AssetID {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
	if _, err := store.Apply(ctx, mediaA, nextMediaOperation(), media.Reserve{DeclaredBytes: media.QuotaBytes}); err == nil {
		t.Fatal("a reservation over the quota succeeded")
	}

	wantPostgresSpans(t, h.Spans(t), parent, []map[string]string{
		{"operation": "media.apply", "outcome": "committed"},
		{"operation": "media.apply", "outcome": "replayed"},
		{"operation": "media.apply", "outcome": "error"},
	}, []string{"ok", "ok", "error"})
}

// S9, S10: without a tracer or a correlation in the context, as on
// background paths, the stores keep committing.
func TestStoresApplyWithoutATracer(t *testing.T) {
	server := pgtest.Start(t)
	store, _ := openStore(t, server)
	mustCreate(t, store, tenantA)
	apply(t, store, tenantA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 480}})
	mediaStore, _ := openMedia(t, server, nil)
	run(t, mediaStore, mediaA, media.Reserve{DeclaredBytes: 10})
}
