package access

// Role identifies an authenticated actor category understood by the policy.
type Role string

const (
	RoleOwner     Role = "owner"
	RoleModerator Role = "moderator"
	RoleOperator  Role = "operator"
	RoleOBS       Role = "obs"
)

// Valid reports whether the role belongs to the closed policy vocabulary.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleModerator, RoleOperator, RoleOBS:
		return true
	default:
		return false
	}
}

// Resource identifies a tenant-scoped resource family.
type Resource string

const (
	ResourceCanvas        Resource = "canvas"
	ResourceMembership    Resource = "membership"
	ResourceInvitation    Resource = "invitation"
	ResourceSubscription  Resource = "subscription"
	ResourceOBSCredential Resource = "obs_credential"
)

// Valid reports whether the resource belongs to the closed policy vocabulary.
func (r Resource) Valid() bool {
	switch r {
	case ResourceCanvas, ResourceMembership, ResourceInvitation, ResourceSubscription, ResourceOBSCredential:
		return true
	default:
		return false
	}
}

// Action identifies an operation that may be authorized for a resource.
type Action string

const (
	ActionView       Action = "view"
	ActionEdit       Action = "edit"
	ActionAdminister Action = "administer"
	ActionActivate   Action = "activate"
	ActionDeactivate Action = "deactivate"
	ActionRotate     Action = "rotate"
)

// Valid reports whether the action belongs to the closed policy vocabulary.
func (a Action) Valid() bool {
	switch a {
	case ActionView, ActionEdit, ActionAdminister, ActionActivate, ActionDeactivate, ActionRotate:
		return true
	default:
		return false
	}
}

// SubscriptionStatus identifies whether tenant product access is enabled.
type SubscriptionStatus string

const (
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionInactive SubscriptionStatus = "inactive"
)

// Valid reports whether the status belongs to the closed policy vocabulary.
func (s SubscriptionStatus) Valid() bool {
	return s == SubscriptionActive || s == SubscriptionInactive
}

// InvitationStatus identifies the current invitation lifecycle state.
type InvitationStatus string

const (
	InvitationNone     InvitationStatus = "none"
	InvitationPending  InvitationStatus = "pending"
	InvitationAccepted InvitationStatus = "accepted"
	InvitationRevoked  InvitationStatus = "revoked"
)

// Valid reports whether the status belongs to the closed policy vocabulary.
func (s InvitationStatus) Valid() bool {
	switch s {
	case InvitationNone, InvitationPending, InvitationAccepted, InvitationRevoked:
		return true
	default:
		return false
	}
}

// MembershipStatus identifies whether the actor currently belongs to a tenant.
type MembershipStatus string

const (
	MembershipNone     MembershipStatus = "none"
	MembershipActive   MembershipStatus = "active"
	MembershipInactive MembershipStatus = "inactive"
)

// Valid reports whether the status belongs to the closed policy vocabulary.
func (s MembershipStatus) Valid() bool {
	switch s {
	case MembershipNone, MembershipActive, MembershipInactive:
		return true
	default:
		return false
	}
}

// Request contains the current facts required to make one authorization decision.
type Request struct {
	ActorTenantID    string
	ResourceTenantID string
	Role             Role
	Resource         Resource
	Action           Action
	Subscription     SubscriptionStatus
	Invitation       InvitationStatus
	Membership       MembershipStatus
	TargetRole       Role
}
