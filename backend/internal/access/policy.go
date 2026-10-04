package access

type rule struct {
	role        Role
	resource    Resource
	action      Action
	targetRoles []Role
}

// allowRules is the complete positive policy. Absence from this table is denial.
var allowRules = [...]rule{
	{role: RoleOwner, resource: ResourceCanvas, action: ActionView},
	{role: RoleOwner, resource: ResourceCanvas, action: ActionEdit},
	{role: RoleOwner, resource: ResourceMembership, action: ActionView},
	{role: RoleOwner, resource: ResourceMembership, action: ActionAdminister, targetRoles: []Role{RoleOwner, RoleModerator}},
	{role: RoleOwner, resource: ResourceInvitation, action: ActionView},
	{role: RoleOwner, resource: ResourceInvitation, action: ActionAdminister},
	{role: RoleOwner, resource: ResourceSubscription, action: ActionView},
	{role: RoleOwner, resource: ResourceOBSCredential, action: ActionView},
	{role: RoleOwner, resource: ResourceOBSCredential, action: ActionRotate},

	{role: RoleModerator, resource: ResourceCanvas, action: ActionView},
	{role: RoleModerator, resource: ResourceCanvas, action: ActionEdit},
	{role: RoleModerator, resource: ResourceMembership, action: ActionView},
	{role: RoleModerator, resource: ResourceMembership, action: ActionAdminister, targetRoles: []Role{RoleModerator}},

	{role: RoleOperator, resource: ResourceSubscription, action: ActionView},
	{role: RoleOperator, resource: ResourceSubscription, action: ActionActivate},
	{role: RoleOperator, resource: ResourceSubscription, action: ActionDeactivate},

	{role: RoleOBS, resource: ResourceCanvas, action: ActionView},
}

// Authorize evaluates one request against the closed, positive policy.
// It intentionally exposes only a decision; public denial behavior belongs to AUTH-03.
func Authorize(request Request) bool {
	if !validRequest(request) {
		return false
	}
	if request.ActorTenantID != request.ResourceTenantID {
		return false
	}
	if request.Membership != MembershipActive {
		return false
	}
	if request.Subscription != SubscriptionActive && !isSubscriptionRecovery(request) {
		return false
	}

	for _, candidate := range allowRules {
		if candidate.matches(request) {
			return true
		}
	}

	return false
}

func validRequest(request Request) bool {
	if request.ActorTenantID == "" || request.ResourceTenantID == "" {
		return false
	}
	if !request.Role.Valid() || !request.Resource.Valid() || !request.Action.Valid() {
		return false
	}
	if !request.Subscription.Valid() || !request.Invitation.Valid() || !request.Membership.Valid() {
		return false
	}
	return request.TargetRole == "" || request.TargetRole.Valid()
}

func isSubscriptionRecovery(request Request) bool {
	return request.Role == RoleOperator && request.Resource == ResourceSubscription
}

func (candidate rule) matches(request Request) bool {
	if candidate.role != request.Role || candidate.resource != request.Resource || candidate.action != request.Action {
		return false
	}
	if len(candidate.targetRoles) == 0 {
		return true
	}
	for _, targetRole := range candidate.targetRoles {
		if targetRole == request.TargetRole {
			return true
		}
	}
	return false
}
