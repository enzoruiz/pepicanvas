package subscription

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

var (
	// ErrInvalidRelayConfig reports a cut relay configuration that cannot run.
	ErrInvalidRelayConfig = errors.New("subscription: invalid cut relay config")
	// ErrRelayClosed reports a cut relay that was closed.
	ErrRelayClosed = errors.New("subscription: cut relay closed")
)

// Cutter applies one confirmed access cut to the live connections.
// *revocation.Registry implements it.
type Cutter interface {
	Apply(ctx context.Context, cut revocation.Cut) (revocation.Report, error)
}

// Defaults applied when a RelayConfig field is zero.
const (
	DefaultRelayPollInterval   = time.Second
	DefaultRelayAttemptTimeout = 5 * time.Second
	DefaultRelayMinBackoff     = 100 * time.Millisecond
	DefaultRelayMaxBackoff     = 5 * time.Second
	DefaultRelayBatchSize      = 100
)

// RelayConfig configures a CutRelay. Store and Cutter are required.
type RelayConfig struct {
	Store  CutStore
	Cutter Cutter
	// Logger records each outcome with the cut's correlation_id attribute.
	Logger *slog.Logger
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
	// Correlate returns ctx carrying the cut's correlation ID, so the
	// Cutter's own logs carry it too. It defaults to returning ctx.
	Correlate func(ctx context.Context, correlationID string) context.Context
	// PollInterval bounds how long a pending cut waits without a Notify,
	// including the cuts found pending at start.
	PollInterval time.Duration
	// AttemptTimeout bounds one Apply and one store call.
	AttemptTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait after a failed pass; it
	// doubles per consecutive failure.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// BatchSize bounds the cuts applied per pass.
	BatchSize int
}

// RelayCounters are totals since the relay started.
type RelayCounters struct {
	// Applied counts cuts applied and settled, Exceeded among them.
	Applied  uint64
	Exceeded uint64
	// Rejected counts cuts the Cutter can never apply; any is a bug.
	Rejected uint64
	// Retries counts failed applies; their cuts stay pending.
	Retries uint64
	// StoreErrors counts failed store calls; their cuts stay pending.
	StoreErrors uint64
	// Unknown counts cuts of a kind this release does not know, once per
	// cut; they stay pending for a release that knows it.
	Unknown uint64
}

// CutRelay applies the pending access cuts of the cut outbox to the live
// connections, oldest first, and settles each one once applied. A cut stays
// pending until then, so a cut committed by a process that stopped before
// applying it is applied by the next relay (CTRL-CONS-01). Applying a cut
// twice has one effect: its operation ID is "cut-<id>", which the Cutter
// deduplicates.
type CutRelay struct {
	config RelayConfig
	// failures counts consecutive failed passes. Only the loop uses it.
	failures int
	// unknownLogged holds the pending cuts of an unknown kind already
	// logged, among those of the last pass, so each is logged once. Only
	// the loop uses it.
	unknownLogged map[int64]struct{}
	wake          chan struct{}
	stop          chan struct{}
	done          chan struct{}
	// abort cancels the apply in flight when Close runs out of time.
	abort       context.Context
	cancelAbort context.CancelFunc

	mu      sync.Mutex
	started bool
	closed  bool

	applied, exceeded, rejected, retries, storeErrors, unknown atomic.Uint64
}

// NewCutRelay validates the configuration. Start runs it.
func NewCutRelay(config RelayConfig) (*CutRelay, error) {
	if config.Store == nil || config.Cutter == nil {
		return nil, errors.Join(ErrInvalidRelayConfig, errors.New("store and cutter are required"))
	}
	if config.PollInterval < 0 || config.AttemptTimeout < 0 || config.MinBackoff < 0 || config.MaxBackoff < 0 || config.BatchSize < 0 {
		return nil, errors.Join(ErrInvalidRelayConfig, errors.New("durations and sizes must not be negative"))
	}
	defaults := []struct {
		field *time.Duration
		value time.Duration
	}{
		{&config.PollInterval, DefaultRelayPollInterval},
		{&config.AttemptTimeout, DefaultRelayAttemptTimeout},
		{&config.MinBackoff, DefaultRelayMinBackoff},
		{&config.MaxBackoff, DefaultRelayMaxBackoff},
	}
	for _, d := range defaults {
		if *d.field == 0 {
			*d.field = d.value
		}
	}
	if config.MinBackoff > config.MaxBackoff {
		return nil, errors.Join(ErrInvalidRelayConfig, errors.New("min backoff exceeds max backoff"))
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultRelayBatchSize
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Correlate == nil {
		config.Correlate = func(ctx context.Context, _ string) context.Context { return ctx }
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	abort, cancelAbort := context.WithCancel(context.Background())
	return &CutRelay{
		config: config,
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		abort:  abort, cancelAbort: cancelAbort,
	}, nil
}

// Start runs the relay loop. Cuts left pending by an earlier process are
// applied on the first pass.
func (r *CutRelay) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRelayClosed
	}
	if r.started {
		return errors.New("subscription: cut relay already started")
	}
	r.started = true
	go r.run()
	return nil
}

// Notify wakes the loop after a commit that wrote a cut. It never blocks.
func (r *CutRelay) Notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Running reports whether the loop is started and not closed.
func (r *CutRelay) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && !r.closed
}

// Counters returns the totals since start.
func (r *CutRelay) Counters() RelayCounters {
	return RelayCounters{
		Applied:     r.applied.Load(),
		Exceeded:    r.exceeded.Load(),
		Rejected:    r.rejected.Load(),
		Retries:     r.retries.Load(),
		StoreErrors: r.storeErrors.Load(),
		Unknown:     r.unknown.Load(),
	}
}

// Close stops the loop with the contract of realtime.Hub.Close: no new
// apply starts, the apply in flight finishes, and Close waits for the loop
// to exit. If ctx ends first, the apply in flight is cancelled and ctx's
// error is returned once the loop exited. Cuts not yet settled stay pending
// for the next start. Close is idempotent.
func (r *CutRelay) Close(ctx context.Context) error {
	r.mu.Lock()
	first := !r.closed
	r.closed = true
	started := r.started
	r.mu.Unlock()
	if !started {
		return nil
	}
	if first {
		close(r.stop)
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		r.cancelAbort()
		<-r.done
		return ctx.Err()
	}
}

func (r *CutRelay) run() {
	defer close(r.done)
	for {
		wait := r.pass()
		if r.stopping() {
			return
		}
		if wait == 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-r.stop:
			timer.Stop()
			return
		case <-r.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (r *CutRelay) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// pass applies up to BatchSize pending cuts, oldest first. It returns zero
// when a full batch settled, so a backlog does not wait for the poll, the
// backoff when a cut stayed pending, or the poll interval.
func (r *CutRelay) pass() time.Duration {
	// The pass reads the store after every commit notified so far.
	select {
	case <-r.wake:
	default:
	}
	ctx, cancel := context.WithTimeout(r.abort, r.config.AttemptTimeout)
	cuts, err := r.config.Store.PendingCuts(ctx, r.config.BatchSize)
	cancel()
	if err != nil {
		r.storeFailed("read pending cuts", err)
		return r.backoff()
	}
	logged := make(map[int64]struct{})
	defer func() { r.unknownLogged = logged }()
	skipped := false
	for _, pending := range cuts {
		if r.stopping() {
			return 0
		}
		cut, err := pending.cut()
		if err != nil {
			// Applying it as anything else could cut the wrong connections,
			// and settling it would lose it for a release that knows its
			// kind. It stays pending and does not hold back later cuts.
			r.skipUnknown(pending, logged, err)
			skipped = true
			continue
		}
		if !r.deliver(pending, cut) {
			return r.backoff()
		}
	}
	r.failures = 0
	if len(cuts) == r.config.BatchSize && !skipped {
		return 0
	}
	return r.config.PollInterval
}

// skipUnknown leaves a cut of an unknown kind pending and logs it once, with
// its kind and never its account.
func (r *CutRelay) skipUnknown(pending PendingCut, logged map[int64]struct{}, err error) {
	logged[pending.ID] = struct{}{}
	if _, seen := r.unknownLogged[pending.ID]; seen {
		return
	}
	r.unknown.Add(1)
	r.cutLog(pending).Warn("access cut kind unknown", slog.String("error", err.Error()))
}

func (r *CutRelay) cutLog(pending PendingCut) *slog.Logger {
	return r.config.Logger.With(
		slog.Int64("cut_id", pending.ID),
		slog.String("kind", string(pending.Kind)),
		slog.String("tenant_id", pending.TenantID),
		slog.Uint64("version", pending.Version),
		slog.String("correlation_id", pending.CorrelationID),
	)
}

// deliver applies one cut and records its outcome. It reports whether the
// cut settled. A cut the Cutter applied, even over its close budget, or can
// never apply settles; any other failure leaves it pending for a retry.
func (r *CutRelay) deliver(pending PendingCut, cut revocation.Cut) bool {
	log := r.cutLog(pending)
	ctx, cancel := context.WithTimeout(r.config.Correlate(r.abort, pending.CorrelationID), r.config.AttemptTimeout)
	report, err := r.config.Cutter.Apply(ctx, cut)
	cancel()
	if r.abort.Err() != nil {
		// Close ran out of time: the cut stays pending as it was.
		return false
	}
	switch {
	case err == nil || errors.Is(err, revocation.ErrBudgetExceeded):
		if !r.settle(pending) {
			return false
		}
		r.applied.Add(1)
		attrs := []any{slog.Int("affected", len(report.Closures)), slog.Int64("max_latency_ms", report.MaxLatency.Milliseconds())}
		if err != nil {
			r.exceeded.Add(1)
			log.Error("access cut applied over the close budget", append(attrs, slog.Int("exceeded", report.Exceeded))...)
			return true
		}
		log.Info("access cut applied", attrs...)
	case errors.Is(err, revocation.ErrInvalidCut) || errors.Is(err, revocation.ErrOperationConflict):
		// Retrying cannot succeed.
		return r.reject(pending, log, err)
	default:
		r.retries.Add(1)
		log.Warn("access cut apply failed", slog.String("error", err.Error()))
		return false
	}
	return true
}

// cut maps a pending cut to the registry's cut: a deactivation targets the
// whole tenant, a revocation the revoked account in that tenant.
func (p PendingCut) cut() (revocation.Cut, error) {
	cut := revocation.Cut{
		OperationID:   fmt.Sprintf("cut-%d", p.ID),
		Version:       p.Version,
		CorrelationID: p.CorrelationID,
	}
	switch p.Kind {
	case CutDeactivation:
		cut.Type = revocation.TypeDeactivation
		cut.Target = revocation.Target{Kind: revocation.TargetTenant, TenantID: p.TenantID}
	case CutRevocation:
		cut.Type = revocation.TypeRevocation
		cut.Target = revocation.Target{Kind: revocation.TargetAccount, TenantID: p.TenantID, ID: string(p.AccountID)}
	default:
		return revocation.Cut{}, fmt.Errorf("subscription: unknown cut kind %q", p.Kind)
	}
	return cut, nil
}

// reject settles a cut that can never be applied and logs it as an error.
func (r *CutRelay) reject(pending PendingCut, log *slog.Logger, err error) bool {
	if !r.settle(pending) {
		return false
	}
	r.rejected.Add(1)
	log.Error("access cut rejected", slog.String("error", err.Error()))
	return true
}

func (r *CutRelay) settle(pending PendingCut) bool {
	ctx, cancel := context.WithTimeout(r.abort, r.config.AttemptTimeout)
	defer cancel()
	if err := r.config.Store.SettleCut(ctx, pending.ID, r.config.Now()); err != nil {
		// The cut stays pending; applying it again has no second effect.
		r.storeFailed("settle cut", err)
		return false
	}
	return true
}

// backoff counts a failed pass and returns the wait before the next one,
// doubling from MinBackoff per consecutive failure, capped at MaxBackoff.
func (r *CutRelay) backoff() time.Duration {
	r.failures++
	delay := r.config.MinBackoff
	for i := 1; i < r.failures && delay < r.config.MaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, r.config.MaxBackoff)
}

func (r *CutRelay) storeFailed(action string, err error) {
	if r.abort.Err() != nil {
		return
	}
	r.storeErrors.Add(1)
	r.config.Logger.Error("access cut store failed", slog.String("action", action), slog.String("error", err.Error()))
}
