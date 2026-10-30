// Package tracing traces the asynchronous boundary between the outbox
// publisher and the realtime actor (CTRL-TRACE-01). Its decorators wrap the
// outbox.Sink and outbox.Actor at composition, so the domain packages stay
// free of platform code: a delivery resumes the correlation its record
// stored, and the actor's submission continues that trace.
package tracing

import (
	"context"
	"errors"

	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

// Kinds of a delivered record.
const (
	kindMutation = "mutation"
	kindUsage    = outbox.KindMediaUsage
)

// Outcomes of a delivery.
const (
	outcomeDelivered     = "delivered"
	outcomeUndeliverable = "undeliverable"
	outcomeRetry         = "retry"
)

// Sink returns a sink that delivers each record through next inside an
// outbox.deliver span of the record's stored correlation, exported by tracer.
// A nil tracer only restores the correlation. The returned sink is an
// outbox.Resyncer exactly when next is, so the publisher's Close still
// reaches the hub.
func Sink(next outbox.Sink, tracer *telemetry.Tracer) outbox.Sink {
	s := &sink{next: next, tracer: tracer}
	if resyncer, ok := next.(outbox.Resyncer); ok {
		return &resyncSink{sink: s, resyncer: resyncer}
	}
	return s
}

type sink struct {
	next   outbox.Sink
	tracer *telemetry.Tracer
}

func (s *sink) Deliver(ctx context.Context, record outbox.Record) error {
	kind := kindMutation
	if record.Usage != nil {
		kind = kindUsage
	}
	ctx = telemetry.Resume(telemetry.WithTracer(ctx, s.tracer), record.Event.CorrelationID)
	ctx, span := telemetry.Start(ctx, telemetry.SpanOutboxDeliver, telemetry.Attr("kind", kind))
	err := s.next.Deliver(ctx, record)
	switch {
	case err == nil:
		span.SetAttr("outcome", outcomeDelivered)
	case errors.Is(err, outbox.ErrUndeliverable):
		span.SetAttr("outcome", outcomeUndeliverable)
	default:
		span.SetAttr("outcome", outcomeRetry)
	}
	span.End(err)
	return err
}

type resyncSink struct {
	*sink
	resyncer outbox.Resyncer
}

func (s *resyncSink) RequireResync() { s.resyncer.RequireResync() }

// Actor returns an actor that submits each command to next inside a
// realtime.submit span exported by tracer; a nil tracer exports nothing. A
// context without a correlation resumes the command's. The returned actor is
// an outbox.Resyncer exactly when next is.
func Actor(next outbox.Actor, tracer *telemetry.Tracer) outbox.Actor {
	a := &actor{next: next, tracer: tracer}
	if resyncer, ok := next.(outbox.Resyncer); ok {
		return &resyncActor{actor: a, resyncer: resyncer}
	}
	return a
}

type actor struct {
	next   outbox.Actor
	tracer *telemetry.Tracer
}

func (a *actor) Submit(ctx context.Context, command realtime.Command) (realtime.Result, error) {
	ctx = telemetry.WithTracer(ctx, a.tracer)
	if _, ok := correlation.FromContext(ctx); !ok {
		ctx = telemetry.Resume(ctx, command.CorrelationID)
	}
	ctx, span := telemetry.Start(ctx, telemetry.SpanRealtimeSubmit, telemetry.Attr("kind", string(command.Kind)))
	result, err := a.next.Submit(ctx, command)
	span.End(err)
	return result, err
}

type resyncActor struct {
	*actor
	resyncer outbox.Resyncer
}

func (a *resyncActor) RequireResync() { a.resyncer.RequireResync() }
