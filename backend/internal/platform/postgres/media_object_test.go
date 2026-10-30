package postgres_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

func mediaCanvas(key media.CanvasKey) canvas.Key {
	return canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}
}

// wantMediaCanvas checks the committed canvas revision and objects, bottom first.
func wantMediaCanvas(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey, revision int64, objects ...canvas.Object) {
	t.Helper()
	loaded, err := postgres.NewCanvasStore(pool).Load(context.Background(), mediaCanvas(key))
	if err != nil {
		t.Fatalf("Load(%+v): %v", key, err)
	}
	if loaded.Revision != revision || !slices.Equal(loaded.Objects, objects) {
		t.Fatalf("canvas %+v revision %d objects %+v, want revision %d objects %+v", key, loaded.Revision, loaded.Objects, revision, objects)
	}
}

// objectRecord is one stored canvas mutation outbox record.
type objectRecord struct {
	revision      int64
	operationID   string
	correlationID string
	objectID      string
	z             int64
	mutation      canvas.Mutation
}

// objectRecords returns the canvas mutation records of a canvas, oldest first,
// leaving out media usage records.
func objectRecords(t *testing.T, pool *pgxpool.Pool, key media.CanvasKey) []objectRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT revision, operation_id, COALESCE(correlation_id, ''), kind, mutation, object_id, z
		FROM canvas_outbox WHERE tenant_id = $1 AND canvas_id = $2 AND kind <> 'media_usage' ORDER BY id`,
		key.TenantID, key.CanvasID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var records []objectRecord
	for rows.Next() {
		var record objectRecord
		var kind string
		var payload []byte
		if err := rows.Scan(&record.revision, &record.operationID, &record.correlationID, &kind, &payload, &record.objectID, &record.z); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if record.mutation, err = outbox.DecodeMutation(kind, payload); err != nil {
			t.Fatalf("decode outbox record %s %s: %v", kind, payload, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return records
}

// US-011 S8, S12, S14: an object-creating completion places the image object
// on top of the canvas, bumps the revision once, and writes its create_object
// record under the media operation in the same transaction. Equal retries
// replay the same result without a second object or record, whatever object
// size they carry; a completion without the object conflicts.
func TestMediaCompleteWithObjectCreatesItsObjectAndRecord(t *testing.T) {
	commits := &commitCounter{}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA}, postgres.WithMediaCommitted(commits.record))
	canvases := postgres.NewCanvasStore(pool)
	ctx := context.Background()
	text, err := canvases.Apply(ctx, mediaCanvas(mediaA), canvas.Operation{ID: "text-1", CorrelationID: "corr-text"},
		canvas.CreateObject{Kind: canvas.KindText, Content: "hi", Transform: canvas.Transform{Width: 10, Height: 10}})
	if err != nil {
		t.Fatalf("create text object: %v", err)
	}
	textObject := canvas.Object{ID: text.ObjectID, Kind: canvas.KindText, Content: "hi", Transform: canvas.Transform{Width: 10, Height: 10}, Z: 1}
	reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 70})
	commitsBefore := commits.of(mediaA)

	operation := media.Operation{ID: "upload-1/complete", CorrelationID: "corr-upload-1"}
	command := media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindGIF, Object: true, ObjectWidth: 300, ObjectHeight: 200}
	completed := mustApplyMedia(t, store, mediaA, operation, command)
	want := media.Result{AssetID: reserved.AssetID, ObjectID: reserved.AssetID, Usage: media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 70}}
	if completed != want {
		t.Fatalf("Complete = %+v, want %+v", completed, want)
	}
	if got := commits.of(mediaA); got != commitsBefore+1 {
		t.Fatalf("commit hook ran %d times for the completion, want once", got-commitsBefore)
	}
	placed := canvas.Transform{X: 810, Y: 440, Width: 300, Height: 200}
	object := canvas.Object{ID: reserved.AssetID, Kind: canvas.KindImage, Content: "ref-1", Transform: placed, Z: 2}
	wantMediaCanvas(t, pool, mediaA, 2, textObject, object)
	records := objectRecords(t, pool, mediaA)
	wantRecord := objectRecord{
		revision: 2, operationID: operation.ID, correlationID: operation.CorrelationID, objectID: reserved.AssetID, z: 2,
		mutation: canvas.CreateObject{Kind: canvas.KindImage, Content: "ref-1", Transform: placed},
	}
	if len(records) != 2 || records[1] != wantRecord {
		t.Fatalf("outbox mutation records = %+v, want the text record and %+v", records, wantRecord)
	}
	snapshot := mediaSnapshot(t, pool, mediaA)

	for _, retry := range []media.Complete{command, {AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindGIF, Object: true, ObjectWidth: 1, ObjectHeight: 1}} {
		if again := mustApplyMedia(t, store, mediaA, media.Operation{ID: operation.ID, CorrelationID: "corr-reconnected"}, retry); again != completed {
			t.Fatalf("retry %+v = %+v, want the committed %+v", retry, again, completed)
		}
	}
	for _, conflicting := range []media.Command{
		media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindGIF},
		media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-2", Kind: media.KindGIF, Object: true, ObjectWidth: 300, ObjectHeight: 200},
	} {
		if result, err := store.Apply(ctx, mediaA, operation, conflicting); !errors.Is(err, media.ErrOperationConflict) {
			t.Fatalf("%+v under the used operation = %+v, %v; want ErrOperationConflict", conflicting, result, err)
		}
	}
	if got := mediaSnapshot(t, pool, mediaA); got != snapshot {
		t.Fatalf("retries changed the canvas:\n got %+v\nwant %+v", got, snapshot)
	}
	if got := commits.of(mediaA); got != commitsBefore+1 {
		t.Fatalf("commit hook ran %d times, want once: replays and conflicts commit nothing", got-commitsBefore)
	}
	wantUsage(t, store, pool, mediaA, 70, 0)
}

// US-011 S14: once the completion's operation is no longer retained, the same
// command is a new one, and as for any completion of an asset that is
// already ready, it is rejected without a second object.
func TestMediaCompleteWithObjectAfterRetentionCreatesNothing(t *testing.T) {
	const ttl = time.Minute
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA},
		postgres.WithMediaClock(clock.Now), postgres.WithMediaRetention(ttl), postgres.WithReservationTTL(time.Hour))
	reserved := run(t, store, mediaA, media.Reserve{DeclaredBytes: 40})
	operation := nextMediaOperation()
	command := media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}
	completed := mustApplyMedia(t, store, mediaA, operation, command)
	object := canvas.Object{ID: reserved.AssetID, Kind: canvas.KindImage, Content: "ref-1", Transform: canvas.Transform{X: 952, Y: 532, Width: 16, Height: 16}, Z: 1}

	clock.Set(start.Add(ttl - time.Microsecond))
	if replay := mustApplyMedia(t, store, mediaA, operation, command); replay != completed {
		t.Fatalf("retry just before expiry = %+v, want %+v", replay, completed)
	}
	clock.Set(start.Add(ttl))
	if result, err := store.Apply(context.Background(), mediaA, operation, command); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("retry after expiry = %+v, %v; want ErrNotFound", result, err)
	}
	wantMediaCanvas(t, pool, mediaA, 1, object)
	if records := objectRecords(t, pool, mediaA); len(records) != 1 {
		t.Fatalf("outbox mutation records = %+v, want one", records)
	}
	wantUsage(t, store, pool, mediaA, 40, 0)
}

// US-011 S7, US-016 S9: an object-creating completion of a missing canvas or
// of an inactive tenant writes nothing; an equal retry of one committed
// before the deactivation still replays its object.
func TestMediaCompleteWithObjectNeedsAnActiveCanvas(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, _, key := mediaStreamer(t, s, "media-object@example.com")
	store := postgres.NewMediaStore(s.pool)
	committed := run(t, store, key, media.Reserve{DeclaredBytes: 10})
	operation := nextMediaOperation()
	command := media.Complete{AssetID: committed.AssetID, ReferenceID: "ref-1", Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}
	completed := mustApplyMedia(t, store, key, operation, command)
	pending := run(t, store, key, media.Reserve{DeclaredBytes: 20})
	mustDeactivate(t, s, operator, "op-deactivate-media-object", "media-object@example.com")
	before := mediaSnapshot(t, s.pool, key)

	runErr(t, store, key, media.Complete{AssetID: pending.AssetID, ReferenceID: "ref-2", Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}, media.ErrInactive)
	if replay := mustApplyMedia(t, store, key, operation, command); replay != completed || replay.ObjectID != committed.AssetID {
		t.Fatalf("retry after deactivation = %+v, want the committed %+v", replay, completed)
	}
	if got := mediaSnapshot(t, s.pool, key); got != before {
		t.Fatalf("inactive completion changed the canvas:\n got %+v\nwant %+v", got, before)
	}

	missing := media.CanvasKey{TenantID: key.TenantID, CanvasID: "missing-canvas"}
	runErr(t, store, missing, media.Complete{AssetID: pending.AssetID, ReferenceID: "ref-2", Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}, media.ErrNotFound)
}

// The object-creating completion locks the canvas before the quota while
// every other media command locks the quota and then writes usage records
// that reference the canvas. Interleaving both, and canvas mutations, on one
// canvas must neither deadlock nor lose an effect.
func TestConcurrentObjectCompletionsReservationsAndMutationsCommit(t *testing.T) {
	store, pool := openMedia(t, pgtest.Start(t), []media.CanvasKey{mediaA})
	canvases := postgres.NewCanvasStore(pool)
	ctx := context.Background()
	const workers, rounds = 8, 4
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for range rounds {
				reserved, err := store.Apply(ctx, mediaA, nextMediaOperation(), media.Reserve{DeclaredBytes: 10})
				if err != nil {
					t.Errorf("worker %d Reserve: %v", w, err)
					return
				}
				reference := "ref-" + reserved.AssetID
				if _, err := store.Apply(ctx, mediaA, nextMediaOperation(), media.Complete{AssetID: reserved.AssetID, ReferenceID: reference, Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}); err != nil {
					t.Errorf("worker %d Complete: %v", w, err)
					return
				}
				if _, err := canvases.Apply(ctx, mediaCanvas(mediaA), canvas.Operation{ID: "text-" + reserved.AssetID},
					canvas.CreateObject{Kind: canvas.KindText, Content: "t", Transform: canvas.Transform{Width: 1, Height: 1}}); err != nil {
					t.Errorf("worker %d CreateObject: %v", w, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	loaded, err := canvases.Load(ctx, mediaCanvas(mediaA))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := 2 * workers * rounds; len(loaded.Objects) != want || loaded.Revision != int64(want) {
		t.Fatalf("canvas has %d objects at revision %d, want %d of each", len(loaded.Objects), loaded.Revision, want)
	}
	if records := objectRecords(t, pool, mediaA); len(records) != 2*workers*rounds {
		t.Fatalf("outbox has %d mutation records, want %d", len(records), 2*workers*rounds)
	}
	wantUsage(t, store, pool, mediaA, 10*workers*rounds, 0)
}
