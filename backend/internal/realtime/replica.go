package realtime

import (
	"context"
	"errors"
)

var (
	// ErrForeignEvent reports an event addressed to another tenant or canvas.
	ErrForeignEvent = errors.New("realtime: event belongs to another canvas")
	// ErrInvalidEvent reports an event that cannot be part of a canvas order.
	ErrInvalidEvent = errors.New("realtime: invalid event")
)

// DefaultReplicaWindow bounds how far ahead of the applied sequence a replica
// buffers out-of-order events. Events beyond it are recovered by CatchUp.
const DefaultReplicaWindow = 1024

// Fetcher replays the retained events after a sequence, as Hub.Events does.
type Fetcher func(ctx context.Context, after uint64) ([]Event, error)

// Replica converges one canvas state from an unreliable event stream: it
// ignores duplicates, buffers reordered events until their gap is filled,
// and recovers lost events by replaying them through a Fetcher.
type Replica struct {
	key     Key
	state   *State
	pending map[uint64]Event
}

// NewReplica returns an empty replica that accepts only events of key.
func NewReplica(key Key) *Replica {
	return &Replica{key: key, state: NewState(), pending: map[uint64]Event{}}
}

// Receive offers one delivered event to the replica.
func (r *Replica) Receive(event Event) error {
	if event.Key != r.key {
		return ErrForeignEvent
	}
	if event.Sequence == 0 || event.ObjectID == "" || (event.Kind != KindSet && event.Kind != KindDelete && event.Kind != KindUsage) {
		return ErrInvalidEvent
	}
	applied := r.state.Sequence()
	if event.Sequence <= applied || event.Sequence > applied+DefaultReplicaWindow {
		return nil
	}
	event.Payload = clonePayload(event.Payload)
	r.pending[event.Sequence] = event
	for next, ok := r.pending[applied+1]; ok; next, ok = r.pending[applied+1] {
		delete(r.pending, next.Sequence)
		r.state.Apply(next)
		applied = next.Sequence
	}
	return nil
}

// Waiting reports whether buffered events wait for a missing sequence.
func (r *Replica) Waiting() bool {
	return len(r.pending) > 0
}

// CatchUp replays every retained event after the applied sequence. It returns
// ErrResyncRequired, without changing state, when the gap is no longer retained.
func (r *Replica) CatchUp(ctx context.Context, fetch Fetcher) error {
	events, err := fetch(ctx, r.state.Sequence())
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := r.Receive(event); err != nil {
			return err
		}
	}
	return nil
}

// State returns the converged canvas state. Callers must not modify it.
func (r *Replica) State() *State {
	return r.state
}
