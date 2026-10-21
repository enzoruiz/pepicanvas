package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrInvalidConfig reports a publisher configuration that cannot run.
	ErrInvalidConfig = errors.New("outbox: invalid config")
	// ErrClosed reports a publisher that was closed.
	ErrClosed = errors.New("outbox: publisher closed")
	// ErrUndeliverable marks a delivery the sink can never accept. The record
	// settles as rejected and is not retried; clients recover its state by
	// resynchronization.
	ErrUndeliverable = errors.New("outbox: undeliverable")
	// ErrInvalidEvent marks a committed event the actor rejects as an invalid
	// command. It is undeliverable and signals that the canvas and actor
	// limits disagree, so the publisher counts it apart and logs it as an
	// error.
	ErrInvalidEvent = errors.New("outbox: event invalid for the actor")
)

// Outcome is how a record settled.
type Outcome string

const (
	OutcomeDelivered Outcome = "delivered"
	OutcomeRejected  Outcome = "rejected"
)

// MaxErrorBytes bounds the stored text of the last delivery error.
const MaxErrorBytes = 512

// Store persists outbox records. Settle and Retry ignore records that are
// already settled.
type Store interface {
	// Heads returns the oldest pending record of up to limit canvases, the
	// records due soonest first. A record that cannot be decoded is returned
	// with Malformed set, so it never fails the batch.
	Heads(ctx context.Context, limit int) ([]Record, error)
	// Settle marks a pending record delivered or rejected.
	Settle(ctx context.Context, id int64, outcome Outcome, at time.Time, reason string) error
	// Retry records a failed delivery and when to try again.
	Retry(ctx context.Context, id int64, attempts int, next time.Time, reason string) error
	// Purge deletes up to limit records settled at or before cutoff.
	Purge(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// Sink delivers one committed event to the realtime actor of its canvas.
// A nil error means the actor answered; an error wrapping ErrUndeliverable
// means it never will; any other error is retried. Redelivering a record
// must not repeat its effect.
type Sink interface {
	Deliver(ctx context.Context, record Record) error
}

// Resyncer is implemented by a sink whose clients must resynchronize once the
// publisher stops, because their event stream no longer follows the commits.
// Close calls RequireResync once, after the last delivery.
type Resyncer interface {
	RequireResync()
}

// Defaults applied when a Config field is zero.
const (
	DefaultPollInterval   = time.Second
	DefaultAttemptTimeout = 5 * time.Second
	DefaultMinBackoff     = 100 * time.Millisecond
	DefaultMaxBackoff     = 30 * time.Second
	DefaultBatchSize      = 100
	DefaultRetention      = 24 * time.Hour
	DefaultPurgeBatch     = 1000
)

// Config configures a Publisher. Store and Sink are required.
type Config struct {
	Store  Store
	Sink   Sink
	Logger *slog.Logger
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
	// PollInterval bounds how long a pending record waits without a Notify,
	// including the records found pending at start.
	PollInterval time.Duration
	// AttemptTimeout bounds one delivery.
	AttemptTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait after a failed delivery; it
	// doubles per attempt.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// BatchSize bounds the canvases served per pass.
	BatchSize int
	// Retention is how long settled records are kept before purge.
	Retention time.Duration
	// PurgeBatch bounds the records one purge deletes.
	PurgeBatch int
}

// Counters are totals since the publisher started.
type Counters struct {
	Delivered uint64
	Rejected  uint64
	Retries   uint64
	Purged    uint64
	// Invalid counts rejected records the actor found invalid; any is a bug.
	Invalid uint64
	// StoreErrors counts failed store calls; records stay pending.
	StoreErrors uint64
}

// Publisher delivers pending outbox records, oldest first per canvas. A
// record that fails blocks only the later records of its canvas.
type Publisher struct {
	config Config
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	// abort cancels the delivery in flight when Close runs out of time.
	abort       context.Context
	cancelAbort context.CancelFunc

	mu      sync.Mutex
	started bool
	closed  bool

	delivered, rejected, retries, purged, invalid, storeErrors atomic.Uint64
}

// NewPublisher validates the configuration. Start runs it.
func NewPublisher(config Config) (*Publisher, error) {
	if config.Store == nil || config.Sink == nil {
		return nil, errors.Join(ErrInvalidConfig, errors.New("store and sink are required"))
	}
	if config.PollInterval < 0 || config.AttemptTimeout < 0 || config.MinBackoff < 0 || config.MaxBackoff < 0 ||
		config.BatchSize < 0 || config.Retention < 0 || config.PurgeBatch < 0 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("durations and sizes must not be negative"))
	}
	defaults := []struct {
		field *time.Duration
		value time.Duration
	}{
		{&config.PollInterval, DefaultPollInterval},
		{&config.AttemptTimeout, DefaultAttemptTimeout},
		{&config.MinBackoff, DefaultMinBackoff},
		{&config.MaxBackoff, DefaultMaxBackoff},
		{&config.Retention, DefaultRetention},
	}
	for _, d := range defaults {
		if *d.field == 0 {
			*d.field = d.value
		}
	}
	if config.MinBackoff > config.MaxBackoff {
		return nil, errors.Join(ErrInvalidConfig, errors.New("min backoff exceeds max backoff"))
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultBatchSize
	}
	if config.PurgeBatch == 0 {
		config.PurgeBatch = DefaultPurgeBatch
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	abort, cancelAbort := context.WithCancel(context.Background())
	return &Publisher{
		config: config,
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		abort:  abort, cancelAbort: cancelAbort,
	}, nil
}

// Start runs the delivery loop. Records left pending by an earlier process
// are delivered on the first pass.
func (p *Publisher) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.started {
		return errors.New("outbox: publisher already started")
	}
	p.started = true
	go p.run()
	return nil
}

// Notify wakes the loop after a commit. It never blocks.
func (p *Publisher) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Running reports whether the loop is started and not closed.
func (p *Publisher) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started && !p.closed
}

// Counters returns the totals since start.
func (p *Publisher) Counters() Counters {
	return Counters{
		Delivered:   p.delivered.Load(),
		Rejected:    p.rejected.Load(),
		Retries:     p.retries.Load(),
		Purged:      p.purged.Load(),
		Invalid:     p.invalid.Load(),
		StoreErrors: p.storeErrors.Load(),
	}
}

// Close stops the loop with the contract of realtime.Hub.Close: no new
// delivery starts, the delivery in flight finishes, and Close waits for the
// loop to exit. If ctx ends first, the delivery in flight is cancelled and
// ctx's error is returned once the loop exited. Undelivered records stay
// pending for the next start, and a sink that is a Resyncer is told once
// that its clients must resync. Close is idempotent.
func (p *Publisher) Close(ctx context.Context) error {
	p.mu.Lock()
	first := !p.closed
	p.closed = true
	started := p.started
	p.mu.Unlock()
	if !started {
		if first {
			p.requireResync()
		}
		return nil
	}
	if first {
		close(p.stop)
		// Both branches below return only after the loop exited.
		defer p.requireResync()
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		p.cancelAbort()
		<-p.done
		return ctx.Err()
	}
}

func (p *Publisher) requireResync() {
	if resyncer, ok := p.config.Sink.(Resyncer); ok {
		resyncer.RequireResync()
	}
}

func (p *Publisher) run() {
	defer close(p.done)
	for {
		progressed, wait := p.pass()
		if p.stopping() {
			return
		}
		if progressed {
			continue
		}
		p.purge()
		timer := time.NewTimer(wait)
		select {
		case <-p.stop:
			timer.Stop()
			return
		case <-p.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (p *Publisher) stopping() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// pass delivers every due head once. It reports whether any record settled
// and how long to wait for the next due record.
func (p *Publisher) pass() (bool, time.Duration) {
	wait := p.config.PollInterval
	heads, err := p.config.Store.Heads(p.abort, p.config.BatchSize)
	if err != nil {
		p.storeFailed("read pending records", err)
		return false, wait
	}
	progressed := false
	for _, record := range heads {
		if p.stopping() {
			break
		}
		now := p.config.Now()
		if due := record.NextAttempt.Sub(now); due > 0 {
			wait = min(wait, due)
			continue
		}
		settled, retryIn := p.deliver(record)
		progressed = progressed || settled
		if retryIn > 0 {
			wait = min(wait, retryIn)
		}
	}
	return progressed, wait
}

// deliver runs one attempt and records its outcome. It returns whether the
// record settled, or the backoff before its next attempt. A malformed record
// is rejected without reaching the sink.
func (p *Publisher) deliver(record Record) (bool, time.Duration) {
	err := record.Malformed
	if err == nil {
		ctx, cancel := context.WithTimeout(p.abort, p.config.AttemptTimeout)
		err = p.config.Sink.Deliver(ctx, record)
		cancel()
		if p.abort.Err() != nil {
			// Close ran out of time: the record stays pending as it was.
			return false, 0
		}
	}
	log := p.config.Logger.With(
		slog.Int64("outbox_id", record.ID),
		slog.String("tenant_id", record.Event.Key.TenantID),
		slog.String("canvas_id", record.Event.Key.CanvasID),
		slog.Int64("revision", record.Event.Result.Revision),
		slog.String("correlation_id", record.Event.CorrelationID),
	)
	now := p.config.Now()
	switch {
	case err == nil:
		if p.settle(record, OutcomeDelivered, now, "") {
			p.delivered.Add(1)
			log.Info("outbox record delivered")
			return true, 0
		}
	case errors.Is(err, ErrUndeliverable) || record.Malformed != nil:
		if p.settle(record, OutcomeRejected, now, err.Error()) {
			p.rejected.Add(1)
			if errors.Is(err, ErrInvalidEvent) {
				p.invalid.Add(1)
				log.Error("outbox record invalid for the actor", slog.String("error", truncate(err.Error())))
				return true, 0
			}
			log.Warn("outbox record rejected", slog.String("error", truncate(err.Error())))
			return true, 0
		}
	default:
		attempts := record.Attempts + 1
		backoff := p.backoff(attempts)
		storeCtx, cancel := context.WithTimeout(p.abort, p.config.AttemptTimeout)
		defer cancel()
		if storeErr := p.config.Store.Retry(storeCtx, record.ID, attempts, now.Add(backoff), truncate(err.Error())); storeErr != nil {
			p.storeFailed("record retry", storeErr)
			return false, p.config.MinBackoff
		}
		p.retries.Add(1)
		log.Warn("outbox delivery failed", slog.Int("attempts", attempts), slog.Duration("backoff", backoff), slog.String("error", truncate(err.Error())))
		return false, backoff
	}
	return false, p.config.MinBackoff
}

func (p *Publisher) settle(record Record, outcome Outcome, at time.Time, reason string) bool {
	ctx, cancel := context.WithTimeout(p.abort, p.config.AttemptTimeout)
	defer cancel()
	if err := p.config.Store.Settle(ctx, record.ID, outcome, at, truncate(reason)); err != nil {
		p.storeFailed("settle record", err)
		return false
	}
	return true
}

func (p *Publisher) purge() {
	ctx, cancel := context.WithTimeout(p.abort, p.config.AttemptTimeout)
	defer cancel()
	purged, err := p.config.Store.Purge(ctx, p.config.Now().Add(-p.config.Retention), p.config.PurgeBatch)
	if err != nil {
		p.storeFailed("purge settled records", err)
		return
	}
	if purged > 0 {
		p.purged.Add(uint64(purged))
		p.config.Logger.Info("outbox settled records purged", slog.Int("count", purged))
	}
}

// backoff doubles from MinBackoff per failed attempt, capped at MaxBackoff.
func (p *Publisher) backoff(attempts int) time.Duration {
	delay := p.config.MinBackoff
	for i := 1; i < attempts && delay < p.config.MaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, p.config.MaxBackoff)
}

func (p *Publisher) storeFailed(action string, err error) {
	if p.abort.Err() != nil {
		return
	}
	p.storeErrors.Add(1)
	p.config.Logger.Error("outbox store failed", slog.String("action", action), slog.String("error", truncate(err.Error())))
}

// truncate bounds error text to MaxErrorBytes without splitting a rune.
func truncate(text string) string {
	if len(text) <= MaxErrorBytes {
		return text
	}
	cut := MaxErrorBytes
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut]
}
