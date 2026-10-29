package httpx_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// spanRecorder is an exporter that keeps every exported span.
type spanRecorder struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
}

func (r *spanRecorder) Export(_ context.Context, spans []telemetry.SpanData) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

// tracedServe serves one request through Correlate and Trace with a recording
// tracer and returns the response and the exported spans.
func tracedServe(t *testing.T, next http.Handler, req *http.Request) (*httptest.ResponseRecorder, []telemetry.SpanData) {
	t.Helper()
	rec := &spanRecorder{}
	tracer, err := telemetry.New(telemetry.Config{Exporter: rec})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	httpx.Correlate(httpx.Trace(tracer)(httpx.Private(next))).ServeHTTP(w, req)
	if err := tracer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w, rec.spans
}

func TestTraceStartsTheRootSpanInTheRequestCorrelation(t *testing.T) {
	token := decoyToken(t)
	inboundTrace := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	inboundID, _ := correlation.New()

	var seen telemetry.SpanContext
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen, _ = telemetry.SpanContextFrom(r.Context())
		w.WriteHeader(http.StatusCreated)
	})
	req := httptest.NewRequest(http.MethodGet, "/items/"+token+"?code="+token, nil)
	req.Header.Set("traceparent", inboundTrace)
	req.Header.Set(httpx.CorrelationHeader, inboundID.String())
	req.Header.Set("Authorization", "Bearer "+token)
	w, spans := tracedServe(t, mux, req)

	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	span := spans[0]
	id := w.Header().Get(httpx.CorrelationHeader)
	if span.TraceID != id || id == inboundID.String() || strings.Contains(inboundTrace, id) {
		t.Fatalf("trace = %q, correlation = %q; inbound IDs must be ignored", span.TraceID, id)
	}
	if span.Name != "http.request" || span.Status != "ok" || span.ParentSpanID != "" {
		t.Fatalf("span = %+v", span)
	}
	want := map[string]string{"method": "GET", "route": "GET /items/{id}", "status": "201"}
	if !maps.Equal(span.Attributes, want) {
		t.Fatalf("attributes = %v, want %v", span.Attributes, want)
	}
	sc, err := telemetry.ParseTraceparent(w.Header().Get("traceparent"))
	if err != nil {
		t.Fatalf("traceparent response header %q: %v", w.Header().Get("traceparent"), err)
	}
	if sc.TraceID.String() != id || string(sc.SpanID) != span.SpanID || seen != sc {
		t.Fatalf("traceparent = %+v, handler span = %+v, exported span %s", sc, seen, span.SpanID)
	}
	raw, _ := json.Marshal(span)
	if strings.Contains(string(raw), token) {
		t.Fatalf("exported span leaked the decoy: %s", raw)
	}
}

func TestTraceRecordsOnlyRegisteredRoutesAndTheWrittenStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /silent", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /dir/", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/tree/{name}/", func(http.ResponseWriter, *http.Request) {})
	token := decoyToken(t)
	mux.HandleFunc("POST /written", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	for _, tc := range []struct {
		method, path, route, status string
	}{
		{http.MethodGet, "/silent", "GET /silent", "200"},
		{http.MethodPost, "/written", "POST /written", "200"},
		{http.MethodGet, "/missing/" + token, "unmatched", "404"},
		{http.MethodDelete, "/silent", "unmatched", "405"},
		// A trailing-slash redirect reports the registered pattern.
		{http.MethodGet, "/dir", "GET /dir/", "307"},
		// For CONNECT the mux reports the redirect's target path.
		{http.MethodConnect, "/tree/" + token, "unmatched", "307"},
	} {
		_, spans := tracedServe(t, mux, httptest.NewRequest(tc.method, tc.path, nil))
		if len(spans) != 1 {
			t.Fatalf("%s %s: exported %d spans", tc.method, tc.path, len(spans))
		}
		want := map[string]string{"method": tc.method, "route": tc.route, "status": tc.status}
		if !maps.Equal(spans[0].Attributes, want) {
			t.Errorf("%s %s: attributes = %v, want %v", tc.method, tc.path, spans[0].Attributes, want)
		}
	}
}

func TestTraceEndsTheSpanWithAnErrorAndRepanics(t *testing.T) {
	rec := &spanRecorder{}
	tracer, err := telemetry.New(telemetry.Config{Exporter: rec})
	if err != nil {
		t.Fatal(err)
	}
	h := httpx.Correlate(httpx.Trace(tracer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	func() {
		defer func() {
			if v := recover(); v != "boom" {
				t.Fatalf("recovered %v, want the handler panic", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if err := tracer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.spans) != 1 || rec.spans[0].Status != "error" {
		t.Fatalf("spans = %+v, want one error span", rec.spans)
	}
}

func TestTraceMarksServerErrorsAsFailedSpans(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusServiceUnavailable:  "error",
		http.StatusInternalServerError: "error",
		http.StatusForbidden:           "ok",
	} {
		rec := &spanRecorder{}
		tracer, err := telemetry.New(telemetry.Config{Exporter: rec})
		if err != nil {
			t.Fatal(err)
		}
		h := httpx.Correlate(httpx.Trace(tracer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if err := tracer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(rec.spans) != 1 || rec.spans[0].Status != want {
			t.Errorf("status %d: spans = %+v, want one %q span", status, rec.spans, want)
		}
	}
}

func TestTraceWithoutTracerStillCorrelatesTheRequestSpan(t *testing.T) {
	var seen telemetry.SpanContext
	var ok bool
	h := httpx.Correlate(httpx.Trace(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, ok = telemetry.SpanContextFrom(r.Context())
	})))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || seen.TraceID.String() != w.Header().Get(httpx.CorrelationHeader) {
		t.Fatalf("span context = %+v, %v; correlation %q", seen, ok, w.Header().Get(httpx.CorrelationHeader))
	}
	if got := w.Header().Get("traceparent"); got != seen.Traceparent() {
		t.Fatalf("traceparent = %q, want %q", got, seen.Traceparent())
	}
}

func TestTraceKeepsPrivateHeadersAcrossFlush(t *testing.T) {
	w := httptest.NewRecorder()
	h := httpx.Correlate(httpx.Trace(nil)(httpx.Private(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	}))))
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assertPrivateHeaders(t, w.Result().Header)
	if !w.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}
