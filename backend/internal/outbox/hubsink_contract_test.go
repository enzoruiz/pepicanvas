package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

// TestHubSinkKeysOperationsByOutboxRecord covers a canvas that was deleted and
// recreated, or restored to an earlier revision: its revisions start again,
// and the hub must still apply each new record once.
func TestHubSinkKeysOperationsByOutboxRecord(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	actor := &answers{hub: hub}
	sink := outbox.NewHubSink(actor)
	ctx := context.Background()
	before := outbox.Record{ID: 7, Event: resync.Event{Key: canvasA, Mutation: canvas.SetContent{ObjectID: objectA, Content: "before"}, Result: canvas.Result{Revision: 1, ObjectID: objectA}}}
	after := outbox.Record{ID: 42, Event: resync.Event{Key: canvasA, Mutation: canvas.SetContent{ObjectID: objectA, Content: "after"}, Result: canvas.Result{Revision: 1, ObjectID: objectA}}}
	for _, r := range []outbox.Record{before, after, after} {
		if err := sink.Deliver(ctx, r); err != nil {
			t.Fatalf("Deliver(record %d): %v", r.ID, err)
		}
	}
	events := hubEvents(t, hub, canvasA)
	if len(events) != 2 || events[0].OperationID != "outbox-7" || events[1].OperationID != "outbox-42" {
		t.Fatalf("hub events = %+v, want outbox-7 then outbox-42", events)
	}
	if results := actor.list(); len(results) != 3 || results[1] != results[2] || results[1].Sequence != 2 {
		t.Fatalf("actor answers = %+v, want the redelivery answered like the first delivery", results)
	}
}

// TestHubSinkAcceptsEveryValidMutationAtTheCanvasLimits pins the agreement
// between canvas validation and the actor's command limits: a committed
// event is never invalid for the actor.
func TestHubSinkAcceptsEveryValidMutationAtTheCanvasLimits(t *testing.T) {
	longID := strings.Repeat("a", canvas.MaxIdentifierBytes)
	key := canvas.Key{TenantID: strings.Repeat("t", canvas.MaxIdentifierBytes), CanvasID: strings.Repeat("c", canvas.MaxIdentifierBytes)}
	operation := canvas.Operation{ID: longID, CorrelationID: strings.Repeat("~", canvas.MaxIdentifierBytes)}
	edge := canvas.Transform{X: -canvas.MaxCoordinate, Y: canvas.MaxCoordinate, Width: canvas.MaxObjectSize, Height: canvas.MaxObjectSize, Rotation: math.Nextafter(360, 0)}
	contents := map[string]string{
		"control":   strings.Repeat("\x01", canvas.MaxContentBytes),
		"quote":     strings.Repeat(`"`, canvas.MaxContentBytes),
		"html":      strings.Repeat("<", canvas.MaxContentBytes),
		"separator": strings.Repeat("\u2028", canvas.MaxContentBytes/3),
	}
	mutations := map[string]canvas.Mutation{
		"set_dimensions max": canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: canvas.MaxWidth, Height: canvas.MaxHeight}},
		"set_dimensions min": canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: canvas.MinWidth, Height: canvas.MinHeight}},
		"set_transform":      canvas.SetTransform{ObjectID: longID, Transform: edge},
		"delete_object":      canvas.DeleteObject{ObjectID: longID},
	}
	for name, content := range contents {
		mutations["create_object "+name] = canvas.CreateObject{Kind: canvas.KindText, Content: content, Transform: edge}
		mutations["set_content "+name] = canvas.SetContent{ObjectID: longID, Content: content}
	}
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	sink := outbox.NewHubSink(hub)
	id := int64(math.MaxInt64 - int64(len(mutations)))
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := canvas.Validate(key, operation, mutation); err != nil {
				t.Fatalf("canvas.Validate: %v", err)
			}
			id++
			r := outbox.Record{ID: id, Event: resync.Event{Key: key, Mutation: mutation, CorrelationID: operation.CorrelationID,
				Result: canvas.Result{Revision: math.MaxInt64, ObjectID: longID, Z: math.MaxInt64}}}
			if err := sink.Deliver(context.Background(), r); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
		})
	}
}

func TestHubSinkMarksActorInvalidCommandsAsInvalidEvents(t *testing.T) {
	content := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "x"}, canvas.Result{Revision: 1, ObjectID: objectA}, "")
	tests := []struct {
		name      string
		record    outbox.Record
		authorize error
		invalid   bool
	}{
		{name: "invalid command", record: record(canvasA, content.Event.Mutation, content.Event.Result, "bad correlation"), invalid: true},
		{name: "access denied", record: content, authorize: fmt.Errorf("guard: %w", access.ErrAccessDenied)},
		{name: "transient failure", record: content, authorize: errors.New("busy")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newHub(t, realtime.Config{Authorize: func(context.Context, realtime.Command) error { return tt.authorize }})
			err := outbox.NewHubSink(hub).Deliver(context.Background(), tt.record)
			if got := errors.Is(err, outbox.ErrInvalidEvent); got != tt.invalid {
				t.Fatalf("Deliver error %v invalid event = %v, want %v", err, got, tt.invalid)
			}
		})
	}
	// A conflict is undeliverable but not invalid.
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	sink := outbox.NewHubSink(hub)
	if err := sink.Deliver(context.Background(), content); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	conflicting := record(canvasA, canvas.SetContent{ObjectID: objectA, Content: "y"}, content.Event.Result, "")
	if err := sink.Deliver(context.Background(), conflicting); !errors.Is(err, outbox.ErrUndeliverable) || errors.Is(err, outbox.ErrInvalidEvent) {
		t.Fatalf("Deliver conflicting record = %v, want undeliverable and not invalid", err)
	}
}
