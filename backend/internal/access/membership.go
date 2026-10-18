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
