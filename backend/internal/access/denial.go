package access

import "errors"

// ErrAccessDenied is the only public authorization denial. It intentionally
// carries no resource, tenant, role, state, or internal failure information.
var ErrAccessDenied = errors.New("access denied")

// Check returns the shared public denial unless the current request matches an
// explicit positive policy rule.
func Check(request Request) error {
	if !Authorize(request) {
		return ErrAccessDenied
	}
	return nil
}
