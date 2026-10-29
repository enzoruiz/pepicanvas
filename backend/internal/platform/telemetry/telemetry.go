// Package telemetry records spans that share the correlation ID as their
// W3C trace ID and exports them through a bounded, non-blocking queue
// (CTRL-TRACE-01). Span names and attribute keys are closed vocabularies and
// attribute values are redacted and bounded, so exported traces carry neither
// secrets nor unbounded cardinality.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// SpanID is a canonical span identifier: 16 lowercase hex characters.
type SpanID string

// SpanContext identifies a span within its trace.
type SpanContext struct {
	TraceID correlation.ID
	SpanID  SpanID
}

// ErrInvalidTraceparent reports a traceparent value that is not in the
// canonical W3C form this package emits.
var ErrInvalidTraceparent = errors.New("telemetry: invalid traceparent")

// Traceparent renders the W3C traceparent header value, always sampled.
func (sc SpanContext) Traceparent() string {
	return "00-" + string(sc.TraceID) + "-" + string(sc.SpanID) + "-01"
}

// ParseTraceparent accepts only version 00 with lowercase hex, non-zero
// identifiers, and exact field lengths.
func ParseTraceparent(s string) (SpanContext, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 4 || parts[0] != "00" || !lowerHex(parts[3], 2) ||
		!lowerHex(parts[1], 32) || !lowerHex(parts[2], 16) || allZero(parts[1]) || allZero(parts[2]) {
		return SpanContext{}, ErrInvalidTraceparent
	}
	return SpanContext{TraceID: correlation.ID(parts[1]), SpanID: SpanID(parts[2])}, nil
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func allZero(s string) bool {
	return strings.Trim(s, "0") == ""
}

// SpanName names a span. Only the declared constants are exported; any other
// name is exported as "unknown".
type SpanName string

const (
	SpanHTTPRequest    SpanName = "http.request"
	SpanPostgres       SpanName = "postgres"
	SpanOutboxDeliver  SpanName = "outbox.deliver"
	SpanRealtimeSubmit SpanName = "realtime.submit"
	SpanS3             SpanName = "s3"
	SpanSubprocess     SpanName = "subprocess"
	SpanMediaInspect   SpanName = "media.inspect"
)

func (n SpanName) exported() string {
	switch n {
	case SpanHTTPRequest, SpanPostgres, SpanOutboxDeliver, SpanRealtimeSubmit, SpanS3, SpanSubprocess, SpanMediaInspect:
		return string(n)
	default:
		return "unknown"
	}
}

const (
	// MaxAttributeBytes bounds every exported attribute value.
	MaxAttributeBytes = 64
	// maxAttributes bounds the attributes of one span.
	maxAttributes = 8
)

// Attribute is one key/value pair recorded on a span.
type Attribute struct {
	Key   string
	Value string
}

// Attr returns an attribute for Start.
func Attr(key, value string) Attribute {
	return Attribute{Key: key, Value: value}
}

func allowedKey(key string) bool {
	switch key {
	case "route", "method", "status", "outcome", "operation", "kind":
		return true
	default:
		return false
	}
}

// boundValue redacts v and cuts it to MaxAttributeBytes on a rune boundary.
func boundValue(v string) string {
	v = secret.Scrub(v)
	if len(v) <= MaxAttributeBytes {
		return v
	}
	cut := MaxAttributeBytes
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut]
}

// SpanData is the exported form of an ended span.
type SpanData struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Name         string            `json:"name"`
	Start        time.Time         `json:"start"`
	End          time.Time         `json:"end"`
	Status       string            `json:"status"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// Span is an operation in progress. Its methods are safe for concurrent use.
type Span struct {
	tracer *Tracer

	mu    sync.Mutex
	ended bool
	data  SpanData
}

// SetAttr records an attribute from the closed key set; other keys, keys
// beyond the per-span limit, and calls after End are ignored.
func (s *Span) SetAttr(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.setAttr(key, value)
}

func (s *Span) setAttr(key, value string) {
	if !allowedKey(key) {
		return
	}
	if _, ok := s.data.Attributes[key]; !ok && len(s.data.Attributes) >= maxAttributes {
		return
	}
	if s.data.Attributes == nil {
		s.data.Attributes = make(map[string]string)
	}
	s.data.Attributes[key] = boundValue(value)
}

// End finishes the span with status "ok", or "error" when err is non-nil; the
// error text is never exported. A second End is ignored.
func (s *Span) End(err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.data.End = time.Now()
	s.data.Status = "ok"
	if err != nil {
		s.data.Status = "error"
	}
	data := s.data
	s.mu.Unlock()
	if s.tracer != nil {
		s.tracer.enqueue(data)
	}
}

type tracerKey struct{}

type spanKey struct{}

// WithTracer returns a context whose spans are exported by tracer. A nil
// tracer disables export.
func WithTracer(ctx context.Context, tracer *Tracer) context.Context {
	return context.WithValue(ctx, tracerKey{}, tracer)
}

// SpanContextFrom returns the context's current span, if any.
func SpanContextFrom(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(spanKey{}).(SpanContext)
	return sc, ok
}

// Resume returns a context continuing the stored correlation, such as the one
// kept by an outbox record. A malformed value is ignored, so the next Start
// begins a fresh trace.
func Resume(ctx context.Context, stored string) context.Context {
	id, err := correlation.Parse(stored)
	if err != nil {
		return ctx
	}
	return correlation.WithID(ctx, id)
}

// Start begins a span named name in the context's trace. The trace ID is the
// context's correlation ID; without one a new ID is created and stored in the
// returned context so logs stay correlated even when export is disabled. The
// span's parent is the context's current span within the same trace.
func Start(ctx context.Context, name SpanName, attrs ...Attribute) (context.Context, *Span) {
	tracer, _ := ctx.Value(tracerKey{}).(*Tracer)
	span := &Span{tracer: tracer}
	trace, ok := correlation.FromContext(ctx)
	if !ok {
		id, err := correlation.New()
		if err != nil {
			span.ended = true // without randomness nothing can be traced
			return ctx, span
		}
		trace = id
		ctx = correlation.WithID(ctx, trace)
	}
	id, err := newSpanID()
	if err != nil {
		span.ended = true
		return ctx, span
	}
	span.data = SpanData{TraceID: trace.String(), SpanID: string(id), Name: name.exported(), Start: time.Now()}
	if parent, ok := SpanContextFrom(ctx); ok && parent.TraceID == trace {
		span.data.ParentSpanID = string(parent.SpanID)
	}
	for _, a := range attrs {
		span.setAttr(a.Key, a.Value)
	}
	return context.WithValue(ctx, spanKey{}, SpanContext{TraceID: trace, SpanID: id}), span
}

func newSpanID() (SpanID, error) {
	buf := make([]byte, 8)
	for {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if id := hex.EncodeToString(buf); !allZero(id) {
			return SpanID(id), nil
		}
	}
}
