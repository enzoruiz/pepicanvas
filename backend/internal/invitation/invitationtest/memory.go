// Package invitationtest provides an in-memory invitation store for tests of
// the invitation service and of the adapters built on it.
package invitationtest

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

type state struct {
	tenants     map[string]invitation.Tenant
	memberships map[[2]string]access.Role
	invitations map[string]invitation.Stored
	records     map[string]invitation.Record
	emails      map[access.AccountID]string
	versions    map[string]uint64
	cuts        map[cutKey]invitation.RevocationCut
	revocations map[string]invitation.MemberRevocationRecord
}

// cutKey is the unique key of an access cut: its tenant and version.
type cutKey struct {
	tenantID string
	version  uint64
}

// merge applies the changes tx made to base onto s, keeping what concurrent
// transactions committed to other rows meanwhile.
func (s state) merge(base, tx state) {
	mergeMap(s.tenants, base.tenants, tx.tenants)
	mergeMap(s.memberships, base.memberships, tx.memberships)
	mergeMap(s.invitations, base.invitations, tx.invitations)
	mergeMap(s.records, base.records, tx.records)
	mergeMap(s.emails, base.emails, tx.emails)
	mergeMap(s.versions, base.versions, tx.versions)
	mergeMap(s.cuts, base.cuts, tx.cuts)
	mergeMap(s.revocations, base.revocations, tx.revocations)
}

func mergeMap[K, V comparable](dst, base, tx map[K]V) {
	for k, v := range tx {
		if old, ok := base[k]; !ok || old != v {
			dst[k] = v
		}
	}
	for k := range base {
		if _, ok := tx[k]; !ok {
			delete(dst, k)
		}
	}
}

func (s state) clone() state {
	return state{
		tenants:     maps.Clone(s.tenants),
		memberships: maps.Clone(s.memberships),
		invitations: maps.Clone(s.invitations),
		records:     maps.Clone(s.records),
		emails:      maps.Clone(s.emails),
		versions:    maps.Clone(s.versions),
		cuts:        maps.Clone(s.cuts),
		revocations: maps.Clone(s.revocations),
	}
}

// MemoryStore is an in-memory invitation.Store. Transactions run one at a
// time on a copy of the state and replace it only on commit. A hook set with
// BeforeOnce runs a concurrent transaction in the middle of another one; the
// lock steps then read the rows that transaction committed, as a PostgreSQL
// row lock does, and the insert steps see its unique keys.
type MemoryStore struct {
	mu    sync.Mutex
	state state
	// outbox holds every committed access cut in commit order, as the
	// access_cuts table that revocations and deactivations share.
	outbox            []subscription.PendingCut
	applied           map[int64]time.Time
	failures          map[string]error
	before            map[string]func()
	transactions      int
	invitationLookups int
	operationLookups  int
}

var (
	_ invitation.Store      = (*MemoryStore)(nil)
	_ subscription.CutStore = (*MemoryStore)(nil)
)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		state: state{
			tenants:     map[string]invitation.Tenant{},
			memberships: map[[2]string]access.Role{},
			invitations: map[string]invitation.Stored{},
			records:     map[string]invitation.Record{},
			emails:      map[access.AccountID]string{},
			versions:    map[string]uint64{},
			cuts:        map[cutKey]invitation.RevocationCut{},
			revocations: map[string]invitation.MemberRevocationRecord{},
		},
		applied:  map[int64]time.Time{},
		failures: map[string]error{},
		before:   map[string]func(){},
	}
}

// CanvasID is the canvas AddTenant gives a tenant.
func CanvasID(tenantID string) string {
	return "canvas-" + tenantID
}

// AddTenant seeds a tenant with the canvas CanvasID(tenantID), its owner
// membership, and access version 1.
func (m *MemoryStore) AddTenant(tenantID string, owner access.AccountID, status access.SubscriptionStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.tenants[tenantID] = invitation.Tenant{ID: tenantID, Owner: owner, CanvasID: CanvasID(tenantID), Status: status}
	m.state.memberships[[2]string{tenantID, string(owner)}] = access.RoleOwner
	m.state.versions[tenantID] = 1
}

// AddAccount seeds the email of an account. Members lists only moderators
// with an email, as the accounts join does.
func (m *MemoryStore) AddAccount(account access.AccountID, email string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.emails[account] = email
}

// SetStatus changes a tenant's subscription status.
func (m *MemoryStore) SetStatus(tenantID string, status access.SubscriptionStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant := m.state.tenants[tenantID]
	tenant.Status = status
	m.state.tenants[tenantID] = tenant
}

// AddMember seeds a membership.
func (m *MemoryStore) AddMember(tenantID string, account access.AccountID, role access.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.memberships[[2]string{tenantID, string(account)}] = role
}

// FailOnce makes the next call of step (a Tx method name, or "commit") fail with err.
func (m *MemoryStore) FailOnce(step string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[step] = err
}

// BeforeOnce runs fn at the start of the next call of step (a Tx method
// name) with the store unlocked, as a concurrent transaction that commits
// while the current one is in progress.
func (m *MemoryStore) BeforeOnce(step string, fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.before[step] = fn
}

// Invitations returns the committed invitations by ID.
func (m *MemoryStore) Invitations() map[string]invitation.Stored {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.state.invitations)
}

// Membership returns the account's committed role in the tenant.
func (m *MemoryStore) Membership(tenantID string, account access.AccountID) (access.Role, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	role, ok := m.state.memberships[[2]string{tenantID, string(account)}]
	return role, ok
}

// Memberships counts the committed memberships.
func (m *MemoryStore) Memberships() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.state.memberships)
}

// AccessVersion returns the committed access version of a tenant.
func (m *MemoryStore) AccessVersion(tenantID string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.versions[tenantID]
}

// Cuts returns the committed revocation cuts by tenant and version.
func (m *MemoryStore) Cuts() []invitation.RevocationCut {
	m.mu.Lock()
	defer m.mu.Unlock()
	cuts := slices.Collect(maps.Values(m.state.cuts))
	slices.SortFunc(cuts, func(a, b invitation.RevocationCut) int {
		if c := strings.Compare(a.TenantID, b.TenantID); c != 0 {
			return c
		}
		return cmp.Compare(a.Version, b.Version)
	})
	return cuts
}

// MemberRevocations returns the committed membership revocation records by
// operation ID.
func (m *MemoryStore) MemberRevocations() map[string]invitation.MemberRevocationRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.state.revocations)
}

// DeactivateTenant commits what a US-003 deactivation of the tenant writes to
// the facts this store shares with it: the tenant turns inactive, its access
// version increments, and a deactivation cut joins the outbox.
func (m *MemoryStore) DeactivateTenant(tenantID, correlationID string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant := m.state.tenants[tenantID]
	tenant.Status = access.SubscriptionInactive
	m.state.tenants[tenantID] = tenant
	m.state.versions[tenantID]++
	m.publish(subscription.PendingCut{Kind: subscription.CutDeactivation, AccessCut: subscription.AccessCut{
		TenantID: tenantID, Version: m.state.versions[tenantID], CorrelationID: correlationID, CreatedAt: at,
	}})
}

// Outbox returns every committed access cut, pending or applied, in commit
// order.
func (m *MemoryStore) Outbox() []subscription.PendingCut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.outbox)
}

// AppliedAt returns when the cut was settled.
func (m *MemoryStore) AppliedAt(id int64) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.applied[id]
	return at, ok
}

// PendingCuts returns up to limit committed cuts not yet settled, oldest
// first. FailOnce("PendingCuts", err) makes the next call fail.
func (m *MemoryStore) PendingCuts(_ context.Context, limit int) ([]subscription.PendingCut, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("PendingCuts"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		// As PostgreSQL, which refuses a limit it cannot use.
		return nil, fmt.Errorf("invitationtest: pending cuts: invalid limit %d", limit)
	}
	var pending []subscription.PendingCut
	for _, cut := range m.outbox {
		if _, settled := m.applied[cut.ID]; !settled && len(pending) < limit {
			pending = append(pending, cut)
		}
	}
	return pending, nil
}

// SettleCut marks a pending cut applied; an applied or unknown cut is
// unchanged. FailOnce("SettleCut", err) makes the next call fail.
func (m *MemoryStore) SettleCut(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("SettleCut"); err != nil {
		return err
	}
	if _, settled := m.applied[id]; !settled && id >= 1 && id <= int64(len(m.outbox)) {
		m.applied[id] = at
	}
	return nil
}

// publish appends a committed cut to the outbox with the next ID. The caller
// holds m.mu.
func (m *MemoryStore) publish(cut subscription.PendingCut) {
	cut.ID = int64(len(m.outbox)) + 1
	m.outbox = append(m.outbox, cut)
}

// Records returns the committed operation records by operation ID.
func (m *MemoryStore) Records() map[string]invitation.Record {
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

// InvitationLookups counts reads and writes of invitation rows.
func (m *MemoryStore) InvitationLookups() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invitationLookups
}

// OperationLookups counts reads and writes of operation records.
func (m *MemoryStore) OperationLookups() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.operationLookups
}

// InTx runs fn on a copy of the state and commits the copy if fn succeeds.
func (m *MemoryStore) InTx(_ context.Context, fn func(invitation.Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transactions++
	base := m.state.clone()
	tx := &memoryTx{store: m, state: m.state.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	if err := m.fail("commit"); err != nil {
		return err
	}
	m.state.merge(base, tx.state)
	var written []invitation.RevocationCut
	for key, cut := range tx.state.cuts {
		if _, ok := base.cuts[key]; !ok {
			written = append(written, cut)
		}
	}
	slices.SortFunc(written, func(a, b invitation.RevocationCut) int {
		return cmp.Or(strings.Compare(a.TenantID, b.TenantID), cmp.Compare(a.Version, b.Version))
	})
	for _, cut := range written {
		m.publish(subscription.PendingCut{Kind: subscription.CutRevocation, AccountID: cut.AccountID, AccessCut: subscription.AccessCut{
			TenantID: cut.TenantID, Version: cut.Version, CorrelationID: cut.CorrelationID, CreatedAt: cut.CreatedAt,
		}})
	}
	return nil
}

// step runs the hook of step with the store unlocked, then consumes its
// injected failure. The caller holds m.mu.
func (m *MemoryStore) step(name string) error {
	if fn := m.before[name]; fn != nil {
		delete(m.before, name)
		m.mu.Unlock()
		fn()
		m.mu.Lock()
	}
	return m.fail(name)
}

// fail consumes the injected failure of step. The caller holds m.mu.
func (m *MemoryStore) fail(step string) error {
	err := m.failures[step]
	delete(m.failures, step)
	return err
}

type memoryTx struct {
	store *MemoryStore
	state state
}

// relock refreshes a locked invitation row from the committed state, so the
// lock observes what concurrent transactions committed before it.
func (t *memoryTx) relock(id string) {
	if committed, ok := t.store.state.invitations[id]; ok {
		t.state.invitations[id] = committed
	}
}

func (t *memoryTx) LockOwnedTenant(_ context.Context, owner access.AccountID) (invitation.Tenant, bool, error) {
	if err := t.store.step("LockOwnedTenant"); err != nil {
		return invitation.Tenant{}, false, err
	}
	for _, tenant := range t.state.tenants {
		if tenant.Owner == owner {
			return tenant, true, nil
		}
	}
	return invitation.Tenant{}, false, nil
}

func (t *memoryTx) LockTenant(_ context.Context, tenantID string) (invitation.Tenant, bool, error) {
	if err := t.store.step("LockTenant"); err != nil {
		return invitation.Tenant{}, false, err
	}
	if committed, ok := t.store.state.tenants[tenantID]; ok {
		t.state.tenants[tenantID] = committed
	}
	tenant, ok := t.state.tenants[tenantID]
	return tenant, ok, nil
}

func (t *memoryTx) Operation(_ context.Context, operationID string, notBefore time.Time) (invitation.Record, bool, error) {
	t.store.operationLookups++
	if err := t.store.step("Operation"); err != nil {
		return invitation.Record{}, false, err
	}
	// A read sees what other transactions committed, as in READ COMMITTED.
	if committed, ok := t.store.state.records[operationID]; ok {
		t.state.records[operationID] = committed
	}
	record, ok := t.state.records[operationID]
	if !ok || !record.AcceptedAt.After(notBefore) {
		return invitation.Record{}, false, nil
	}
	return record, true, nil
}

func (t *memoryTx) InsertInvitation(_ context.Context, stored invitation.Stored) error {
	t.store.invitationLookups++
	if err := t.store.step("InsertInvitation"); err != nil {
		return err
	}
	// The unique index also sees rows other transactions committed meanwhile.
	for _, rows := range []map[string]invitation.Stored{t.store.state.invitations, t.state.invitations} {
		for _, existing := range rows {
			if existing.TokenHash == stored.TokenHash {
				return invitation.ErrTokenTaken
			}
		}
	}
	t.state.invitations[stored.ID] = stored
	return nil
}

func (t *memoryTx) PendingInvitations(_ context.Context, tenantID string, now time.Time) ([]invitation.Stored, error) {
	t.store.invitationLookups++
	if err := t.store.step("PendingInvitations"); err != nil {
		return nil, err
	}
	var pending []invitation.Stored
	for _, stored := range t.state.invitations {
		if stored.TenantID == tenantID && stored.Pending(now) {
			pending = append(pending, stored)
		}
	}
	slices.SortFunc(pending, func(a, b invitation.Stored) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return pending, nil
}

func (t *memoryTx) LockInvitation(_ context.Context, tenantID, invitationID string) (invitation.Stored, bool, error) {
	t.store.invitationLookups++
	if err := t.store.step("LockInvitation"); err != nil {
		return invitation.Stored{}, false, err
	}
	t.relock(invitationID)
	stored, ok := t.state.invitations[invitationID]
	if !ok || stored.TenantID != tenantID {
		return invitation.Stored{}, false, nil
	}
	return stored, true, nil
}

func (t *memoryTx) LockInvitationByToken(_ context.Context, tokenHash [32]byte) (invitation.Stored, bool, error) {
	t.store.invitationLookups++
	if err := t.store.step("LockInvitationByToken"); err != nil {
		return invitation.Stored{}, false, err
	}
	for id, stored := range t.store.state.invitations {
		if stored.TokenHash == tokenHash {
			t.relock(id)
		}
	}
	for _, stored := range t.state.invitations {
		if stored.TokenHash == tokenHash {
			return stored, true, nil
		}
	}
	return invitation.Stored{}, false, nil
}

func (t *memoryTx) RevokeInvitation(_ context.Context, invitationID string, at time.Time) error {
	t.store.invitationLookups++
	if err := t.store.step("RevokeInvitation"); err != nil {
		return err
	}
	stored := t.state.invitations[invitationID]
	stored.RevokedAt = at
	t.state.invitations[invitationID] = stored
	return nil
}

func (t *memoryTx) IsMember(_ context.Context, tenantID string, account access.AccountID) (bool, error) {
	if err := t.store.step("IsMember"); err != nil {
		return false, err
	}
	key := [2]string{tenantID, string(account)}
	if role, ok := t.store.state.memberships[key]; ok {
		t.state.memberships[key] = role
	}
	_, ok := t.state.memberships[key]
	return ok, nil
}

func (t *memoryTx) AddModerator(_ context.Context, tenantID string, account access.AccountID) error {
	if err := t.store.step("AddModerator"); err != nil {
		return err
	}
	// The primary key also sees rows other transactions committed meanwhile.
	key := [2]string{tenantID, string(account)}
	if _, ok := t.store.state.memberships[key]; ok {
		return invitation.ErrMemberTaken
	}
	if _, ok := t.state.memberships[key]; ok {
		return invitation.ErrMemberTaken
	}
	t.state.memberships[key] = access.RoleModerator
	return nil
}

func (t *memoryTx) MarkAccepted(_ context.Context, invitationID string, account access.AccountID, at time.Time) error {
	t.store.invitationLookups++
	if err := t.store.step("MarkAccepted"); err != nil {
		return err
	}
	stored := t.state.invitations[invitationID]
	stored.AcceptedAt = at
	stored.AcceptedBy = account
	t.state.invitations[invitationID] = stored
	return nil
}

func (t *memoryTx) RecordOperation(_ context.Context, record invitation.Record, purgeBefore time.Time) error {
	t.store.operationLookups++
	if err := t.store.step("RecordOperation"); err != nil {
		return err
	}
	for id, r := range t.state.records {
		if !r.AcceptedAt.After(purgeBefore) {
			delete(t.state.records, id)
		}
	}
	// The primary key also sees rows other transactions committed meanwhile.
	if r, ok := t.store.state.records[record.OperationID]; ok && r.AcceptedAt.After(purgeBefore) {
		return invitation.ErrOperationRetained
	}
	if _, ok := t.state.records[record.OperationID]; ok {
		return invitation.ErrOperationRetained
	}
	t.state.records[record.OperationID] = record
	return nil
}

func (t *memoryTx) ModeratedCanvases(_ context.Context, account access.AccountID) ([]invitation.ModeratedCanvas, error) {
	if err := t.store.step("ModeratedCanvases"); err != nil {
		return nil, err
	}
	var moderated []invitation.ModeratedCanvas
	for key, role := range t.state.memberships {
		tenant, ok := t.state.tenants[key[0]]
		if key[1] != string(account) || role != access.RoleModerator || !ok {
			continue
		}
		moderated = append(moderated, invitation.ModeratedCanvas{
			Membership: access.Membership{TenantID: tenant.ID, Role: role, Subscription: tenant.Status},
			CanvasID:   tenant.CanvasID,
		})
	}
	slices.SortFunc(moderated, func(a, b invitation.ModeratedCanvas) int {
		return strings.Compare(a.Membership.TenantID, b.Membership.TenantID)
	})
	return moderated, nil
}

func (t *memoryTx) LockOwnedTenantForUpdate(_ context.Context, owner access.AccountID) (invitation.Tenant, bool, error) {
	if err := t.store.step("LockOwnedTenantForUpdate"); err != nil {
		return invitation.Tenant{}, false, err
	}
	for id, tenant := range t.store.state.tenants {
		if tenant.Owner == owner {
			t.state.tenants[id] = tenant
			t.state.versions[id] = t.store.state.versions[id]
			return tenant, true, nil
		}
	}
	return invitation.Tenant{}, false, nil
}

func (t *memoryTx) Members(_ context.Context, tenantID string) ([]invitation.Member, error) {
	if err := t.store.step("Members"); err != nil {
		return nil, err
	}
	var members []invitation.Member
	for key, role := range t.state.memberships {
		email, ok := t.state.emails[access.AccountID(key[1])]
		if key[0] != tenantID || role != access.RoleModerator || !ok {
			continue
		}
		member := invitation.Member{AccountID: access.AccountID(key[1]), Email: email}
		for _, stored := range t.state.invitations {
			if stored.TenantID == tenantID && stored.AcceptedBy == member.AccountID && stored.AcceptedAt.After(member.AcceptedAt) {
				member.AcceptedAt = stored.AcceptedAt
			}
		}
		members = append(members, member)
	}
	slices.SortFunc(members, func(a, b invitation.Member) int {
		if c := a.AcceptedAt.Compare(b.AcceptedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.AccountID), string(b.AccountID))
	})
	return members, nil
}

func (t *memoryTx) MemberRevocation(_ context.Context, operationID string, notBefore time.Time) (invitation.MemberRevocationRecord, bool, error) {
	t.store.operationLookups++
	if err := t.store.step("MemberRevocation"); err != nil {
		return invitation.MemberRevocationRecord{}, false, err
	}
	if committed, ok := t.store.state.revocations[operationID]; ok {
		t.state.revocations[operationID] = committed
	}
	record, ok := t.state.revocations[operationID]
	if !ok || !record.AcceptedAt.After(notBefore) {
		return invitation.MemberRevocationRecord{}, false, nil
	}
	return record, true, nil
}

func (t *memoryTx) LockMembership(_ context.Context, tenantID string, account access.AccountID) (access.Role, bool, error) {
	if err := t.store.step("LockMembership"); err != nil {
		return "", false, err
	}
	key := [2]string{tenantID, string(account)}
	if role, ok := t.store.state.memberships[key]; ok {
		t.state.memberships[key] = role
	} else {
		delete(t.state.memberships, key)
	}
	role, ok := t.state.memberships[key]
	return role, ok, nil
}

func (t *memoryTx) RemoveModerator(_ context.Context, tenantID string, account access.AccountID) error {
	if err := t.store.step("RemoveModerator"); err != nil {
		return err
	}
	delete(t.state.memberships, [2]string{tenantID, string(account)})
	return nil
}

func (t *memoryTx) IncrementAccessVersion(_ context.Context, tenantID string) (uint64, error) {
	if err := t.store.step("IncrementAccessVersion"); err != nil {
		return 0, err
	}
	t.state.versions[tenantID]++
	return t.state.versions[tenantID], nil
}

func (t *memoryTx) RecordCut(_ context.Context, cut invitation.RevocationCut) error {
	if err := t.store.step("RecordCut"); err != nil {
		return err
	}
	if err := storedCorrelation(cut.CorrelationID); err != nil {
		return err
	}
	t.state.cuts[cutKey{cut.TenantID, cut.Version}] = cut
	return nil
}

func (t *memoryTx) RecordMemberRevocation(_ context.Context, record invitation.MemberRevocationRecord, purgeBefore time.Time) error {
	t.store.operationLookups++
	if err := t.store.step("RecordMemberRevocation"); err != nil {
		return err
	}
	if err := storedCorrelation(record.CorrelationID); err != nil {
		return err
	}
	for id, r := range t.state.revocations {
		if !r.AcceptedAt.After(purgeBefore) {
			delete(t.state.revocations, id)
		}
	}
	// The primary key also sees rows other transactions committed meanwhile.
	if r, ok := t.store.state.revocations[record.OperationID]; ok && r.AcceptedAt.After(purgeBefore) {
		return invitation.ErrOperationRetained
	}
	if _, ok := t.state.revocations[record.OperationID]; ok {
		return invitation.ErrOperationRetained
	}
	t.state.revocations[record.OperationID] = record
	return nil
}

func (t *memoryTx) MemberAccess(_ context.Context, tenantID string, account access.AccountID) (invitation.MemberAccess, bool, error) {
	if err := t.store.step("MemberAccess"); err != nil {
		return invitation.MemberAccess{}, false, err
	}
	// A plain read sees what other transactions committed, as in READ
	// COMMITTED.
	committed := t.store.state
	role, ok := committed.memberships[[2]string{tenantID, string(account)}]
	tenant, exists := committed.tenants[tenantID]
	if !ok || !exists {
		return invitation.MemberAccess{}, false, nil
	}
	return invitation.MemberAccess{
		Membership: access.Membership{TenantID: tenantID, Role: role, Subscription: tenant.Status},
		Version:    committed.versions[tenantID],
	}, true, nil
}

// storedCorrelation refuses a correlation PostgreSQL's 128-octet check
// refuses.
func storedCorrelation(correlationID string) error {
	if len(correlationID) > 128 {
		return fmt.Errorf("invitationtest: correlation of %d octets", len(correlationID))
	}
	return nil
}
