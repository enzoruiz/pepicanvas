package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
)

// canvasKey is the one synthetic canvas the harness serves.
var canvasKey = realtime.Key{TenantID: "latency-tenant", CanvasID: "latency-canvas"}

const (
	// defaultBuffer bounds the frames queued per subscriber.
	defaultBuffer = 256
	// maxBodyBytes bounds a command body: the largest payload the hub
	// accepts plus room for the identifiers and JSON envelope.
	maxBodyBytes = realtime.MaxPayloadBytes + 1024
)

// allowAll accepts every command. The harness serves one synthetic tenant on
// a loopback address only and is never wired into cmd/server, so there is no
// account or membership to re-evaluate before acceptance.
func allowAll(context.Context, realtime.Command) error { return nil }

type options struct {
	// clock returns the server time used for ping and acceptance stamps;
	// it defaults to the monotonic time since newProbe.
	clock  func() time.Duration
	static string
	buffer int
	// logSize bounds the events the hub retains for replay; zero keeps the
	// hub default.
	logSize int
	// host is the listening address; requests must name it.
	host string
}

// probe is the TE-010 harness: ping, commands on a real realtime.Hub, an SSE
// stream of accepted events, and static files.
type probe struct {
	// hosts are the only Host values a request may name, and http://<host>
	// for one of them the only Origin it may send.
	hosts  []string
	clock  func() time.Duration
	hub    *realtime.Hub
	mux    *http.ServeMux
	buffer int

	// publishMu serializes broadcasting so events leave in sequence order;
	// published is the last sequence broadcast.
	publishMu sync.Mutex
	published uint64

	mu            sync.Mutex
	subscribers   map[*subscriber]struct{}
	streamsClosed bool

	// stampMu guards stamps, the acceptance stamp of each accepted
	// sequence, and stampOrder, those sequences oldest first.
	stampMu    sync.Mutex
	stamps     map[uint64]time.Duration
	stampOrder []uint64
}

type subscriber struct {
	frames chan []byte
	// gone is closed when the probe disconnects the subscriber.
	gone chan struct{}
	// abort unblocks a write stuck on a client that stopped reading.
	abort func()
}

type commandRequest struct {
	OperationID string          `json:"operationId"`
	ObjectID    string          `json:"objectId"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
}

type acceptedResponse struct {
	Outcome       string `json:"outcome"`
	Sequence      uint64 `json:"sequence"`
	AcceptedAtNs  int64  `json:"acceptedAtNs"`
	CorrelationID string `json:"correlationId"`
}

type rejectedResponse struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

type canvasFrame struct {
	Sequence      uint64          `json:"sequence"`
	OperationID   string          `json:"operationId"`
	ObjectID      string          `json:"objectId"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	CorrelationID string          `json:"correlationId"`
}

// checkStatic reports whether dir can be served at /.
func checkStatic(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("static directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("static directory: %s is not a directory", dir)
	}
	return nil
}

func newProbe(opts options) (*probe, error) {
	if err := checkStatic(opts.static); err != nil {
		return nil, err
	}
	if opts.host == "" {
		return nil, errors.New("host is required")
	}
	if opts.clock == nil {
		start := time.Now()
		opts.clock = func() time.Duration { return time.Since(start) }
	}
	if opts.buffer <= 0 {
		opts.buffer = defaultBuffer
	}
	hub, err := realtime.NewHub(realtime.Config{Authorize: allowAll, LogSize: opts.logSize})
	if err != nil {
		return nil, err
	}
	p := &probe{
		hosts:       hostForms(opts.host),
		clock:       opts.clock,
		hub:         hub,
		mux:         http.NewServeMux(),
		buffer:      opts.buffer,
		subscribers: map[*subscriber]struct{}{},
		stamps:      map[uint64]time.Duration{},
	}
	p.mux.HandleFunc("/probe/ping", p.ping)
	p.mux.HandleFunc("/probe/commands", p.command)
	p.mux.HandleFunc("/probe/events", p.events)
	p.mux.HandleFunc("/probe/", http.NotFound)
	p.mux.Handle("/", http.FileServer(http.Dir(opts.static)))
	return p, nil
}

// hostForms returns the Host values that name the listening address: the
// address itself and, on port 80 only, the address without the default port,
// which clients and browser origins omit.
func hostForms(listen string) []string {
	name, port, err := net.SplitHostPort(listen)
	if err != nil || port != "80" {
		return []string{listen}
	}
	if strings.Contains(name, ":") {
		name = "[" + name + "]"
	}
	return []string{listen, name}
}

// ServeHTTP refuses a request that names another host or comes from another
// origin before any route runs. The harness accepts every command, so a page
// on another origin, or one reached through a rebound hostname, could
// otherwise inject commands into a measurement or read its stream.
func (p *probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !slices.Contains(p.hosts, r.Host) || (origin != "" && !slices.ContainsFunc(p.hosts, func(host string) bool {
		return origin == "http://"+host
	})) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p.mux.ServeHTTP(w, r)
}

// closeStreams disconnects every subscriber and refuses new ones, so an HTTP
// Shutdown is not held open by streams that never go idle.
func (p *probe) closeStreams() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamsClosed = true
	for sub := range p.subscribers {
		p.dropLocked(sub)
	}
}

// Close disconnects every stream and then closes the hub.
func (p *probe) Close(ctx context.Context) error {
	p.closeStreams()
	return p.hub.Close(ctx)
}

func (p *probe) ping(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ServerAtNs int64 `json:"serverAtNs"`
	}{int64(p.clock())})
}

func (p *probe) command(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	req, err := decodeCommand(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := correlation.New()
	if err != nil {
		http.Error(w, "correlation unavailable", http.StatusInternalServerError)
		return
	}
	result, err := p.hub.Submit(correlation.WithID(r.Context(), id), realtime.Command{
		Key:           canvasKey,
		OperationID:   req.OperationID,
		ObjectID:      req.ObjectID,
		Kind:          realtime.Kind(req.Kind),
		Payload:       []byte(req.Payload),
		CorrelationID: id.String(),
	})
	switch {
	case errors.Is(err, realtime.ErrInvalidCommand):
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	case errors.Is(err, realtime.ErrClosed):
		http.Error(w, "hub closed", http.StatusServiceUnavailable)
		return
	case errors.Is(err, realtime.ErrOperationConflict):
		writeJSON(w, http.StatusConflict, rejectedResponse{Outcome: "rejected", Reason: "operation_conflict"})
		return
	case err != nil:
		http.Error(w, "command not applied", http.StatusInternalServerError)
		return
	}
	if result.Outcome != realtime.OutcomeAccepted {
		writeJSON(w, http.StatusConflict, rejectedResponse{Outcome: string(result.Outcome), Reason: string(result.Reason)})
		return
	}
	acceptedAt := p.stamp(result.Sequence)
	p.publish(result.Sequence)
	writeJSON(w, http.StatusOK, acceptedResponse{
		Outcome:       string(result.Outcome),
		Sequence:      result.Sequence,
		AcceptedAtNs:  int64(acceptedAt),
		CorrelationID: result.CorrelationID,
	})
}

// stamp returns the acceptance stamp of sequence. The first acceptance reads
// the clock; a retried operation, which the hub answers with the same
// sequence, gets that first stamp back, so a retry never shortens the
// measured latency. Stamps are kept as long as the hub keeps results for
// deduplication, oldest evicted first.
func (p *probe) stamp(sequence uint64) time.Duration {
	p.stampMu.Lock()
	defer p.stampMu.Unlock()
	if at, ok := p.stamps[sequence]; ok {
		return at
	}
	at := p.clock()
	p.stamps[sequence] = at
	p.stampOrder = append(p.stampOrder, sequence)
	if overflow := len(p.stampOrder) - realtime.DefaultRetentionMaxOperations; overflow > 0 {
		for _, old := range p.stampOrder[:overflow] {
			delete(p.stamps, old)
		}
		p.stampOrder = append([]uint64(nil), p.stampOrder[overflow:]...)
	}
	return at
}

// decodeCommand reads exactly one JSON object with known fields. A set must
// carry an object payload; the hub rejects every other malformed command.
func decodeCommand(body io.Reader) (commandRequest, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var req commandRequest
	if err := decoder.Decode(&req); err != nil {
		return commandRequest{}, errors.New("malformed command body")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return commandRequest{}, errors.New("malformed command body")
	}
	if req.Kind == string(realtime.KindSet) && !bytes.HasPrefix(bytes.TrimSpace(req.Payload), []byte("{")) {
		return commandRequest{}, errors.New("set requires an object payload")
	}
	return req, nil
}

// publish broadcasts every retained event after the last published one, up
// to at least sequence. Concurrent accepted commands publish under one lock,
// so subscribers see each event once and in sequence order.
func (p *probe) publish(sequence uint64) {
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	if sequence <= p.published {
		return
	}
	events, err := p.hub.Events(context.Background(), canvasKey, p.published)
	if err != nil {
		log.Printf("latencyprobe: publish after sequence %d: %v", p.published, err)
		return
	}
	for _, event := range events {
		if event.Sequence <= p.published {
			continue
		}
		if frame, err := encodeFrame(event); err != nil {
			log.Printf("latencyprobe: encode event %d: %v", event.Sequence, err)
		} else {
			p.broadcast(frame)
		}
		p.published = event.Sequence
	}
}

// encodeFrame renders event as an SSE canvas frame whose id is its sequence,
// so a reconnecting EventSource sends it back as Last-Event-ID.
func encodeFrame(event realtime.Event) ([]byte, error) {
	data, err := json.Marshal(canvasFrame{
		Sequence:      event.Sequence,
		OperationID:   event.OperationID,
		ObjectID:      event.ObjectID,
		Kind:          string(event.Kind),
		Payload:       json.RawMessage(event.Payload),
		CorrelationID: event.CorrelationID,
	})
	if err != nil {
		return nil, err
	}
	frame := fmt.Appendf(nil, "id: %d\nevent: canvas\ndata: ", event.Sequence)
	return append(append(frame, data...), "\n\n"...), nil
}

// broadcast queues frame for every subscriber and disconnects any whose
// queue is full instead of waiting for it.
func (p *probe) broadcast(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range p.subscribers {
		select {
		case sub.frames <- frame:
		default:
			p.dropLocked(sub)
		}
	}
}

func (p *probe) dropLocked(sub *subscriber) {
	delete(p.subscribers, sub)
	close(sub.gone)
	sub.abort()
}

func (p *probe) events(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	var after uint64
	resuming := false
	if cursor := r.Header.Get("Last-Event-ID"); cursor != "" {
		parsed, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			http.Error(w, "malformed Last-Event-ID", http.StatusBadRequest)
			return
		}
		after, resuming = parsed, true
	}
	controller := http.NewResponseController(w)
	sub := &subscriber{
		frames: make(chan []byte, p.buffer),
		gone:   make(chan struct{}),
		abort:  func() { _ = controller.SetWriteDeadline(time.Now()) },
	}
	replay, status := p.subscribe(sub, resuming, after)
	switch status {
	case http.StatusOK:
	case http.StatusNoContent:
		w.WriteHeader(status)
		return
	case http.StatusServiceUnavailable:
		http.Error(w, "streams closed", status)
		return
	default:
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer func() {
		p.mu.Lock()
		delete(p.subscribers, sub)
		p.mu.Unlock()
	}()

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	for _, frame := range replay {
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	if controller.Flush() != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.gone:
			return
		case frame := <-sub.frames:
			if _, err := w.Write(frame); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}

// subscribe registers sub and, for a reconnecting stream, returns the
// retained events after the cursor that were already broadcast. Holding
// publishMu means no broadcast runs between that snapshot and the
// registration, so each later event reaches sub once, live. A cursor the hub
// no longer retains answers 204: the stream ends without replay, which stops
// EventSource from retrying, and the render wait still times out.
func (p *probe) subscribe(sub *subscriber, resuming bool, after uint64) ([][]byte, int) {
	p.publishMu.Lock()
	defer p.publishMu.Unlock()
	var replay [][]byte
	if resuming {
		events, err := p.hub.Events(context.Background(), canvasKey, after)
		switch {
		case errors.Is(err, realtime.ErrResyncRequired):
			return nil, http.StatusNoContent
		case err != nil:
			log.Printf("latencyprobe: replay after sequence %d: %v", after, err)
			return nil, http.StatusInternalServerError
		}
		for _, event := range events {
			if event.Sequence > p.published {
				break
			}
			frame, err := encodeFrame(event)
			if err != nil {
				log.Printf("latencyprobe: encode event %d: %v", event.Sequence, err)
				return nil, http.StatusInternalServerError
			}
			replay = append(replay, frame)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.streamsClosed {
		return nil, http.StatusServiceUnavailable
	}
	p.subscribers[sub] = struct{}{}
	return replay, http.StatusOK
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
