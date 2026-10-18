package locking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// DefaultTTL is how long a lease lives without an accepted heartbeat.
const DefaultTTL = 5 * time.Second

// ErrClosed reports work submitted after the manager began shutting down.
var ErrClosed = errors.New("locking: manager closed")

// Authorizer reports whether holder may hold the lease on key. It runs
// before every acquire and for every lease on every heartbeat; any error
// denies the acquire or ends the lease. It must return once ctx ends, or
// Close cannot abort it within its deadline.
type Authorizer func(ctx context.Context, holder Holder, key Key) error

// Timer is a pending call scheduled by a Clock.
type Timer interface {
	Stop() bool
}

// Clock tells time and schedules expiries.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type systemClock struct{}

func (systemClock) Now() time.Time                            { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Config configures a Manager.
type Config struct {
	// Authorize is required: a manager without one cannot fail closed.
	Authorize Authorizer
	// TTL is the lease lifetime after the last accepted heartbeat. It
	// defaults to DefaultTTL.
	TTL time.Duration
	// Retention bounds the acquire and release answers kept per account for
	// deduplication.
	Retention Retention
	// Clock defaults to the system clock.
	Clock Clock
	// Logger records every acquire and every lease that ends. It should be
	// built on platform/logging so records carry the correlation_id of the
	// operation that set the lease and are redacted. It defaults to
	// discarding.
	Logger *slog.Logger
}

// Manager holds every lease of the process.
type Manager struct {
	config Config
	// abort cancels in-flight authorizations when Close runs out of time.
	abortCtx context.Context
	abort    context.CancelFunc
	inflight sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	sessions   map[*Session]struct{}
	leases     map[Key]*entry
	operations map[scope]*operations
}

type entry struct {
	lease   Lease
	session *Session
	timer   Timer
	// ctx carries the values of the operation that last set the deadline,
	// so the asynchronous expiry logs its correlation.
	ctx context.Context
}

// NewManager validates config and returns an empty manager.
func NewManager(config Config) (*Manager, error) {
	if config.Authorize == nil {
		return nil, fmt.Errorf("%w: authorizer is required", ErrInvalidConfig)
	}
	if config.TTL < 0 || config.Retention.TTL < 0 || config.Retention.MaxOperations < 0 {
		return nil, fmt.Errorf("%w: negative ttl or retention", ErrInvalidConfig)
	}
	if config.TTL == 0 {
		config.TTL = DefaultTTL
	}
	if config.Retention.TTL == 0 {
		config.Retention.TTL = DefaultRetentionTTL
	}
	if config.Retention.MaxOperations == 0 {
		config.Retention.MaxOperations = DefaultRetentionMaxOperations
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	abortCtx, abort := context.WithCancel(context.Background())
	return &Manager{
		config:     config,
		abortCtx:   abortCtx,
		abort:      abort,
		sessions:   map[*Session]struct{}{},
		leases:     map[Key]*entry{},
		operations: map[scope]*operations{},
	}, nil
}

// Session is one connection's view of the manager. Every lease it acquires
// ends when its connection context ends or Detach is called.
type Session struct {
	manager *Manager
	holder  Holder
	ctx     context.Context
	stop    func() bool

	// Guarded by manager.mu.
	detached bool
	held     map[Key]*entry
	lost     map[Key]Reason
}

// Attach binds a session to conn, the context of one live connection; it
// should be the TE-002 lease context so an access cut ends it. A session
// whose context already ended is rejected.
func (m *Manager) Attach(conn context.Context, holder Holder) (*Session, error) {
	if err := holder.validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if conn.Err() != nil {
		return nil, ErrDetached
	}
	session := &Session{
		manager: m,
		holder:  holder,
		ctx:     context.WithoutCancel(conn),
		held:    map[Key]*entry{},
		lost:    map[Key]Reason{},
	}
	m.sessions[session] = struct{}{}
	session.stop = context.AfterFunc(conn, session.Detach)
	return session, nil
}

// Lookup returns the current lease on key, if any.
func (m *Manager) Lookup(key Key) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.liveLocked(key, m.config.Clock.Now()); e != nil {
		return e.lease, true
	}
	return Lease{}, false
}

// Close rejects new work with ErrClosed at once, releases every lease, and
// waits for in-flight acquires and heartbeats, which then fail with
// ErrClosed. If ctx ends first, Close cancels their authorizations, waits
// for them to return, and returns ctx's error. It is idempotent.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for session := range m.sessions {
			session.stop()
			session.detached = true
			clear(session.lost)
		}
		clear(m.sessions)
		for _, e := range m.leases {
			m.removeLocked(e)
			m.logEnded(ctx, e, ReasonShutdown)
		}
		clear(m.operations)
	}
	m.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		m.inflight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		m.abort()
		<-drained
		return ctx.Err()
	}
}

// Acquire grants the lease on the requested object when it is free or
// already held by this session, renewing it in the latter case. A lease
// held by another session is denied and the answer names its holder. A
// retry with the same operation ID, from this or a later session of the
// same account, returns the retained answer without a new effect; it does
// not mean the lease is still held, which only Heartbeat and Lookup tell.
func (s *Session) Acquire(ctx context.Context, request Request) (Result, error) {
	if err := request.validate(); err != nil {
		return Result{}, err
	}
	m := s.manager
	authCtx, done, err := s.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer done()
	key := s.key(request)
	authErr := m.config.Authorize(authCtx, s.holder, key)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := s.aliveLocked(); err != nil {
		return Result{}, err
	}
	if authErr != nil {
		return Result{}, authErr
	}
	now := m.config.Clock.Now()
	if answer, ok, err := m.lookupLocked(s.scope(), kindAcquire, request, now); ok || err != nil {
		return answer, err
	}
	result := Result{OperationID: request.OperationID, Outcome: OutcomeGranted}
	switch e := m.liveLocked(key, now); {
	case e == nil:
		e = &entry{lease: Lease{Key: key, Holder: s.holder}, session: s}
		m.leases[key] = e
		s.held[key] = e
		delete(s.lost, key)
		m.renewLocked(ctx, e, now)
		result.Lease = e.lease
	case e.session == s:
		m.renewLocked(ctx, e, now)
		result.Lease = e.lease
	default:
		result.Outcome = OutcomeDenied
		result.Lease = e.lease
	}
	m.rememberLocked(s.scope(), kindAcquire, request, result, now)
	m.config.Logger.InfoContext(ctx, "lease acquire",
		slog.String("outcome", string(result.Outcome)),
		slog.String("operation_id", request.OperationID),
		slog.String("tenant_id", key.TenantID),
		slog.String("canvas_id", key.CanvasID),
		slog.String("object_id", key.ObjectID))
	return result, nil
}

// Heartbeat renews every lease the session still holds and is still
// authorized for, and reports every lease it lost since the previous
// heartbeat. A lease whose deadline has been reached is not renewed.
func (s *Session) Heartbeat(ctx context.Context) (Renewal, error) {
	m := s.manager
	authCtx, done, err := s.begin(ctx)
	if err != nil {
		return Renewal{}, err
	}
	defer done()
	m.mu.Lock()
	keys := make([]Key, 0, len(s.held))
	for key := range s.held {
		keys = append(keys, key)
	}
	m.mu.Unlock()

	denied := map[Key]bool{}
	for _, key := range keys {
		if err := m.config.Authorize(authCtx, s.holder, key); err != nil {
			denied[key] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := s.aliveLocked(); err != nil {
		return Renewal{}, err
	}
	now := m.config.Clock.Now()
	var renewal Renewal
	for _, key := range keys {
		e := m.liveLocked(key, now)
		if e == nil || e.session != s {
			continue
		}
		if denied[key] {
			m.endLocked(ctx, e, ReasonUnauthorized)
			continue
		}
		m.renewLocked(ctx, e, now)
		renewal.Renewed = append(renewal.Renewed, e.lease)
	}
	for key, reason := range s.lost {
		renewal.Lost = append(renewal.Lost, Loss{Key: key, Reason: reason})
	}
	clear(s.lost)
	slices.SortFunc(renewal.Renewed, func(a, b Lease) int { return compareKeys(a.Key, b.Key) })
	slices.SortFunc(renewal.Lost, func(a, b Loss) int { return compareKeys(a.Key, b.Key) })
	return renewal, nil
}

// Release ends a lease the session holds, freeing it at once. A retry with
// the same operation ID returns the retained answer without a new effect.
func (s *Session) Release(ctx context.Context, request Request) (Result, error) {
	if err := request.validateRelease(); err != nil {
		return Result{}, err
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := s.aliveLocked(); err != nil {
		return Result{}, err
	}
	now := m.config.Clock.Now()
	if answer, ok, err := m.lookupLocked(s.scope(), kindRelease, request, now); ok || err != nil {
		return answer, err
	}
	e := m.liveLocked(s.key(request), now)
	if e == nil || e.session != s {
		return Result{}, ErrNotHolder
	}
	m.removeLocked(e)
	m.logEnded(ctx, e, request.Reason)
	result := Result{OperationID: request.OperationID, Outcome: OutcomeReleased, Reason: request.Reason, Lease: e.lease}
	m.rememberLocked(s.scope(), kindRelease, request, result, now)
	return result, nil
}

// Detach releases every lease of the session and rejects further work. The
// session's connection context ending calls it; transports may call it
// directly on disconnect. It is idempotent.
func (s *Session) Detach() {
	s.stop()
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.detached {
		return
	}
	s.detached = true
	delete(m.sessions, s)
	for _, e := range s.held {
		m.removeLocked(e)
		m.logEnded(s.ctx, e, ReasonDetached)
	}
	clear(s.lost)
}

// begin admits one operation that calls the authorizer outside the lock.
// The returned context also ends when Close runs out of time.
func (s *Session) begin(ctx context.Context) (context.Context, func(), error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := s.aliveLocked(); err != nil {
		return nil, nil, err
	}
	m.inflight.Add(1)
	authCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.abortCtx, cancel)
	return authCtx, func() {
		stop()
		cancel()
		m.inflight.Done()
	}, nil
}

func (s *Session) aliveLocked() error {
	if s.manager.closed {
		return ErrClosed
	}
	if s.detached {
		return ErrDetached
	}
	return nil
}

func (s *Session) scope() scope {
	return scope{tenantID: s.holder.TenantID, accountID: s.holder.AccountID}
}

func (s *Session) key(request Request) Key {
	return Key{TenantID: s.holder.TenantID, CanvasID: request.CanvasID, ObjectID: request.ObjectID}
}

// liveLocked returns the lease on key, ending it first if its deadline has
// been reached so an expiry is exact even before its timer runs.
func (m *Manager) liveLocked(key Key, now time.Time) *entry {
	e := m.leases[key]
	if e != nil && !now.Before(e.lease.Expires) {
		m.endLocked(e.ctx, e, ReasonExpired)
		return nil
	}
	return e
}

// renewLocked moves the deadline to TTL after now and reschedules expiry.
func (m *Manager) renewLocked(ctx context.Context, e *entry, now time.Time) {
	if e.timer != nil {
		e.timer.Stop()
	}
	e.ctx = context.WithoutCancel(ctx)
	e.lease.Expires = now.Add(m.config.TTL)
	e.timer = m.config.Clock.AfterFunc(m.config.TTL, func() { m.expire(e) })
}

func (m *Manager) expire(e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[e.lease.Key] == e {
		m.liveLocked(e.lease.Key, m.config.Clock.Now())
	}
}

// endLocked removes a lease the holder did not release and records why.
func (m *Manager) endLocked(ctx context.Context, e *entry, reason Reason) {
	m.removeLocked(e)
	e.session.lost[e.lease.Key] = reason
	m.logEnded(ctx, e, reason)
}

func (m *Manager) removeLocked(e *entry) {
	delete(m.leases, e.lease.Key)
	delete(e.session.held, e.lease.Key)
	e.timer.Stop()
}

// logEnded records why a lease ended. Holder identifiers are never logged.
func (m *Manager) logEnded(ctx context.Context, e *entry, reason Reason) {
	m.config.Logger.InfoContext(ctx, "lease ended",
		slog.String("reason", string(reason)),
		slog.String("tenant_id", e.lease.Key.TenantID),
		slog.String("canvas_id", e.lease.Key.CanvasID),
		slog.String("object_id", e.lease.Key.ObjectID))
}
