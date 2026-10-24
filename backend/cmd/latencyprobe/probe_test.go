package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

type harness struct {
	probe  *probe
	server *httptest.Server
}

func startHarness(t *testing.T, opts options) *harness {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	return startOn(t, server, opts)
}

func startOn(t *testing.T, server *httptest.Server, opts options) *harness {
	t.Helper()
	if opts.static == "" {
		opts.static = t.TempDir()
	}
	p, err := newProbe(opts)
	if err != nil {
		t.Fatalf("newProbe: %v", err)
	}
	server.Config.Handler = p
	server.Config.RegisterOnShutdown(p.closeStreams)
	server.Start()
	t.Cleanup(func() {
		p.closeStreams()
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Close(ctx)
	})
	return &harness{probe: p, server: server}
}

type pingBody struct {
	ServerAtNs int64 `json:"serverAtNs"`
}

func (h *harness) ping(t *testing.T) int64 {
	t.Helper()
	resp, err := http.Get(h.server.URL + "/probe/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping status = %d", resp.StatusCode)
	}
	var body pingBody
	decodeStrict(t, resp.Body, &body)
	return body.ServerAtNs
}

type commandResponse struct {
	Outcome       string `json:"outcome"`
	Sequence      uint64 `json:"sequence"`
	AcceptedAtNs  int64  `json:"acceptedAtNs"`
	CorrelationID string `json:"correlationId"`
}

type rejectedBody struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

func (h *harness) post(t *testing.T, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(h.server.URL+"/probe/commands", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	return resp.StatusCode, data
}

func (h *harness) accept(t *testing.T, body string) commandResponse {
	t.Helper()
	status, data := h.post(t, body)
	if status != http.StatusOK {
		t.Fatalf("command status = %d body %s", status, data)
	}
	var out commandResponse
	decodeStrict(t, bytes.NewReader(data), &out)
	if out.Outcome != "accepted" || out.Sequence == 0 || !hex32.MatchString(out.CorrelationID) {
		t.Fatalf("command response = %+v", out)
	}
	return out
}

func decodeStrict(t *testing.T, r io.Reader, v any) {
	t.Helper()
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

type frame struct {
	event string
	data  string
}

type canvasEvent struct {
	Sequence      uint64          `json:"sequence"`
	OperationID   string          `json:"operationId"`
	ObjectID      string          `json:"objectId"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	CorrelationID string          `json:"correlationId"`
}

type stream struct {
	frames chan frame
	cancel context.CancelFunc
	done   chan struct{}
}

// subscribe opens /probe/events and waits for the ready frame.
func (h *harness) subscribe(t *testing.T) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+"/probe/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("subscribe: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("subscribe status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q", got)
	}
	s := &stream{frames: make(chan frame, 4096), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.frames)
		defer resp.Body.Close()
		readFrames(resp.Body, s.frames)
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
	})
	first := s.next(t)
	if first.event != "ready" || first.data != "{}" {
		t.Fatalf("first frame = %+v, want ready {}", first)
	}
	return s
}

func readFrames(r io.Reader, out chan<- frame) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	var current frame
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			out <- current
			current = frame{}
		case strings.HasPrefix(line, "event: "):
			current.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func (s *stream) next(t *testing.T) frame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			t.Fatalf("stream ended")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a frame")
	}
	return frame{}
}

func (s *stream) nextCanvas(t *testing.T) canvasEvent {
	t.Helper()
	f := s.next(t)
	if f.event != "canvas" {
		t.Fatalf("frame event = %q, want canvas", f.event)
	}
	var event canvasEvent
	decodeStrict(t, strings.NewReader(f.data), &event)
	return event
}

// quiet asserts that no frame arrives for a short while.
func (s *stream) quiet(t *testing.T) {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			t.Fatalf("unexpected frame %+v", f)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

// ended waits for the server to close the stream.
func (s *stream) ended(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("stream still open")
		}
	}
}

func TestPingReturnsIncreasingServerClockWithoutCaching(t *testing.T) {
	h := startHarness(t, options{})
	var previous int64 = -1
	for i := 0; i < 3; i++ {
		resp, err := http.Get(h.server.URL + "/probe/ping")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("cache control = %q", got)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body pingBody
		decodeStrict(t, resp.Body, &body)
		resp.Body.Close()
		if body.ServerAtNs <= previous {
			t.Fatalf("serverAtNs %d not after %d", body.ServerAtNs, previous)
		}
		previous = body.ServerAtNs
	}
}

func TestAcceptedCommandIsStampedAndBroadcast(t *testing.T) {
	h := startHarness(t, options{})
	s := h.subscribe(t)

	tests := []struct {
		name    string
		body    string
		kind    string
		payload string
	}{
		{"create", `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{"op":"create","x":1,"y":2,"width":30,"height":40,"rotation":0}}`, "set", `{"op":"create","x":1,"y":2,"width":30,"height":40,"rotation":0}`},
		{"move", `{"operationId":"op-2","objectId":"box-1","kind":"set","payload":{"op":"move","x":5,"y":6}}`, "set", `{"op":"move","x":5,"y":6}`},
		{"media", `{"operationId":"op-3","objectId":"audio-1","kind":"set","payload":{"op":"media","action":"play"}}`, "set", `{"op":"media","action":"play"}`},
		{"delete", `{"operationId":"op-4","objectId":"box-1","kind":"delete"}`, "delete", `null`},
	}
	var previous uint64
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := h.ping(t)
			out := h.accept(t, tc.body)
			after := h.ping(t)
			if out.AcceptedAtNs < before || out.AcceptedAtNs > after {
				t.Errorf("acceptedAtNs %d outside [%d, %d]", out.AcceptedAtNs, before, after)
			}
			if out.Sequence != previous+1 {
				t.Errorf("sequence = %d, want %d", out.Sequence, previous+1)
			}
			previous = out.Sequence

			var request struct {
				OperationID string `json:"operationId"`
				ObjectID    string `json:"objectId"`
			}
			_ = json.Unmarshal([]byte(tc.body), &request)
			event := s.nextCanvas(t)
			want := canvasEvent{
				Sequence:      out.Sequence,
				OperationID:   request.OperationID,
				ObjectID:      request.ObjectID,
				Kind:          tc.kind,
				CorrelationID: out.CorrelationID,
			}
			gotPayload := string(event.Payload)
			event.Payload = nil
			if !reflect.DeepEqual(event, want) {
				t.Errorf("event = %+v, want %+v", event, want)
			}
			if gotPayload != tc.payload {
				t.Errorf("payload = %s, want %s", gotPayload, tc.payload)
			}
		})
	}
}

func TestSetAfterDeleteIsRejectedAndNotBroadcast(t *testing.T) {
	h := startHarness(t, options{})
	s := h.subscribe(t)
	h.accept(t, `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{"op":"create","x":0,"y":0,"width":1,"height":1,"rotation":0}}`)
	h.accept(t, `{"operationId":"op-2","objectId":"box-1","kind":"delete"}`)
	s.nextCanvas(t)
	s.nextCanvas(t)

	status, data := h.post(t, `{"operationId":"op-3","objectId":"box-1","kind":"set","payload":{"op":"move","x":1,"y":1}}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	var out rejectedBody
	decodeStrict(t, bytes.NewReader(data), &out)
	if out != (rejectedBody{Outcome: "rejected", Reason: "object_deleted"}) {
		t.Errorf("response = %+v", out)
	}
	s.quiet(t)
}

func TestMalformedCommandsAreRejectedAndNotBroadcast(t *testing.T) {
	h := startHarness(t, options{})
	s := h.subscribe(t)
	oversized := `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{"blob":"` +
		strings.Repeat("a", realtime.MaxPayloadBytes) + `"}}`
	hugeBody := `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{"blob":"` +
		strings.Repeat("a", 2*realtime.MaxPayloadBytes) + `"}}`

	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{},"extra":1}`},
		{"delete with payload", `{"operationId":"op-1","objectId":"box-1","kind":"delete","payload":{}}`},
		{"delete with null payload", `{"operationId":"op-1","objectId":"box-1","kind":"delete","payload":null}`},
		{"missing operationId", `{"objectId":"box-1","kind":"set","payload":{}}`},
		{"missing objectId", `{"operationId":"op-1","kind":"set","payload":{}}`},
		{"unknown kind", `{"operationId":"op-1","objectId":"box-1","kind":"move","payload":{}}`},
		{"set without payload", `{"operationId":"op-1","objectId":"box-1","kind":"set"}`},
		{"set with non-object payload", `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":[1]}`},
		{"oversized payload", oversized},
		{"oversized body", hugeBody},
		{"not json", `operationId=op-1`},
		{"trailing data", `{"operationId":"op-1","objectId":"box-1","kind":"delete"} {}`},
		{"empty body", ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, data := h.post(t, tc.body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %.80s)", status, data)
			}
		})
	}
	s.quiet(t)

	// The first accepted command still gets sequence 1: nothing was ordered.
	out := h.accept(t, `{"operationId":"op-1","objectId":"box-1","kind":"delete"}`)
	if out.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", out.Sequence)
	}
}

func TestCommandMethodAndUnknownProbeRoutes(t *testing.T) {
	h := startHarness(t, options{})
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/probe/commands", http.StatusMethodNotAllowed},
		{http.MethodPost, "/probe/ping", http.StatusMethodNotAllowed},
		{http.MethodPost, "/probe/events", http.StatusMethodNotAllowed},
		{http.MethodGet, "/probe/unknown", http.StatusNotFound},
	}
	for _, tc := range tests {
		req, _ := http.NewRequest(tc.method, h.server.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestEventsArriveInSequenceOrderWithConcurrentCommands(t *testing.T) {
	const writers = 50
	h := startHarness(t, options{})
	s := h.subscribe(t)

	responses := make([]commandResponse, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, data := h.post(t, fmt.Sprintf(`{"operationId":"op-%d","objectId":"box-%d","kind":"set","payload":{"op":"move","x":%d,"y":0}}`, i, i%7, i))
			if status != http.StatusOK {
				t.Errorf("status = %d body %s", status, data)
				return
			}
			if err := json.Unmarshal(data, &responses[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	bySequence := map[uint64]string{}
	for i, out := range responses {
		bySequence[out.Sequence] = fmt.Sprintf("op-%d", i)
	}
	for want := uint64(1); want <= writers; want++ {
		event := s.nextCanvas(t)
		if event.Sequence != want {
			t.Fatalf("event sequence = %d, want %d", event.Sequence, want)
		}
		if event.OperationID != bySequence[want] {
			t.Errorf("sequence %d operation = %s, want %s", want, event.OperationID, bySequence[want])
		}
	}
	s.quiet(t)
}

func TestRetriedOperationIsNotBroadcastTwice(t *testing.T) {
	h := startHarness(t, options{})
	s := h.subscribe(t)
	body := `{"operationId":"op-1","objectId":"box-1","kind":"set","payload":{"op":"rotate","rotation":90}}`
	first := h.accept(t, body)
	second := h.accept(t, body)
	if second.Sequence != first.Sequence || second.CorrelationID != first.CorrelationID {
		t.Errorf("retry = %+v, first %+v", second, first)
	}
	if event := s.nextCanvas(t); event.Sequence != first.Sequence {
		t.Errorf("event sequence = %d", event.Sequence)
	}
	s.quiet(t)
}

func TestSlowSubscriberIsDisconnectedWhileFastOneKeepsReceiving(t *testing.T) {
	const commands = 64
	server := httptest.NewUnstartedServer(nil)
	// Tiny kernel buffers make the server's writes to a non-reading client
	// block quickly, so its bounded queue fills.
	server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(4096)
		}
		return ctx
	}
	h := startOn(t, server, options{buffer: 4})
	fast := h.subscribe(t)

	slow, err := net.Dial("tcp", h.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	_ = slow.(*net.TCPConn).SetReadBuffer(4096)
	fmt.Fprintf(slow, "GET /probe/events HTTP/1.1\r\nHost: probe\r\n\r\n")
	slowReader := bufio.NewReader(slow)
	// Read the response head and the ready frame, then stop reading.
	for {
		line, err := slowReader.ReadString('\n')
		if err != nil {
			t.Fatalf("slow read: %v", err)
		}
		if strings.HasPrefix(line, "data: {}") {
			break
		}
	}

	blob := strings.Repeat("a", 60<<10)
	for i := range commands {
		h.accept(t, fmt.Sprintf(`{"operationId":"op-%d","objectId":"box-1","kind":"set","payload":{"blob":"%s"}}`, i, blob))
		event := fast.nextCanvas(t)
		if event.Sequence != uint64(i+1) {
			t.Fatalf("fast sequence = %d, want %d", event.Sequence, i+1)
		}
	}

	_ = slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest, err := io.ReadAll(slowReader)
	if err != nil && !isReset(err) {
		t.Fatalf("slow stream did not end: %v", err)
	}
	if got := bytes.Count(rest, []byte("event: canvas")); got >= commands {
		t.Errorf("slow subscriber received all %d events, want a disconnect", got)
	}
}

func isReset(err error) bool {
	return strings.Contains(err.Error(), "connection reset")
}

func TestCloseEndsStreamsAndClosesHub(t *testing.T) {
	h := startHarness(t, options{})
	first, second := h.subscribe(t), h.subscribe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.probe.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	first.ended(t)
	second.ended(t)

	status, _ := h.post(t, `{"operationId":"op-1","objectId":"box-1","kind":"delete"}`)
	if status != http.StatusServiceUnavailable {
		t.Errorf("command after close = %d, want 503", status)
	}
	resp, err := http.Get(h.server.URL + "/probe/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("subscribe after close = %d, want 503", resp.StatusCode)
	}
}

func TestStaticFilesAreServedAndProbeRoutesWin(t *testing.T) {
	static := t.TempDir()
	if err := os.MkdirAll(filepath.Join(static, "latency"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "latency", "index.html"), []byte("<p>probe</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(static, "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "probe", "ping"), []byte("static"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := startHarness(t, options{static: static})

	resp, err := http.Get(h.server.URL + "/latency/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "<p>probe</p>" {
		t.Errorf("/latency/ = %d %q", resp.StatusCode, body)
	}
	if got := h.ping(t); got < 0 {
		t.Errorf("ping = %d", got)
	}
}

func TestNewProbeRejectsMissingStaticDirectory(t *testing.T) {
	for _, static := range []string{filepath.Join(t.TempDir(), "missing"), writeFile(t)} {
		if _, err := newProbe(options{static: static}); err == nil {
			t.Errorf("newProbe(%q) succeeded", static)
		}
	}
}

func writeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListenAddressMustBeLoopback(t *testing.T) {
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.10.0.3:0", true},
		{"[::1]:8080", true},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{":8080", false},
		{"localhost:8080", false},
		{"192.168.1.10:8080", false},
		{"[::ffff:192.168.1.10]:8080", false},
		{"127.0.0.1", false},
	}
	for _, tc := range tests {
		err := validateListen(tc.addr)
		if (err == nil) != tc.ok {
			t.Errorf("validateListen(%q) = %v, want ok=%v", tc.addr, err, tc.ok)
		}
	}
}

func TestRunRefusesBadArgumentsBeforeListening(t *testing.T) {
	static := t.TempDir()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"non-loopback", []string{"-listen", "0.0.0.0:0", "-static", static}, "latencyprobe: -listen must be a loopback IP address and port"},
		{"hostname", []string{"-listen", "localhost:0", "-static", static}, "latencyprobe: -listen must be a loopback IP address and port"},
		{"missing static", []string{"-listen", "127.0.0.1:0"}, usage},
		{"missing listen", []string{"-static", static}, usage},
		{"extra argument", []string{"-listen", "127.0.0.1:0", "-static", static, "x"}, usage},
		{"static not a directory", []string{"-listen", "127.0.0.1:0", "-static", filepath.Join(static, "missing")}, "latencyprobe: -static"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q", stdout.String())
			}
			if !strings.HasPrefix(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want prefix %q", stderr.String(), tc.want)
			}
		})
	}
}

// syncBuffer lets the test read stdout while run writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunServesUntilCancelledThenShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, []string{"-listen", "127.0.0.1:0", "-static", t.TempDir()}, &stdout, &stderr)
	}()

	var base string
	deadline := time.Now().Add(5 * time.Second)
	for base == "" {
		if time.Now().After(deadline) {
			t.Fatalf("no listening line; stderr %q", stderr.String())
		}
		if line, ok := strings.CutPrefix(stdout.String(), "latencyprobe: listening on "); ok {
			base = strings.TrimSpace(line)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("base = %q", base)
	}

	h := &harness{server: &httptest.Server{URL: base}}
	if h.ping(t) < 0 {
		t.Fatal("negative ping")
	}
	s := h.subscribe(t)
	h.accept(t, `{"operationId":"op-1","objectId":"box-1","kind":"delete"}`)
	s.nextCanvas(t)

	cancel()
	s.ended(t)
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit = %d, stderr %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if _, err := http.Get(base + "/probe/ping"); err == nil {
		t.Error("server still listening after shutdown")
	}
}
