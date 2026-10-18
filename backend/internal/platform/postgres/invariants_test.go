package postgres_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

func TestConcurrentDifferentMutationsUnderOneOperationIDHaveOneWinner(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	operation := nextOperation(t)
	groups := [2]canvas.Mutation{createText, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}}}

	type outcome struct {
		result canvas.Result
		err    error
	}
	var wg sync.WaitGroup
	outcomes := [2][]outcome{make([]outcome, 8), make([]outcome, 8)}
	for i := range 16 {
		wg.Go(func() {
			group, slot := i%2, i/2
			result, err := store.Apply(context.Background(), tenantA, operation, groups[group])
			if err != nil && !errors.Is(err, canvas.ErrOperationConflict) {
				t.Errorf("concurrent Apply(%T): %v", groups[group], err)
			}
			outcomes[group][slot] = outcome{result, err}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	winner := -1
	for group, list := range outcomes {
		if list[0].err == nil {
			if winner != -1 {
				t.Fatalf("both groups won: %+v", outcomes)
			}
			winner = group
		}
	}
	if winner == -1 {
		t.Fatalf("no group won: %+v", outcomes)
	}
	for _, o := range outcomes[winner] {
		if o.err != nil || o.result != outcomes[winner][0].result {
			t.Fatalf("winning group %T outcomes differ: %+v", groups[winner], outcomes[winner])
		}
	}
	for _, o := range outcomes[1-winner] {
		if !errors.Is(o.err, canvas.ErrOperationConflict) {
			t.Fatalf("losing group %T outcome = %+v, want ErrOperationConflict", groups[1-winner], o)
		}
	}

	got := mustLoad(t, store, tenantA)
	if got.Revision != 1 || outcomes[winner][0].result.Revision != 1 || operationRows(t, pool, tenantA) != 1 {
		t.Fatalf("canvas %+v after one winning operation, want revision 1 and one operation row", got)
	}
	if _, created := groups[winner].(canvas.CreateObject); created != (len(got.Objects) == 1) || created == (got.Dimensions != canvas.DefaultDimensions) {
		t.Fatalf("canvas %+v does not hold exactly the effect of %T", got, groups[winner])
	}
}

func TestConcurrentDeleteAndEditsOfOneObject(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	object := apply(t, store, tenantA, createText).ObjectID
	base := mustLoad(t, store, tenantA).Revision

	var (
		wg                 sync.WaitGroup
		lock               sync.Mutex
		successes, missing int
		deletes            int
	)
	for i := range 20 {
		wg.Go(func() {
			var mutation canvas.Mutation
			switch i % 3 {
			case 0:
				mutation = canvas.DeleteObject{ObjectID: object}
			case 1:
				mutation = canvas.SetContent{ObjectID: object, Content: "edit"}
			default:
				mutation = canvas.SetTransform{ObjectID: object, Transform: canvas.Transform{X: i, Width: 5, Height: 5}}
			}
			_, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation)
			lock.Lock()
			defer lock.Unlock()
			switch {
			case err == nil:
				successes++
				if _, deleted := mutation.(canvas.DeleteObject); deleted {
					deletes++
				}
			case errors.Is(err, canvas.ErrNotFound):
				missing++
			default:
				t.Errorf("concurrent Apply(%T): %v", mutation, err)
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	got := mustLoad(t, store, tenantA)
	if successes+missing != 20 || deletes != 1 {
		t.Fatalf("successes %d, not found %d, deletes %d; want 20 outcomes and one delete", successes, missing, deletes)
	}
	if got.Revision != base+int64(successes) || len(got.Objects) != 0 {
		t.Fatalf("canvas %+v, want revision %d and the object gone", got, base+int64(successes))
	}
}

func TestLoadIsASnapshotUnderConcurrentCreates(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)

	var creators sync.WaitGroup
	for i := range 24 {
		creators.Go(func() {
			mutation := canvas.CreateObject{Kind: canvas.KindText, Content: "x", Transform: canvas.Transform{X: i, Width: 1, Height: 1}}
			if _, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation); err != nil {
				t.Errorf("concurrent CreateObject: %v", err)
			}
		})
	}
	done := make(chan struct{})
	go func() {
		creators.Wait()
		close(done)
	}()

	loads := 0
	for finished := false; !finished; loads++ {
		select {
		case <-done:
			finished = true
		default:
		}
		loaded, err := store.Load(context.Background(), tenantA)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if int64(len(loaded.Objects)) != loaded.Revision {
			t.Fatalf("load %d: %d objects at revision %d, want one object per revision", loads, len(loaded.Objects), loaded.Revision)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if final := mustLoad(t, store, tenantA); final.Revision != 24 || len(final.Objects) != 24 {
		t.Fatalf("final canvas has %d objects at revision %d, want 24", len(final.Objects), final.Revision)
	}
}

func TestNotFoundNeverRevealsAnotherTenant(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	tenantB := canvas.Key{TenantID: "tenant-b", CanvasID: tenantA.CanvasID}
	missingCanvas := canvas.Key{TenantID: "tenant-c", CanvasID: tenantA.CanvasID}
	mustCreate(t, store, tenantA)
	mustCreate(t, store, tenantB)
	foreign := apply(t, store, tenantB, createText).ObjectID

	for _, mutationFor := range []func(string) canvas.Mutation{
		func(id string) canvas.Mutation { return canvas.SetContent{ObjectID: id, Content: "x"} },
		func(id string) canvas.Mutation {
			return canvas.SetTransform{ObjectID: id, Transform: canvas.Transform{Width: 1, Height: 1}}
		},
		func(id string) canvas.Mutation { return canvas.DeleteObject{ObjectID: id} },
	} {
		errorText := func(key canvas.Key, objectID string) string {
			_, err := store.Apply(context.Background(), key, nextOperation(t), mutationFor(objectID))
			if !errors.Is(err, canvas.ErrNotFound) {
				t.Fatalf("%T on %+v: error = %v, want ErrNotFound", mutationFor(objectID), key, err)
			}
			return err.Error()
		}
		crossTenant := errorText(tenantA, foreign)
		missingObject := errorText(tenantA, "missing-object")
		absentCanvas := errorText(missingCanvas, foreign)
		if crossTenant != missingObject || crossTenant != absentCanvas {
			t.Fatalf("%T not-found texts differ: cross-tenant %q, missing object %q, missing canvas %q", mutationFor(foreign), crossTenant, missingObject, absentCanvas)
		}
		if strings.Contains(crossTenant, foreign) {
			t.Fatalf("%T not-found text %q reveals the object ID", mutationFor(foreign), crossTenant)
		}
	}
}

func TestBoundariesRoundTrip(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	key := canvas.Key{TenantID: strings.Repeat("t", canvas.MaxIdentifierBytes), CanvasID: strings.Repeat("c", canvas.MaxIdentifierBytes)}
	operation := canvas.Operation{ID: strings.Repeat("o", canvas.MaxIdentifierBytes), CorrelationID: strings.Repeat("r", canvas.MaxIdentifierBytes)}
	content := strings.Repeat("😀", canvas.MaxContentBytes/4)
	mustCreate(t, store, key)

	low := canvas.Transform{X: -canvas.MaxCoordinate, Y: -canvas.MaxCoordinate, Width: canvas.MaxObjectSize, Height: 1, Rotation: math.Nextafter(360, 0)}
	created := mustApply(t, store, key, operation, canvas.CreateObject{Kind: canvas.KindText, Content: content, Transform: low})
	want := canvas.Object{ID: created.ObjectID, Kind: canvas.KindText, Content: content, Transform: low, Z: created.Z}
	if got := mustLoad(t, store, key); len(got.Objects) != 1 || !reflect.DeepEqual(got.Objects[0], want) {
		t.Fatalf("boundary object = %+v, want %+v", got.Objects, want)
	}
	var correlation string
	if err := pool.QueryRow(context.Background(), "SELECT correlation_id FROM canvas_operations WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3", key.TenantID, key.CanvasID, operation.ID).Scan(&correlation); err != nil || correlation != operation.CorrelationID {
		t.Fatalf("stored correlation ID = %q (err %v), want the 128-byte ID", correlation, err)
	}

	high := canvas.Transform{X: canvas.MaxCoordinate, Y: canvas.MaxCoordinate, Width: 1, Height: canvas.MaxObjectSize, Rotation: math.Nextafter(360, 0)}
	moved := apply(t, store, key, canvas.SetTransform{ObjectID: created.ObjectID, Transform: high})
	want.Transform, want.Z = high, moved.Z
	if got := mustLoad(t, store, key); len(got.Objects) != 1 || !reflect.DeepEqual(got.Objects[0], want) {
		t.Fatalf("boundary transform = %+v, want %+v", got.Objects, want)
	}

	before := mustLoad(t, store, key)
	oversized := content + "a"
	if _, err := store.Apply(context.Background(), key, nextOperation(t), canvas.SetContent{ObjectID: created.ObjectID, Content: oversized}); !errors.Is(err, canvas.ErrInvalid) {
		t.Fatalf("%d-byte content: error = %v, want ErrInvalid", len(oversized), err)
	}
	if got := mustLoad(t, store, key); !reflect.DeepEqual(got, before) {
		t.Fatalf("oversized content changed the canvas:\n got %+v\nwant %+v", got, before)
	}
}
