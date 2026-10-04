package access

import "testing"

func TestCheckReturnsOnePublicDenialForEveryRejectedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request Request
	}{
		{name: "missing", request: Request{}},
		{name: "foreign tenant", request: requestWith(func(r *Request) {
			r.ResourceTenantID = "tenant-b"
		})},
		{name: "unauthorized action", request: activeRequest(RoleOBS, ResourceCanvas, ActionEdit)},
		{name: "inactive access", request: requestWith(func(r *Request) {
			r.Membership = MembershipInactive
		})},
		{name: "invited only", request: requestWith(func(r *Request) {
			r.Invitation = InvitationPending
			r.Membership = MembershipNone
		})},
		{name: "unknown input", request: requestWith(func(r *Request) {
			r.Role = Role("unknown")
		})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Check(tt.request)
			if err != ErrAccessDenied {
				t.Fatalf("Check() error = %v, want the shared public denial", err)
			}
			if err.Error() != "access denied" {
				t.Fatalf("Check() error text = %q, want %q", err.Error(), "access denied")
			}
		})
	}
}

func TestCheckAllowsAnExplicitPositivePolicyMatch(t *testing.T) {
	t.Parallel()

	if err := Check(activeRequest(RoleOwner, ResourceCanvas, ActionView)); err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
}

func requestWith(mutate func(*Request)) Request {
	request := activeRequest(RoleOwner, ResourceCanvas, ActionView)
	mutate(&request)
	return request
}
