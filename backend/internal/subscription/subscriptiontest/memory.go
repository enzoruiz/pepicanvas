// Package subscriptiontest provides an in-memory subscription store for tests
// of the activation service and of the adapters built on it.
package subscriptiontest

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

type state struct {
	byEmail     map[string]access.AccountID
	operators   map[access.AccountID]bool
	tenants     map[string]subscription.Tenant
	memberships map[[2]string]access.Role
	canvases    map[canvas.Key]canvas.Dimensions
	records     map[string]subscription.Record
}

func (s state) clone() state {
	return state{
		byEmail:     maps.Clone(s.byEmail),
		operators:   maps.Clone(s.operators),
		tenants:     maps.Clone(s.tenants),
		memberships: maps.Clone(s.memberships),
		canvases:    maps.Clone(s.canvases),
		records:     maps.Clone(s.records),
	}
}

// MemoryStore is an in-memory subscription.Store. Transactions run one at a
// time on a copy of the state and replace it only on commit, which gives the
// serialization the PostgreSQL account lock provides.
type MemoryStore struct {
	mu             sync.Mutex
	state          state
	failures       map[string]error
	beforeRecord   func()
	transactions   int
	accountLookups int
}

var _ subscription.Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		state: state{
			byEmail:     map[string]access.AccountID{},
			operators:   map[access.AccountID]bool{},
			tenants:     map[string]subscription.Tenant{},
			memberships: map[[2]string]access.Role{},
			canvases:    map[canvas.Key]canvas.Dimensions{},
			records:     map[string]subscription.Record{},
		},
		failures: map[string]error{},
	}
}

// AddAccount seeds an account with a normalized email.
func (m *MemoryStore) AddAccount(id access.AccountID, email string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.byEmail[email] = id
}

// GrantOperator gives the account the operator role.
func (m *MemoryStore) GrantOperator(id access.AccountID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.operators[id] = true
}

// RevokeOperator removes the operator role.
func (m *MemoryStore) RevokeOperator(id access.AccountID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.state.operators, id)
}

// SetStatus changes a tenant's subscription status.
func (m *MemoryStore) SetStatus(tenantID string, status access.SubscriptionStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant := m.state.tenants[tenantID]
	tenant.Status = status
	m.state.tenants[tenantID] = tenant
}

// FailOnce makes the next call of step (a Tx method name, or "commit") fail with err.
func (m *MemoryStore) FailOnce(step string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[step] = err
}

// BeforeRecordOnce runs fn at the start of the next RecordActivation with the
// store unlocked, as a concurrent transaction that commits before that
// activation inserts its record.
func (m *MemoryStore) BeforeRecordOnce(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeRecord = fn
}

// Tenants returns the committed tenants.
func (m *MemoryStore) Tenants() []subscription.Tenant {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenants := make([]subscription.Tenant, 0, len(m.state.tenants))
	for _, tenant := range m.state.tenants {
		tenants = append(tenants, tenant)
	}
	return tenants
}

// Canvases returns the committed canvases and their dimensions.
func (m *MemoryStore) Canvases() map[canvas.Key]canvas.Dimensions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.state.canvases)
}

// Membership returns the account's committed role in the tenant.
func (m *MemoryStore) Membership(tenantID string, account access.AccountID) (access.Role, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	role, ok := m.state.memberships[[2]string{tenantID, string(account)}]
	return role, ok
}

// Records returns the committed activation records by operation ID.
func (m *MemoryStore) Records() map[string]subscription.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.state.records)
}

// Transactions counts started transactions.
func (m *MemoryStore) Transactions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transactions
}

// AccountLookups counts LockAccount calls.
func (m *MemoryStore) AccountLookups() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accountLookups
}

// InTx runs fn on a copy of the state and commits the copy if fn succeeds.
func (m *MemoryStore) InTx(_ context.Context, fn func(subscription.Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transactions++
	tx := &memoryTx{store: m, state: m.state.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	if err := m.fail("commit"); err != nil {
		return err
	}
	m.state = tx.state
	return nil
}

// IsOperator reads the committed operator grant.
func (m *MemoryStore) IsOperator(_ context.Context, account access.AccountID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.operators[account], nil
}

// OwnedTenant returns the committed tenant the account owns.
func (m *MemoryStore) OwnedTenant(_ context.Context, account access.AccountID) (subscription.Tenant, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ownedBy(m.state, account)
}

// fail consumes the injected failure of step. The caller holds m.mu.
func (m *MemoryStore) fail(step string) error {
	err := m.failures[step]
	delete(m.failures, step)
	return err
}

func ownedBy(s state, account access.AccountID) (subscription.Tenant, bool, error) {
	for _, tenant := range s.tenants {
		if tenant.Owner == account {
			return tenant, true, nil
		}
	}
	return subscription.Tenant{}, false, nil
}

type memoryTx struct {
	store *MemoryStore
	state state
}

func (t *memoryTx) IsOperator(_ context.Context, account access.AccountID) (bool, error) {
	if err := t.store.fail("IsOperator"); err != nil {
		return false, err
	}
	return t.state.operators[account], nil
}

func (t *memoryTx) Activation(_ context.Context, operationID string, notBefore time.Time) (subscription.Record, bool, error) {
	if err := t.store.fail("Activation"); err != nil {
		return subscription.Record{}, false, err
	}
	record, ok := t.state.records[operationID]
	if !ok || !record.AcceptedAt.After(notBefore) {
		return subscription.Record{}, false, nil
	}
	return record, true, nil
}

func (t *memoryTx) LockAccount(_ context.Context, email string) (access.AccountID, bool, error) {
	t.store.accountLookups++
	if err := t.store.fail("LockAccount"); err != nil {
		return "", false, err
	}
	id, ok := t.state.byEmail[email]
	return id, ok, nil
}

func (t *memoryTx) TenantOwnedBy(_ context.Context, account access.AccountID) (subscription.Tenant, bool, error) {
	if err := t.store.fail("TenantOwnedBy"); err != nil {
		return subscription.Tenant{}, false, err
	}
	return ownedBy(t.state, account)
}

func (t *memoryTx) CreateTenant(_ context.Context, tenant subscription.Tenant) error {
	if err := t.store.fail("CreateTenant"); err != nil {
		return err
	}
	t.state.tenants[tenant.ID] = tenant
	t.state.memberships[[2]string{tenant.ID, string(tenant.Owner)}] = access.RoleOwner
	t.state.canvases[canvas.Key{TenantID: tenant.ID, CanvasID: tenant.CanvasID}] = canvas.DefaultDimensions
	return nil
}

func (t *memoryTx) ActivateTenant(_ context.Context, tenantID string) error {
	if err := t.store.fail("ActivateTenant"); err != nil {
		return err
	}
	tenant := t.state.tenants[tenantID]
	tenant.Status = access.SubscriptionActive
	t.state.tenants[tenantID] = tenant
	return nil
}

func (t *memoryTx) RecordActivation(_ context.Context, record subscription.Record, purgeBefore time.Time) error {
	if fn := t.store.beforeRecord; fn != nil {
		t.store.beforeRecord = nil
		t.store.mu.Unlock()
		fn()
		t.store.mu.Lock()
	}
	if err := t.store.fail("RecordActivation"); err != nil {
		return err
	}
	for id, r := range t.state.records {
		if !r.AcceptedAt.After(purgeBefore) {
			delete(t.state.records, id)
		}
	}
	// The primary key also sees rows other transactions committed meanwhile.
	if r, ok := t.store.state.records[record.OperationID]; ok && r.AcceptedAt.After(purgeBefore) {
		return subscription.ErrOperationRetained
	}
	if _, ok := t.state.records[record.OperationID]; ok {
		return subscription.ErrOperationRetained
	}
	t.state.records[record.OperationID] = record
	return nil
}
