package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

func usageRecord(id int64, key canvas.Key, usage outbox.MediaUsage, correlationID string) outbox.Record {
	return outbox.Record{ID: id, Event: resync.Event{Key: key, CorrelationID: correlationID}, Usage: &usage}
}

// jsonFields returns the sorted top-level field names of a JSON object.
func jsonFields(t *testing.T, payload []byte) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func TestMediaUsageCodecCarriesOnlyTheAggregateUsage(t *testing.T) {
	usage := outbox.MediaUsage{Version: 3, QuotaBytes: 1_000_000_000, UsedBytes: 700, ReservedBytes: 300}
	payload, err := outbox.EncodeMediaUsage(usage)
	if err != nil {
		t.Fatalf("EncodeMediaUsage: %v", err)
	}
	if want := `{"version":3,"quota_bytes":1000000000,"used_bytes":700,"reserved_bytes":300}`; string(payload) != want {
		t.Fatalf("payload = %s, want %s", payload, want)
	}
	decoded, err := outbox.DecodeMediaUsage(payload)
	if err != nil || decoded != usage {
		t.Fatalf("DecodeMediaUsage = %+v, %v; want %+v", decoded, err, usage)
	}
}

func TestMediaUsageCodecRejectsMalformedUsage(t *testing.T) {
	for name, usage := range map[string]outbox.MediaUsage{
		"no version":      {QuotaBytes: 10},
		"negative used":   {Version: 1, QuotaBytes: 10, UsedBytes: -1},
		"over the quota":  {Version: 1, QuotaBytes: 10, UsedBytes: 6, ReservedBytes: 5},
		"negative quota":  {Version: 1, QuotaBytes: -1},
		"negative reserv": {Version: 1, QuotaBytes: 10, ReservedBytes: -1},
	} {
		if _, err := outbox.EncodeMediaUsage(usage); !errors.Is(err, outbox.ErrMalformed) {
			t.Fatalf("%s: EncodeMediaUsage = %v, want ErrMalformed", name, err)
		}
	}
	for name, payload := range map[string]string{
		"asset field":    `{"version":1,"quota_bytes":10,"used_bytes":0,"reserved_bytes":0,"asset_id":"a"}`,
		"over the quota": `{"version":1,"quota_bytes":10,"used_bytes":11,"reserved_bytes":0}`,
		"trailing data":  `{"version":1,"quota_bytes":10,"used_bytes":0,"reserved_bytes":0} {}`,
		"not json":       `nope`,
	} {
		if _, err := outbox.DecodeMediaUsage([]byte(payload)); !errors.Is(err, outbox.ErrMalformed) {
			t.Fatalf("%s: DecodeMediaUsage = %v, want ErrMalformed", name, err)
		}
	}
}

func TestHubSinkSubmitsAMediaUsageRecordAsAUsageCommand(t *testing.T) {
	commands := &seen{}
	hub := newHub(t, realtime.Config{Authorize: commands.authorize})
	sink := outbox.NewHubSink(hub)
	usage := outbox.MediaUsage{Version: 2, QuotaBytes: 1_000_000_000, UsedBytes: 5, ReservedBytes: 7}
	if err := sink.Deliver(context.Background(), usageRecord(41, canvasA, usage, "corr-9")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	want := realtime.Command{
		Key:         realtime.Key{TenantID: canvasA.TenantID, CanvasID: canvasA.CanvasID},
		OperationID: "outbox-41", ObjectID: outbox.MediaUsageObjectID, Kind: realtime.KindUsage, CorrelationID: "corr-9",
		Payload: []byte(`{"version":2,"quota_bytes":1000000000,"used_bytes":5,"reserved_bytes":7}`),
	}
	if got := commands.list(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("hub saw %+v, want %+v", got, want)
	}
	if fields := jsonFields(t, want.Payload); !reflect.DeepEqual(fields, []string{"quota_bytes", "reserved_bytes", "used_bytes", "version"}) {
		t.Fatalf("payload fields = %v", fields)
	}

	// Redelivery answers the same result with a single effect.
	if err := sink.Deliver(context.Background(), usageRecord(41, canvasA, usage, "corr-9")); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	events := hubEvents(t, hub, canvasA)
	if len(events) != 1 || events[0].Kind != realtime.KindUsage || events[0].OperationID != "outbox-41" {
		t.Fatalf("events after redelivery = %+v, want one usage event", events)
	}
}

func TestHubSinkRejectsAMalformedMediaUsageRecordAsUndeliverable(t *testing.T) {
	hub := newHub(t, realtime.Config{Authorize: allowAll})
	bad := outbox.MediaUsage{Version: 0, QuotaBytes: 10}
	if err := outbox.NewHubSink(hub).Deliver(context.Background(), usageRecord(1, canvasA, bad, "")); !errors.Is(err, outbox.ErrUndeliverable) {
		t.Fatalf("Deliver = %v, want ErrUndeliverable", err)
	}
	if events := hubEvents(t, hub, canvasA); len(events) != 0 {
		t.Fatalf("hub events = %+v, want none", events)
	}
}
