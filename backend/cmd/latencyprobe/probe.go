package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
}

// probe is the TE-010 harness: ping, commands on a real realtime.Hub, an SSE
// stream of accepted events, and static files.
type probe struct {
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

func newProbe(opts options) (*probe, error) {
	info, err := os.Stat(opts.static)
	if err != nil {
		return nil, fmt.Errorf("static directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("static directory: %s is not a directory", opts.static)
	}
	if opts.clock == nil {
		start := time.Now()
		opts.clock = func() time.Duration { return time.Since(start) }
	}
	if opts.buffer <= 0 {
		opts.buffer = defaultBuffer
	}
	hub, err := realtime.NewHub(realtime.Config{Authorize: allowAll})
	if err != nil {
		return nil, err
	}
	p := &probe{
		clock:       opts.clock,
		hub:         hub,
		mux:         http.NewServeMux(),
		buffer:      opts.buffer,
		subscribers: map[*subscriber]struct{}{},
	}
	p.mux.HandleFunc("/probe/ping", p.ping)
	p.mux.HandleFunc("/probe/commands", p.command)
	p.mux.HandleFunc("/probe/events", p.events)
	p.mux.HandleFunc("/probe/", http.NotFound)
	p.mux.Handle("/", http.FileServer(http.Dir(opts.static)))
	return p, nil
}

func (p *probe) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

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
	acceptedAt := p.clock()
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
	p.publish(result.Sequence)
	writeJSON(w, http.StatusOK, acceptedResponse{
		Outcome:       string(result.Outcome),
		Sequence:      result.Sequence,
		AcceptedAtNs:  int64(acceptedAt),
		CorrelationID: result.CorrelationID,
	})
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
		data, err := json.Marshal(canvasFrame{
			Sequence:      event.Sequence,
			OperationID:   event.OperationID,
			ObjectID:      event.ObjectID,
			Kind:          string(event.Kind),
			Payload:       json.RawMessage(event.Payload),
			CorrelationID: event.CorrelationID,
		})
		if err != nil {
			log.Printf("latencyprobe: encode event %d: %v", event.Sequence, err)
		} else {
			p.broadcast(append(append([]byte("event: canvas\ndata: "), data...), "\n\n"...))
		}
		p.published = event.Sequence
	}
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
	controller := http.NewResponseController(w)
	sub := &subscriber{
		frames: make(chan []byte, p.buffer),
		gone:   make(chan struct{}),
		abort:  func() { _ = controller.SetWriteDeadline(time.Now()) },
	}
	p.mu.Lock()
	if p.streamsClosed {
		p.mu.Unlock()
		http.Error(w, "streams closed", http.StatusServiceUnavailable)
		return
	}
	p.subscribers[sub] = struct{}{}
	p.mu.Unlock()
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
