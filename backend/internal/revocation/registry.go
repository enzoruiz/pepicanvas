package revocation

import (
	"cmp"
	"context"
	"errors"
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
	// Close and must make a pending Close return.
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

	mu      sync.Mutex
	tenants map[string]*tenant
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
	return &Registry{config: config, tenants: map[string]*tenant{}}, nil
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
// first returns ctx's error while the cut completes.
func (r *Registry) Apply(ctx context.Context, cut Cut) (Report, error) {
	if err := cut.validate(); err != nil {
		return Report{}, err
	}
	start := r.config.Now()
	r.mu.Lock()
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

	report := Report{
		OperationID:   cut.OperationID,
		Type:          cut.Type,
		Target:        cut.Target,
		Version:       cut.Version,
		CorrelationID: cut.CorrelationID,
		Closures:      r.closeAll(affected, cut.reason(), start),
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
	}
	return op.answer()
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
func (r *Registry) closeAll(leases []*Lease, reason Reason, start time.Time) []Closure {
	closures := make([]Closure, len(leases))
	var wg sync.WaitGroup
	for i, lease := range leases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closures[i] = r.closeOne(lease, reason, start)
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
// from start, then aborts.
func (r *Registry) closeOne(lease *Lease, reason Reason, start time.Time) Closure {
	graceful := r.config.Budget - r.config.Budget/4 - r.config.Now().Sub(start)
	ctx, cancel := context.WithTimeout(context.Background(), graceful)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- lease.conn.Close(ctx, reason) }()
	aborted := false
	select {
	case err := <-closed:
		if err != nil {
			lease.conn.Abort()
			aborted = true
		}
	case <-ctx.Done():
		lease.conn.Abort()
		aborted = true
	}
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
