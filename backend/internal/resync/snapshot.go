// Package resync brings a client that lost or skipped committed canvas events
// back to the current committed state. The server reads a snapshot from the
// store after reauthorizing the client; the client adopts it instead of
// replaying commands, and keeps following events numbered by revision.
package resync

import (
	"slices"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

// Event is one committed canvas mutation. Result.Revision is its position in
// the canvas order: the revision the mutation produced.
type Event struct {
	Key           canvas.Key
	Mutation      canvas.Mutation
	Result        canvas.Result
	CorrelationID string
}

// Playback is the media state a snapshot delivers. Playback is ephemeral and
// never restored, so every media object is delivered stopped at position zero.
type Playback struct {
	Playing  bool
	Position time.Duration
}

// Snapshot is the committed state of one canvas at Canvas.Revision. It holds
// no locks, cursors, presence, or other ephemeral state.
type Snapshot struct {
	Canvas canvas.Canvas
	// Playback holds one entry per video and audio object, always stopped.
	Playback      map[string]Playback
	CorrelationID string
}

// NewSnapshot returns the snapshot of a committed canvas.
func NewSnapshot(c canvas.Canvas, correlationID string) Snapshot {
	c.Objects = slices.Clone(c.Objects)
	playback := map[string]Playback{}
	for _, object := range c.Objects {
		if object.Kind == canvas.KindVideo || object.Kind == canvas.KindAudio {
			playback[object.ID] = Playback{}
		}
	}
	return Snapshot{Canvas: c, Playback: playback, CorrelationID: correlationID}
}

// Revision returns the revision the snapshot was read at.
func (s Snapshot) Revision() int64 { return s.Canvas.Revision }
