package resync

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

var (
	// ErrInvalidConfig reports a service that cannot fail closed.
	ErrInvalidConfig = errors.New("resync: invalid config")
	// ErrInvalidRequest reports a resync request rejected before access or the store.
	ErrInvalidRequest = errors.New("resync: invalid request")
)

// MaxCorrelationBytes bounds the correlation ID a request carries.
const MaxCorrelationBytes = 128

// Request asks for the committed state of one canvas. CorrelationID, when
// set, is bounded printable ASCII and is echoed in the snapshot.
type Request struct {
	Key           canvas.Key
	CorrelationID string
}

func (r Request) validate() error {
	if err := canvas.ValidateKey(r.Key); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if len(r.CorrelationID) > MaxCorrelationBytes {
		return fmt.Errorf("%w: correlation identifier exceeds %d bytes", ErrInvalidRequest, MaxCorrelationBytes)
	}
	for i := 0; i < len(r.CorrelationID); i++ {
		if c := r.CorrelationID[i]; c <= ' ' || c > '~' {
			return fmt.Errorf("%w: malformed correlation identifier", ErrInvalidRequest)
		}
	}
	return nil
}

// Loader reads the committed state of a canvas; canvas.Store satisfies it.
// Load must read one consistent revision and honor ctx.
type Loader interface {
	Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error)
}

// Access loads the current access facts of the connection's actor for viewing
// one canvas (TE-001). It is called before the read and again before delivery.
// The transport must bind it to the same actor as the lease's grant; the
// service checks only that the facts describe viewing the requested canvas.
type Access func(ctx context.Context, key canvas.Key) (access.Request, error)

// Lease is the live connection asking to resync; *revocation.Lease satisfies
// it. Guard runs work only while the lease is live, under a context that ends
// when the lease is cut, shut down, or released, and Err reports why it ended.
type Lease interface {
	Guard(operation func(context.Context) error) error
	Err() error
}

// Service answers resync requests with the committed state of a canvas.
type Service struct {
	loader Loader
	access Access
}

// NewService returns a service over a committed-state loader and an access
// facts loader.
func NewService(loader Loader, facts Access) (*Service, error) {
	if loader == nil || facts == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("loader and access are required"))
	}
	return &Service{loader: loader, access: facts}, nil
}

// Snapshot reauthorizes the lease's actor and returns the committed state of
// the requested canvas, never replayed commands or ephemeral state. Access is checked before
// the read and again immediately before delivery, and the read runs under
// the lease: a denied, cut, released, or shut-down connection receives the
// zero Snapshot and an error. The snapshot carries the request's correlation ID.
func (s *Service) Snapshot(ctx context.Context, lease Lease, request Request) (Snapshot, error) {
	if err := request.validate(); err != nil {
		return Snapshot{}, err
	}
	key := request.Key
	if lease == nil || (reflect.ValueOf(lease).Kind() == reflect.Pointer && reflect.ValueOf(lease).IsNil()) {
		return Snapshot{}, access.ErrAccessDenied
	}
	var snapshot Snapshot
	delivered := false
	err := lease.Guard(func(leaseCtx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(leaseCtx, cancel)
		defer stop()

		facts := func() (access.Request, error) { return s.facts(ctx, key) }
		var committed canvas.Canvas
		read := func() (err error) {
			committed, err = s.loader.Load(ctx, key)
			if err == nil && committed.Key != key {
				return canvas.ErrNotFound
			}
			return err
		}
		if err := access.GuardReadDelivery(facts, read); err != nil {
			return s.leaseErr(lease, err)
		}
		deliver := func() error {
			// The lease may have been cut while the store answered.
			if err := lease.Err(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			snapshot, delivered = NewSnapshot(committed, request.CorrelationID), true
			return nil
		}
		return s.leaseErr(lease, access.GuardReadDelivery(facts, deliver))
	})
	if err != nil {
		return Snapshot{}, err
	}
	if !delivered {
		// A lease that reports success without running the work is not live.
		return Snapshot{}, access.ErrAccessDenied
	}
	return snapshot, nil
}

// facts loads the access facts and rejects facts that do not describe viewing
// this canvas, so a wrong facts answer cannot authorize another resource.
func (s *Service) facts(ctx context.Context, key canvas.Key) (access.Request, error) {
	request, err := s.access(ctx, key)
	if err != nil {
		return access.Request{}, err
	}
	if request.Resource != access.ResourceCanvas || request.Action != access.ActionView || request.ResourceTenantID != key.TenantID {
		return access.Request{}, access.ErrAccessDenied
	}
	return request, nil
}

// leaseErr reports why the lease ended when it ended, instead of the
// cancellation it caused.
func (s *Service) leaseErr(lease Lease, err error) error {
	if err == nil {
		return nil
	}
	if ended := lease.Err(); ended != nil {
		return ended
	}
	return err
}
