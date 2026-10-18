package resync_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

var (
	keyA  = canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}
	keyB  = canvas.Key{TenantID: "tenant-b", CanvasID: "canvas-1"}
	place = canvas.Transform{X: 1, Y: 2, Width: 30, Height: 40}
)

// history is a committed sequence of canvas states and the events between them.
type history struct {
	states []canvas.Canvas // states[r] is the state at revision r
	events []resync.Event  // events[r-1] produces revision r
}

// newHistory commits n mutations that cover every kind, starting from an
// empty canvas, and records the state after each one.
func newHistory(t *testing.T, key canvas.Key, n int) *history {
	t.Helper()
	h := &history{states: []canvas.Canvas{{Key: key, Dimensions: canvas.DefaultDimensions, Objects: []canvas.Object{}}}}
	var nextZ int64 = 1
	for i := 1; i <= n; i++ {
		current := h.states[len(h.states)-1]
		var mutation canvas.Mutation
		result := canvas.Result{Revision: current.Revision + 1}
		switch {
		case len(current.Objects) < 2 || i%5 == 0:
			kind := canvas.KindText
			if i%2 == 0 {
				kind = canvas.KindVideo
			}
			mutation = canvas.CreateObject{Kind: kind, Content: fmt.Sprintf("content-%d", i), Transform: place}
			result.ObjectID, result.Z = fmt.Sprintf("obj-%d", i), nextZ
			nextZ++
		case i%5 == 1:
			mutation = canvas.SetContent{ObjectID: current.Objects[0].ID, Content: fmt.Sprintf("edit-%d", i)}
			result.ObjectID = current.Objects[0].ID
		case i%5 == 2:
			mutation = canvas.SetTransform{ObjectID: current.Objects[0].ID, Transform: canvas.Transform{X: i, Y: -i, Width: 5, Height: 6, Rotation: float64(i % 360)}}
			result.ObjectID, result.Z = current.Objects[0].ID, nextZ
			nextZ++
		case i%5 == 3:
			mutation = canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320 + i%1000, Height: 180 + i%1000}}
		default:
			mutation = canvas.DeleteObject{ObjectID: current.Objects[len(current.Objects)-1].ID}
			result.ObjectID = current.Objects[len(current.Objects)-1].ID
		}
		next, err := canvas.Fold(current, mutation, result)
		if err != nil {
			t.Fatalf("history step %d: %v", i, err)
		}
		h.states = append(h.states, next)
		h.events = append(h.events, resync.Event{Key: key, Mutation: mutation, Result: result, CorrelationID: fmt.Sprintf("corr-%d", i)})
	}
	return h
}

// event returns the event that produces revision r.
func (h *history) event(r int64) resync.Event { return h.events[r-1] }

func (h *history) snapshot(r int64) resync.Snapshot { return resync.NewSnapshot(h.states[r], "") }

func (h *history) last() int64 { return int64(len(h.events)) }

func synced(t *testing.T, h *history, r int64) *resync.Replica {
	t.Helper()
	replica := resync.NewReplica(h.states[0].Key)
	if err := replica.Adopt(h.snapshot(r)); err != nil {
		t.Fatalf("Adopt(%d): %v", r, err)
	}
	return replica
}

func receive(t *testing.T, replica *resync.Replica, h *history, revisions ...int64) {
	t.Helper()
	for _, r := range revisions {
		if err := replica.Receive(h.event(r)); err != nil {
			t.Fatalf("Receive(%d): %v", r, err)
		}
	}
}

func requireState(t *testing.T, replica *resync.Replica, h *history, r int64) {
	t.Helper()
	if got, want := replica.State(), h.snapshot(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("replica state = %+v, want revision %d %+v", got, r, want)
	}
	if replica.NeedsResync() {
		t.Fatalf("replica at revision %d still needs resync", r)
	}
}

func span(from, to int64) []int64 {
	var revisions []int64
	for r := from; r <= to; r++ {
		revisions = append(revisions, r)
	}
	return revisions
}

func TestNewReplicaNeedsASnapshot(t *testing.T) {
	replica := resync.NewReplica(keyA)
	if !replica.NeedsResync() {
		t.Fatal("a new replica does not need resync")
	}
	if got := replica.State(); !reflect.DeepEqual(got, resync.Snapshot{}) {
		t.Fatalf("new replica state = %+v, want none", got)
	}
}

func TestContiguousEventsFollowTheCommittedState(t *testing.T) {
	h := newHistory(t, keyA, 20)
	replica := synced(t, h, 0)
	for r := int64(1); r <= h.last(); r++ {
		receive(t, replica, h, r)
		requireState(t, replica, h, r)
	}
}

func TestDuplicatesAreIgnored(t *testing.T) {
	h := newHistory(t, keyA, 6)
	replica := synced(t, h, 0)
	receive(t, replica, h, 1, 2, 1, 3, 2, 3, 4, 4, 5, 6, 6)
	requireState(t, replica, h, 6)
}

func TestReorderedEventsWaitForTheirGap(t *testing.T) {
	h := newHistory(t, keyA, 6)
	replica := synced(t, h, 0)
	receive(t, replica, h, 1, 3, 4)
	if !replica.NeedsResync() {
		t.Fatal("a gap at revision 2 is not reported")
	}
	if got := replica.Revision(); got != 1 {
		t.Fatalf("revision = %d, want 1 while 2 is missing", got)
	}
	receive(t, replica, h, 2, 6, 5)
	requireState(t, replica, h, 6)
}

func TestEventLossIsDetectedAndResyncConverges(t *testing.T) {
	h := newHistory(t, keyA, 12)
	cases := map[string]struct {
		delivered []int64
		lost      int64 // the revision the client still knows it misses
	}{
		"one event":                 {[]int64{1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12}, 3},
		"several consecutive":       {[]int64{1, 2, 7, 8, 9, 10, 11, 12}, 3},
		"several scattered":         {[]int64{1, 3, 4, 6, 9, 10, 12}, 2},
		"everything after the head": {[]int64{12}, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			replica := synced(t, h, 0)
			receive(t, replica, h, tc.delivered...)
			if !replica.NeedsResync() {
				t.Fatal("loss is not reported")
			}
			if got := replica.Revision(); got != tc.lost-1 {
				t.Fatalf("revision = %d, want %d before the lost event", got, tc.lost-1)
			}
			// The snapshot is taken after the client noticed the loss, at
			// the current committed revision; nothing is replayed.
			if err := replica.Adopt(h.snapshot(h.last())); err != nil {
				t.Fatalf("Adopt: %v", err)
			}
			requireState(t, replica, h, h.last())
		})
	}
}

func TestTailLossIsDetectedByANotice(t *testing.T) {
	h := newHistory(t, keyA, 8)
	replica := synced(t, h, 0)
	receive(t, replica, h, span(1, 5)...)
	replica.Notice(5)
	requireState(t, replica, h, 5)
	replica.Notice(8)
	if !replica.NeedsResync() {
		t.Fatal("events 6 to 8 lost at the tail are not reported")
	}
	if err := replica.Adopt(h.snapshot(8)); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	requireState(t, replica, h, 8)
}

func TestSnapshotConcurrentWithEvents(t *testing.T) {
	h := newHistory(t, keyA, 10)
	t.Run("events before the snapshot arrive first", func(t *testing.T) {
		replica := resync.NewReplica(keyA)
		receive(t, replica, h, 5, 7, 6, 8)
		if err := replica.Adopt(h.snapshot(6)); err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		requireState(t, replica, h, 8)
		receive(t, replica, h, 9, 10)
		requireState(t, replica, h, 10)
	})
	t.Run("events at or below the snapshot arrive late", func(t *testing.T) {
		replica := resync.NewReplica(keyA)
		if err := replica.Adopt(h.snapshot(7)); err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		receive(t, replica, h, 3, 6, 7, 8, 5)
		requireState(t, replica, h, 8)
	})
	t.Run("a snapshot older than a buffered event leaves only the newer gap", func(t *testing.T) {
		replica := resync.NewReplica(keyA)
		receive(t, replica, h, 9)
		if err := replica.Adopt(h.snapshot(6)); err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		if !replica.NeedsResync() || replica.Revision() != 6 {
			t.Fatalf("NeedsResync = %v at %d, want true at 6", replica.NeedsResync(), replica.Revision())
		}
		receive(t, replica, h, 7, 8)
		requireState(t, replica, h, 9)
	})
}

func TestRepeatedAndStaleSnapshotsChangeNothing(t *testing.T) {
	h := newHistory(t, keyA, 8)
	replica := synced(t, h, 0)
	receive(t, replica, h, span(1, 6)...)
	for _, r := range []int64{6, 4, 0} {
		if err := replica.Adopt(h.snapshot(r)); err != nil {
			t.Fatalf("Adopt(%d): %v", r, err)
		}
		requireState(t, replica, h, 6)
	}
	receive(t, replica, h, 7, 8)
	requireState(t, replica, h, 8)
}

func TestForeignStateIsRejected(t *testing.T) {
	h := newHistory(t, keyA, 4)
	foreign := newHistory(t, keyB, 4)
	replica := synced(t, h, 0)
	receive(t, replica, h, 1, 2)
	if err := replica.Receive(foreign.event(3)); !errors.Is(err, resync.ErrForeign) {
		t.Fatalf("Receive(foreign) = %v, want ErrForeign", err)
	}
	if err := replica.Adopt(foreign.snapshot(4)); !errors.Is(err, resync.ErrForeign) {
		t.Fatalf("Adopt(foreign) = %v, want ErrForeign", err)
	}
	requireState(t, replica, h, 2)
	receive(t, replica, h, 3, 4)
	requireState(t, replica, h, 4)
}

func TestInvalidEventsAreRejected(t *testing.T) {
	h := newHistory(t, keyA, 3)
	replica := synced(t, h, 0)
	receive(t, replica, h, 1)
	valid := h.event(2)
	for name, event := range map[string]resync.Event{
		"zero revision":    {Key: keyA, Mutation: valid.Mutation},
		"missing mutation": {Key: keyA, Result: valid.Result},
	} {
		if err := replica.Receive(event); !errors.Is(err, resync.ErrInvalidEvent) {
			t.Fatalf("Receive(%s) = %v, want ErrInvalidEvent", name, err)
		}
	}
	requireState(t, replica, h, 1)
}

func TestAnInconsistentEventRequiresResync(t *testing.T) {
	h := newHistory(t, keyA, 4)
	replica := synced(t, h, 0)
	receive(t, replica, h, 1)
	bad := h.event(2)
	bad.Mutation = canvas.DeleteObject{ObjectID: "never-created"}
	bad.Result.ObjectID = "never-created"
	if err := replica.Receive(bad); !errors.Is(err, resync.ErrInvalidEvent) {
		t.Fatalf("Receive(inconsistent) = %v, want ErrInvalidEvent", err)
	}
	if !replica.NeedsResync() || replica.Revision() != 1 {
		t.Fatalf("NeedsResync = %v at %d, want true at 1", replica.NeedsResync(), replica.Revision())
	}
	if err := replica.Adopt(h.snapshot(1)); err != nil {
		t.Fatalf("Adopt at the same revision: %v", err)
	}
	receive(t, replica, h, 2, 3, 4)
	requireState(t, replica, h, 4)
}

func TestBufferIsBounded(t *testing.T) {
	h := newHistory(t, keyA, resync.DefaultWindow+10)
	replica := synced(t, h, 0)
	receive(t, replica, h, span(2, h.last())...)
	if got := replica.Buffered(); got != resync.DefaultWindow {
		t.Fatalf("buffered = %d, want %d", got, resync.DefaultWindow)
	}
	if err := replica.Adopt(h.snapshot(h.last())); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	requireState(t, replica, h, h.last())
	if got := replica.Buffered(); got != 0 {
		t.Fatalf("buffered after adoption = %d, want 0", got)
	}
}

func TestSnapshotsCarryOnlyStoppedMedia(t *testing.T) {
	state := canvas.Canvas{Key: keyA, Dimensions: canvas.DefaultDimensions, Revision: 6, Objects: []canvas.Object{
		{ID: "text", Kind: canvas.KindText, Content: "t", Transform: place, Z: 1},
		{ID: "image", Kind: canvas.KindImage, Content: "i", Transform: place, Z: 2},
		{ID: "video", Kind: canvas.KindVideo, Content: "v", Transform: place, Z: 3},
		{ID: "audio", Kind: canvas.KindAudio, Content: "a", Transform: place, Z: 4},
	}}
	snapshot := resync.NewSnapshot(state, "corr-1")
	if snapshot.Revision() != 6 || snapshot.CorrelationID != "corr-1" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	media := 0
	for _, object := range state.Objects {
		playback, ok := snapshot.Playback[object.ID]
		switch object.Kind {
		case canvas.KindVideo, canvas.KindAudio:
			media++
			if !ok || playback != (resync.Playback{}) {
				t.Fatalf("media %s playback = %+v, %v; want stopped at zero", object.ID, playback, ok)
			}
		default:
			if ok {
				t.Fatalf("%s object %s has playback", object.Kind, object.ID)
			}
		}
	}
	if media != 2 || len(snapshot.Playback) != media {
		t.Fatalf("playback entries = %d, media objects = %d", len(snapshot.Playback), media)
	}
}

func TestOverflowKeepsTheNextRevision(t *testing.T) {
	h := newHistory(t, keyA, resync.DefaultWindow+10)
	replica := synced(t, h, 0)
	receive(t, replica, h, span(3, h.last())...)
	receive(t, replica, h, 1, 2)
	if replica.Revision() < 3 {
		t.Fatalf("revision = %d after the gap filled, want the buffered revisions applied", replica.Revision())
	}
	if !replica.NeedsResync() {
		t.Fatal("events dropped by the overflow are not reported")
	}
	if err := replica.Adopt(h.snapshot(h.last())); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	requireState(t, replica, h, h.last())
}
