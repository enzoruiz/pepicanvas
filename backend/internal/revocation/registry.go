package revocation

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

var (
	// ErrInvalidConfig reports a registry configuration that cannot fail closed.
	ErrInvalidConfig = errors.New("revocation: invalid config")
	// ErrAccessRevoked reports a grant or lease cut by a confirmed change.
	ErrAccessRevoked = errors.New("revocation: access revoked")
	// ErrStaleGrant reports a grant older than the retained cut history; the
	// caller must read the access facts again.
	ErrStaleGrant = errors.New("revocation: stale grant")
	// ErrBudgetExceeded reports a cut that closed a connection after the
	// budget. The cut is still fully applied.
	ErrBudgetExceeded = errors.New("revocation: close budget exceeded")
	// ErrClosed reports work submitted after the registry began shutting
	// down, and ends every lease the shutdown closed.
	ErrClosed = errors.New("revocation: registry closed")
)

// Defaults applied when a Config field is zero.
const (
	DefaultBudget  = 2 * time.Second
	DefaultHistory = 4096
)

// Conn is the transport side of one live connection.
type Conn interface {
	// Close sends reason and closes gracefully. It must return once ctx ends.
	Close(ctx context.Context, reason Reason) error
	// Abort terminates the connection at once. It may run concurrently with
	// Close and must make a pending Close return promptly: the registry
	// waits for both, so a hung Abort or Close blocks the cut or shutdown.
	Abort()
}

// Config configures a Registry.
type Config struct {
	// Budget bounds the time from a cut reaching the registry to every
	// affected connection being closed. Graceful closes get three quarters
	// of it; the rest is reserved for aborting.
	Budget time.Duration
	// History bounds the cuts retained per tenant to reject stale grants.
	History int
	// Retention bounds the cut results kept per tenant for deduplication.
	// Its TTL must not be shorter than Budget.
	Retention Retention
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
	// Logger records each confirmation and closure. It should be built on
	// platform/logging so records carry the context's correlation_id and
	// are redacted. It defaults to discarding.
	Logger *slog.Logger
}

// Closure reports how one affected connection was closed.
type Closure struct {
	Client   Client
	Latency  time.Duration
	Aborted  bool
	Exceeded bool
}

// Report is the authoritative answer for one applied cut.
type Report struct {
	OperationID   string
	Type          Type
	Target        Target
	Version       uint64
	CorrelationID string
	Closures      []Closure
	MaxLatency    time.Duration
	Exceeded      int
}

// Registry tracks live connections by tenant and applies cuts to them.
type Registry struct {
	config Config
	// abort makes every pending close abort when Close runs out of time.
	abort       context.Context
	cancelAbort context.CancelFunc
	// work counts cuts and the shutdown still closing connections.
	work sync.WaitGroup

	mu      sync.Mutex
	tenants map[string]*tenant
	closed  bool
}

type tenant struct {
	leases  map[*Lease]struct{}
	history history
	ops     *operations
}

// NewRegistry validates the configuration and returns an empty registry.
func NewRegistry(config Config) (*Registry, error) {
	if config.Budget < 0 || config.History < 0 || config.Retention.TTL < 0 || config.Retention.MaxOperations < 0 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("budget and history must not be negative"))
	}
	if config.Budget == 0 {
		config.Budget = DefaultBudget
	}
	if config.History == 0 {
		config.History = DefaultHistory
	}
	if config.Retention.TTL == 0 {
		config.Retention.TTL = DefaultRetentionTTL
	}
	if config.Retention.MaxOperations == 0 {
		config.Retention.MaxOperations = DefaultRetentionMaxOperations
	}
	if config.Retention.TTL < config.Budget {
		return nil, errors.Join(ErrInvalidConfig, errors.New("retention must outlast the close budget"))
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	abort, cancelAbort := context.WithCancel(context.Background())
	return &Registry{config: config, abort: abort, cancelAbort: cancelAbort, tenants: map[string]*tenant{}}, nil
}

// Register admits a connection authorized by grant, or rejects it when a
// cut confirmed after the grant was read already applies to it.
func (r *Registry) Register(grant Grant, conn Conn) (*Lease, error) {
	if err := grant.validate(); err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.Join(ErrInvalidGrant, errors.New("connection is required"))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	state := r.tenant(grant.TenantID)
	if err := state.history.check(grant); err != nil {
		return nil, err
	}
	lease := newLease(r, grant, conn)
	state.leases[lease] = struct{}{}
	return lease, nil
}

// Apply records a confirmed cut, rejects further use of every affected
// lease at once, and closes their connections within the budget. It returns
// ErrBudgetExceeded with the full report when any close ran over. A retry
// with the same operation ID and cut returns the same answer without a
// second cut, waiting for it if it is still closing; a retry whose ctx ends
// first returns ctx's error while the cut completes. After Close, every
// call, retries included, returns ErrClosed.
func (r *Registry) Apply(ctx context.Context, cut Cut) (Report, error) {
	if err := cut.validate(); err != nil {
		return Report{}, err
	}
	start := r.config.Now()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Report{}, ErrClosed
	}
	state := r.tenant(cut.Target.TenantID)
	op, run, err := state.ops.start(cut, start)
	if err != nil {
		r.mu.Unlock()
		return Report{}, err
	}
	if !run {
		r.mu.Unlock()
		select {
		case <-op.done:
			return op.answer()
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	}
	r.work.Add(1)
	defer r.work.Done()
	defer close(op.done)
	state.history.record(cut.Target, cut.Version, r.config.History)
	var affected []*Lease
	for lease := range state.leases {
		if lease.grant.Version < cut.Version && cut.Target.matches(lease.grant) {
			delete(state.leases, lease)
			lease.end(ErrAccessRevoked)
			affected = append(affected, lease)
		}
	}
	r.mu.Unlock()
	r.config.Logger.InfoContext(ctx, "access cut confirmed",
		"operation_id", cut.OperationID,
		"type", string(cut.Type),
		"target_kind", string(cut.Target.Kind),
		"tenant_id", cut.Target.TenantID,
		"version", cut.Version,
		"affected", len(affected),
	)

	report := Report{
		OperationID:   cut.OperationID,
		Type:          cut.Type,
		Target:        cut.Target,
		Version:       cut.Version,
		CorrelationID: cut.CorrelationID,
		Closures:      r.closeAll(context.WithoutCancel(ctx), affected, cut.reason(), start),
	}
	for _, closure := range report.Closures {
		report.MaxLatency = max(report.MaxLatency, closure.Latency)
		if closure.Exceeded {
			report.Exceeded++
		}
	}
	op.report = report
	if report.Exceeded > 0 {
		op.err = ErrBudgetExceeded
		r.config.Logger.ErrorContext(ctx, "access cut exceeded close budget",
			"operation_id", cut.OperationID,
			"tenant_id", cut.Target.TenantID,
			"exceeded", report.Exceeded,
			"max_latency_ms", report.MaxLatency.Milliseconds(),
		)
	}
	return op.answer()
}

// Close stops admitting connections and cuts, closes every live connection
// with the retryable ReasonShutdown within the budget, and waits for cuts in
// progress. If ctx ends first, every pending close is aborted and ctx's
// error is returned once all of them have returned. Close is idempotent.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	first := !r.closed
	r.closed = true
	var leases []*Lease
	if first {
		for _, state := range r.tenants {
			for lease := range state.leases {
				delete(state.leases, lease)
				lease.end(ErrClosed)
				leases = append(leases, lease)
			}
		}
		r.work.Add(1)
	}
	r.mu.Unlock()

	if first {
		start := r.config.Now()
		go func() {
			defer r.work.Done()
			r.closeAll(context.WithoutCancel(ctx), leases, ReasonShutdown, start)
		}()
	}
	done := make(chan struct{})
	go func() {
		r.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		r.cancelAbort()
		<-done
		return ctx.Err()
	}
}

func (r *Registry) tenant(id string) *tenant {
	state, ok := r.tenants[id]
	if !ok {
		state = &tenant{leases: map[*Lease]struct{}{}, history: history{latest: map[Target]uint64{}}, ops: newOperations(r.config.Retention)}
		r.tenants[id] = state
	}
	return state
}

func (r *Registry) release(lease *Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.tenants[lease.grant.TenantID]; ok {
		delete(state.leases, lease)
	}
}

// closeAll closes every lease concurrently and returns the closures in a
// stable order.
func (r *Registry) closeAll(ctx context.Context, leases []*Lease, reason Reason, start time.Time) []Closure {
	closures := make([]Closure, len(leases))
	var wg sync.WaitGroup
	for i, lease := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closures[i] = r.closeOne(lease, reason, start)
			r.config.Logger.InfoContext(ctx, "connection closed",
				"tenant_id", lease.grant.TenantID,
				"client", string(closures[i].Client),
				"reason", string(reason),
				"latency_ms", closures[i].Latency.Milliseconds(),
				"aborted", closures[i].Aborted,
			)
		}()
	}
	wg.Wait()
	slices.SortFunc(closures, func(a, b Closure) int {
		if a.Client != b.Client {
			if a.Client < b.Client {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Latency, b.Latency)
	})
	return closures
}

// closeOne closes gracefully within three quarters of the budget measured
// from start, then aborts; it aborts at once when Close runs out of time.
// It always waits for the transport Close to return, so no goroutine
// outlives the cut or the shutdown.
func (r *Registry) closeOne(lease *Lease, reason Reason, start time.Time) Closure {
	graceful := r.config.Budget - r.config.Budget/4 - r.config.Now().Sub(start)
	ctx, cancel := context.WithTimeout(context.Background(), graceful)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- lease.conn.Close(ctx, reason) }()
	select {
	case err := <-closed:
		if err == nil {
			return r.closure(lease, start, false)
		}
		lease.conn.Abort()
	case <-ctx.Done():
		lease.conn.Abort()
		<-closed
	case <-r.abort.Done():
		cancel()
		lease.conn.Abort()
		<-closed
	}
	return r.closure(lease, start, true)
}

// closure measures one close once its transport Close has returned.
func (r *Registry) closure(lease *Lease, start time.Time, aborted bool) Closure {
	latency := r.config.Now().Sub(start)
	return Closure{
		Client:   lease.grant.Client,
		Latency:  latency,
		Aborted:  aborted,
		Exceeded: latency > r.config.Budget,
	}
}

// history retains the latest confirmed cut version per target in arrival
// order. Evicting an entry raises floor, so grants read before any evicted
// cut are stale rather than silently admitted.
type history struct {
	latest map[Target]uint64
	order  []entry
	floor  uint64
}

type entry struct {
	target  Target
	version uint64
}

func (h *history) record(target Target, version uint64, limit int) {
	if version > h.latest[target] {
		h.latest[target] = version
	}
	h.order = append(h.order, entry{target: target, version: version})
	for len(h.order) > limit {
		oldest := h.order[0]
		h.order = h.order[1:]
		if h.latest[oldest.target] <= oldest.version {
			delete(h.latest, oldest.target)
		}
		h.floor = max(h.floor, oldest.version)
	}
}

func (h *history) check(grant Grant) error {
	if grant.Version < h.floor {
		return ErrStaleGrant
	}
	for _, target := range grant.targets() {
		if h.latest[target] > grant.Version {
			return ErrAccessRevoked
		}
	}
	return nil
}
