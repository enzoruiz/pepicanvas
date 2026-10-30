package telemetry_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

func TestCloseAfterADrainedCloseAlwaysReturnsNil(t *testing.T) {
	rec := &recorder{}
	tracer := newTracer(t, rec)
	ctx := telemetry.WithTracer(context.Background(), tracer)
	_, span := telemetry.Start(ctx, telemetry.SpanHTTPRequest)
	span.End(nil)
	closeTracer(t, tracer)

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 200 {
		if err := tracer.Close(ended); err != nil {
			t.Fatalf("Close #%d with an ended context after a drained Close = %v, want nil", i+2, err)
		}
	}
	if got := tracer.Stats(); got != (telemetry.Stats{Exported: 1}) {
		t.Fatalf("Stats = %+v, want 1 exported", got)
	}
}

// logRecords decodes JSON log lines whose message is msg.
func logRecords(t *testing.T, logs *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if record["msg"] == msg {
			records = append(records, record)
		}
	}
	return records
}

func TestDroppedSpansAreLoggedOncePerIntervalWithCountsOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		exporter := &stuck{entered: make(chan struct{})}
		tracer := newTracer(t, exporter, func(c *telemetry.Config) {
			c.QueueSize = 1
			c.BatchSize = 1
			c.Interval = time.Second
			c.ExportTimeout = time.Second
			c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		})
		defer tracer.Close(context.Background()) // ends the bubble's worker if a check fails
		ctx := telemetry.WithTracer(context.Background(), tracer)
		end := func() {
			_, span := telemetry.Start(ctx, telemetry.SpanSubprocess, telemetry.Attr("operation", "ffprobe"))
			span.End(nil)
		}
		end()
		<-exporter.entered // the worker holds the first span
		for range 4 {
			end() // one queued, three dropped
		}
		if got := tracer.Stats().Dropped; got != 3 {
			t.Fatalf("Dropped = %d, want 3", got)
		}

		time.Sleep(5 * time.Second)
		synctest.Wait()
		records := logRecords(t, &logs, "telemetry spans dropped")
		if len(records) != 1 {
			t.Fatalf("%d drop records, want 1; logs = %s", len(records), logs.String())
		}
		keys := make([]string, 0, len(records[0]))
		for k := range records[0] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"dropped_total", "level", "msg", "spans", "time"}) ||
			records[0]["level"] != "WARN" || records[0]["spans"] != float64(3) || records[0]["dropped_total"] != float64(3) {
			t.Fatalf("drop record = %v, want WARN with spans 3 and dropped_total 3 only", records[0])
		}

		time.Sleep(5 * time.Second) // no new drop, no new record
		synctest.Wait()
		closeTracer(t, tracer)
		if got := len(logRecords(t, &logs, "telemetry spans dropped")); got != 1 {
			t.Fatalf("%d drop records after quiet intervals, want 1", got)
		}
		if strings.Contains(logs.String(), "ffprobe") || strings.Contains(logs.String(), "subprocess") {
			t.Fatalf("logs echo span content: %s", logs.String())
		}
		if got := tracer.Stats(); got.Dropped != 3 || got.Failed != 2 || got.Exported != 0 {
			t.Fatalf("Stats = %+v, want 3 dropped, 2 failed", got)
		}
	})
}
