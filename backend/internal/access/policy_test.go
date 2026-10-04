package access

import "testing"

func TestAuthorizeExplicitAllowRulesAndTenantIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       Role
		resource   Resource
		action     Action
		targetRole Role
	}{
		{name: "owner views canvas", role: RoleOwner, resource: ResourceCanvas, action: ActionView},
		{name: "owner edits canvas", role: RoleOwner, resource: ResourceCanvas, action: ActionEdit},
		{name: "owner views memberships", role: RoleOwner, resource: ResourceMembership, action: ActionView},
		{name: "owner administers owner membership", role: RoleOwner, resource: ResourceMembership, action: ActionAdminister, targetRole: RoleOwner},
		{name: "moderator views memberships", role: RoleModerator, resource: ResourceMembership, action: ActionView},
		{name: "moderator administers moderator membership", role: RoleModerator, resource: ResourceMembership, action: ActionAdminister, targetRole: RoleModerator},
		{name: "owner views invitations", role: RoleOwner, resource: ResourceInvitation, action: ActionView},
		{name: "owner administers invitations", role: RoleOwner, resource: ResourceInvitation, action: ActionAdminister},
		{name: "owner views subscription", role: RoleOwner, resource: ResourceSubscription, action: ActionView},
		{name: "operator views subscription", role: RoleOperator, resource: ResourceSubscription, action: ActionView},
		{name: "operator activates subscription", role: RoleOperator, resource: ResourceSubscription, action: ActionActivate},
		{name: "operator deactivates subscription", role: RoleOperator, resource: ResourceSubscription, action: ActionDeactivate},
		{name: "owner views OBS credential", role: RoleOwner, resource: ResourceOBSCredential, action: ActionView},
		{name: "owner rotates OBS credential", role: RoleOwner, resource: ResourceOBSCredential, action: ActionRotate},
		{name: "OBS views canvas", role: RoleOBS, resource: ResourceCanvas, action: ActionView},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := activeRequest(tt.role, tt.resource, tt.action)
			request.TargetRole = tt.targetRole

			t.Run("same tenant is allowed", func(t *testing.T) {
				if !Authorize(request) {
					t.Fatal("Authorize() = false, want true")
				}
			})

			t.Run("foreign tenant is denied", func(t *testing.T) {
				request.ResourceTenantID = "tenant-b"
				if Authorize(request) {
					t.Fatal("Authorize() = true, want false")
				}
			})
		})
	}
}

func TestAuthorizeDeniesUnlistedRoleResourceActions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       Role
		resource   Resource
		action     Action
		targetRole Role
	}{
		{name: "OBS cannot edit canvas", role: RoleOBS, resource: ResourceCanvas, action: ActionEdit},
		{name: "OBS cannot view invitations", role: RoleOBS, resource: ResourceInvitation, action: ActionView},
		{name: "OBS cannot view memberships", role: RoleOBS, resource: ResourceMembership, action: ActionView},
		{name: "OBS cannot view subscription", role: RoleOBS, resource: ResourceSubscription, action: ActionView},
		{name: "OBS cannot view credentials", role: RoleOBS, resource: ResourceOBSCredential, action: ActionView},
		{name: "moderator cannot administer owner", role: RoleModerator, resource: ResourceMembership, action: ActionAdminister, targetRole: RoleOwner},
		{name: "moderator cannot administer invitations", role: RoleModerator, resource: ResourceInvitation, action: ActionAdminister},
		{name: "moderator cannot rotate OBS credential", role: RoleModerator, resource: ResourceOBSCredential, action: ActionRotate},
		{name: "operator cannot view canvas", role: RoleOperator, resource: ResourceCanvas, action: ActionView},
		{name: "operator cannot administer memberships", role: RoleOperator, resource: ResourceMembership, action: ActionAdminister, targetRole: RoleModerator},
		{name: "operator cannot administer invitations", role: RoleOperator, resource: ResourceInvitation, action: ActionAdminister},
		{name: "operator cannot rotate OBS credential", role: RoleOperator, resource: ResourceOBSCredential, action: ActionRotate},
		{name: "owner cannot activate subscription", role: RoleOwner, resource: ResourceSubscription, action: ActionActivate},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := activeRequest(tt.role, tt.resource, tt.action)
			request.TargetRole = tt.targetRole
			if Authorize(request) {
				t.Fatal("Authorize() = true, want false")
			}
		})
	}
}

func TestAuthorizeRequiresActiveTenantAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		subscription SubscriptionStatus
		invitation   InvitationStatus
		membership   MembershipStatus
	}{
		{name: "inactive subscription", subscription: SubscriptionInactive, invitation: InvitationNone, membership: MembershipActive},
		{name: "pending invitation without membership", subscription: SubscriptionActive, invitation: InvitationPending, membership: MembershipNone},
		{name: "accepted invitation without membership", subscription: SubscriptionActive, invitation: InvitationAccepted, membership: MembershipNone},
		{name: "revoked invitation without membership", subscription: SubscriptionActive, invitation: InvitationRevoked, membership: MembershipNone},
		{name: "inactive membership", subscription: SubscriptionActive, invitation: InvitationAccepted, membership: MembershipInactive},
		{name: "unrelated actor", subscription: SubscriptionActive, invitation: InvitationNone, membership: MembershipNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := activeRequest(RoleOwner, ResourceCanvas, ActionView)
			request.Subscription = tt.subscription
			request.Invitation = tt.invitation
			request.Membership = tt.membership
			if Authorize(request) {
				t.Fatal("Authorize() = true, want false")
			}
		})
	}
}

func TestAuthorizeInactiveSubscriptionBlocksEveryTenantResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       Role
		resource   Resource
		action     Action
		targetRole Role
	}{
		{name: "canvas", role: RoleOwner, resource: ResourceCanvas, action: ActionView},
		{name: "membership", role: RoleOwner, resource: ResourceMembership, action: ActionView},
		{name: "invitation", role: RoleOwner, resource: ResourceInvitation, action: ActionView},
		{name: "subscription", role: RoleOwner, resource: ResourceSubscription, action: ActionView},
		{name: "OBS credential", role: RoleOwner, resource: ResourceOBSCredential, action: ActionView},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := activeRequest(tt.role, tt.resource, tt.action)
			request.Subscription = SubscriptionInactive
			request.TargetRole = tt.targetRole
			if Authorize(request) {
				t.Fatal("Authorize() = true, want false")
			}
		})
	}
}

func TestAuthorizeAllowsExplicitSubscriptionRecoveryOperations(t *testing.T) {
	t.Parallel()

	for _, action := range []Action{ActionView, ActionActivate} {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()

			request := activeRequest(RoleOperator, ResourceSubscription, action)
			request.Subscription = SubscriptionInactive
			if !Authorize(request) {
				t.Fatal("Authorize() = false, want true")
			}
		})
	}
}

func TestAuthorizeFailsClosedForUnknownValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{name: "unknown role", mutate: func(r *Request) { r.Role = Role("unknown") }},
		{name: "unknown resource", mutate: func(r *Request) { r.Resource = Resource("unknown") }},
		{name: "unknown action", mutate: func(r *Request) { r.Action = Action("unknown") }},
		{name: "unknown subscription", mutate: func(r *Request) { r.Subscription = SubscriptionStatus("unknown") }},
		{name: "unknown invitation", mutate: func(r *Request) { r.Invitation = InvitationStatus("unknown") }},
		{name: "unknown membership", mutate: func(r *Request) { r.Membership = MembershipStatus("unknown") }},
		{name: "unknown target role", mutate: func(r *Request) {
			r.Resource = ResourceMembership
			r.Action = ActionAdminister
			r.TargetRole = Role("unknown")
		}},
		{name: "missing actor tenant", mutate: func(r *Request) { r.ActorTenantID = "" }},
		{name: "missing resource tenant", mutate: func(r *Request) { r.ResourceTenantID = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := activeRequest(RoleOwner, ResourceCanvas, ActionView)
			tt.mutate(&request)
			if Authorize(request) {
				t.Fatal("Authorize() = true, want false")
			}
		})
	}
}

func activeRequest(role Role, resource Resource, action Action) Request {
	return Request{
		ActorTenantID:    "tenant-a",
		ResourceTenantID: "tenant-a",
		Role:             role,
		Resource:         resource,
		Action:           action,
		Subscription:     SubscriptionActive,
		Invitation:       InvitationNone,
		Membership:       MembershipActive,
	}
}
