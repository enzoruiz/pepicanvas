// Package canvas defines the persistent canvas model, its invariants, and the
// storage port. Every mutation is validated here before any adapter runs it.
package canvas

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Errors returned by Validate and by Store implementations.
var (
	// ErrInvalid reports a mutation rejected before it reaches storage.
	ErrInvalid = errors.New("canvas: invalid mutation")
	// ErrNotFound reports a missing canvas or object. It never says whether
	// the resource exists in another tenant.
	ErrNotFound = errors.New("canvas: not found")
	// ErrOperationConflict reports an operation ID reused for a different
	// mutation. A new intent requires a new operation ID.
	ErrOperationConflict = errors.New("canvas: operation id reused with a different mutation")
)

// Bounds of the persistent model.
const (
	MinWidth  = 320
	MinHeight = 180
	MaxWidth  = 3840
	MaxHeight = 2160

	MaxIdentifierBytes = 128
	MaxContentBytes    = 4096
	// MaxObjectSize and MaxCoordinate keep objects representable; objects
	// may still lie partly or fully outside the canvas area.
	MaxObjectSize = 32768
	MaxCoordinate = 32768
)

// DefaultDimensions is the size of a newly created canvas.
var DefaultDimensions = Dimensions{Width: 1920, Height: 1080}

// Key identifies one canvas inside one tenant.
type Key struct {
	TenantID string
	CanvasID string
}

// Dimensions is the canvas area in whole pixels.
type Dimensions struct {
	Width  int
	Height int
}

// Kind identifies the content an object renders.
type Kind string

const (
	KindText  Kind = "text"
	KindImage Kind = "image"
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Transform places an object in canvas pixels. Rotation is in degrees within
// [0, 360).
type Transform struct {
	X        int
	Y        int
	Width    int
	Height   int
	Rotation float64
}

// Object is one committed canvas object. A higher Z renders above a lower one.
type Object struct {
	ID        string
	Kind      Kind
	Content   string
	Transform Transform
	Z         int64
}

// Canvas is the committed state of one canvas. Objects are ordered by Z,
// bottom first. Revision counts committed mutations.
type Canvas struct {
	Key        Key
	Dimensions Dimensions
	Revision   int64
	Objects    []Object
}

// Operation identifies one client intent. Retrying the same mutation with the
// same ID returns the original result without a second effect.
type Operation struct {
	ID            string
	CorrelationID string
}

// Mutation is one durable change applied atomically to a canvas.
type Mutation interface {
	validate() error
}

// SetDimensions resizes the canvas without moving, resizing, or reordering objects.
type SetDimensions struct {
	Dimensions Dimensions
}

// CreateObject adds an object above every existing object.
type CreateObject struct {
	Kind      Kind
	Content   string
	Transform Transform
}

// SetContent replaces the content of an object without changing its order.
type SetContent struct {
	ObjectID string
	Content  string
}

// SetTransform places an object and moves it above every other object.
type SetTransform struct {
	ObjectID  string
	Transform Transform
}

// DeleteObject removes an object permanently.
type DeleteObject struct {
	ObjectID string
}

// Result is the committed outcome of one mutation. ObjectID and Z are set for
// object mutations; Z is zero for SetContent and DeleteObject.
type Result struct {
	Revision int64
	ObjectID string
	Z        int64
}

// Store persists canvases. Each Apply commits in a single transaction or not at all.
type Store interface {
	// Create makes the canvas with DefaultDimensions, or returns the existing one.
	Create(ctx context.Context, key Key) (Canvas, error)
	// Load returns the committed canvas state.
	Load(ctx context.Context, key Key) (Canvas, error)
	// Apply validates and commits one mutation.
	Apply(ctx context.Context, key Key, operation Operation, mutation Mutation) (Result, error)
}

// ValidateKey checks a canvas key.
func ValidateKey(key Key) error {
	if err := validateIdentifier("tenant", key.TenantID); err != nil {
		return err
	}
	return validateIdentifier("canvas", key.CanvasID)
}

// Validate checks a mutation and its identity before it reaches storage.
func Validate(key Key, operation Operation, mutation Mutation) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := validateIdentifier("operation", operation.ID); err != nil {
		return err
	}
	if operation.CorrelationID != "" {
		if err := validateIdentifier("correlation", operation.CorrelationID); err != nil {
			return err
		}
	}
	if mutation == nil {
		return fmt.Errorf("%w: missing mutation", ErrInvalid)
	}
	return mutation.validate()
}

func (m SetDimensions) validate() error {
	d := m.Dimensions
	if d.Width < MinWidth || d.Width > MaxWidth || d.Height < MinHeight || d.Height > MaxHeight {
		return fmt.Errorf("%w: dimensions must be within %dx%d and %dx%d", ErrInvalid, MinWidth, MinHeight, MaxWidth, MaxHeight)
	}
	return nil
}

func (m CreateObject) validate() error {
	switch m.Kind {
	case KindText, KindImage, KindVideo, KindAudio:
	default:
		return fmt.Errorf("%w: unknown object kind", ErrInvalid)
	}
	if err := validateContent(m.Content); err != nil {
		return err
	}
	return m.Transform.validate()
}

func (m SetContent) validate() error {
	if err := validateIdentifier("object", m.ObjectID); err != nil {
		return err
	}
	return validateContent(m.Content)
}

func (m SetTransform) validate() error {
	if err := validateIdentifier("object", m.ObjectID); err != nil {
		return err
	}
	return m.Transform.validate()
}

func (m DeleteObject) validate() error {
	return validateIdentifier("object", m.ObjectID)
}

func (t Transform) validate() error {
	if t.Width < 1 || t.Width > MaxObjectSize || t.Height < 1 || t.Height > MaxObjectSize {
		return fmt.Errorf("%w: object size must be within 1 and %d", ErrInvalid, MaxObjectSize)
	}
	if t.X < -MaxCoordinate || t.X > MaxCoordinate || t.Y < -MaxCoordinate || t.Y > MaxCoordinate {
		return fmt.Errorf("%w: object position must be within ±%d", ErrInvalid, MaxCoordinate)
	}
	if math.IsNaN(t.Rotation) || t.Rotation < 0 || t.Rotation >= 360 {
		return fmt.Errorf("%w: rotation must be within [0, 360)", ErrInvalid)
	}
	return nil
}

func validateContent(content string) error {
	if content == "" || len(content) > MaxContentBytes {
		return fmt.Errorf("%w: content must have 1 to %d bytes", ErrInvalid, MaxContentBytes)
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return fmt.Errorf("%w: content must be UTF-8 without NUL", ErrInvalid)
	}
	return nil
}

// validateIdentifier accepts bounded printable ASCII without spaces.
func validateIdentifier(name, value string) error {
	if value == "" || len(value) > MaxIdentifierBytes {
		return fmt.Errorf("%w: %s identifier must have 1 to %d bytes", ErrInvalid, name, MaxIdentifierBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return fmt.Errorf("%w: malformed %s identifier", ErrInvalid, name)
		}
	}
	return nil
}
