package access

import "testing"

// TestModeratorCapabilityMatrix pins the exact capabilities a moderator gets
// from an accepted invitation (US-005): content only. Every other
// resource, action, and target role is denied, including members, invitations,
// subscription, the OBS credential, and the canvas dimensions (administer).
func TestModeratorCapabilityMatrix(t *testing.T) {
	t.Parallel()

	allowed := map[Resource]map[Action]bool{
		ResourceCanvas:     {ActionView: true, ActionEdit: true},
		ResourceMembership: {ActionView: true},
	}
	resources := []Resource{ResourceCanvas, ResourceMembership, ResourceInvitation, ResourceSubscription, ResourceOBSCredential}
	actions := []Action{ActionView, ActionEdit, ActionAdminister, ActionActivate, ActionDeactivate, ActionRotate}
	targets := []Role{"", RoleOwner, RoleModerator, RoleOperator, RoleOBS}

	for _, resource := range resources {
		for _, action := range actions {
			for _, target := range targets {
				request := activeRequest(RoleModerator, resource, action)
				request.Invitation = InvitationAccepted
				request.TargetRole = target
				want := allowed[resource][action]
				if got := Authorize(request); got != want {
					t.Errorf("moderator %s %s target %q: Authorize() = %v, want %v", action, resource, target, got, want)
				}
			}
		}
	}
}
