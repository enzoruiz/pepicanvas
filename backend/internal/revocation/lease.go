package revocation

import (
	"context"
	"errors"
	"sync"
)

// ErrReleased reports use of a lease after its transport released it.
var ErrReleased = errors.New("revocation: lease released")

// Lease is one registered connection. Its context ends, and Guard rejects
// every operation, as soon as a cut applies to it, before the transport has
// finished closing.
type Lease struct {
	registry *Registry
	grant    Grant
	conn     Conn
	ctx      context.Context
	cancel   context.CancelCauseFunc

	mu  sync.Mutex
	err error
}

func newLease(registry *Registry, grant Grant, conn Conn) *Lease {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Lease{registry: registry, grant: grant, conn: conn, ctx: ctx, cancel: cancel}
}

// Context ends when the lease is cut, shut down, or released. Transports
// run reads and writes under it.
func (l *Lease) Context() context.Context { return l.ctx }

// Err returns nil while the lease is live, or why it ended.
func (l *Lease) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Guard runs one read, write, or command only while the lease is live. The
// operation receives the lease context and must stop when it ends.
func (l *Lease) Guard(operation func(context.Context) error) error {
	if err := l.Err(); err != nil {
		return err
	}
	return operation(l.ctx)
}

// Release unregisters a connection its transport already closed.
func (l *Lease) Release() {
	l.registry.release(l)
	l.end(ErrReleased)
}

// end records the first reason the lease ended and cancels its context.
func (l *Lease) end(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
	l.cancel(err)
}
