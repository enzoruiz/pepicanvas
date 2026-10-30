package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

// maxLineBytes bounds one span line; exported spans stay far below it.
const maxLineBytes = 64 << 10

// Query reads JSON-lines spans from r and returns those of trace in start
// order, with the count of malformed lines skipped. A line is malformed when
// it is too long, is not exactly one span object with known fields, or has a
// non-canonical trace or span ID. Blank lines are ignored.
func Query(r io.Reader, trace correlation.ID) ([]SpanData, int, error) {
	if _, err := correlation.Parse(trace.String()); err != nil {
		return nil, 0, err
	}
	br := bufio.NewReaderSize(r, maxLineBytes)
	var spans []SpanData
	malformed := 0
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			malformed++
			err = skipLine(br)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			span, ok := parseSpan(line)
			switch {
			case !ok:
				malformed++
			case span.TraceID == trace.String():
				spans = append(spans, span)
			}
		}
		if err != nil {
			break
		}
	}
	slices.SortStableFunc(spans, func(a, b SpanData) int { return a.Start.Compare(b.Start) })
	return spans, malformed, nil
}

// skipLine discards the rest of an over-long line.
func skipLine(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

func parseSpan(line []byte) (SpanData, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var span SpanData
	if err := dec.Decode(&span); err != nil {
		return SpanData{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SpanData{}, false
	}
	if _, err := correlation.Parse(span.TraceID); err != nil || !lowerHex(span.SpanID, 16) {
		return SpanData{}, false
	}
	return span, true
}
