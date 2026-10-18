package access_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// memoryStore is an in-memory AccountStore and SessionStore with the
// transactional semantics the PostgreSQL adapter must provide.
type memoryStore struct {
	mu            sync.Mutex
	accounts      map[access.AccountID]access.Account
	byEmail       map[string]access.AccountID
	registrations map[string]access.Registration
	correlations  map[string]string
	sessions      map[[32]byte]access.SessionRecord
	createErr     error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		accounts:      map[access.AccountID]access.Account{},
		byEmail:       map[string]access.AccountID{},
		registrations: map[string]access.Registration{},
		correlations:  map[string]string{},
		sessions:      map[[32]byte]access.SessionRecord{},
	}
}

func (m *memoryStore) FindRegistration(_ context.Context, operationID string, notBefore time.Time) (access.Registration, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.registrations[operationID]
	if !ok || !r.AcceptedAt.After(notBefore) {
		return access.Registration{}, false, nil
	}
	return r, true, nil
}

func (m *memoryStore) CreateAccount(_ context.Context, n access.NewAccount) error {
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

func (m *memoryStore) AccountByEmail(_ context.Context, email string) (access.Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return access.Account{}, false, nil
	}
	return m.accounts[id], true, nil
}

func (m *memoryStore) AccountByID(_ context.Context, id access.AccountID) (access.Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	return a, ok, nil
}

func (m *memoryStore) CreateSession(_ context.Context, s access.SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.TokenHash] = s
	return nil
}

func (m *memoryStore) SessionByHash(_ context.Context, hash [32]byte) (access.SessionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[hash]
	return s, ok, nil
}

func (m *memoryStore) TouchSession(_ context.Context, hash [32]byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[hash]; ok && at.After(s.LastSeenAt) {
		s.LastSeenAt = at
		m.sessions[hash] = s
	}
	return nil
}

func (m *memoryStore) DeleteSession(_ context.Context, hash [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, hash)
	return nil
}

func (m *memoryStore) counts() (accounts, sessions int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.accounts), len(m.sessions)
}

// plainHasher is a fast, reversible-free stand-in for argon2id that counts
// verifications, so tests can assert the dummy verification runs.
type plainHasher struct {
	mu       sync.Mutex
	verifies int
}

func (h *plainHasher) Hash(password secret.Value) (string, error) {
	return "plain$" + password.Reveal(), nil
}

func (h *plainHasher) Verify(hash string, password secret.Value) (bool, error) {
	h.mu.Lock()
	h.verifies++
	h.mu.Unlock()
	if !strings.HasPrefix(hash, "plain$") {
		return false, errors.New("unknown hash format")
	}
	return hash == "plain$"+password.Reveal(), nil
}

func (h *plainHasher) verifications() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.verifies
}

// clock is a manually advanced time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
