package locking

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// DefaultTTL is how long a lease lives without an accepted heartbeat.
const DefaultTTL = 5 * time.Second

// Authorizer reports whether holder may hold the lease on key. It runs
// before every acquire and for every lease on every heartbeat; any error
// denies the acquire or ends the lease.
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
	// Clock defaults to the system clock.
	Clock Clock
}

// Manager holds every lease of the process.
type Manager struct {
	config Config

	mu     sync.Mutex
	leases map[Key]*entry
}

type entry struct {
	lease   Lease
	session *Session
	timer   Timer
}

// NewManager validates config and returns an empty manager.
func NewManager(config Config) (*Manager, error) {
	if config.Authorize == nil {
		return nil, fmt.Errorf("%w: authorizer is required", ErrInvalidConfig)
	}
	if config.TTL < 0 {
		return nil, fmt.Errorf("%w: negative ttl", ErrInvalidConfig)
	}
	if config.TTL == 0 {
		config.TTL = DefaultTTL
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	return &Manager{config: config, leases: map[Key]*entry{}}, nil
}

// Session is one connection's view of the manager. Every lease it acquires
// ends when its connection context ends or Detach is called.
type Session struct {
	manager *Manager
	holder  Holder
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
	if conn.Err() != nil {
		return nil, ErrDetached
	}
	session := &Session{manager: m, holder: holder, held: map[Key]*entry{}, lost: map[Key]Reason{}}
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

// Acquire grants the lease on the requested object when it is free or
// already held by this session, renewing it in the latter case. A lease
// held by another session is denied and the answer names its holder.
func (s *Session) Acquire(ctx context.Context, request Request) (Result, error) {
	if err := request.validate(); err != nil {
		return Result{}, err
	}
	if err := s.alive(); err != nil {
		return Result{}, err
	}
	key := s.key(request)
	if err := s.manager.config.Authorize(ctx, s.holder, key); err != nil {
		return Result{}, err
	}

	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.detached {
		return Result{}, ErrDetached
	}
	now := m.config.Clock.Now()
	result := Result{OperationID: request.OperationID, Outcome: OutcomeGranted}
	switch e := m.liveLocked(key, now); {
	case e == nil:
		e = &entry{lease: Lease{Key: key, Holder: s.holder}, session: s}
		m.leases[key] = e
		s.held[key] = e
		delete(s.lost, key)
		m.renewLocked(e, now)
		result.Lease = e.lease
	case e.session == s:
		m.renewLocked(e, now)
		result.Lease = e.lease
	default:
		result.Outcome = OutcomeDenied
		result.Lease = e.lease
	}
	return result, nil
}

// Heartbeat renews every lease the session still holds and is still
// authorized for, and reports every lease it lost since the previous
// heartbeat. A lease whose deadline has been reached is not renewed.
func (s *Session) Heartbeat(ctx context.Context) (Renewal, error) {
	m := s.manager
	m.mu.Lock()
	if s.detached {
		m.mu.Unlock()
		return Renewal{}, ErrDetached
	}
	keys := make([]Key, 0, len(s.held))
	for key := range s.held {
		keys = append(keys, key)
	}
	m.mu.Unlock()

	denied := map[Key]bool{}
	for _, key := range keys {
		if err := m.config.Authorize(ctx, s.holder, key); err != nil {
			denied[key] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s.detached {
		return Renewal{}, ErrDetached
	}
	now := m.config.Clock.Now()
	var renewal Renewal
	for _, key := range keys {
		e := m.liveLocked(key, now)
		if e == nil || e.session != s {
			continue
		}
		if denied[key] {
			m.endLocked(e, ReasonUnauthorized)
			continue
		}
		m.renewLocked(e, now)
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

// Release ends a lease the session holds, freeing it at once.
func (s *Session) Release(ctx context.Context, request Request) (Result, error) {
	if err := request.validateRelease(); err != nil {
		return Result{}, err
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.detached {
		return Result{}, ErrDetached
	}
	e := m.liveLocked(s.key(request), m.config.Clock.Now())
	if e == nil || e.session != s {
		return Result{}, ErrNotHolder
	}
	m.removeLocked(e)
	return Result{OperationID: request.OperationID, Outcome: OutcomeReleased, Reason: request.Reason, Lease: e.lease}, nil
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
	for _, e := range s.held {
		m.removeLocked(e)
	}
	clear(s.lost)
}

func (s *Session) alive() error {
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	if s.detached {
		return ErrDetached
	}
	return nil
}

func (s *Session) key(request Request) Key {
	return Key{TenantID: s.holder.TenantID, CanvasID: request.CanvasID, ObjectID: request.ObjectID}
}

// liveLocked returns the lease on key, ending it first if its deadline has
// been reached so an expiry is exact even before its timer runs.
func (m *Manager) liveLocked(key Key, now time.Time) *entry {
	e := m.leases[key]
	if e != nil && !now.Before(e.lease.Expires) {
		m.endLocked(e, ReasonExpired)
		return nil
	}
	return e
}

// renewLocked moves the deadline to TTL after now and reschedules expiry.
func (m *Manager) renewLocked(e *entry, now time.Time) {
	if e.timer != nil {
		e.timer.Stop()
	}
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
func (m *Manager) endLocked(e *entry, reason Reason) {
	m.removeLocked(e)
	e.session.lost[e.lease.Key] = reason
}

func (m *Manager) removeLocked(e *entry) {
	delete(m.leases, e.lease.Key)
	delete(e.session.held, e.lease.Key)
	e.timer.Stop()
}
