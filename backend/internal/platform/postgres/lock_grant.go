package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
)

// MemberGrant returns the account's accepted role in the tenant, as
// MembershipOf does, with the tenant's confirmed access version read in the
// same statement. A TE-002 grant built from both names the version the
// membership was read at, so a cut confirmed after the read applies to it
// (US-020). It serves owners and moderators alike.
func (s *AccountStore) MemberGrant(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, uint64, bool, error) {
	row, err := db.New(s.pool).GetMemberGrant(ctx, db.GetMemberGrantParams{AccountID: string(account), TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Membership{}, 0, false, nil
	}
	if err != nil {
		return access.Membership{}, 0, false, fmt.Errorf("postgres: load member grant: %w", err)
	}
	return access.Membership{
		TenantID:     tenantID,
		Role:         access.Role(row.Role),
		Subscription: access.SubscriptionStatus(row.SubscriptionStatus),
	}, uint64(row.AccessVersion), true, nil
}
