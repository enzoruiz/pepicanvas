package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// SubscriptionStore implements subscription.Store and subscription.CutStore.
// An activation commits the tenant, its owner membership, its canvas, and the
// operation record in one transaction; a deactivation commits the tenant's
// status and access version, its pending access cut, and the operation record
// in one transaction.
type SubscriptionStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
	// fault, when set by tests, runs after a step ("CreateTenant",
	// "RecordCut") and before commit ("commit"); an error aborts the
	// transaction.
	fault func(ctx context.Context, step string) error
}

var (
	_ subscription.Store    = (*SubscriptionStore)(nil)
	_ subscription.CutStore = (*SubscriptionStore)(nil)
)

// NewSubscriptionStore returns a store over a migrated pool.
func NewSubscriptionStore(pool *pgxpool.Pool) *SubscriptionStore {
	return &SubscriptionStore{pool: pool, now: time.Now}
}

// InTx runs fn in one READ COMMITTED transaction and commits only if fn
// returns nil. The row locks taken by fn are held until commit.
func (s *SubscriptionStore) InTx(ctx context.Context, fn func(subscription.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := fn(&subscriptionTx{store: s, q: db.New(tx)}); err != nil {
			return err
		}
		return s.step(ctx, "commit")
	})
}

func (s *SubscriptionStore) step(ctx context.Context, name string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(ctx, name)
}

// IsOperator reads the current operator grant.
func (s *SubscriptionStore) IsOperator(ctx context.Context, account access.AccountID) (bool, error) {
	_, err := db.New(s.pool).GetOperator(ctx, string(account))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: read operator: %w", err)
	}
	return true, nil
}

// OwnedTenant returns the tenant the account owns.
func (s *SubscriptionStore) OwnedTenant(ctx context.Context, account access.AccountID) (subscription.Tenant, bool, error) {
	tenant, found, err := ownedTenant(ctx, db.New(s.pool), account)
	if err != nil {
		return subscription.Tenant{}, false, fmt.Errorf("postgres: owned tenant: %w", err)
	}
	return tenant, found, nil
}

// GrantOperator gives the account of email the operator role and returns its
// ID. Granting an operator again keeps the first grant. An email that no
// account can have reports subscription.ErrAccountNotFound.
func (s *SubscriptionStore) GrantOperator(ctx context.Context, email string) (access.AccountID, error) {
	normalized, ok := access.NormalizeEmail(email)
	if !ok {
		return "", subscription.ErrAccountNotFound
	}
	var account access.AccountID
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		id, err := q.LockAccountByEmail(ctx, normalized)
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrAccountNotFound
		}
		if err != nil {
			return err
		}
		account = access.AccountID(id)
		return q.GrantOperator(ctx, db.GrantOperatorParams{AccountID: id, GrantedAt: timestamptz(s.now())})
	})
	if errors.Is(err, subscription.ErrAccountNotFound) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("postgres: grant operator: %w", err)
	}
	return account, nil
}

// PendingCuts returns up to limit access cuts not yet applied, oldest first,
// deactivations and revocations alike.
func (s *SubscriptionStore) PendingCuts(ctx context.Context, limit int) ([]subscription.PendingCut, error) {
	if limit <= 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("postgres: pending cuts: invalid limit %d", limit)
	}
	rows, err := db.New(s.pool).ListPendingCuts(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: pending cuts: %w", err)
	}
	cuts := make([]subscription.PendingCut, 0, len(rows))
	for _, row := range rows {
		cuts = append(cuts, subscription.PendingCut{ID: row.ID, Kind: subscription.CutKind(row.Kind), AccountID: access.AccountID(row.AccountID.String), AccessCut: subscription.AccessCut{
			TenantID:      row.TenantID,
			Version:       uint64(row.Version),
			CorrelationID: row.CorrelationID.String,
			CreatedAt:     row.CreatedAt.Time,
		}})
	}
	return cuts, nil
}

// SettleCut marks a pending cut applied at at. An applied cut keeps its first
// applied time; an unknown cut changes nothing.
func (s *SubscriptionStore) SettleCut(ctx context.Context, id int64, at time.Time) error {
	if err := db.New(s.pool).SettleCut(ctx, db.SettleCutParams{ID: id, AppliedAt: timestamptz(at)}); err != nil {
		return fmt.Errorf("postgres: settle cut: %w", err)
	}
	return nil
}

func ownedTenant(ctx context.Context, q *db.Queries, account access.AccountID) (subscription.Tenant, bool, error) {
	row, err := q.GetOwnedTenant(ctx, pgtype.Text{String: string(account), Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return subscription.Tenant{}, false, nil
	}
	if err != nil {
		return subscription.Tenant{}, false, err
	}
	return subscription.Tenant{
		ID:       row.TenantID,
		Owner:    account,
		CanvasID: row.CanvasID.String,
		Status:   access.SubscriptionStatus(row.SubscriptionStatus),
		Version:  uint64(row.AccessVersion),
	}, true, nil
}

type subscriptionTx struct {
	store *SubscriptionStore
	q     *db.Queries
}

func (t *subscriptionTx) IsOperator(ctx context.Context, account access.AccountID) (bool, error) {
	_, err := t.q.LockOperator(ctx, string(account))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (t *subscriptionTx) Activation(ctx context.Context, operationID string, notBefore time.Time) (subscription.Record, bool, error) {
	row, err := t.q.GetActivation(ctx, db.GetActivationParams{OperationID: operationID, NotBefore: timestamptz(notBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return subscription.Record{}, false, nil
	}
	if err != nil {
		return subscription.Record{}, false, err
	}
	record := subscription.Record{
		OperationID: operationID,
		Result: subscription.Activation{
			AccountID: access.AccountID(row.AccountID),
			TenantID:  row.TenantID,
			CanvasID:  row.CanvasID,
			Created:   row.Created,
		},
		CorrelationID: row.CorrelationID.String,
		AcceptedAt:    row.AcceptedAt.Time,
	}
	copy(record.Fingerprint[:], row.Fingerprint)
	return record, true, nil
}

func (t *subscriptionTx) LockOperation(ctx context.Context, operationID string) error {
	return t.q.LockOperation(ctx, operationID)
}

func (t *subscriptionTx) LockAccount(ctx context.Context, email string) (access.AccountID, bool, error) {
	id, err := t.q.LockAccountByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return access.AccountID(id), true, nil
}

func (t *subscriptionTx) TenantOwnedBy(ctx context.Context, account access.AccountID) (subscription.Tenant, bool, error) {
	return ownedTenant(ctx, t.q, account)
}

func (t *subscriptionTx) CreateTenant(ctx context.Context, tenant subscription.Tenant) error {
	if err := t.q.InsertOwnedTenant(ctx, db.InsertOwnedTenantParams{
		TenantID:           tenant.ID,
		SubscriptionStatus: string(tenant.Status),
		OwnerAccountID:     pgtype.Text{String: string(tenant.Owner), Valid: true},
		CanvasID:           pgtype.Text{String: tenant.CanvasID, Valid: true},
	}); err != nil {
		return err
	}
	if err := t.q.InsertMembership(ctx, db.InsertMembershipParams{
		TenantID: tenant.ID, AccountID: string(tenant.Owner), Role: string(access.RoleOwner),
	}); err != nil {
		return err
	}
	// The canvas takes the schema's default dimensions, 1920x1080.
	if err := t.q.CreateCanvas(ctx, db.CreateCanvasParams{TenantID: tenant.ID, CanvasID: tenant.CanvasID}); err != nil {
		return err
	}
	return t.store.step(ctx, "CreateTenant")
}

func (t *subscriptionTx) ActivateTenant(ctx context.Context, tenantID string) error {
	return t.q.SetTenantStatus(ctx, db.SetTenantStatusParams{TenantID: tenantID, SubscriptionStatus: string(access.SubscriptionActive)})
}

func (t *subscriptionTx) RecordActivation(ctx context.Context, record subscription.Record, purgeBefore time.Time) error {
	expired := timestamptz(purgeBefore)
	// The expired row of this operation must go, or its key blocks the insert.
	if err := t.q.DeleteExpiredActivation(ctx, db.DeleteExpiredActivationParams{OperationID: record.OperationID, ExpiredAt: expired}); err != nil {
		return err
	}
	if err := t.q.PurgeExpiredActivations(ctx, expired); err != nil {
		return err
	}
	err := t.q.InsertActivation(ctx, db.InsertActivationParams{
		OperationID:       record.OperationID,
		Fingerprint:       record.Fingerprint[:],
		OperatorAccountID: string(record.OperatorID),
		AccountID:         string(record.Result.AccountID),
		TenantID:          record.Result.TenantID,
		CanvasID:          record.Result.CanvasID,
		Created:           record.Result.Created,
		CorrelationID:     pgtype.Text{String: record.CorrelationID, Valid: record.CorrelationID != ""},
		AcceptedAt:        timestamptz(record.AcceptedAt),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "subscription_activations_pkey" {
		return subscription.ErrOperationRetained
	}
	return err
}

func (t *subscriptionTx) Deactivation(ctx context.Context, operationID string, notBefore time.Time) (subscription.DeactivationRecord, bool, error) {
	row, err := t.q.GetDeactivation(ctx, db.GetDeactivationParams{OperationID: operationID, NotBefore: timestamptz(notBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return subscription.DeactivationRecord{}, false, nil
	}
	if err != nil {
		return subscription.DeactivationRecord{}, false, err
	}
	record := subscription.DeactivationRecord{
		OperationID: operationID,
		OperatorID:  access.AccountID(row.OperatorAccountID),
		Result: subscription.Deactivation{
			AccountID:   access.AccountID(row.AccountID),
			TenantID:    row.TenantID,
			CanvasID:    row.CanvasID,
			Deactivated: row.Deactivated,
			Version:     uint64(row.Version),
		},
		CorrelationID: row.CorrelationID.String,
		AcceptedAt:    row.AcceptedAt.Time,
	}
	copy(record.Fingerprint[:], row.Fingerprint)
	return record, true, nil
}

func (t *subscriptionTx) DeactivateTenant(ctx context.Context, tenantID string) (uint64, error) {
	version, err := t.q.DeactivateTenant(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("tenant %q is not active", tenantID)
	}
	if err != nil {
		return 0, err
	}
	return uint64(version), nil
}

func (t *subscriptionTx) RecordCut(ctx context.Context, cut subscription.AccessCut) error {
	if cut.Version == 0 || cut.Version > math.MaxInt64 {
		return fmt.Errorf("invalid cut version %d", cut.Version)
	}
	if err := t.q.InsertAccessCut(ctx, db.InsertAccessCutParams{
		TenantID:      cut.TenantID,
		Version:       int64(cut.Version),
		CorrelationID: pgtype.Text{String: cut.CorrelationID, Valid: cut.CorrelationID != ""},
		CreatedAt:     timestamptz(cut.CreatedAt),
	}); err != nil {
		return err
	}
	return t.store.step(ctx, "RecordCut")
}

func (t *subscriptionTx) RecordDeactivation(ctx context.Context, record subscription.DeactivationRecord, purgeBefore time.Time) error {
	if record.Result.Version == 0 || record.Result.Version > math.MaxInt64 {
		return fmt.Errorf("invalid deactivation version %d", record.Result.Version)
	}
	expired := timestamptz(purgeBefore)
	// The expired row of this operation must go, or its key blocks the insert.
	if err := t.q.DeleteExpiredDeactivation(ctx, db.DeleteExpiredDeactivationParams{OperationID: record.OperationID, ExpiredAt: expired}); err != nil {
		return err
	}
	if err := t.q.PurgeExpiredDeactivations(ctx, expired); err != nil {
		return err
	}
	err := t.q.InsertDeactivation(ctx, db.InsertDeactivationParams{
		OperationID:       record.OperationID,
		Fingerprint:       record.Fingerprint[:],
		OperatorAccountID: string(record.OperatorID),
		AccountID:         string(record.Result.AccountID),
		TenantID:          record.Result.TenantID,
		CanvasID:          record.Result.CanvasID,
		Deactivated:       record.Result.Deactivated,
		Version:           int64(record.Result.Version),
		CorrelationID:     pgtype.Text{String: record.CorrelationID, Valid: record.CorrelationID != ""},
		AcceptedAt:        timestamptz(record.AcceptedAt),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "subscription_deactivations_pkey" {
		return subscription.ErrOperationRetained
	}
	return err
}
