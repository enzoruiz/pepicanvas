package httpapi_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// Two accounts' requests are exported to one owner-only file; each
// correlation returns only its own request, and no secret sent in the path,
// query, headers, or cookies reaches the file (CTRL-TRACE-01).
func TestRequestsAreTracedPerCorrelationWithoutSecrets(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	exporter, err := telemetry.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tracer, err := telemetry.New(telemetry.Config{Exporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	e.config.Tracer = tracer
	if e.handler, err = httpapi.New(e.config); err != nil {
		t.Fatal(err)
	}

	decoy := "decoy-trace-secret-9c2e"
	inboundTrace := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	e.register("op-http-trace-0001", "alice@example.com")
	e.register("op-http-trace-0002", "bob@example.com")
	alice := e.signIn("alice@example.com")
	bob := e.signIn("bob@example.com")
	inbound := func(r *http.Request) {
		r.Header.Set("traceparent", inboundTrace)
		r.Header.Set("X-Decoy", decoy)
		r.Header.Set("Authorization", "Bearer "+decoy)
	}
	resA := e.do(http.MethodGet, "/api/session?code="+decoy, nil, withSession(alice.token), withoutOrigin(), inbound)
	resB := e.do(http.MethodGet, "/api/session?code="+decoy, nil, withSession(bob.token), withoutOrigin(), inbound)
	resMissing := e.do(http.MethodGet, "/api/"+decoy, nil, withoutOrigin(), inbound)
	if resA.StatusCode != http.StatusOK || resB.StatusCode != http.StatusOK {
		t.Fatalf("session statuses = %d, %d", resA.StatusCode, resB.StatusCode)
	}
	if err := tracer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(); err != nil {
		t.Fatal(err)
	}

	query := func(res *http.Response) []telemetry.SpanData {
		t.Helper()
		id, err := correlation.Parse(res.Header.Get(httpx.CorrelationHeader))
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		spans, malformed, err := telemetry.Query(f, id)
		if err != nil || malformed != 0 {
			t.Fatalf("Query = %v, %d malformed", err, malformed)
		}
		return spans
	}
	spansA, spansB := query(resA), query(resB)
	if len(spansA) != 1 || len(spansB) != 1 || spansA[0].SpanID == spansB[0].SpanID {
		t.Fatalf("spans A = %+v, B = %+v; want one own span each", spansA, spansB)
	}
	for _, spans := range [][]telemetry.SpanData{spansA, spansB} {
		attrs := spans[0].Attributes
		if spans[0].Name != "http.request" || attrs["route"] != "GET /api/session" || attrs["status"] != "200" || attrs["method"] != "GET" {
			t.Fatalf("span = %+v", spans[0])
		}
	}
	if missing := query(resMissing); len(missing) != 1 || missing[0].Attributes["route"] != "unmatched" || missing[0].Attributes["status"] != "404" {
		t.Fatalf("unmatched span = %+v", missing)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"decoy": decoy, "alice session": alice.token, "bob session": bob.token, "alice csrf": alice.csrf,
		"bob csrf": bob.csrf, "password": password, "email": "example.com", "inbound trace": "4bf92f3577b34da6a3ce929d0e0e4736",
	} {
		if strings.Contains(string(raw), value) {
			t.Errorf("exported file contains the %s", name)
		}
	}
}
