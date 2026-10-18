package resync

import (
	"errors"
	"fmt"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

var (
	// ErrForeign reports an event or snapshot of another tenant or canvas.
	ErrForeign = errors.New("resync: state belongs to another canvas")
	// ErrInvalidEvent reports an event that cannot follow the committed order.
	ErrInvalidEvent = errors.New("resync: invalid event")
)

// DefaultWindow bounds the events a replica buffers while it waits for a
// missing revision or a snapshot. Events beyond it are recovered by resync.
const DefaultWindow = 1024

// Replica is the reference client model. It follows committed events in
// revision order, buffers reordered ones, ignores duplicates, and reports a
// discontinuity instead of guessing: the client then resyncs and adopts a
// snapshot. A Replica is not safe for concurrent use.
type Replica struct {
	key     canvas.Key
	synced  bool
	broken  bool // an event could not follow the state; only a snapshot repairs it
	state   canvas.Canvas
	latest  int64 // highest revision known to be committed
	pending map[int64]Event
}

// NewReplica returns a replica of key that needs a snapshot before it holds state.
func NewReplica(key canvas.Key) *Replica {
	return &Replica{key: key, pending: map[int64]Event{}}
}

// Receive offers one delivered event. Duplicates and events at or below the
// current revision are ignored; later ones wait for their gap to be filled.
// An event that cannot follow the state returns ErrInvalidEvent and leaves
// the replica needing resync.
func (r *Replica) Receive(event Event) error {
	if event.Key != r.key {
		return ErrForeign
	}
	if event.Result.Revision < 1 || event.Mutation == nil {
		return ErrInvalidEvent
	}
	r.latest = max(r.latest, event.Result.Revision)
	if r.synced && event.Result.Revision <= r.state.Revision {
		return nil
	}
	r.buffer(event)
	return r.drain()
}

// buffer keeps an event while it waits. A full buffer keeps the lowest
// revisions, which are applied first, and drops the highest one.
func (r *Replica) buffer(event Event) {
	revision := event.Result.Revision
	if _, ok := r.pending[revision]; ok {
		return
	}
	if len(r.pending) >= DefaultWindow {
		highest := revision
		for buffered := range r.pending {
			highest = max(highest, buffered)
		}
		if highest == revision {
			return
		}
		delete(r.pending, highest)
	}
	r.pending[revision] = event
}

// Notice records the latest committed revision announced by the server, for
// example in a heartbeat, so a lost tail is detected.
func (r *Replica) Notice(revision int64) {
	r.latest = max(r.latest, revision)
}

// Adopt replaces the state with a snapshot and applies the buffered events
// after it. A snapshot older than the state, or equal to it while the state
// is consistent, changes nothing, so repeated or late answers are harmless.
func (r *Replica) Adopt(snapshot Snapshot) error {
	if snapshot.Canvas.Key != r.key {
		return ErrForeign
	}
	revision := snapshot.Revision()
	if r.synced && (revision < r.state.Revision || (revision == r.state.Revision && !r.broken)) {
		return nil
	}
	r.state = NewSnapshot(snapshot.Canvas, "").Canvas
	r.synced, r.broken = true, false
	r.latest = max(r.latest, revision)
	for buffered := range r.pending {
		if buffered <= revision {
			delete(r.pending, buffered)
		}
	}
	return r.drain()
}

// drain applies buffered events while the next revision is present.
func (r *Replica) drain() error {
	if !r.synced {
		return nil
	}
	for {
		next, ok := r.pending[r.state.Revision+1]
		if !ok {
			return nil
		}
		delete(r.pending, next.Result.Revision)
		folded, err := canvas.Fold(r.state, next.Mutation, next.Result)
		if err != nil {
			r.broken = true
			return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
		}
		r.state = folded
	}
}

// NeedsResync reports whether the replica has no state, cannot apply the
// next event, or knows of a committed revision it does not hold. A gap may
// still fill by reordering; the transport decides how long to wait.
func (r *Replica) NeedsResync() bool {
	return !r.synced || r.broken || r.latest > r.state.Revision
}

// Revision returns the applied revision.
func (r *Replica) Revision() int64 { return r.state.Revision }

// Buffered returns how many events wait for a missing revision or a snapshot.
func (r *Replica) Buffered() int { return len(r.pending) }

// State returns the applied state, or the zero Snapshot before the first adoption.
func (r *Replica) State() Snapshot {
	if !r.synced {
		return Snapshot{}
	}
	return NewSnapshot(r.state, "")
}
