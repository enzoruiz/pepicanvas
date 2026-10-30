package outbox_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

func mediaKey(key canvas.Key) media.CanvasKey {
	return media.CanvasKey{TenantID: key.TenantID, CanvasID: key.CanvasID}
}

func mustApplyMedia(t *testing.T, store media.Store, key canvas.Key, operation media.Operation, command media.Command) media.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), mediaKey(key), operation, command)
	if err != nil {
		t.Fatalf("Apply(%#v): %v", command, err)
	}
	return result
}

// usageEvents splits hub events into canvas events and decoded usage events,
// each in delivery order.
func usageEvents(t *testing.T, events []realtime.Event) ([]realtime.Event, []outbox.MediaUsage, []string) {
	t.Helper()
	var canvasEvents []realtime.Event
	var usages []outbox.MediaUsage
	var correlations []string
	for _, event := range events {
		if event.Kind != realtime.KindUsage {
			canvasEvents = append(canvasEvents, event)
			continue
		}
		var usage outbox.MediaUsage
		if err := json.Unmarshal(event.Payload, &usage); err != nil {
			t.Fatalf("usage event %s: %v", event.Payload, err)
		}
		if event.ObjectID != outbox.MediaUsageObjectID {
			t.Fatalf("usage event object %q", event.ObjectID)
		}
		usages = append(usages, usage)
		correlations = append(correlations, event.CorrelationID)
	}
	return canvasEvents, usages, correlations
}

func committedUsage(version int64, usage media.Usage) outbox.MediaUsage {
	return outbox.MediaUsage{Version: version, QuotaBytes: usage.QuotaBytes, UsedBytes: usage.UsedBytes, ReservedBytes: usage.ReservedBytes}
}

func TestMediaUsageCommittedBeforeACrashIsDeliveredOnceAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	key := canvasA

	// The crashed process committed canvas mutations and media commands
	// interleaved, and its publisher closed before delivering any of them.
	store, events, pool := openPG(t, server)
	mediaStore := postgres.NewMediaStore(pool)
	crashed := newHub(t, realtime.Config{Authorize: allowAll})
	stopped, err := outbox.NewPublisher(outbox.Config{Store: events, Sink: outbox.NewHubSink(submitOnly{crashed})})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	created := mustCommit(t, store, key, canvas.Operation{ID: "op-1", CorrelationID: "corr-1"},
		canvas.CreateObject{Kind: canvas.KindText, Content: "hi", Transform: canvas.Transform{Width: 10, Height: 10}})
	reserved := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "mop-1", CorrelationID: "corr-media-1"}, media.Reserve{DeclaredBytes: 4000})
	mustCommit(t, store, key, canvas.Operation{ID: "op-2", CorrelationID: "corr-2"}, canvas.SetContent{ObjectID: created.ObjectID, Content: "again"})
	completed := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "mop-2", CorrelationID: "corr-media-2"},
		media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindImage})
	if err := stopped.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := hubEvents(t, crashed, key); len(got) != 0 {
		t.Fatalf("hub confirmed %+v before publication", got)
	}
	if stats := mustStats(t, events, key.TenantID); stats.Pending != 4 {
		t.Fatalf("Stats before restart = %+v, want 4 pending, media records included", stats)
	}
	var mediaRecords []outbox.Record
	for _, record := range mustPendingRecords(t, events, key) {
		if record.Usage != nil {
			mediaRecords = append(mediaRecords, record)
		}
	}
	if len(mediaRecords) != 2 {
		t.Fatalf("pending media records = %+v, want 2", mediaRecords)
	}
	pool.Close()

	server.Restart(t)
	_, events, pool = openPG(t, server)
	commands := &seen{}
	hub := newHub(t, realtime.Config{Authorize: commands.authorize})
	actor := &answers{hub: hub}
	sink := outbox.NewHubSink(actor)
	publisher := start(t, outbox.Config{Store: events, Sink: sink, PollInterval: 10 * time.Millisecond})
	eventually(t, "every record to settle and be counted", func() bool {
		return mustStats(t, events, key.TenantID).Pending == 0 && publisher.Counters().Delivered == 4
	})

	canvasEvents, usages, correlations := usageEvents(t, hubEvents(t, hub, key))
	assertDeliveredInOrder(t, canvasEvents, 1, 2)
	want := []outbox.MediaUsage{committedUsage(1, reserved.Usage), committedUsage(2, completed.Usage)}
	if len(usages) != 2 || usages[0] != want[0] || usages[1] != want[1] {
		t.Fatalf("delivered usages = %+v, want %+v", usages, want)
	}
	if correlations[0] != "corr-media-1" || correlations[1] != "corr-media-2" {
		t.Fatalf("usage correlations = %v", correlations)
	}
	if got := len(commands.list()); got != 4 {
		t.Fatalf("hub saw %d commands, want each record exactly once", got)
	}
	current, err := postgres.NewMediaStore(pool).Usage(ctx, mediaKey(key))
	if err != nil || committedUsage(2, current) != usages[1] {
		t.Fatalf("committed usage %+v (%v), last delivered %+v", current, err, usages[1])
	}

	// A redelivery of a settled record answers the same result once.
	first := actor.list()
	if err := sink.Deliver(ctx, mediaRecords[1]); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	again := actor.list()
	if len(again) != len(first)+1 || again[len(again)-1] != first[len(first)-1] {
		t.Fatalf("redelivery answer %+v, want %+v", again[len(again)-1], first[len(first)-1])
	}
	if got := len(hubEvents(t, hub, key)); got != 4 {
		t.Fatalf("hub has %d events after the redelivery, want 4", got)
	}
}

func TestMediaCommandFailedBeforeCommitNeverReachesTheHub(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	key := canvasA
	var publisher *outbox.Publisher
	store, events, pool := openPG(t, server)
	mediaStore := postgres.NewMediaStore(pool, postgres.WithMediaCommitted(func(media.CanvasKey) { publisher.Notify() }))
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher = start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A deferred constraint trigger fails the transaction at COMMIT, after
	// the usage change and its outbox record were written.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_outbox_commit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_outbox_commit AFTER INSERT ON canvas_outbox
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_outbox_commit();`); err != nil {
		t.Fatalf("install commit fault: %v", err)
	}
	if _, err := mediaStore.Apply(ctx, mediaKey(key), media.Operation{ID: "mop-lost", CorrelationID: "corr-lost"}, media.Reserve{DeclaredBytes: 900}); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("Apply with a commit fault = %v, want the injected failure", err)
	}
	if pending := mustPendingRecords(t, events, key); len(pending) != 0 {
		t.Fatalf("Pending after a failed commit = %+v", pending)
	}
	if usage, err := mediaStore.Usage(ctx, mediaKey(key)); err != nil || usage.ReservedBytes != 0 || usage.UsedBytes != 0 {
		t.Fatalf("usage after a failed commit = %+v, %v", usage, err)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_outbox_commit ON canvas_outbox`); err != nil {
		t.Fatalf("remove commit fault: %v", err)
	}
	kept := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "mop-kept", CorrelationID: "corr-kept"}, media.Reserve{DeclaredBytes: 300})
	eventually(t, "the committed record to settle", func() bool { return mustStats(t, events, key.TenantID).Pending == 0 })
	canvasEvents, usages, correlations := usageEvents(t, hubEvents(t, hub, key))
	if len(canvasEvents) != 0 || len(usages) != 1 || usages[0] != committedUsage(1, kept.Usage) || correlations[0] != "corr-kept" {
		t.Fatalf("hub received canvas %+v usages %+v (%v), want only the committed usage %+v", canvasEvents, usages, correlations, kept.Usage)
	}
}

// US-011 S12 (CTRL-CONS-01): an image upload's completion commits the asset,
// its image object, and both outbox records in one transaction. Committed
// before a crash and before any publication, it reaches the hub exactly once
// after the restart, as one create_object event for the asset's object and
// one usage event.
func TestImageObjectCommittedBeforeACrashIsDeliveredOnceAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	key := canvasA

	store, events, pool := openPG(t, server)
	mediaStore := postgres.NewMediaStore(pool)
	crashed := newHub(t, realtime.Config{Authorize: allowAll})
	stopped, err := outbox.NewPublisher(outbox.Config{Store: events, Sink: outbox.NewHubSink(submitOnly{crashed})})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	reserved := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "upload-1/reserve", CorrelationID: "corr-upload"}, media.Reserve{DeclaredBytes: 4000})
	completed := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "upload-1/complete", CorrelationID: "corr-upload"},
		media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindGIF, Object: true, ObjectWidth: 300, ObjectHeight: 200})
	if completed.ObjectID != reserved.AssetID {
		t.Fatalf("Complete = %+v, want the asset's object", completed)
	}
	if err := stopped.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := hubEvents(t, crashed, key); len(got) != 0 {
		t.Fatalf("hub confirmed %+v before publication", got)
	}
	if stats := mustStats(t, events, key.TenantID); stats.Pending != 3 {
		t.Fatalf("Stats before restart = %+v, want 3 pending: two usages and the object", stats)
	}
	pool.Close()

	server.Restart(t)
	canvases, events, _ := openPG(t, server)
	commands := &seen{}
	hub := newHub(t, realtime.Config{Authorize: commands.authorize})
	publisher := start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	eventually(t, "every record to settle and be counted", func() bool {
		return mustStats(t, events, key.TenantID).Pending == 0 && publisher.Counters().Delivered == 3
	})

	canvasEvents, usages, _ := usageEvents(t, hubEvents(t, hub, key))
	assertDeliveredInOrder(t, canvasEvents, 1, 1)
	if event := canvasEvents[0]; event.ObjectID != reserved.AssetID || event.CorrelationID != "corr-upload" {
		t.Fatalf("object event = %+v, want object %s with the upload's correlation", event, reserved.AssetID)
	}
	if len(usages) != 2 || usages[1] != committedUsage(2, completed.Usage) {
		t.Fatalf("delivered usages = %+v, want the reservation and %+v", usages, completed.Usage)
	}
	if got := len(commands.list()); got != 3 {
		t.Fatalf("hub saw %d commands, want each record exactly once", got)
	}
	loaded, err := canvases.Load(ctx, key)
	if err != nil || loaded.Revision != 1 || len(loaded.Objects) != 1 || loaded.Objects[0].ID != reserved.AssetID {
		t.Fatalf("canvas after restart = %+v, %v; want the one image object", loaded, err)
	}
}

// US-011 S7, S12: an image completion that fails at COMMIT, after its object
// and records were written, leaves no asset, object, revision, or record, and
// the hub never confirms the object.
func TestImageObjectFailedAtCommitNeverReachesTheHub(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	key := canvasA
	var publisher *outbox.Publisher
	store, events, pool := openPG(t, server)
	mediaStore := postgres.NewMediaStore(pool, postgres.WithMediaCommitted(func(media.CanvasKey) { publisher.Notify() }))
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher = start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	if _, err := store.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	reserved := mustApplyMedia(t, mediaStore, key, media.Operation{ID: "upload-1/reserve"}, media.Reserve{DeclaredBytes: 900})
	eventually(t, "the reservation to settle", func() bool { return mustStats(t, events, key.TenantID).Pending == 0 })

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_object_commit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_object_commit AFTER INSERT ON canvas_objects
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_object_commit();`); err != nil {
		t.Fatalf("install commit fault: %v", err)
	}
	complete := media.Complete{AssetID: reserved.AssetID, ReferenceID: "ref-1", Kind: media.KindImage, Object: true, ObjectWidth: 16, ObjectHeight: 16}
	if _, err := mediaStore.Apply(ctx, mediaKey(key), media.Operation{ID: "upload-1/complete"}, complete); err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("Complete with a commit fault = %v, want the injected failure", err)
	}
	if pending := mustPendingRecords(t, events, key); len(pending) != 0 {
		t.Fatalf("Pending after a failed commit = %+v", pending)
	}
	if loaded, err := store.Load(ctx, key); err != nil || loaded.Revision != 0 || len(loaded.Objects) != 0 {
		t.Fatalf("canvas after a failed commit = %+v, %v; want no object and no revision", loaded, err)
	}
	if _, err := mediaStore.Ready(ctx, mediaKey(key), reserved.AssetID); err == nil {
		t.Fatal("asset is ready after a failed commit")
	}
	if usage, err := mediaStore.Usage(ctx, mediaKey(key)); err != nil || usage.UsedBytes != 0 || usage.ReservedBytes != 900 {
		t.Fatalf("usage after a failed commit = %+v, %v; want only the reservation", usage, err)
	}
	time.Sleep(50 * time.Millisecond)
	canvasEvents, usages, _ := usageEvents(t, hubEvents(t, hub, key))
	if len(canvasEvents) != 0 || len(usages) != 1 {
		t.Fatalf("hub received canvas %+v usages %+v, want only the reservation's usage", canvasEvents, usages)
	}
}
