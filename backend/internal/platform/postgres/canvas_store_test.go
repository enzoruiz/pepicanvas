package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

var tenantA = canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"}

// openStore connects to the current server address and migrates it.
func openStore(t *testing.T, server *pgtest.Server) (*postgres.CanvasStore, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return postgres.NewCanvasStore(pool), pool
}

func mustCreate(t *testing.T, store canvas.Store, key canvas.Key) canvas.Canvas {
	t.Helper()
	created, err := store.Create(context.Background(), key)
	if err != nil {
		t.Fatalf("Create(%+v): %v", key, err)
	}
	return created
}

func mustLoad(t *testing.T, store canvas.Store, key canvas.Key) canvas.Canvas {
	t.Helper()
	loaded, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load(%+v): %v", key, err)
	}
	return loaded
}

var operationSeq atomic.Int64

// apply commits a mutation under a fresh operation ID.
func apply(t *testing.T, store canvas.Store, key canvas.Key, mutation canvas.Mutation) canvas.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, nextOperation(t), mutation)
	if err != nil {
		t.Fatalf("Apply(%T): %v", mutation, err)
	}
	return result
}

func nextOperation(t *testing.T) canvas.Operation {
	t.Helper()
	return canvas.Operation{ID: fmt.Sprintf("op-%d", operationSeq.Add(1)), CorrelationID: "0123456789abcdef0123456789abcdef"}
}

func TestCreateCanvasUsesDefaultsAndIsIdempotent(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))

	var wg sync.WaitGroup
	results := make([]canvas.Canvas, 8)
	for i := range results {
		wg.Go(func() {
			created, err := store.Create(context.Background(), tenantA)
			if err != nil {
				t.Errorf("concurrent Create: %v", err)
			}
			results[i] = created
		})
	}
	wg.Wait()

	want := canvas.Canvas{Key: tenantA, Dimensions: canvas.DefaultDimensions, Revision: 0, Objects: []canvas.Object{}}
	for i, got := range results {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent Create %d = %+v, want %+v", i, got, want)
		}
	}
	apply(t, store, tenantA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}})
	if again := mustCreate(t, store, tenantA); again.Dimensions != (canvas.Dimensions{Width: 640, Height: 360}) || again.Revision != 1 {
		t.Fatalf("repeated Create reset the canvas: %+v", again)
	}
}

func TestEveryMutationKindSurvivesRestart(t *testing.T) {
	server := pgtest.Start(t)
	store, _ := openStore(t, server)
	mustCreate(t, store, tenantA)
	text := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: "hello\nworld", Transform: canvas.Transform{X: -40, Y: 25, Width: 300, Height: 80, Rotation: 12.345678901234567}})
	image := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindImage, Content: "asset-1", Transform: canvas.Transform{X: 5000, Y: 3000, Width: 64, Height: 64}})

	steps := []struct {
		name     string
		mutation canvas.Mutation
	}{
		{"dimensions", canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320, Height: 2160}}},
		{"content", canvas.SetContent{ObjectID: text.ObjectID, Content: "edited\n\nlines"}},
		{"transform and order", canvas.SetTransform{ObjectID: text.ObjectID, Transform: canvas.Transform{X: 7, Y: -9, Width: 120, Height: 40, Rotation: 359.999}}},
		{"create", canvas.CreateObject{Kind: canvas.KindAudio, Content: "asset-2", Transform: canvas.Transform{Width: 1, Height: 1}}},
		{"delete", canvas.DeleteObject{ObjectID: image.ObjectID}},
	}
	for _, step := range steps {
		apply(t, store, tenantA, step.mutation)
		want := mustLoad(t, store, tenantA)

		server.Restart(t)
		store, _ = openStore(t, server)

		if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, want) {
			t.Fatalf("after %s and restart:\n got %+v\nwant %+v", step.name, got, want)
		}
	}

	final := mustLoad(t, store, tenantA)
	if final.Revision != 7 || final.Dimensions != (canvas.Dimensions{Width: 320, Height: 2160}) {
		t.Fatalf("final canvas = %+v, want revision 7 and 320x2160", final)
	}
	if ids := objectIDs(final); len(ids) != 2 || ids[0] != text.ObjectID {
		t.Fatalf("final order = %v, want the audio object created last above %s", ids, text.ObjectID)
	}
}

func TestDimensionsAndContentPreserveObjectsAndOrder(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	inside := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindImage, Content: "a", Transform: canvas.Transform{X: 10, Y: 10, Width: 100, Height: 100, Rotation: 30}})
	partly := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindImage, Content: "b", Transform: canvas.Transform{X: 1800, Y: 1000, Width: 400, Height: 400}})
	outside := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindVideo, Content: "c", Transform: canvas.Transform{X: 3000, Y: 2000, Width: 50, Height: 50}})
	before := mustLoad(t, store, tenantA).Objects

	apply(t, store, tenantA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 320, Height: 180}})
	apply(t, store, tenantA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3840, Height: 2160}})
	if got := mustLoad(t, store, tenantA).Objects; !reflect.DeepEqual(got, before) {
		t.Fatalf("resizing changed objects:\n got %+v\nwant %+v", got, before)
	}

	apply(t, store, tenantA, canvas.SetContent{ObjectID: inside.ObjectID, Content: "a2"})
	after := mustLoad(t, store, tenantA)
	if ids := objectIDs(after); !reflect.DeepEqual(ids, []string{inside.ObjectID, partly.ObjectID, outside.ObjectID}) {
		t.Fatalf("content edit changed order: %v", ids)
	}
	if after.Objects[0].Content != "a2" || after.Objects[0].Transform != before[0].Transform {
		t.Fatalf("content edit = %+v, want content a2 and unchanged transform", after.Objects[0])
	}
}

func TestTransformRaisesObjectToTop(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	var ids []string
	for i := range 3 {
		ids = append(ids, apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: fmt.Sprint(i), Transform: canvas.Transform{Width: 10, Height: 10}}).ObjectID)
	}
	moved := canvas.Transform{X: 1, Y: 2, Width: 30, Height: 40, Rotation: 90}
	result := apply(t, store, tenantA, canvas.SetTransform{ObjectID: ids[0], Transform: moved})

	got := mustLoad(t, store, tenantA)
	if order := objectIDs(got); !reflect.DeepEqual(order, []string{ids[1], ids[2], ids[0]}) {
		t.Fatalf("order after transform = %v, want %v on top", order, ids[0])
	}
	top := got.Objects[2]
	if top.Transform != moved || top.Z != result.Z || result.ObjectID != ids[0] || result.Revision != got.Revision {
		t.Fatalf("top object = %+v, result = %+v, revision %d", top, result, got.Revision)
	}
}

func TestConcurrentDimensionsAndZOrderFollowCommitOrder(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	var objects []string
	for i := range 6 {
		objects = append(objects, apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: fmt.Sprint(i), Transform: canvas.Transform{Width: 10, Height: 10}}).ObjectID)
	}
	base := mustLoad(t, store, tenantA).Revision

	type committed struct {
		result     canvas.Result
		dimensions canvas.Dimensions
		transform  bool
	}
	var (
		wg     sync.WaitGroup
		lock   sync.Mutex
		commit []committed
	)
	for i := range 24 {
		wg.Go(func() {
			var mutation canvas.Mutation
			entry := committed{}
			if i%2 == 0 {
				entry.dimensions = canvas.Dimensions{Width: 320 + i, Height: 180 + i}
				mutation = canvas.SetDimensions{Dimensions: entry.dimensions}
			} else {
				entry.transform = true
				mutation = canvas.SetTransform{ObjectID: objects[i%len(objects)], Transform: canvas.Transform{X: i, Width: 10, Height: 10}}
			}
			result, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation)
			if err != nil {
				t.Errorf("concurrent Apply(%T): %v", mutation, err)
				return
			}
			entry.result = result
			lock.Lock()
			commit = append(commit, entry)
			lock.Unlock()
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	sort.Slice(commit, func(a, b int) bool { return commit[a].result.Revision < commit[b].result.Revision })

	got := mustLoad(t, store, tenantA)
	if got.Revision != base+24 {
		t.Fatalf("revision = %d, want %d", got.Revision, base+24)
	}
	var lastDimensions canvas.Dimensions
	lastZ := map[string]int64{}
	var previousZ int64
	for i, entry := range commit {
		if entry.result.Revision != base+int64(i)+1 {
			t.Fatalf("revisions are not gapless: %d at position %d", entry.result.Revision, i)
		}
		if entry.transform {
			if entry.result.Z <= previousZ {
				t.Fatalf("z %d does not follow commit order after %d", entry.result.Z, previousZ)
			}
			previousZ = entry.result.Z
			lastZ[entry.result.ObjectID] = entry.result.Z
		} else {
			lastDimensions = entry.dimensions
		}
	}
	if got.Dimensions != lastDimensions {
		t.Fatalf("dimensions = %+v, want last committed %+v", got.Dimensions, lastDimensions)
	}
	seen := map[int64]bool{}
	for _, object := range got.Objects {
		if seen[object.Z] {
			t.Fatalf("z %d assigned twice", object.Z)
		}
		seen[object.Z] = true
		if z, ok := lastZ[object.ID]; ok && object.Z != z {
			t.Fatalf("object %s z = %d, want its last committed %d", object.ID, object.Z, z)
		}
	}
}

func TestDeleteLeavesNoRecoverableState(t *testing.T) {
	store, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, store, tenantA)
	kept := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: "kept", Transform: canvas.Transform{Width: 1, Height: 1}})
	gone := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: "gone", Transform: canvas.Transform{Width: 1, Height: 1}})
	apply(t, store, tenantA, canvas.DeleteObject{ObjectID: gone.ObjectID})
	after := mustLoad(t, store, tenantA)

	if ids := objectIDs(after); !reflect.DeepEqual(ids, []string{kept.ObjectID}) {
		t.Fatalf("objects after delete = %v, want only %s", ids, kept.ObjectID)
	}
	for _, mutation := range []canvas.Mutation{
		canvas.SetContent{ObjectID: gone.ObjectID, Content: "back"},
		canvas.SetTransform{ObjectID: gone.ObjectID, Transform: canvas.Transform{Width: 1, Height: 1}},
		canvas.DeleteObject{ObjectID: gone.ObjectID},
	} {
		if _, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation); !errors.Is(err, canvas.ErrNotFound) {
			t.Fatalf("%T on deleted object: error = %v, want ErrNotFound", mutation, err)
		}
	}
	if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, after) {
		t.Fatalf("rejected mutations changed the canvas:\n got %+v\nwant %+v", got, after)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM canvas_objects WHERE object_id = $1", gone.ObjectID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("deleted object rows = %d (err %v), want 0", rows, err)
	}
}

func TestTenantsNeverReadOrMutateEachOther(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	tenantB := canvas.Key{TenantID: "tenant-b", CanvasID: tenantA.CanvasID}
	mustCreate(t, store, tenantA)
	mustCreate(t, store, tenantB)
	objectA := apply(t, store, tenantA, canvas.CreateObject{Kind: canvas.KindText, Content: "a", Transform: canvas.Transform{Width: 1, Height: 1}})
	objectB := apply(t, store, tenantB, canvas.CreateObject{Kind: canvas.KindText, Content: "b", Transform: canvas.Transform{Width: 1, Height: 1}})
	apply(t, store, tenantA, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1000, Height: 1000}})
	wantB := mustLoad(t, store, tenantB)

	for _, mutation := range []canvas.Mutation{
		canvas.SetContent{ObjectID: objectB.ObjectID, Content: "stolen"},
		canvas.SetTransform{ObjectID: objectB.ObjectID, Transform: canvas.Transform{Width: 1, Height: 1}},
		canvas.DeleteObject{ObjectID: objectB.ObjectID},
	} {
		if _, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation); !errors.Is(err, canvas.ErrNotFound) {
			t.Fatalf("cross-tenant %T: error = %v, want ErrNotFound", mutation, err)
		}
	}
	if got := mustLoad(t, store, tenantB); !reflect.DeepEqual(got, wantB) || got.Dimensions != canvas.DefaultDimensions {
		t.Fatalf("tenant B changed:\n got %+v\nwant %+v", got, wantB)
	}
	if ids := objectIDs(mustLoad(t, store, tenantA)); !reflect.DeepEqual(ids, []string{objectA.ObjectID}) {
		t.Fatalf("tenant A objects = %v, want only its own", ids)
	}

	missing := canvas.Key{TenantID: "tenant-c", CanvasID: tenantA.CanvasID}
	if _, err := store.Load(context.Background(), missing); !errors.Is(err, canvas.ErrNotFound) {
		t.Fatalf("Load of a missing canvas: error = %v, want ErrNotFound", err)
	}
	if _, err := store.Apply(context.Background(), missing, nextOperation(t), canvas.SetDimensions{Dimensions: canvas.DefaultDimensions}); !errors.Is(err, canvas.ErrNotFound) {
		t.Fatalf("Apply to a missing canvas: error = %v, want ErrNotFound", err)
	}
}

func TestInvalidMutationLeavesCanvasUnchanged(t *testing.T) {
	store, _ := openStore(t, pgtest.Start(t))
	want := mustCreate(t, store, tenantA)
	for _, mutation := range []canvas.Mutation{
		canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 319, Height: 180}},
		canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 3840, Height: 2161}},
		canvas.CreateObject{Kind: canvas.KindText, Content: "a\x00", Transform: canvas.Transform{Width: 1, Height: 1}},
	} {
		if _, err := store.Apply(context.Background(), tenantA, nextOperation(t), mutation); !errors.Is(err, canvas.ErrInvalid) {
			t.Fatalf("%+v: error = %v, want ErrInvalid", mutation, err)
		}
	}
	if got := mustLoad(t, store, tenantA); !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid mutations changed the canvas: %+v", got)
	}
}

func objectIDs(c canvas.Canvas) []string {
	ids := make([]string, 0, len(c.Objects))
	for _, object := range c.Objects {
		ids = append(ids, object.ID)
	}
	return ids
}
