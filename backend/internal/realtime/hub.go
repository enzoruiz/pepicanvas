package realtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrInvalidConfig reports a hub configuration that cannot fail closed.
	ErrInvalidConfig = errors.New("realtime: invalid config")
	// ErrClosed reports work submitted after the hub stopped accepting it.
	ErrClosed = errors.New("realtime: hub closed")
	// ErrResyncRequired reports a replay cursor older than the retained events.
	ErrResyncRequired = errors.New("realtime: resync required")
)

// Authorizer re-evaluates current access immediately before acceptance.
// A non-nil error rejects the command without ordering or retaining it.
type Authorizer func(context.Context, Command) error

// Defaults applied when a Config field is zero.
const (
	DefaultQueueSize = 64
	DefaultLogSize   = 1024
)

// Config configures a Hub. Authorize is required.
type Config struct {
	Authorize Authorizer
	// QueueSize bounds the pending commands per canvas.
	QueueSize int
	// LogSize bounds the accepted events retained per canvas for replay.
	LogSize int
	// Retention bounds the operation results kept per canvas for deduplication.
	Retention Retention
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
}

// Hub routes commands to one actor per canvas.
type Hub struct {
	config Config
	stop   chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	actors map[Key]*actor
	closed bool
}

type request struct {
	ctx     context.Context
	command Command
	reply   chan reply
}

type reply struct {
	result Result
	err    error
}

// NewHub validates the configuration and returns a running hub.
func NewHub(config Config) (*Hub, error) {
	if config.Authorize == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("authorizer is required"))
	}
	if config.QueueSize < 0 || config.LogSize < 0 || config.Retention.TTL < 0 || config.Retention.MaxOperations < 0 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("sizes must not be negative"))
	}
	if config.QueueSize == 0 {
		config.QueueSize = DefaultQueueSize
	}
	if config.LogSize == 0 {
		config.LogSize = DefaultLogSize
	}
	if config.Retention.TTL == 0 {
		config.Retention.TTL = DefaultRetentionTTL
	}
	if config.Retention.MaxOperations == 0 {
		config.Retention.MaxOperations = DefaultRetentionMaxOperations
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Hub{config: config, stop: make(chan struct{}), actors: map[Key]*actor{}}, nil
}

// Submit validates, authorizes, orders, and applies one command. A caller
// whose context ends after enqueueing may retry with the same operation ID.
func (h *Hub) Submit(ctx context.Context, command Command) (Result, error) {
	if err := command.validate(); err != nil {
		return Result{}, err
	}
	command.Payload = clonePayload(command.Payload)
	target, err := h.actor(command.Key)
	if err != nil {
		return Result{}, err
	}
	replies := make(chan reply, 1)
	select {
	case target.inbox <- request{ctx: ctx, command: command, reply: replies}:
	case <-h.stop:
		return Result{}, ErrClosed
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	select {
	case answer := <-replies:
		return answer.result, answer.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Events returns the retained accepted events after the given sequence.
func (h *Hub) Events(_ context.Context, key Key, after uint64) ([]Event, error) {
	h.mu.Lock()
	target := h.actors[key]
	h.mu.Unlock()
	if target == nil {
		return nil, nil
	}
	return target.events(after)
}

// Close stops every actor and waits for them to exit or for ctx to end.
func (h *Hub) Close(ctx context.Context) error {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.stop)
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hub) actor(key Key) (*actor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	target, ok := h.actors[key]
	if !ok {
		target = &actor{
			config: h.config,
			inbox:  make(chan request, h.config.QueueSize),
			state:  NewState(),
			ops:    newOperations(h.config.Retention),
		}
		h.actors[key] = target
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			target.run(h.stop)
		}()
	}
	return target, nil
}
