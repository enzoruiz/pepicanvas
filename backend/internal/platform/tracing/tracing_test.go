package tracing_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing/tracingtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

const stored = "0123456789abcdef0123456789abcdef"

// seen is what a fake observed in the context of one call.
type seen struct {
	correlation correlation.ID
	span        telemetry.SpanContext
	hasSpan     bool
}

func observe(ctx context.Context) seen {
	id, _ := correlation.FromContext(ctx)
	sc, ok := telemetry.SpanContextFrom(ctx)
	return seen{correlation: id, span: sc, hasSpan: ok}
}

// fakeSink records the context of each delivery and answers err.
type fakeSink struct {
	mu   sync.Mutex
	err  error
	seen []seen
}

func (s *fakeSink) Deliver(ctx context.Context, _ outbox.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, observe(ctx))
	return s.err
}

// resyncingSink also counts RequireResync calls.
type resyncingSink struct {
	fakeSink
	resyncs int
}

func (s *resyncingSink) RequireResync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncs++
}

// fakeActor records the context of each submission and answers err.
type fakeActor struct {
	mu   sync.Mutex
	err  error
	seen []seen
}

func (a *fakeActor) Submit(ctx context.Context, command realtime.Command) (realtime.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, observe(ctx))
	if a.err != nil {
		return realtime.Result{}, a.err
	}
	return realtime.Result{OperationID: command.OperationID, Outcome: realtime.OutcomeAccepted, Sequence: 1, CorrelationID: command.CorrelationID}, nil
}

type resyncingActor struct {
	fakeActor
	resyncs int
}

func (a *resyncingActor) RequireResync() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resyncs++
}

func mutationRecord(id int64, correlationID string) outbox.Record {
	return outbox.Record{
		ID: id,
		Event: resync.Event{
			Key:           canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"},
			Mutation:      canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}},
			Result:        canvas.Result{Revision: id},
			CorrelationID: correlationID,
		},
	}
}

func usageRecord(id int64, correlationID string) outbox.Record {
	return outbox.Record{
		ID: id,
		Event: resync.Event{
			Key:           canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"},
			CorrelationID: correlationID,
		},
		Usage: &outbox.MediaUsage{Version: 1, QuotaBytes: 100, UsedBytes: 10},
	}
}

func only(t *testing.T, spans []telemetry.SpanData, name telemetry.SpanName) telemetry.SpanData {
	t.Helper()
	named := tracingtest.Named(spans, name)
	if len(named) != 1 {
		t.Fatalf("%s spans = %+v, want exactly one among %+v", name, named, spans)
	}
	return named[0]
}

func wantAttributes(t *testing.T, span telemetry.SpanData, want map[string]string) {
	t.Helper()
	if !maps.Equal(span.Attributes, want) {
		t.Fatalf("%s attributes = %v, want %v", span.Name, span.Attributes, want)
	}
}

// S1, L9: the sink resumes the record's stored correlation, so the delivery
// joins the trace of the request that committed it, and exports one
// outbox.deliver span whose kind and outcome are closed values.
func TestSinkResumesTheStoredCorrelationInADeliverSpan(t *testing.T) {
	cases := []struct {
		name    string
		record  outbox.Record
		err     error
		kind    string
		outcome string
		status  string
	}{
		{name: "delivered mutation", record: mutationRecord(1, stored), kind: "mutation", outcome: "delivered", status: "ok"},
		{name: "delivered usage", record: usageRecord(2, stored), kind: "media_usage", outcome: "delivered", status: "ok"},
		{name: "undeliverable", record: mutationRecord(3, stored), err: fmt.Errorf("%w: secret-decoy", outbox.ErrUndeliverable), kind: "mutation", outcome: "undeliverable", status: "error"},
		{name: "retried", record: usageRecord(4, stored), err: errors.New("hub busy secret-decoy"), kind: "media_usage", outcome: "retry", status: "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := tracingtest.Start(t)
			next := &fakeSink{err: c.err}
			err := tracing.Sink(next, h.Tracer).Deliver(context.Background(), c.record)
			if !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
				t.Fatalf("Deliver = %v, want %v", err, c.err)
			}
			span := only(t, h.Spans(t), telemetry.SpanOutboxDeliver)
			if span.TraceID != stored || span.Status != c.status || span.ParentSpanID != "" {
				t.Fatalf("span = %+v, want trace %s, status %s, no parent", span, stored, c.status)
			}
			wantAttributes(t, span, map[string]string{"kind": c.kind, "outcome": c.outcome})
			if len(next.seen) != 1 {
				t.Fatalf("deliveries = %d, want 1", len(next.seen))
			}
			got := next.seen[0]
			if got.correlation != stored || !got.hasSpan || got.span.SpanID != telemetry.SpanID(span.SpanID) {
				t.Fatalf("next saw %+v, want correlation %s inside span %s", got, stored, span.SpanID)
			}
		})
	}
}

// A record whose stored correlation is missing or malformed is still
// delivered, in a fresh trace that never adopts the stored value.
func TestSinkStartsAFreshTraceForAMalformedStoredCorrelation(t *testing.T) {
	for _, value := range []string{"", "not-a-correlation", strings.ToUpper(stored)} {
		h := tracingtest.Start(t)
		next := &fakeSink{}
		if err := tracing.Sink(next, h.Tracer).Deliver(context.Background(), mutationRecord(1, value)); err != nil {
			t.Fatalf("Deliver(%q) = %v", value, err)
		}
		span := only(t, h.Spans(t), telemetry.SpanOutboxDeliver)
		if _, err := correlation.Parse(span.TraceID); err != nil || span.TraceID == value {
			t.Fatalf("stored %q: trace %q, want a fresh canonical ID", value, span.TraceID)
		}
		if next.seen[0].correlation.String() != span.TraceID {
			t.Fatalf("stored %q: next saw correlation %q, want %q", value, next.seen[0].correlation, span.TraceID)
		}
	}
}

// S9, S10: without a tracer (export disabled) deliveries still run and keep
// the stored correlation, so local logs stay correlated.
func TestSinkAndActorWithoutTracerKeepDeliveringCorrelated(t *testing.T) {
	actor := &fakeActor{}
	sink := tracing.Sink(outbox.NewHubSink(tracing.Actor(actor, nil)), nil)
	if err := sink.Deliver(context.Background(), mutationRecord(1, stored)); err != nil {
		t.Fatalf("Deliver = %v", err)
	}
	if len(actor.seen) != 1 || actor.seen[0].correlation != stored || !actor.seen[0].hasSpan {
		t.Fatalf("actor saw %+v, want correlation %s inside a span", actor.seen, stored)
	}

	failing := &fakeSink{err: errors.New("down")}
	if err := tracing.Sink(failing, nil).Deliver(context.Background(), usageRecord(2, stored)); err == nil || err.Error() != "down" {
		t.Fatalf("Deliver = %v, want the sink's error unchanged", err)
	}
}

// L9: the actor decorator wraps each submission in a realtime.submit span,
// a child of the delivery, with the command's kind.
func TestActorWrapsSubmitInAChildSpan(t *testing.T) {
	h := tracingtest.Start(t)
	actor := &fakeActor{}
	sink := tracing.Sink(outbox.NewHubSink(tracing.Actor(actor, h.Tracer)), h.Tracer)
	if err := sink.Deliver(context.Background(), mutationRecord(1, stored)); err != nil {
		t.Fatalf("Deliver = %v", err)
	}
	actor.err = errors.New("full secret-decoy")
	if err := sink.Deliver(context.Background(), usageRecord(2, stored)); err == nil {
		t.Fatal("Deliver succeeded with a failing actor")
	}
	spans := h.Spans(t)
	delivers := tracingtest.Named(spans, telemetry.SpanOutboxDeliver)
	submits := tracingtest.Named(spans, telemetry.SpanRealtimeSubmit)
	if len(delivers) != 2 || len(submits) != 2 {
		t.Fatalf("spans = %+v, want two deliveries and two submissions", spans)
	}
	for i, want := range []struct{ kind, status string }{{"set", "ok"}, {"usage", "error"}} {
		submit := submits[i]
		if submit.TraceID != stored || submit.ParentSpanID != delivers[i].SpanID || submit.Status != want.status {
			t.Fatalf("submit %d = %+v, want trace %s, parent %s, status %s", i, submit, stored, delivers[i].SpanID, want.status)
		}
		wantAttributes(t, submit, map[string]string{"kind": want.kind})
		if got := actor.seen[i]; got.span.SpanID != telemetry.SpanID(submit.SpanID) {
			t.Fatalf("actor saw span %s, want %s", got.span.SpanID, submit.SpanID)
		}
	}
	for _, span := range spans {
		for _, v := range span.Attributes {
			if strings.Contains(v, "secret-decoy") {
				t.Fatalf("span %s exports error text: %v", span.Name, span.Attributes)
			}
		}
	}
}

// Called directly, the actor decorator resumes the command's correlation.
func TestActorResumesTheCommandCorrelationWithoutOne(t *testing.T) {
	h := tracingtest.Start(t)
	actor := &fakeActor{}
	command := realtime.Command{
		Key: realtime.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}, OperationID: "outbox-1",
		ObjectID: "canvas", Kind: realtime.KindDelete, CorrelationID: stored,
	}
	result, err := tracing.Actor(actor, h.Tracer).Submit(context.Background(), command)
	if err != nil || result.OperationID != "outbox-1" || result.CorrelationID != stored {
		t.Fatalf("Submit = %+v, %v", result, err)
	}
	span := only(t, h.Spans(t), telemetry.SpanRealtimeSubmit)
	if span.TraceID != stored || actor.seen[0].correlation != stored {
		t.Fatalf("span %+v, actor saw %+v; want trace %s", span, actor.seen[0], stored)
	}
	wantAttributes(t, span, map[string]string{"kind": "delete"})
}

// L9: the decorators forward RequireResync exactly when what they wrap
// supports it, so the publisher's Close still reaches the hub.
func TestDecoratorsForwardRequireResync(t *testing.T) {
	sink := &resyncingSink{}
	wrapped := tracing.Sink(sink, nil)
	resyncer, ok := wrapped.(outbox.Resyncer)
	if !ok {
		t.Fatal("a sink over a Resyncer is not a Resyncer")
	}
	resyncer.RequireResync()
	if sink.resyncs != 1 {
		t.Fatalf("sink resyncs = %d, want 1", sink.resyncs)
	}
	if _, ok := tracing.Sink(&fakeSink{}, nil).(outbox.Resyncer); ok {
		t.Fatal("a sink over a plain sink claims to be a Resyncer")
	}

	actor := &resyncingActor{}
	wrappedActor := tracing.Actor(actor, nil)
	actorResyncer, ok := wrappedActor.(outbox.Resyncer)
	if !ok {
		t.Fatal("an actor over a Resyncer is not a Resyncer")
	}
	actorResyncer.RequireResync()
	if actor.resyncs != 1 {
		t.Fatalf("actor resyncs = %d, want 1", actor.resyncs)
	}
	if _, ok := tracing.Actor(&fakeActor{}, nil).(outbox.Resyncer); ok {
		t.Fatal("an actor over a plain actor claims to be a Resyncer")
	}
	// HubSink forwards to its actor only when the actor is a Resyncer.
	outbox.NewHubSink(wrappedActor).RequireResync()
	if actor.resyncs != 2 {
		t.Fatalf("actor resyncs through HubSink = %d, want 2", actor.resyncs)
	}
}

// memoryStore is an outbox store over records kept in memory.
type memoryStore struct {
	mu       sync.Mutex
	records  []outbox.Record
	settled  map[int64]outbox.Outcome
	notified chan struct{}
}

func (s *memoryStore) pending() []outbox.Record {
	var pending []outbox.Record
	for _, r := range s.records {
		if _, ok := s.settled[r.ID]; !ok {
			pending = append(pending, r)
		}
	}
	return pending
}

func (s *memoryStore) Heads(_ context.Context, limit int) ([]outbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	heads := map[canvas.Key]bool{}
	var out []outbox.Record
	for _, r := range s.pending() {
		if !heads[r.Event.Key] && len(out) < limit {
			heads[r.Event.Key] = true
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memoryStore) Pending(_ context.Context, key canvas.Key, limit int) ([]outbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []outbox.Record
	for _, r := range s.pending() {
		if r.Event.Key == key && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memoryStore) Settle(_ context.Context, id int64, outcome outbox.Outcome, _ time.Time, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settled[id]; !ok {
		s.settled[id] = outcome
		if len(s.settled) == len(s.records) {
			close(s.notified)
		}
	}
	return nil
}

func (s *memoryStore) Retry(context.Context, int64, int, time.Time, string) error { return nil }

func (s *memoryStore) Purge(context.Context, time.Time, int) (int, error) { return 0, nil }

// S1, S5: through the publisher and a real hub, the committed record's
// correlation reaches the actor's event, the delivery and submission spans
// share its trace, and the publisher's Close still requires resync at the
// hub through both decorators.
func TestPublisherDeliversThroughTheDecoratorsToTheHub(t *testing.T) {
	h := tracingtest.Start(t)
	hub, err := realtime.NewHub(realtime.Config{Authorize: func(context.Context, realtime.Command) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{records: []outbox.Record{mutationRecord(1, stored)}, settled: map[int64]outbox.Outcome{}, notified: make(chan struct{})}
	publisher, err := outbox.NewPublisher(outbox.Config{
		Store: store,
		Sink:  tracing.Sink(outbox.NewHubSink(tracing.Actor(hub, h.Tracer)), h.Tracer),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.notified:
	case <-time.After(5 * time.Second):
		t.Fatal("record not settled")
	}
	if outcome := store.settled[1]; outcome != outbox.OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", outcome)
	}
	key := realtime.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	events, err := hub.Events(context.Background(), key, 0)
	if err != nil || len(events) != 1 || events[0].CorrelationID != stored {
		t.Fatalf("Events = %+v, %v; want one event correlated %s", events, err, stored)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Events(ctx, key, 0); !errors.Is(err, realtime.ErrResyncRequired) {
		t.Fatalf("Events after publisher Close = %v, want ErrResyncRequired", err)
	}
	if err := hub.Close(ctx); err != nil {
		t.Fatal(err)
	}

	spans := h.Spans(t)
	deliver := only(t, spans, telemetry.SpanOutboxDeliver)
	submit := only(t, spans, telemetry.SpanRealtimeSubmit)
	if deliver.TraceID != stored || submit.TraceID != stored || submit.ParentSpanID != deliver.SpanID {
		t.Fatalf("deliver %+v, submit %+v; want one trace %s, submit inside deliver", deliver, submit, stored)
	}
}
