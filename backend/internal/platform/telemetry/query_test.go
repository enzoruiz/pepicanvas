package telemetry_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

func jsonLine(t *testing.T, span telemetry.SpanData) string {
	t.Helper()
	raw, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestQueryReturnsOnlyTheTraceInStartOrderAndCountsMalformedLines(t *testing.T) {
	a, _ := correlation.New()
	b, _ := correlation.New()
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	late := spanData(a, "00000000000000a2", t0.Add(2*time.Second))
	early := spanData(a, "00000000000000a1", t0)
	other := spanData(b, "00000000000000b1", t0.Add(time.Second))
	badSpan := spanData(a, "XYZ", t0)

	input := strings.Join([]string{
		jsonLine(t, late),
		"not json",
		jsonLine(t, other),
		"",
		`{"trace_id":"` + strings.ToUpper(a.String()) + `","span_id":"00000000000000a3","name":"x","status":"ok"}`,
		jsonLine(t, badSpan),
		`{"trace_id":"` + a.String() + `","span_id":"00000000000000a4","name":"x","status":"ok","secret":"decoy"}`,
		"   ",
		strings.Repeat("x", 70_000),
		jsonLine(t, early),
	}, "\n") + "\n"

	spans, malformed, err := telemetry.Query(strings.NewReader(input), a)
	if err != nil {
		t.Fatalf("Query = %v", err)
	}
	if malformed != 5 {
		t.Fatalf("malformed = %d, want 5", malformed)
	}
	if len(spans) != 2 || spans[0].SpanID != early.SpanID || spans[1].SpanID != late.SpanID {
		t.Fatalf("spans = %+v, want early then late of trace %s", spans, a)
	}

	spans, _, err = telemetry.Query(strings.NewReader(input), b)
	if err != nil || len(spans) != 1 || spans[0].SpanID != other.SpanID {
		t.Fatalf("Query(b) = %+v, %v", spans, err)
	}
}

func TestQueryWithoutTrailingNewlineReadsTheLastLine(t *testing.T) {
	a, _ := correlation.New()
	spans, malformed, err := telemetry.Query(strings.NewReader(jsonLine(t, spanData(a, "00000000000000a1", time.Now()))), a)
	if err != nil || malformed != 0 || len(spans) != 1 {
		t.Fatalf("Query = %+v, %d, %v", spans, malformed, err)
	}
}

func TestQueryRejectsANonCanonicalTraceID(t *testing.T) {
	for _, bad := range []correlation.ID{"", "ABC", correlation.ID(strings.Repeat("A", 32))} {
		if _, _, err := telemetry.Query(strings.NewReader(""), bad); !errors.Is(err, correlation.ErrInvalid) {
			t.Errorf("Query(%q) = %v, want correlation.ErrInvalid", bad, err)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestQueryReportsReadErrors(t *testing.T) {
	a, _ := correlation.New()
	if _, _, err := telemetry.Query(failingReader{}, a); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Query = %v, want the read error", err)
	}
}
