package postgres_test

import (
	"context"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// US-020 S8: a lock session's grant reads the owner's or the moderator's
// membership with the tenant's confirmed access version, so a later cut
// applies to it; a deactivation reads at its new version.
func TestMemberGrantReadsTheMembershipAtTheAccessVersion(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, a, _ := streamer(t, s, "alice@example.com")
	moderator := seedAccount(t, s, "moderator@example.com")
	stranger := seedAccount(t, s, "stranger@example.com")
	exec(t, s.pool, "INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", a.TenantID, string(moderator))
	seedAccount(t, s, "bob@example.com")
	b := mustActivate(t, s, operator, "op-activate-bob-lock", "bob@example.com")

	type grant struct {
		membership access.Membership
		version    uint64
		found      bool
	}
	read := func(account access.AccountID, tenantID string) grant {
		t.Helper()
		membership, version, found, err := s.accounts.MemberGrant(ctx, account, tenantID)
		if err != nil {
			t.Fatalf("MemberGrant(%s, %s): %v", account, tenantID, err)
		}
		return grant{membership, version, found}
	}
	active := func(role access.Role, version uint64) grant {
		return grant{access.Membership{TenantID: a.TenantID, Role: role, Subscription: access.SubscriptionActive}, version, true}
	}

	for name, c := range map[string]struct {
		got, want grant
	}{
		"owner":            {read(a.AccountID, a.TenantID), active(access.RoleOwner, 1)},
		"moderator":        {read(moderator, a.TenantID), active(access.RoleModerator, 1)},
		"stranger":         {read(stranger, a.TenantID), grant{}},
		"other tenant":     {read(moderator, b.TenantID), grant{}},
		"unknown tenant":   {read(a.AccountID, "tenant-missing"), grant{}},
		"unknown account":  {read(access.AccountID("00000000000000000000000000000000"), a.TenantID), grant{}},
		"other owner in A": {read(b.AccountID, a.TenantID), grant{}},
	} {
		if c.got != c.want {
			t.Errorf("%s: MemberGrant = %+v, want %+v", name, c.got, c.want)
		}
	}

	mustDeactivate(t, s, operator, "op-deactivate-lock-01", "alice@example.com")
	inactive := grant{access.Membership{TenantID: a.TenantID, Role: access.RoleModerator, Subscription: access.SubscriptionInactive}, 2, true}
	if got := read(moderator, a.TenantID); got != inactive {
		t.Fatalf("MemberGrant after the deactivation = %+v, want %+v", got, inactive)
	}
	// MembershipOf reads the same membership.
	if m, found, err := s.accounts.MembershipOf(ctx, moderator, a.TenantID); err != nil || !found || m != inactive.membership {
		t.Fatalf("MembershipOf = %+v, %v, %v; want %+v", m, found, err, inactive.membership)
	}
	if got := read(b.AccountID, b.TenantID); got != (grant{access.Membership{TenantID: b.TenantID, Role: access.RoleOwner, Subscription: access.SubscriptionActive}, 1, true}) {
		t.Fatalf("MemberGrant of B = %+v, want B's owner at version 1", got)
	}
}
