// Package accesstest provides in-memory access ports for tests of the
// access service and of the adapters built on it.
package accesstest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// MemoryStore is an in-memory AccountStore, SessionStore, and MembershipStore
// with the transactional semantics the PostgreSQL adapter must provide.
type MemoryStore struct {
	mu            sync.Mutex
	accounts      map[access.AccountID]access.Account
	byEmail       map[string]access.AccountID
	registrations map[string]access.Registration
	correlations  map[string]string
	sessions      map[[32]byte]access.SessionRecord
	createErr     error
	memberships   map[[2]string]access.Membership
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts:      map[access.AccountID]access.Account{},
		byEmail:       map[string]access.AccountID{},
		registrations: map[string]access.Registration{},
		correlations:  map[string]string{},
		sessions:      map[[32]byte]access.SessionRecord{},
		memberships:   map[[2]string]access.Membership{},
	}
}

func (m *MemoryStore) FindRegistration(_ context.Context, operationID string, notBefore time.Time) (access.Registration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.registrations[operationID]
	if !ok || !r.AcceptedAt.After(notBefore) {
		return access.Registration{}, false, nil
	}
	return r, true, nil
}

func (m *MemoryStore) CreateAccount(_ context.Context, n access.NewAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	for id, r := range m.registrations {
		if !r.AcceptedAt.After(n.PurgeBefore) {
			delete(m.registrations, id)
		}
	}
	if _, ok := m.registrations[n.Registration.OperationID]; ok {
		return access.ErrOperationRetained
	}
	if _, ok := m.byEmail[n.Account.Email]; ok {
		return access.ErrIdentityTaken
	}
	m.accounts[n.Account.ID] = n.Account
	m.byEmail[n.Account.Email] = n.Account.ID
	m.registrations[n.Registration.OperationID] = n.Registration
	m.correlations[n.Registration.OperationID] = n.CorrelationID
	return nil
}

func (m *MemoryStore) AccountByEmail(_ context.Context, email string) (access.Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return access.Account{}, false, nil
	}
	return m.accounts[id], true, nil
}

func (m *MemoryStore) AccountByID(_ context.Context, id access.AccountID) (access.Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	return a, ok, nil
}

func (m *MemoryStore) CreateSession(_ context.Context, s access.SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.TokenHash] = s
	return nil
}

func (m *MemoryStore) SessionByHash(_ context.Context, hash [32]byte) (access.SessionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[hash]
	return s, ok, nil
}

func (m *MemoryStore) TouchSession(_ context.Context, hash [32]byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[hash]; ok && at.After(s.LastSeenAt) {
		s.LastSeenAt = at
		m.sessions[hash] = s
	}
	return nil
}

func (m *MemoryStore) DeleteSession(_ context.Context, hash [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, hash)
	return nil
}

// Counts returns the number of stored accounts and sessions.
func (m *MemoryStore) Counts() (accounts, sessions int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.accounts), len(m.sessions)
}

// PlainHasher is a fast stand-in for argon2id that counts verifications,
// so tests can assert the dummy verification runs.
type PlainHasher struct {
	mu       sync.Mutex
	verifies int
}

func (h *PlainHasher) Hash(password secret.Value) (string, error) {
	return "plain$" + password.Reveal(), nil
}

func (h *PlainHasher) Verify(hash string, password secret.Value) (bool, error) {
	h.mu.Lock()
	h.verifies++
	h.mu.Unlock()
	if !strings.HasPrefix(hash, "plain$") {
		return false, errors.New("unknown hash format")
	}
	return hash == "plain$"+password.Reveal(), nil
}

// Verifications returns how many verifications ran.
func (h *PlainHasher) Verifications() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.verifies
}

// Clock is a manually advanced time source.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock {
	return &Clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// FailCreate makes CreateAccount fail with err until it is called with nil.
func (m *MemoryStore) FailCreate(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createErr = err
}

// Correlation returns the correlation ID retained with a registration.
func (m *MemoryStore) Correlation(operationID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.correlations[operationID]
}

// SetMembership grants account an accepted role in a tenant.
func (m *MemoryStore) SetMembership(account access.AccountID, membership access.Membership) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memberships[[2]string{string(account), membership.TenantID}] = membership
}

// MembershipOf implements access.MembershipStore.
func (m *MemoryStore) MembershipOf(_ context.Context, account access.AccountID, tenantID string) (access.Membership, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	membership, ok := m.memberships[[2]string{string(account), tenantID}]
	return membership, ok, nil
}
