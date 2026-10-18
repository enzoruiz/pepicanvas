// Package outbox delivers committed canvas mutations to the realtime actor.
// Each mutation and its outbox record commit in one PostgreSQL transaction;
// a retrying publisher later delivers every pending record at least once, in
// revision order per canvas, and keeps undelivered records pending across
// restarts. No broker is involved.
package outbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

// ErrMalformed reports a mutation that cannot be encoded or a stored record
// that cannot be decoded into a valid mutation.
var ErrMalformed = errors.New("outbox: malformed mutation")

// Record is one outbox entry: a committed canvas event and its delivery
// bookkeeping. Records of one canvas are numbered by revision, and a higher
// ID was committed later.
type Record struct {
	ID    int64
	Event resync.Event
	// CreatedAt is when the mutation was accepted.
	CreatedAt time.Time
	// Attempts counts failed deliveries.
	Attempts int
	// NextAttempt is the earliest time of the next delivery.
	NextAttempt time.Time
}

// Mutation kinds as stored in the outbox.
const (
	kindSetDimensions = "set_dimensions"
	kindCreateObject  = "create_object"
	kindSetContent    = "set_content"
	kindSetTransform  = "set_transform"
	kindDeleteObject  = "delete_object"
)

// wireMutation is the stored form of every mutation kind.
type wireMutation struct {
	Width    int         `json:"width,omitempty"`
	Height   int         `json:"height,omitempty"`
	ObjectID string      `json:"object_id,omitempty"`
	Kind     canvas.Kind `json:"kind,omitempty"`
	Content  string      `json:"content,omitempty"`
	// Transform is set for create_object and set_transform.
	Transform *wireTransform `json:"transform,omitempty"`
}

type wireTransform struct {
	X        int     `json:"x"`
	Y        int     `json:"y"`
	W        int     `json:"w"`
	H        int     `json:"h"`
	Rotation float64 `json:"rotation"`
}

func toWire(t canvas.Transform) *wireTransform {
	return &wireTransform{X: t.X, Y: t.Y, W: t.Width, H: t.Height, Rotation: t.Rotation}
}

func (w *wireTransform) transform() canvas.Transform {
	if w == nil {
		return canvas.Transform{}
	}
	return canvas.Transform{X: w.X, Y: w.Y, Width: w.W, Height: w.H, Rotation: w.Rotation}
}

// EncodeMutation returns the stored kind and JSON payload of a mutation.
func EncodeMutation(mutation canvas.Mutation) (string, []byte, error) {
	var kind string
	var wire wireMutation
	switch m := mutation.(type) {
	case canvas.SetDimensions:
		kind, wire = kindSetDimensions, wireMutation{Width: m.Dimensions.Width, Height: m.Dimensions.Height}
	case canvas.CreateObject:
		kind, wire = kindCreateObject, wireMutation{Kind: m.Kind, Content: m.Content, Transform: toWire(m.Transform)}
	case canvas.SetContent:
		kind, wire = kindSetContent, wireMutation{ObjectID: m.ObjectID, Content: m.Content}
	case canvas.SetTransform:
		kind, wire = kindSetTransform, wireMutation{ObjectID: m.ObjectID, Transform: toWire(m.Transform)}
	case canvas.DeleteObject:
		kind, wire = kindDeleteObject, wireMutation{ObjectID: m.ObjectID}
	default:
		return "", nil, fmt.Errorf("%w: unsupported mutation %T", ErrMalformed, mutation)
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return kind, payload, nil
}

// DecodeMutation rebuilds a mutation from its stored kind and payload and
// checks it against the canvas invariants.
func DecodeMutation(kind string, payload []byte) (canvas.Mutation, error) {
	var wire wireMutation
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	var mutation canvas.Mutation
	switch kind {
	case kindSetDimensions:
		mutation = canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: wire.Width, Height: wire.Height}}
	case kindCreateObject:
		mutation = canvas.CreateObject{Kind: wire.Kind, Content: wire.Content, Transform: wire.Transform.transform()}
	case kindSetContent:
		mutation = canvas.SetContent{ObjectID: wire.ObjectID, Content: wire.Content}
	case kindSetTransform:
		mutation = canvas.SetTransform{ObjectID: wire.ObjectID, Transform: wire.Transform.transform()}
	case kindDeleteObject:
		mutation = canvas.DeleteObject{ObjectID: wire.ObjectID}
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrMalformed, kind)
	}
	// The key and operation are placeholders: only the mutation is checked.
	placeholder := canvas.Key{TenantID: "t", CanvasID: "c"}
	if err := canvas.Validate(placeholder, canvas.Operation{ID: "o"}, mutation); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return mutation, nil
}
