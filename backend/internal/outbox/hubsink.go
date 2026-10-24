package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

// CanvasObjectID is the object ID of commands that change the canvas itself.
// Object IDs are 32 hex characters, so it never names an object.
const CanvasObjectID = "canvas"

// Actor accepts commands for the realtime actor of a canvas. *realtime.Hub
// implements it.
type Actor interface {
	Submit(ctx context.Context, command realtime.Command) (realtime.Result, error)
}

// HubSink delivers outbox records to the realtime actor. A media usage record
// becomes a usage command, which changes no canvas object. Each record is
// submitted under its own tenant and canvas with the operation ID
// outbox-<record ID>. It is stable across redeliveries, so the actor
// recognizes a redelivery within its retention and answers it without a
// second effect, and unique across the outbox, so a canvas whose revisions
// start again (recreated or restored) never collides with an earlier event.
type HubSink struct {
	actor Actor
}

var (
	_ Sink     = (*HubSink)(nil)
	_ Resyncer = (*HubSink)(nil)
)

// NewHubSink returns a sink over actor.
func NewHubSink(actor Actor) *HubSink {
	return &HubSink{actor: actor}
}

// hubPayload is the payload of a set command. Its fields are fixed, so one
// record always encodes to the same bytes.
type hubPayload struct {
	Kind     string          `json:"kind"`
	Mutation json.RawMessage `json:"mutation"`
	Revision int64           `json:"revision"`
	ObjectID string          `json:"object_id"`
	Z        int64           `json:"z"`
}

// Deliver submits the record. A rejection decided by the actor is an answer;
// a command the actor can never accept is ErrUndeliverable; any other error
// is retried.
func (s *HubSink) Deliver(ctx context.Context, record Record) error {
	command, err := hubCommand(record)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUndeliverable, err)
	}
	if _, err := s.actor.Submit(ctx, command); err != nil {
		if errors.Is(err, realtime.ErrInvalidCommand) {
			return fmt.Errorf("%w: %w: %w", ErrUndeliverable, ErrInvalidEvent, err)
		}
		if errors.Is(err, realtime.ErrOperationConflict) || errors.Is(err, access.ErrAccessDenied) {
			return fmt.Errorf("%w: %w", ErrUndeliverable, err)
		}
		return err
	}
	return nil
}

// RequireResync marks the actor's event stream as no longer following the
// commits, when the actor supports it, as *realtime.Hub does.
func (s *HubSink) RequireResync() {
	if resyncer, ok := s.actor.(Resyncer); ok {
		resyncer.RequireResync()
	}
}

func hubCommand(record Record) (realtime.Command, error) {
	event := record.Event
	if record.Usage != nil {
		payload, err := EncodeMediaUsage(*record.Usage)
		if err != nil {
			return realtime.Command{}, err
		}
		return realtime.Command{
			Key:           realtime.Key{TenantID: event.Key.TenantID, CanvasID: event.Key.CanvasID},
			OperationID:   fmt.Sprintf("outbox-%d", record.ID),
			ObjectID:      MediaUsageObjectID,
			Kind:          realtime.KindUsage,
			Payload:       payload,
			CorrelationID: event.CorrelationID,
		}, nil
	}
	kind, mutation, err := EncodeMutation(event.Mutation)
	if err != nil {
		return realtime.Command{}, err
	}
	command := realtime.Command{
		Key:           realtime.Key{TenantID: event.Key.TenantID, CanvasID: event.Key.CanvasID},
		OperationID:   fmt.Sprintf("outbox-%d", record.ID),
		ObjectID:      event.Result.ObjectID,
		Kind:          realtime.KindSet,
		CorrelationID: event.CorrelationID,
	}
	switch event.Mutation.(type) {
	case canvas.SetDimensions:
		command.ObjectID = CanvasObjectID
	case canvas.DeleteObject:
		command.Kind = realtime.KindDelete
		return command, nil
	}
	command.Payload, err = json.Marshal(hubPayload{
		Kind: kind, Mutation: mutation,
		Revision: event.Result.Revision, ObjectID: event.Result.ObjectID, Z: event.Result.Z,
	})
	if err != nil {
		return realtime.Command{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return command, nil
}
