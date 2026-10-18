package resync

import (
	"context"
	"errors"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

// ErrInvalidConfig reports a service that cannot fail closed.
var ErrInvalidConfig = errors.New("resync: invalid config")

// Loader reads the committed state of a canvas; canvas.Store satisfies it.
// Load must read one consistent revision and honor ctx.
type Loader interface {
	Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error)
}

// Access loads the current access facts of the connection's actor for viewing
// one canvas (TE-001). It is called before the read and again before delivery.
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
// key, never replayed commands or ephemeral state. Access is checked before
// the read and again immediately before delivery, and the read runs under
// the lease: a denied, cut, released, or shut-down connection receives the
// zero Snapshot and an error. The snapshot carries ctx's correlation ID.
func (s *Service) Snapshot(ctx context.Context, lease Lease, key canvas.Key) (Snapshot, error) {
	if err := canvas.ValidateKey(key); err != nil {
		return Snapshot{}, err
	}
	if lease == nil {
		return Snapshot{}, access.ErrAccessDenied
	}
	var snapshot Snapshot
	err := lease.Guard(func(leaseCtx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(leaseCtx, cancel)
		defer stop()

		facts := func() (access.Request, error) { return s.facts(ctx, key) }
		var committed canvas.Canvas
		read := func() (err error) {
			committed, err = s.loader.Load(ctx, key)
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
			id, _ := correlation.FromContext(ctx)
			snapshot = NewSnapshot(committed, id.String())
			return nil
		}
		return s.leaseErr(lease, access.GuardReadDelivery(facts, deliver))
	})
	if err != nil {
		return Snapshot{}, err
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
