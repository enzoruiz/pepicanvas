package mediaexec

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/tracing/tracingtest"
)

const inspectTrace = correlation.ID("fedcba9876543210fedcba9876543210")

// S1, L9: an inspection runs in a media.inspect span of the request's trace
// and each contained command in a subprocess span inside it, named by the
// configured tool and the closed outcome only.
func TestInspectRecordsInspectAndSubprocessSpans(t *testing.T) {
	cases := []struct {
		name        string
		failDecode  bool
		wantInspect map[string]string
		wantStatus  string
		wantRuns    []map[string]string
		wantRunStat []string
	}{
		{
			name:        "accepted image",
			wantInspect: map[string]string{"outcome": "ok"},
			wantStatus:  "ok",
			wantRuns:    []map[string]string{{"operation": "ffprobe", "outcome": "ok"}, {"operation": "ffmpeg", "outcome": "ok"}},
			wantRunStat: []string{"ok", "ok"},
		},
		{
			name:        "failed decode",
			failDecode:  true,
			wantInspect: map[string]string{"outcome": string(ErrorCommandFailed)},
			wantStatus:  "error",
			wantRuns:    []map[string]string{{"operation": "ffprobe", "outcome": "ok"}, {"operation": "ffmpeg", "outcome": string(ErrorCommandFailed)}},
			wantRunStat: []string{"ok", "error"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config := testConfig(t)
			var runnerTrace correlation.ID
			runner := runnerFunc(func(ctx context.Context, command Command, stdout, _ io.Writer) error {
				runnerTrace, _ = correlation.FromContext(ctx)
				switch command.Path {
				case config.FFprobePath:
					_, _ = io.WriteString(stdout, imageProbe())
				case config.FFmpegPath:
					if c.failDecode {
						return errors.New("decode failed")
					}
				}
				return nil
			})
			adapter, err := New(config, runner)
			if err != nil {
				t.Fatal(err)
			}
			h := tracingtest.Start(t)
			ctx, _ := telemetry.Start(h.Context(correlation.WithID(context.Background(), inspectTrace)), telemetry.SpanHTTPRequest)
			parent, _ := telemetry.SpanContextFrom(ctx)
			_, err = adapter.Inspect(ctx, strings.NewReader("x"))
			if (err != nil) != c.failDecode {
				t.Fatalf("Inspect error = %v, want failure %v", err, c.failDecode)
			}
			if runnerTrace != inspectTrace {
				t.Fatalf("runner context correlation = %q, want %q", runnerTrace, inspectTrace)
			}

			spans := h.Spans(t)
			inspects := tracingtest.Named(spans, telemetry.SpanMediaInspect)
			if len(inspects) != 1 {
				t.Fatalf("media.inspect spans = %+v, want one", inspects)
			}
			inspect := inspects[0]
			if inspect.TraceID != inspectTrace.String() || inspect.ParentSpanID != string(parent.SpanID) || inspect.Status != c.wantStatus {
				t.Fatalf("inspect span = %+v, want trace %s, parent %s, status %s", inspect, inspectTrace, parent.SpanID, c.wantStatus)
			}
			if !maps.Equal(inspect.Attributes, c.wantInspect) {
				t.Fatalf("inspect attributes = %v, want %v", inspect.Attributes, c.wantInspect)
			}
			runs := tracingtest.Named(spans, telemetry.SpanSubprocess)
			if len(runs) != len(c.wantRuns) {
				t.Fatalf("subprocess spans = %+v, want %d", runs, len(c.wantRuns))
			}
			for i, run := range runs {
				if run.TraceID != inspectTrace.String() || run.ParentSpanID != inspect.SpanID || run.Status != c.wantRunStat[i] {
					t.Fatalf("subprocess span %d = %+v, want trace %s, parent %s, status %s", i, run, inspectTrace, inspect.SpanID, c.wantRunStat[i])
				}
				if !maps.Equal(run.Attributes, c.wantRuns[i]) {
					t.Fatalf("subprocess span %d attributes = %v, want %v", i, run.Attributes, c.wantRuns[i])
				}
			}
		})
	}
}

// S9, S10: without a tracer the inspection still runs and stays correlated.
func TestInspectWithoutATracerKeepsTheCorrelation(t *testing.T) {
	config := testConfig(t)
	var runnerTrace correlation.ID
	runner := runnerFunc(func(ctx context.Context, command Command, stdout, _ io.Writer) error {
		runnerTrace, _ = correlation.FromContext(ctx)
		if command.Path == config.FFprobePath {
			_, _ = io.WriteString(stdout, imageProbe())
		}
		return nil
	})
	adapter, err := New(config, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := correlation.WithID(context.Background(), inspectTrace)
	if _, err := adapter.Inspect(ctx, strings.NewReader("x")); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if runnerTrace != inspectTrace {
		t.Fatalf("runner context correlation = %q, want %q", runnerTrace, inspectTrace)
	}
}
