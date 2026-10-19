package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

const (
	objectA = "0123456789abcdef0123456789abcdef"
	objectB = "fedcba9876543210fedcba9876543210"
)

// seen records every command an Authorizer is asked about.
type seen struct {
	mu       sync.Mutex
	commands []realtime.Command
}

func (s *seen) authorize(_ context.Context, command realtime.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
	return nil
}

func (s *seen) list() []realtime.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]realtime.Command(nil), s.commands...)
}

func newHub(t *testing.T, config realtime.Config) *realtime.Hub {
	t.Helper()
	hub, err := realtime.NewHub(config)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	return hub
}

func hubEvents(t *testing.T, hub *realtime.Hub, key canvas.Key) []realtime.Event {
	t.Helper()
	events, err := hub.Events(context.Background(), realtime.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}, 0)
	if err != nil {
		t.Fatalf("Events(%v): %v", key, err)
	}
	return events
}

func record(key canvas.Key, mutation canvas.Mutation, result canvas.Result, correlationID string) outbox.Record {
	return outbox.Record{ID: result.Revision, Event: resync.Event{Key: key, Mutation: mutation, Result: result, CorrelationID: correlationID}}
}

func TestHubSinkSubmitsTheCommittedEventUnderItsOwnKey(t *testing.T) {
	transform := canvas.Transform{X: -9, Y: 8, Width: 7, Height: 6, Rotation: 12.5}
	tests := []struct {
		name   string
		record outbox.Record
		want   realtime.Command
	}{
		{
			name:   "create_object",
			record: record(canvasA, canvas.CreateObject{Kind: canvas.KindText, Content: "hi", Transform: transform}, canvas.Result{Revision: 1, ObjectID: objectA, Z: 1}, "corr-1"),
			want: realtime.Command{
				OperationID: "canvas-revision-1", ObjectID: objectA, Kind: realtime.KindSet, CorrelationID: "corr-1",
				Payload: []byte(`{"kind":"create_object","mutation":{"kind":"text","content":"hi","transform":{"x":-9,"y":8,"w":7,"h":6,"rotation":12.5}},"revision":1,"object_id":"` + objectA + `","z":1}`),
			},
		},
		{
			name:   "set_dimensions",
			record: record(canvasA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}}, canvas.Result{Revision: 2}, "corr-2"),
			want: realtime.Command{
				OperationID: "canvas-revision-2", ObjectID: "canvas", Kind: realtime.KindSet, CorrelationID: "corr-2",
				Payload: []byte(`{"kind":"set_dimensions","mutation":{"width":1280,"height":720},"revision":2,"object_id":"","z":0}`),
			},
		},
		{
			name:   "set_content",
			record: record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "twice"}, canvas.Result{Revision: 3, ObjectID: objectA}, ""),
			want: realtime.Command{
				OperationID: "canvas-revision-3", ObjectID: objectA, Kind: realtime.KindSet,
				Payload: []byte(`{"kind":"set_content","mutation":{"object_id":"` + objectA + `","content":"twice"},"revision":3,"object_id":"` + objectA + `","z":0}`),
			},
		},
		{
			name:   "set_transform",
			record: record(canvasA, canvas.SetTransform{ObjectID: objectA, Transform: transform}, canvas.Result{Revision: 4, ObjectID: objectA, Z: 2}, "corr-4"),
			want: realtime.Command{
				OperationID: "canvas-revision-4", ObjectID: objectA, Kind: realtime.KindSet, CorrelationID: "corr-4",
				Payload: []byte(`{"kind":"set_transform","mutation":{"object_id":"` + objectA + `","transform":{"x":-9,"y":8,"w":7,"h":6,"rotation":12.5}},"revision":4,"object_id":"` + objectA + `","z":2}`),
			},
		},
		{
			name:   "delete_object",
			record: record(canvasA, canvas.DeleteObject{ObjectID: objectA}, canvas.Result{Revision: 5, ObjectID: objectA}, "corr-5"),
			want:   realtime.Command{OperationID: "canvas-revision-5", ObjectID: objectA, Kind: realtime.KindDelete, CorrelationID: "corr-5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands := &seen{}
			sink := outbox.NewHubSink(newHub(t, realtime.Config{Authorize: commands.authorize}))
			if err := sink.Deliver(context.Background(), tt.record); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			want := tt.want
			want.Key = realtime.Key{TenantID: canvasA.TenantID, CanvasID: canvasA.CanvasID}
			got := commands.list()
			if len(got) != 1 {
				t.Fatalf("authorizer saw %d commands, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0], want) {
				t.Fatalf("command =\n%+v\nwant\n%+v\npayload %s", got[0], want, got[0].Payload)
			}
		})
	}
}

func TestHubSinkClassifiesActorErrors(t *testing.T) {
	content := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "")
	transient := errors.New("membership store unavailable")
	tests := []struct {
		name        string
		record      outbox.Record
		authorize   error
		closeHub    bool
		cancel      bool
		undelivered bool
		cause       error
	}{
		{name: "invalid command", record: record(canvasA, content.Event.Mutation, content.Event.Result, "bad correlation"), undelivered: true, cause: realtime.ErrInvalidCommand},
		{name: "access denied", record: content, authorize: fmt.Errorf("guard: %w", access.ErrAccessDenied), undelivered: true, cause: access.ErrAccessDenied},
		{name: "transient authorizer failure", record: content, authorize: transient, cause: transient},
		{name: "closed hub", record: content, closeHub: true, cause: realtime.ErrClosed},
		{name: "cancelled attempt", record: content, cancel: true, cause: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newHub(t, realtime.Config{Authorize: func(context.Context, realtime.Command) error { return tt.authorize }})
			if tt.closeHub {
				if err := hub.Close(context.Background()); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			if tt.cancel {
				cancel()
			}
			defer cancel()
			err := outbox.NewHubSink(hub).Deliver(ctx, tt.record)
			if !errors.Is(err, tt.cause) {
				t.Fatalf("Deliver error = %v, want %v", err, tt.cause)
			}
			if got := errors.Is(err, outbox.ErrUndeliverable); got != tt.undelivered {
				t.Fatalf("Deliver error %v undeliverable = %v, want %v", err, got, tt.undelivered)
			}
		})
	}
}

func TestHubSinkTreatsAnActorRejectionAsAnAnswer(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: (&seen{}).authorize})
	sink := outbox.NewHubSink(hub)
	ctx := context.Background()
	if err := sink.Deliver(ctx, record(canvasA, canvas.DeleteObject{ObjectID: objectA}, canvas.Result{Revision: 1, ObjectID: objectA}, "")); err != nil {
		t.Fatalf("Deliver delete: %v", err)
	}
	// The actor rejects a set on a deleted object; the record is answered.
	if err := sink.Deliver(ctx, record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "late"}, canvas.Result{Revision: 2, ObjectID: objectA}, "")); err != nil {
		t.Fatalf("Deliver rejected set = %v, want nil", err)
	}
	if events := hubEvents(t, hub, canvasA); len(events) != 1 {
		t.Fatalf("hub events = %+v, want only the delete", events)
	}
}

// answers records the results the actor returns to the sink.
type answers struct {
	hub     *realtime.Hub
	mu      sync.Mutex
	results []realtime.Result
}

func (a *answers) Submit(ctx context.Context, command realtime.Command) (realtime.Result, error) {
	result, err := a.hub.Submit(ctx, command)
	if err == nil {
		a.mu.Lock()
		a.results = append(a.results, result)
		a.mu.Unlock()
	}
	return result, err
}

func (a *answers) list() []realtime.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]realtime.Result(nil), a.results...)
}

func TestHubSinkRedeliveryProducesOneEffectAndTheSameAnswer(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: (&seen{}).authorize})
	actor := &answers{hub: hub}
	sink := outbox.NewHubSink(actor)
	ctx := context.Background()
	content := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "corr-1")
	for range 2 {
		if err := sink.Deliver(ctx, content); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	// A retry over a new connection carries another correlation ID.
	content.Event.CorrelationID = "corr-retry"
	if err := sink.Deliver(ctx, content); err != nil {
		t.Fatalf("Deliver with another correlation: %v", err)
	}
	results := actor.list()
	if len(results) != 3 || results[0] != results[1] || results[1] != results[2] || results[0].Sequence != 1 || results[0].Outcome != realtime.OutcomeAccepted {
		t.Fatalf("actor answers = %+v, want three equal accepted answers with sequence 1", results)
	}
	events := hubEvents(t, hub, canvasA)
	if len(events) != 1 || events[0].Sequence != 1 || events[0].OperationID != "canvas-revision-1" {
		t.Fatalf("hub events = %+v, want one event with sequence 1", events)
	}
}

func TestHubSinkRejectsAnotherEventUnderTheSameRevision(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: (&seen{}).authorize})
	sink := outbox.NewHubSink(hub)
	ctx := context.Background()
	if err := sink.Deliver(ctx, record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	other := record(canvasA, canvas.SetContent{ObjectID: objectB, Content: "y"}, canvas.Result{Revision: 1, ObjectID: objectB}, "")
	err := sink.Deliver(ctx, other)
	if !errors.Is(err, outbox.ErrUndeliverable) || !errors.Is(err, realtime.ErrOperationConflict) {
		t.Fatalf("Deliver of another revision 1 = %v, want undeliverable operation conflict", err)
	}
	// The same revision of another tenant's canvas is a different operation.
	if err := sink.Deliver(ctx, record(canvasB, other.Event.Mutation, other.Event.Result, "")); err != nil {
		t.Fatalf("Deliver to another tenant: %v", err)
	}
	if events := hubEvents(t, hub, canvasA); len(events) != 1 || events[0].ObjectID != objectA {
		t.Fatalf("hub events = %+v, want only the first record", events)
	}
}

func TestHubSinkRedeliveryAfterRetentionIsANewOperation(t *testing.T) {
	now := &clock{now: t0}
	hub := newHub(t, realtime.Config{Authorize: (&seen{}).authorize, Now: now.Now, Retention: realtime.Retention{TTL: time.Minute}})
	actor := &answers{hub: hub}
	sink := outbox.NewHubSink(actor)
	ctx := context.Background()
	content := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "")
	if err := sink.Deliver(ctx, content); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	now.Advance(time.Minute - time.Nanosecond)
	if err := sink.Deliver(ctx, content); err != nil {
		t.Fatalf("Deliver before expiry: %v", err)
	}
	if events := hubEvents(t, hub, canvasA); len(events) != 1 {
		t.Fatalf("hub events before expiry = %+v, want 1", events)
	}
	now.Advance(time.Nanosecond)
	if err := sink.Deliver(ctx, content); err != nil {
		t.Fatalf("Deliver at expiry: %v", err)
	}
	events := hubEvents(t, hub, canvasA)
	if len(events) != 2 || events[1].Sequence != 2 || events[1].OperationID != "canvas-revision-1" {
		t.Fatalf("hub events at expiry = %+v, want a second operation with sequence 2", events)
	}
	if results := actor.list(); len(results) != 3 || results[1].Sequence != 1 || results[2].Sequence != 2 {
		t.Fatalf("actor answers = %+v, want sequences 1, 1, 2", results)
	}
}

func TestHubSinkRetryAfterATimedOutAttemptHasOneEffect(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	// The first authorization outlives the attempt and ignores its context,
	// so the command is applied after the publisher gave up on it.
	hub := newHub(t, realtime.Config{Authorize: func(context.Context, realtime.Command) error {
		once.Do(func() { <-release })
		return nil
	}})
	actor := &answers{hub: hub}
	sink := outbox.NewHubSink(actor)
	content := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "corr-1")

	attempt, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := sink.Deliver(attempt, content)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, outbox.ErrUndeliverable) {
		t.Fatalf("timed out Deliver = %v, want a retryable deadline error", err)
	}
	close(release)
	eventually(t, "the timed out command to apply", func() bool { return len(hubEvents(t, hub, canvasA)) == 1 })

	if err := sink.Deliver(context.Background(), content); err != nil {
		t.Fatalf("retry Deliver: %v", err)
	}
	events := hubEvents(t, hub, canvasA)
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("hub events = %+v, want one event with sequence 1", events)
	}
	if results := actor.list(); len(results) != 1 || results[0].Sequence != 1 || results[0].Outcome != realtime.OutcomeAccepted {
		t.Fatalf("retry answer = %+v, want the original accepted sequence 1", results)
	}
}
