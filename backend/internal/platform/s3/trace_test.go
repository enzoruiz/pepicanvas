package s3_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing/tracingtest"
)

// S1, L9: every storage call runs in an s3 span of the request's trace that
// names the operation and its closed outcome, and never the key, bucket, or
// error text.
func TestStorageCallsRecordS3Spans(t *testing.T) {
	server := s3test.Start(t)
	storage := server.Storage(t)
	h := tracingtest.Start(t)
	const trace = correlation.ID("fedcba9876543210fedcba9876543210")
	ctx, _ := telemetry.Start(h.Context(correlation.WithID(context.Background(), trace)), telemetry.SpanHTTPRequest)
	parent, _ := telemetry.SpanContextFrom(ctx)
	const key = "tenant-a/canvas-1/private-key-decoy"

	if err := storage.Put(ctx, key, owner, strings.NewReader("bytes"), 5); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := storage.Put(ctx, key, owner, strings.NewReader("bytes"), 5); !errors.Is(err, media.ErrObjectExists) {
		t.Fatalf("second Put = %v, want ErrObjectExists", err)
	}
	body, err := storage.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = body.Close()
	if _, err := storage.Stat(ctx, key); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if objects, err := storage.List(ctx, "", 10); err != nil || len(objects) != 1 {
		t.Fatalf("List = %v, %v", objects, err)
	}
	if err := storage.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := storage.Stat(ctx, key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}

	want := []struct{ operation, outcome, status string }{
		{"put", "ok", "ok"},
		{"put", "exists", "error"},
		{"open", "ok", "ok"},
		{"stat", "ok", "ok"},
		{"list", "ok", "ok"},
		{"delete", "ok", "ok"},
		{"stat", "not_found", "error"},
	}
	spans := tracingtest.Named(h.Spans(t), telemetry.SpanS3)
	if len(spans) != len(want) {
		t.Fatalf("s3 spans = %+v, want %d", spans, len(want))
	}
	for i, span := range spans {
		if span.TraceID != trace.String() || span.ParentSpanID != string(parent.SpanID) || span.Status != want[i].status {
			t.Fatalf("span %d = %+v, want trace %s, parent %s, status %s", i, span, trace, parent.SpanID, want[i].status)
		}
		if attrs := map[string]string{"operation": want[i].operation, "outcome": want[i].outcome}; !maps.Equal(span.Attributes, attrs) {
			t.Fatalf("span %d attributes = %v, want %v", i, span.Attributes, attrs)
		}
	}
}
