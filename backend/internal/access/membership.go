package access

import "context"

// Membership is an account's accepted role in one tenant, with the tenant's
// subscription status. US-002 and US-005 own creating these facts.
type Membership struct {
	TenantID     string
	Role         Role
	Subscription SubscriptionStatus
}

// MembershipStore reads the current access facts of an account in a tenant.
type MembershipStore interface {
	// MembershipOf returns found=false when the account has no accepted
	// role in the tenant, including when the tenant does not exist.
	MembershipOf(ctx context.Context, account AccountID, tenantID string) (Membership, bool, error)
}

// CanvasRequest builds the authorization request for an account acting on a
// tenant's canvas. Without a membership the actor has no tenant, so the
// policy denies it like any other cross-tenant request.
func CanvasRequest(membership Membership, found bool, tenantID string, action Action) Request {
	request := Request{
		ResourceTenantID: tenantID,
		Resource:         ResourceCanvas,
		Action:           action,
		Subscription:     SubscriptionInactive,
		Invitation:       InvitationNone,
		Membership:       MembershipNone,
	}
	if !found {
		return request
	}
	request.ActorTenantID = membership.TenantID
	request.Role = membership.Role
	request.Subscription = membership.Subscription
	request.Membership = MembershipActive
	if membership.Role == RoleModerator {
		request.Invitation = InvitationAccepted
	}
	return request
}

// OperatorRequest builds the authorization request for an account acting on a
// tenant's subscription. Operators act platform-wide, so an operator's actor
// tenant is the target tenant; a non-operator has no tenant and is denied.
func OperatorRequest(operator bool, tenantID string, action Action) Request {
	request := Request{
		ResourceTenantID: tenantID,
		Resource:         ResourceSubscription,
		Action:           action,
		Subscription:     SubscriptionInactive,
		Invitation:       InvitationNone,
		Membership:       MembershipNone,
	}
	if !operator {
		return request
	}
	request.ActorTenantID = tenantID
	request.Role = RoleOperator
	request.Membership = MembershipActive
	return request
}

// InvitationRequest builds the authorization request for an account acting on
// a tenant's invitations. Only the account that owns the tenant acts on them,
// so a non-owner has no actor tenant and is denied like any other stranger.
func InvitationRequest(owner bool, tenantID string, status SubscriptionStatus, action Action) Request {
	request := Request{
		ResourceTenantID: tenantID,
		Resource:         ResourceInvitation,
		Action:           action,
		Subscription:     SubscriptionInactive,
		Invitation:       InvitationNone,
		Membership:       MembershipNone,
	}
	if !owner {
		return request
	}
	request.ActorTenantID = tenantID
	request.Role = RoleOwner
	request.Subscription = status
	request.Membership = MembershipActive
	return request
}
