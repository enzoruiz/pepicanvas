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
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
)

// InvitationStore implements invitation.Store. Owner actions share-lock the
// owner's tenant; revocations and acceptances lock the invitation row, so the
// later of the two sees the earlier commit. The memberships primary key
// orders one account's acceptances of two invitations of a tenant. A
// membership revocation locks the owner's tenant FOR UPDATE and commits the
// membership delete, the access version, its access cut, and its record in
// one transaction.
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
		ID:       row.TenantID,
		Owner:    access.AccountID(row.OwnerAccountID.String),
		CanvasID: row.CanvasID.String,
		Status:   access.SubscriptionStatus(row.SubscriptionStatus),
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
	case invitation.OperationAccept:
		record.Acceptance = invitation.Acceptance{TenantID: row.TenantID, CanvasID: row.CanvasID.String, InvitationID: row.InvitationID}
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
	err := t.q.InsertMembership(ctx, db.InsertMembershipParams{
		TenantID: tenantID, AccountID: string(account), Role: string(access.RoleModerator),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "memberships_pkey" {
		return invitation.ErrMemberTaken
	}
	if err != nil {
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
	case invitation.OperationAccept:
		params.InvitationID = record.Acceptance.InvitationID
		params.Status = pgtype.Text{String: string(access.InvitationAccepted), Valid: true}
		params.CanvasID = pgtype.Text{String: record.Acceptance.CanvasID, Valid: true}
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

func (t *invitationTx) ModeratedCanvases(ctx context.Context, account access.AccountID) ([]invitation.ModeratedCanvas, error) {
	rows, err := t.q.ListModeratedCanvases(ctx, string(account))
	if err != nil {
		return nil, err
	}
	moderated := make([]invitation.ModeratedCanvas, 0, len(rows))
	for _, row := range rows {
		moderated = append(moderated, invitation.ModeratedCanvas{
			Membership: access.Membership{
				TenantID:     row.TenantID,
				Role:         access.RoleModerator,
				Subscription: access.SubscriptionStatus(row.SubscriptionStatus),
			},
			CanvasID: row.CanvasID.String,
		})
	}
	return moderated, t.store.step(ctx, "ModeratedCanvases")
}

func (t *invitationTx) LockOwnedTenantForUpdate(ctx context.Context, owner access.AccountID) (invitation.Tenant, bool, error) {
	row, err := t.q.LockOwnedTenantUpdate(ctx, pgtype.Text{String: string(owner), Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.Tenant{}, false, t.store.step(ctx, "LockOwnedTenantForUpdate")
	}
	if err != nil {
		return invitation.Tenant{}, false, err
	}
	tenant := invitation.Tenant{ID: row.TenantID, Owner: owner, Status: access.SubscriptionStatus(row.SubscriptionStatus)}
	return tenant, true, t.store.step(ctx, "LockOwnedTenantForUpdate")
}

func (t *invitationTx) Members(ctx context.Context, tenantID string) ([]invitation.Member, error) {
	rows, err := t.q.ListTenantModerators(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	members := make([]invitation.Member, 0, len(rows))
	for _, row := range rows {
		members = append(members, invitation.Member{AccountID: access.AccountID(row.AccountID), Email: row.Email, AcceptedAt: utc(row.AcceptedAt)})
	}
	return members, t.store.step(ctx, "Members")
}

func (t *invitationTx) MemberRevocation(ctx context.Context, operationID string, notBefore time.Time) (invitation.MemberRevocationRecord, bool, error) {
	row, err := t.q.GetMembershipRevocation(ctx, db.GetMembershipRevocationParams{OperationID: operationID, NotBefore: timestamptz(notBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.MemberRevocationRecord{}, false, t.store.step(ctx, "MemberRevocation")
	}
	if err != nil {
		return invitation.MemberRevocationRecord{}, false, err
	}
	record := invitation.MemberRevocationRecord{
		OperationID:   operationID,
		OwnerID:       access.AccountID(row.OwnerAccountID),
		Result:        invitation.MemberRevocation{TenantID: row.TenantID, AccountID: access.AccountID(row.AccountID)},
		Version:       uint64(row.Version),
		CorrelationID: row.CorrelationID.String,
		AcceptedAt:    utc(row.AcceptedAt),
	}
	copy(record.Fingerprint[:], row.Fingerprint)
	return record, true, t.store.step(ctx, "MemberRevocation")
}

func (t *invitationTx) LockMembership(ctx context.Context, tenantID string, account access.AccountID) (access.Role, bool, error) {
	role, err := t.q.LockMembership(ctx, db.LockMembershipParams{TenantID: tenantID, AccountID: string(account)})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, t.store.step(ctx, "LockMembership")
	}
	if err != nil {
		return "", false, err
	}
	return access.Role(role), true, t.store.step(ctx, "LockMembership")
}

func (t *invitationTx) RemoveModerator(ctx context.Context, tenantID string, account access.AccountID) error {
	n, err := t.q.DeleteModeratorMembership(ctx, db.DeleteModeratorMembershipParams{TenantID: tenantID, AccountID: string(account)})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("remove moderator: %d moderator memberships, want 1", n)
	}
	return t.store.step(ctx, "RemoveModerator")
}

func (t *invitationTx) IncrementAccessVersion(ctx context.Context, tenantID string) (uint64, error) {
	version, err := t.q.IncrementAccessVersion(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return uint64(version), t.store.step(ctx, "IncrementAccessVersion")
}

func (t *invitationTx) RecordCut(ctx context.Context, cut invitation.RevocationCut) error {
	if cut.Version == 0 || cut.Version > math.MaxInt64 {
		return fmt.Errorf("invalid cut version %d", cut.Version)
	}
	if err := t.q.InsertRevocationCut(ctx, db.InsertRevocationCutParams{
		TenantID:      cut.TenantID,
		AccountID:     pgtype.Text{String: string(cut.AccountID), Valid: true},
		Version:       int64(cut.Version),
		CorrelationID: pgtype.Text{String: cut.CorrelationID, Valid: cut.CorrelationID != ""},
		CreatedAt:     timestamptz(cut.CreatedAt),
	}); err != nil {
		return err
	}
	return t.store.step(ctx, "RecordCut")
}

func (t *invitationTx) RecordMemberRevocation(ctx context.Context, record invitation.MemberRevocationRecord, purgeBefore time.Time) error {
	if record.Version == 0 || record.Version > math.MaxInt64 {
		return fmt.Errorf("invalid revocation version %d", record.Version)
	}
	expired := timestamptz(purgeBefore)
	// The expired row of this operation must go, or its key blocks the insert.
	if err := t.q.DeleteExpiredMembershipRevocation(ctx, db.DeleteExpiredMembershipRevocationParams{OperationID: record.OperationID, ExpiredAt: expired}); err != nil {
		return err
	}
	if err := t.q.PurgeExpiredMembershipRevocations(ctx, expired); err != nil {
		return err
	}
	err := t.q.InsertMembershipRevocation(ctx, db.InsertMembershipRevocationParams{
		OperationID:    record.OperationID,
		Fingerprint:    record.Fingerprint[:],
		OwnerAccountID: string(record.OwnerID),
		TenantID:       record.Result.TenantID,
		AccountID:      string(record.Result.AccountID),
		Version:        int64(record.Version),
		CorrelationID:  pgtype.Text{String: record.CorrelationID, Valid: record.CorrelationID != ""},
		AcceptedAt:     timestamptz(record.AcceptedAt),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "membership_revocations_pkey" {
		return invitation.ErrOperationRetained
	}
	if err != nil {
		return err
	}
	return t.store.step(ctx, "RecordMemberRevocation")
}

func (t *invitationTx) MemberAccess(ctx context.Context, tenantID string, account access.AccountID) (invitation.MemberAccess, bool, error) {
	row, err := t.q.GetMemberAccess(ctx, db.GetMemberAccessParams{TenantID: tenantID, AccountID: string(account)})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitation.MemberAccess{}, false, t.store.step(ctx, "MemberAccess")
	}
	if err != nil {
		return invitation.MemberAccess{}, false, err
	}
	member := invitation.MemberAccess{
		Membership: access.Membership{TenantID: tenantID, Role: access.Role(row.Role), Subscription: access.SubscriptionStatus(row.SubscriptionStatus)},
		Version:    uint64(row.AccessVersion),
	}
	return member, true, t.store.step(ctx, "MemberAccess")
}
