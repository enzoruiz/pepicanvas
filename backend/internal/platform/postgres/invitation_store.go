package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
)

// InvitationStore implements invitation.Store. Owner actions share-lock the
// owner's tenant; revocations and acceptances lock the invitation row, so the
// later of the two sees the earlier commit.
type InvitationStore struct {
	pool *pgxpool.Pool
	// fault, when set by tests, runs after each Tx step, named after its
	// method ("InsertInvitation"), and before commit ("commit"); an error
	// aborts the transaction.
	fault func(ctx context.Context, step string) error
}

var _ invitation.Store = (*InvitationStore)(nil)

// NewInvitationStore returns a store over a migrated pool.
func NewInvitationStore(pool *pgxpool.Pool) *InvitationStore {
	return &InvitationStore{pool: pool}
}

// Now is the clock for invitation.WithClock. PostgreSQL keeps microseconds,
// so the current time is truncated to them and an answer replayed from
// storage equals the first one.
func (s *InvitationStore) Now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// InTx runs fn in one READ COMMITTED transaction and commits only if fn
// returns nil. The row locks taken by fn are held until commit.
func (s *InvitationStore) InTx(ctx context.Context, fn func(invitation.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := fn(&invitationTx{store: s, q: db.New(tx)}); err != nil {
			return err
		}
		return s.step(ctx, "commit")
	})
}

func (s *InvitationStore) step(ctx context.Context, name string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(ctx, name)
}

type invitationTx struct {
	store *InvitationStore
	q     *db.Queries
}

func (t *invitationTx) LockOwnedTenant(ctx context.Context, owner access.AccountID) (invitation.Tenant, bool, error) {
	row, err := t.q.LockOwnedTenantShare(ctx, pgtype.Text{String: string(owner), Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Tenant{}, false, t.store.step(ctx, "LockOwnedTenant")
	}
	if err != nil {
		return invitation.Tenant{}, false, err
	}
	tenant := invitation.Tenant{ID: row.TenantID, Owner: owner, Status: access.SubscriptionStatus(row.SubscriptionStatus)}
	return tenant, true, t.store.step(ctx, "LockOwnedTenant")
}

func (t *invitationTx) LockTenant(ctx context.Context, tenantID string) (invitation.Tenant, bool, error) {
	row, err := t.q.LockTenantShare(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Tenant{}, false, t.store.step(ctx, "LockTenant")
	}
	if err != nil {
		return invitation.Tenant{}, false, err
	}
	tenant := invitation.Tenant{
		ID:     row.TenantID,
		Owner:  access.AccountID(row.OwnerAccountID.String),
		Status: access.SubscriptionStatus(row.SubscriptionStatus),
	}
	return tenant, true, t.store.step(ctx, "LockTenant")
}

func (t *invitationTx) Operation(ctx context.Context, operationID string, notBefore time.Time) (invitation.Record, bool, error) {
	row, err := t.q.GetInvitationOperation(ctx, db.GetInvitationOperationParams{OperationID: operationID, NotBefore: timestamptz(notBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Record{}, false, t.store.step(ctx, "Operation")
	}
	if err != nil {
		return invitation.Record{}, false, err
	}
	record := invitation.Record{
		OperationID:   operationID,
		Kind:          invitation.OperationKind(row.Kind),
		OwnerID:       access.AccountID(row.OwnerAccountID),
		CorrelationID: row.CorrelationID.String,
		AcceptedAt:    utc(row.AcceptedAt),
	}
	copy(record.Fingerprint[:], row.Fingerprint)
	switch record.Kind {
	case invitation.OperationCreate:
		record.Invitation = invitation.Invitation{
			ID:        row.InvitationID,
			TenantID:  row.TenantID,
			CreatedAt: utc(row.CreatedAt),
			ExpiresAt: utc(row.ExpiresAt),
		}
	case invitation.OperationRevoke:
		record.Revocation = invitation.Revocation{InvitationID: row.InvitationID, Status: access.InvitationStatus(row.Status.String)}
	}
	return record, true, t.store.step(ctx, "Operation")
}

func (t *invitationTx) InsertInvitation(ctx context.Context, stored invitation.Stored) error {
	err := t.q.InsertInvitation(ctx, db.InsertInvitationParams{
		InvitationID: stored.ID,
		TenantID:     stored.TenantID,
		TokenHash:    stored.TokenHash[:],
		CreatedBy:    string(stored.CreatedBy),
		CreatedAt:    timestamptz(stored.CreatedAt),
		ExpiresAt:    timestamptz(stored.ExpiresAt),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "invitations_token_hash_key" {
		return invitation.ErrTokenTaken
	}
	if err != nil {
		return err
	}
	return t.store.step(ctx, "InsertInvitation")
}

func (t *invitationTx) PendingInvitations(ctx context.Context, tenantID string, now time.Time) ([]invitation.Stored, error) {
	rows, err := t.q.ListPendingInvitations(ctx, db.ListPendingInvitationsParams{TenantID: tenantID, Now: timestamptz(now)})
	if err != nil {
		return nil, err
	}
	pending := make([]invitation.Stored, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, storedInvitation(row))
	}
	return pending, t.store.step(ctx, "PendingInvitations")
}

func (t *invitationTx) LockInvitation(ctx context.Context, tenantID, invitationID string) (invitation.Stored, bool, error) {
	row, err := t.q.LockInvitation(ctx, db.LockInvitationParams{TenantID: tenantID, InvitationID: invitationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Stored{}, false, t.store.step(ctx, "LockInvitation")
	}
	if err != nil {
		return invitation.Stored{}, false, err
	}
	return storedInvitation(row), true, t.store.step(ctx, "LockInvitation")
}

func (t *invitationTx) LockInvitationByToken(ctx context.Context, tokenHash [32]byte) (invitation.Stored, bool, error) {
	row, err := t.q.LockInvitationByToken(ctx, tokenHash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Stored{}, false, t.store.step(ctx, "LockInvitationByToken")
	}
	if err != nil {
		return invitation.Stored{}, false, err
	}
	return storedInvitation(row), true, t.store.step(ctx, "LockInvitationByToken")
}

func (t *invitationTx) RevokeInvitation(ctx context.Context, invitationID string, at time.Time) error {
	n, err := t.q.RevokeInvitation(ctx, db.RevokeInvitationParams{InvitationID: invitationID, RevokedAt: timestamptz(at)})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("revoke invitation: %d rows are neither revoked nor accepted, want 1", n)
	}
	return t.store.step(ctx, "RevokeInvitation")
}

func (t *invitationTx) IsMember(ctx context.Context, tenantID string, account access.AccountID) (bool, error) {
	member, err := t.q.IsTenantMember(ctx, db.IsTenantMemberParams{TenantID: tenantID, AccountID: string(account)})
	if err != nil {
		return false, err
	}
	return member, t.store.step(ctx, "IsMember")
}

func (t *invitationTx) AddModerator(ctx context.Context, tenantID string, account access.AccountID) error {
	if err := t.q.InsertMembership(ctx, db.InsertMembershipParams{
		TenantID: tenantID, AccountID: string(account), Role: string(access.RoleModerator),
	}); err != nil {
		return err
	}
	return t.store.step(ctx, "AddModerator")
}

func (t *invitationTx) MarkAccepted(ctx context.Context, invitationID string, account access.AccountID, at time.Time) error {
	n, err := t.q.MarkInvitationAccepted(ctx, db.MarkInvitationAcceptedParams{
		InvitationID: invitationID,
		AcceptedAt:   timestamptz(at),
		AcceptedBy:   pgtype.Text{String: string(account), Valid: true},
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("mark invitation accepted: %d rows are neither revoked nor accepted, want 1", n)
	}
	return t.store.step(ctx, "MarkAccepted")
}

func (t *invitationTx) RecordOperation(ctx context.Context, record invitation.Record, purgeBefore time.Time) error {
	expired := timestamptz(purgeBefore)
	// The expired row of this operation must go, or its key blocks the insert.
	if err := t.q.DeleteExpiredInvitationOperation(ctx, db.DeleteExpiredInvitationOperationParams{OperationID: record.OperationID, ExpiredAt: expired}); err != nil {
		return err
	}
	if err := t.q.PurgeExpiredInvitationOperations(ctx, expired); err != nil {
		return err
	}
	params := db.InsertInvitationOperationParams{
		OperationID:    record.OperationID,
		Kind:           string(record.Kind),
		OwnerAccountID: string(record.OwnerID),
		Fingerprint:    record.Fingerprint[:],
		CorrelationID:  pgtype.Text{String: record.CorrelationID, Valid: record.CorrelationID != ""},
		AcceptedAt:     timestamptz(record.AcceptedAt),
	}
	switch record.Kind {
	case invitation.OperationCreate:
		params.InvitationID = record.Invitation.ID
		params.CreatedAt = timestamptz(record.Invitation.CreatedAt)
		params.ExpiresAt = timestamptz(record.Invitation.ExpiresAt)
	case invitation.OperationRevoke:
		params.InvitationID = record.Revocation.InvitationID
		params.Status = pgtype.Text{String: string(record.Revocation.Status), Valid: true}
	default:
		return fmt.Errorf("record invitation operation: unknown kind %q", record.Kind)
	}
	n, err := t.q.InsertInvitationOperation(ctx, params)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "invitation_operations_pkey" {
		return invitation.ErrOperationRetained
	}
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("record invitation operation: invitation %q not found", params.InvitationID)
	}
	return t.store.step(ctx, "RecordOperation")
}

func storedInvitation(row db.Invitation) invitation.Stored {
	stored := invitation.Stored{
		ID:         row.InvitationID,
		TenantID:   row.TenantID,
		CreatedBy:  access.AccountID(row.CreatedBy),
		CreatedAt:  utc(row.CreatedAt),
		ExpiresAt:  utc(row.ExpiresAt),
		RevokedAt:  utc(row.RevokedAt),
		AcceptedAt: utc(row.AcceptedAt),
		AcceptedBy: access.AccountID(row.AcceptedBy.String),
	}
	copy(stored.TokenHash[:], row.TokenHash)
	return stored
}

// utc returns a stored time in UTC, or the zero time for NULL.
func utc(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}
