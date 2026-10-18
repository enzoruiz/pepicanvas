// Package realtime serializes canvas commands through one actor per canvas,
// assigns a stable acceptance order, and deduplicates retried operations.
package realtime

import (
	"errors"
	"fmt"
)

// Limits bound every client-supplied identifier and payload.
const (
	MaxIdentifierBytes = 128
	MaxPayloadBytes    = 64 << 10
)

// ErrInvalidCommand reports a command rejected before authorization or ordering.
var ErrInvalidCommand = errors.New("realtime: invalid command")

// Key identifies one canvas inside one tenant. Equal canvas IDs in different
// tenants are distinct canvases with independent order and deduplication.
type Key struct {
	TenantID string
	CanvasID string
}

// Kind identifies the effect of a command on its target object.
type Kind string

const (
	KindSet    Kind = "set"
	KindDelete Kind = "delete"
)

// Command is one client intent addressed to a canvas object.
type Command struct {
	Key           Key
	OperationID   string
	ObjectID      string
	Kind          Kind
	Payload       []byte
	CorrelationID string
}

// Outcome reports whether the actor accepted a command into the canvas order.
type Outcome string

const (
	OutcomeAccepted Outcome = "accepted"
	OutcomeRejected Outcome = "rejected"
)

// Reason explains a domain rejection decided by the actor.
type Reason string

// ReasonObjectDeleted rejects any command that targets a deleted object.
const ReasonObjectDeleted Reason = "object_deleted"

// Result is the authoritative answer for one operation. Sequence is zero
// unless the command was accepted.
type Result struct {
	OperationID   string
	Outcome       Outcome
	Sequence      uint64
	Reason        Reason
	CorrelationID string
}

// Event is one accepted command in canvas acceptance order.
type Event struct {
	Key           Key
	Sequence      uint64
	OperationID   string
	ObjectID      string
	Kind          Kind
	Payload       []byte
	CorrelationID string
}

func (c Command) validate() error {
	for name, value := range map[string]string{
		"tenant":    c.Key.TenantID,
		"canvas":    c.Key.CanvasID,
		"operation": c.OperationID,
		"object":    c.ObjectID,
	} {
		if value == "" {
			return fmt.Errorf("%w: missing %s identifier", ErrInvalidCommand, name)
		}
		if !validIdentifier(value) {
			return fmt.Errorf("%w: malformed %s identifier", ErrInvalidCommand, name)
		}
	}
	if c.CorrelationID != "" && !validIdentifier(c.CorrelationID) {
		return fmt.Errorf("%w: malformed correlation identifier", ErrInvalidCommand)
	}
	switch c.Kind {
	case KindSet:
		if len(c.Payload) > MaxPayloadBytes {
			return fmt.Errorf("%w: payload exceeds %d bytes", ErrInvalidCommand, MaxPayloadBytes)
		}
	case KindDelete:
		if len(c.Payload) != 0 {
			return fmt.Errorf("%w: delete carries a payload", ErrInvalidCommand)
		}
	default:
		return fmt.Errorf("%w: unknown kind", ErrInvalidCommand)
	}
	return nil
}

// validIdentifier accepts bounded printable ASCII without spaces.
func validIdentifier(value string) bool {
	if len(value) > MaxIdentifierBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return false
		}
	}
	return true
}

func (c Command) event(sequence uint64) Event {
	return Event{
		Key:           c.Key,
		Sequence:      sequence,
		OperationID:   c.OperationID,
		ObjectID:      c.ObjectID,
		Kind:          c.Kind,
		Payload:       clonePayload(c.Payload),
		CorrelationID: c.CorrelationID,
	}
}

func clonePayload(payload []byte) []byte {
	if payload == nil {
		return nil
	}
	return append([]byte{}, payload...)
}
