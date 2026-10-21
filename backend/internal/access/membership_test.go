package access_test

import (
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
)

func TestCanvasRequestGrantsOnlyActiveMembersOfTheTenant(t *testing.T) {
	active := access.SubscriptionActive
	cases := map[string]struct {
		membership access.Membership
		found      bool
		tenant     string
		want       bool
	}{
		"owner of an active tenant":     {access.Membership{TenantID: "tenant-a", Role: access.RoleOwner, Subscription: active}, true, "tenant-a", true},
		"moderator of an active tenant": {access.Membership{TenantID: "tenant-a", Role: access.RoleModerator, Subscription: active}, true, "tenant-a", true},
		"owner without subscription":    {access.Membership{TenantID: "tenant-a", Role: access.RoleOwner, Subscription: access.SubscriptionInactive}, true, "tenant-a", false},
		"authenticated, no membership":  {access.Membership{}, false, "tenant-a", false},
		"membership of another tenant":  {access.Membership{TenantID: "tenant-b", Role: access.RoleOwner, Subscription: active}, true, "tenant-a", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, action := range []access.Action{access.ActionView, access.ActionEdit} {
				err := access.Check(access.CanvasRequest(c.membership, c.found, c.tenant, action))
				if got := err == nil; got != c.want {
					t.Fatalf("%s: allowed = %v, want %v", action, got, c.want)
				}
			}
		})
	}
}

func TestOperatorRequestGrantsOnlyOperatorsSubscriptionActions(t *testing.T) {
	cases := map[string]struct {
		operator bool
		action   access.Action
		want     bool
	}{
		"operator activates":     {true, access.ActionActivate, true},
		"operator deactivates":   {true, access.ActionDeactivate, true},
		"operator views":         {true, access.ActionView, true},
		"operator administers":   {true, access.ActionAdminister, false},
		"non-operator activates": {false, access.ActionActivate, false},
		"non-operator views":     {false, access.ActionView, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := access.Check(access.OperatorRequest(c.operator, "tenant-a", c.action))
			if got := err == nil; got != c.want {
				t.Fatalf("allowed = %v, want %v", got, c.want)
			}
		})
	}
	if access.Check(access.OperatorRequest(true, "", access.ActionActivate)) == nil {
		t.Fatal("an operator request without a tenant was allowed")
	}
}
