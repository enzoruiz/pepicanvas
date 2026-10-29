package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// journeyScenario names the persisted run of the trace correlation journey.
const journeyScenario = "te-015-trace-correlation"

// journeyPassword and journeyInvitation are decoys: neither may reach the
// trace file or the logs.
const (
	journeyPassword   = "correct horse battery"
	journeyInvitation = "pci_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
)

// streamerSession is one signed-in account that owns a canvas.
type streamerSession struct {
	tenant, cookie, csrf string
}

func signUpOwner(t *testing.T, server *inProcess, dsn, name string) streamerSession {
	t.Helper()
	email := name + "@example.com"
	if res, body := server.do(t, http.MethodPost, "/api/accounts", "application/json",
		[]byte(`{"operation_id":"op-journey-`+name+`-01","email":"`+email+`","password":"`+journeyPassword+`"}`), nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("register %s = %d %s", name, res.StatusCode, body)
	}
	res, body := server.do(t, http.MethodPost, "/api/sessions", "application/json",
		[]byte(`{"email":"`+email+`","password":"`+journeyPassword+`"}`), nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("sign in %s = %d %s", name, res.StatusCode, body)
	}
	var signedIn struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &signedIn); err != nil {
		t.Fatal(err)
	}
	s := streamerSession{tenant: "tenant-" + name, csrf: signedIn.CSRF}
	for _, c := range res.Cookies() {
		if c.Name == "__Host-pcs" {
			s.cookie = c.Name + "=" + c.Value
		}
	}
	if s.cookie == "" || s.csrf == "" {
		t.Fatalf("sign in %s returned no session", name)
	}
	execSQL(t, dsn,
		"INSERT INTO tenants (tenant_id, subscription_status) VALUES ('"+s.tenant+"', 'active')",
		"INSERT INTO canvases (tenant_id, canvas_id) VALUES ('"+s.tenant+"', 'main')",
		"INSERT INTO memberships (tenant_id, account_id, role) SELECT '"+s.tenant+"', account_id, 'owner' FROM accounts WHERE email = '"+email+"'",
	)
	return s
}

// resize changes the canvas dimensions of s, carrying the invitation decoy in
// the query and a header, and returns the response's correlation.
func resize(t *testing.T, server *inProcess, s streamerSession, op string) correlation.ID {
	t.Helper()
	res, body := server.do(t, http.MethodPut, "/api/tenants/"+s.tenant+"/canvases/main/dimensions?invite="+journeyInvitation,
		"application/json", []byte(`{"operation_id":"`+op+`","width":1280,"height":720}`),
		map[string]string{"Cookie": s.cookie, "X-CSRF-Token": s.csrf, "X-Invitation": journeyInvitation})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("resize %s = %d %s", s.tenant, res.StatusCode, body)
	}
	id, err := correlation.Parse(res.Header.Get(httpx.CorrelationHeader))
	if err != nil {
		t.Fatalf("resize correlation %q: %v", res.Header.Get(httpx.CorrelationHeader), err)
	}
	return id
}

// traceOf reads the spans of id from the trace file.
func traceOf(t *testing.T, path string, id correlation.ID) []telemetry.SpanData {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	spans, malformed, err := telemetry.Query(file, id)
	if err != nil || malformed != 0 {
		t.Fatalf("Query(%s) = %d spans, %d malformed, %v", id, len(spans), malformed, err)
	}
	return spans
}

// awaitDelivery waits until the trace file holds the outbox.deliver span of id.
func awaitDelivery(t *testing.T, path string, id correlation.ID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, span := range traceOf(t, path, id) {
			if span.Name == string(telemetry.SpanOutboxDeliver) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no outbox.deliver span of %s in the trace file", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// logLinesOf returns the log lines whose correlation_id is id.
func logLinesOf(logs string, id correlation.ID) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["correlation_id"] == id.String() {
			lines = append(lines, line)
		}
	}
	return lines
}

// checkJourneyTrace asserts that the spans of id form one trace from the HTTP
// request through PostgreSQL and, past the outbox, to the realtime actor.
func checkJourneyTrace(t *testing.T, spans []telemetry.SpanData, id correlation.ID) {
	t.Helper()
	named := map[string][]telemetry.SpanData{}
	for _, span := range spans {
		if span.TraceID != id.String() {
			t.Errorf("span %s has trace %s, want %s", span.Name, span.TraceID, id)
		}
		named[span.Name] = append(named[span.Name], span)
	}
	for _, name := range []telemetry.SpanName{
		telemetry.SpanHTTPRequest, telemetry.SpanPostgres, telemetry.SpanOutboxDeliver, telemetry.SpanRealtimeSubmit,
	} {
		if len(named[string(name)]) != 1 {
			t.Fatalf("trace %s has %d %s spans, want 1; spans %v", id, len(named[string(name)]), name, spans)
		}
	}
	request, store := named[string(telemetry.SpanHTTPRequest)][0], named[string(telemetry.SpanPostgres)][0]
	if store.ParentSpanID != request.SpanID {
		t.Errorf("postgres span parent = %q, want the request span %q", store.ParentSpanID, request.SpanID)
	}
	deliver, submit := named[string(telemetry.SpanOutboxDeliver)][0], named[string(telemetry.SpanRealtimeSubmit)][0]
	if submit.ParentSpanID != deliver.SpanID {
		t.Errorf("realtime.submit span parent = %q, want the outbox.deliver span %q", submit.ParentSpanID, deliver.SpanID)
	}
	if request.Attributes["route"] != "PUT /api/tenants/{tenantID}/canvases/{canvasID}/dimensions" || request.Attributes["status"] != "200" {
		t.Errorf("request span attributes = %v", request.Attributes)
	}
}

// S5, S11, S13, L11a: over real PostgreSQL with export enabled, a resize by
// account A is queryable by its correlation from the HTTP request through
// PostgreSQL and, past the outbox, to the realtime actor, both in the trace
// file and in the local logs; account B's journey stays apart; no credential
// or invitation decoy reaches the trace file or the logs; and the run is
// persisted as durable evidence the independent reader classifies PASS.
func TestJourneyCorrelatesAResizeAcrossTheOutboxPerAccount(t *testing.T) {
	startedAt := time.Now()
	db := pgtest.Start(t)
	dsn := db.DSN(t)
	traceFile := filepath.Join(t.TempDir(), "spans.jsonl")
	server := runInProcess(t, map[string]string{
		"PEPICANVAS_DATABASE_URL":   dsn,
		"PEPICANVAS_LISTEN_ADDR":    "127.0.0.1:0",
		"PEPICANVAS_ALLOWED_ORIGIN": testOrigin,
		"PEPICANVAS_TRACE_FILE":     traceFile,
	})
	alice := signUpOwner(t, server, dsn, "alice")
	bob := signUpOwner(t, server, dsn, "bob")

	aliceID := resize(t, server, alice, "op-journey-size-a1")
	bobID := resize(t, server, bob, "op-journey-size-b1")
	if aliceID == bobID {
		t.Fatalf("both accounts got correlation %s", aliceID)
	}
	awaitDelivery(t, traceFile, aliceID)
	awaitDelivery(t, traceFile, bobID)
	if err := server.stop(); err != nil {
		t.Fatalf("run = %v\n%s", err, server.logs)
	}
	logs := server.logs.String()

	aliceSpans, bobSpans := traceOf(t, traceFile, aliceID), traceOf(t, traceFile, bobID)
	checkJourneyTrace(t, aliceSpans, aliceID)
	checkJourneyTrace(t, bobSpans, bobID)
	bobSpanIDs := map[string]bool{}
	for _, span := range bobSpans {
		bobSpanIDs[span.SpanID] = true
	}
	for _, span := range aliceSpans {
		if bobSpanIDs[span.SpanID] {
			t.Errorf("span %s %s is in both accounts' traces", span.Name, span.SpanID)
		}
	}

	aliceLogs, bobLogs := logLinesOf(logs, aliceID), logLinesOf(logs, bobID)
	for name, c := range map[string]struct {
		lines       []string
		own, others string
	}{
		"alice": {aliceLogs, alice.tenant, bob.tenant},
		"bob":   {bobLogs, bob.tenant, alice.tenant},
	} {
		joined := strings.Join(c.lines, "\n")
		for _, msg := range []string{"canvas dimensions confirmed", "outbox record delivered"} {
			if logRecord(joined, msg) == nil {
				t.Errorf("%s's correlated logs lack %q:\n%s", name, msg, joined)
			}
		}
		if record := logRecord(joined, "outbox record delivered"); record != nil && record["tenant_id"] != c.own {
			t.Errorf("%s's delivery log names tenant %v, want %s", name, record["tenant_id"], c.own)
		}
		if strings.Contains(joined, c.others) {
			t.Errorf("%s's correlated logs mention %s:\n%s", name, c.others, joined)
		}
	}

	data, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{
		"alice session token": strings.TrimPrefix(alice.cookie, "__Host-pcs="),
		"alice CSRF token":    alice.csrf,
		"bob session token":   strings.TrimPrefix(bob.cookie, "__Host-pcs="),
		"bob CSRF token":      bob.csrf,
		"password":            journeyPassword,
		"invitation decoy":    journeyInvitation,
		"invitation prefix":   "pci_",
		"database URL":        dsn,
	} {
		if strings.Contains(string(data), s) {
			t.Errorf("trace file contains the %s", name)
		}
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}

	persistJourney(t, startedAt, aliceID, aliceSpans, aliceLogs)
}

// persistJourney writes A's spans and log lines as a durable run under
// PEPICANVAS_EVIDENCE_DIR, or a temp directory, recording FAIL when any
// earlier check failed, and checks that the independent reader classifies it
// accordingly.
func persistJourney(t *testing.T, startedAt time.Time, id correlation.ID, spans []telemetry.SpanData, logLines []string) {
	t.Helper()
	repo := filepath.Join("..", "..", "..")
	pins, err := latency.LoadPins(filepath.Join(repo, "releases", "client-support-release.json"),
		filepath.Join(repo, "backend", "go.mod"), filepath.Join(repo, "frontend", "package-lock.json"))
	if err != nil {
		t.Fatalf("LoadPins: %v", err)
	}
	root := os.Getenv("PEPICANVAS_EVIDENCE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	writer, err := evidence.Begin(root, evidence.Run{
		Scenario: journeyScenario, StartedAt: startedAt, CorrelationID: id,
		Versions: pins, Role: "evidence-reader", RetentionDays: 30,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	var trace bytes.Buffer
	for _, span := range spans {
		line, err := json.Marshal(span)
		if err != nil {
			t.Fatal(err)
		}
		trace.Write(append(line, '\n'))
	}
	if err := writer.Add("spans.jsonl", "trace-spans", trace.Bytes()); err != nil {
		t.Fatalf("Add spans: %v", err)
	}
	if err := writer.Add("logs.jsonl", "log-records", []byte(strings.Join(logLines, "\n")+"\n")); err != nil {
		t.Fatalf("Add logs: %v", err)
	}
	result, want := evidence.ResultPass, evidence.Pass
	if t.Failed() {
		result, want = evidence.ResultFail, evidence.Fail
	}
	if err := writer.Complete(result, time.Now()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	report := evidence.Check(writer.Dir(), pins, time.Now())
	if report.Classification != want || report.Scenario != journeyScenario || report.Recorded != result {
		t.Fatalf("Check(%s) = %+v, want %s", writer.Dir(), report, want)
	}
	t.Logf("evidence run %s", writer.Dir())
}
