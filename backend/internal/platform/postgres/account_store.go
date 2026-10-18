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
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
)

const uniqueViolation = "23505"

// AccountStore implements the access account, session, and membership ports.
type AccountStore struct {
	pool *pgxpool.Pool
}

var (
	_ access.AccountStore    = (*AccountStore)(nil)
	_ access.SessionStore    = (*AccountStore)(nil)
	_ access.MembershipStore = (*AccountStore)(nil)
)

// NewAccountStore returns a store over a migrated pool.
func NewAccountStore(pool *pgxpool.Pool) *AccountStore {
	return &AccountStore{pool: pool}
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// FindRegistration returns the registration of operationID accepted after notBefore.
func (s *AccountStore) FindRegistration(ctx context.Context, operationID string, notBefore time.Time) (access.Registration, bool, error) {
	row, err := db.New(s.pool).GetRegistration(ctx, db.GetRegistrationParams{OperationID: operationID, NotBefore: timestamptz(notBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Registration{}, false, nil
	}
	if err != nil {
		return access.Registration{}, false, fmt.Errorf("postgres: find registration: %w", err)
	}
	registration := access.Registration{
		OperationID: row.OperationID,
		AccountID:   access.AccountID(row.AccountID),
		AcceptedAt:  row.AcceptedAt.Time,
	}
	copy(registration.EmailDigest[:], row.EmailDigest)
	return registration, true, nil
}

// CreateAccount commits the account and its registration in one transaction,
// after dropping expired registrations.
func (s *AccountStore) CreateAccount(ctx context.Context, account access.NewAccount) error {
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		expired := timestamptz(account.PurgeBefore)
		// The expired row of this operation must go, or its key blocks the insert.
		if err := q.DeleteExpiredRegistration(ctx, db.DeleteExpiredRegistrationParams{OperationID: account.Registration.OperationID, ExpiredAt: expired}); err != nil {
			return err
		}
		if err := q.PurgeExpiredRegistrations(ctx, expired); err != nil {
			return err
		}
		if err := q.InsertAccount(ctx, db.InsertAccountParams{
			AccountID:    string(account.Account.ID),
			Email:        account.Account.Email,
			PasswordHash: account.Account.PasswordHash,
			CreatedAt:    timestamptz(account.Registration.AcceptedAt),
		}); err != nil {
			return err
		}
		return q.InsertRegistration(ctx, db.InsertRegistrationParams{
			OperationID:   account.Registration.OperationID,
			EmailDigest:   account.Registration.EmailDigest[:],
			AccountID:     string(account.Registration.AccountID),
			CorrelationID: pgtype.Text{String: account.CorrelationID, Valid: account.CorrelationID != ""},
			AcceptedAt:    timestamptz(account.Registration.AcceptedAt),
		})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		switch pgErr.ConstraintName {
		case "accounts_email_key":
			return access.ErrIdentityTaken
		case "account_registrations_pkey":
			return access.ErrOperationRetained
		}
	}
	if err != nil {
		return fmt.Errorf("postgres: create account: %w", err)
	}
	return nil
}

// AccountByEmail returns the account registered with a normalized email.
func (s *AccountStore) AccountByEmail(ctx context.Context, email string) (access.Account, bool, error) {
	row, err := db.New(s.pool).GetAccountByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Account{}, false, nil
	}
	if err != nil {
		return access.Account{}, false, fmt.Errorf("postgres: account by email: %w", err)
	}
	return access.Account{ID: access.AccountID(row.AccountID), Email: row.Email, PasswordHash: row.PasswordHash}, true, nil
}

// AccountByID returns the account with the given ID.
func (s *AccountStore) AccountByID(ctx context.Context, id access.AccountID) (access.Account, bool, error) {
	row, err := db.New(s.pool).GetAccountByID(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Account{}, false, nil
	}
	if err != nil {
		return access.Account{}, false, fmt.Errorf("postgres: account by id: %w", err)
	}
	return access.Account{ID: access.AccountID(row.AccountID), Email: row.Email, PasswordHash: row.PasswordHash}, true, nil
}

// CreateSession stores the session and drops the account's expired sessions.
func (s *AccountStore) CreateSession(ctx context.Context, session access.SessionRecord) error {
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.PurgeExpiredSessions(ctx, db.PurgeExpiredSessionsParams{
			AccountID:     string(session.AccountID),
			IdleBefore:    timestamptz(session.CreatedAt.Add(-access.SessionIdleTimeout)),
			CreatedBefore: timestamptz(session.CreatedAt.Add(-access.SessionAbsoluteLifetime)),
		}); err != nil {
			return err
		}
		return q.InsertSession(ctx, db.InsertSessionParams{
			TokenHash:  session.TokenHash[:],
			AccountID:  string(session.AccountID),
			CreatedAt:  timestamptz(session.CreatedAt),
			LastSeenAt: timestamptz(session.LastSeenAt),
		})
	})
	if err != nil {
		return fmt.Errorf("postgres: create session: %w", err)
	}
	return nil
}

// SessionByHash returns the session stored under a token hash.
func (s *AccountStore) SessionByHash(ctx context.Context, hash [32]byte) (access.SessionRecord, bool, error) {
	row, err := db.New(s.pool).GetSession(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return access.SessionRecord{}, false, nil
	}
	if err != nil {
		return access.SessionRecord{}, false, fmt.Errorf("postgres: load session: %w", err)
	}
	return access.SessionRecord{
		TokenHash:  hash,
		AccountID:  access.AccountID(row.AccountID),
		CreatedAt:  row.CreatedAt.Time,
		LastSeenAt: row.LastSeenAt.Time,
	}, true, nil
}

// TouchSession moves the session's last use forward to at.
func (s *AccountStore) TouchSession(ctx context.Context, hash [32]byte, at time.Time) error {
	if err := db.New(s.pool).TouchSession(ctx, db.TouchSessionParams{TokenHash: hash[:], SeenAt: timestamptz(at)}); err != nil {
		return fmt.Errorf("postgres: touch session: %w", err)
	}
	return nil
}

// DeleteSession removes the session; a missing session is not an error.
func (s *AccountStore) DeleteSession(ctx context.Context, hash [32]byte) error {
	if err := db.New(s.pool).DeleteSession(ctx, hash[:]); err != nil {
		return fmt.Errorf("postgres: delete session: %w", err)
	}
	return nil
}

// MembershipOf returns the account's accepted role in the tenant.
func (s *AccountStore) MembershipOf(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, bool, error) {
	row, err := db.New(s.pool).GetMembership(ctx, db.GetMembershipParams{AccountID: string(account), TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Membership{}, false, nil
	}
	if err != nil {
		return access.Membership{}, false, fmt.Errorf("postgres: load membership: %w", err)
	}
	return access.Membership{
		TenantID:     tenantID,
		Role:         access.Role(row.Role),
		Subscription: access.SubscriptionStatus(row.SubscriptionStatus),
	}, true, nil
}
