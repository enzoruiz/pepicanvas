package access

import (
	"errors"
	"testing"
)

func TestGuardMutationReauthorizesAtTimeOfUse(t *testing.T) {
	t.Parallel()

	current := activeRequest(RoleOwner, ResourceCanvas, ActionEdit)
	if err := Check(current); err != nil {
		t.Fatalf("earlier Check() error = %v, want nil", err)
	}
	current.Membership = MembershipInactive

	loaderCalls := 0
	callbackCalls := 0
	err := GuardMutation(func() (Request, error) {
		loaderCalls++
		return current, nil
	}, func() error {
		callbackCalls++
		return nil
	})

	assertDeniedWithoutCallback(t, err, loaderCalls, callbackCalls)
}

func TestGuardReadDeliveryReauthorizesAtTimeOfUse(t *testing.T) {
	t.Parallel()

	current := activeRequest(RoleOwner, ResourceCanvas, ActionView)
	if err := Check(current); err != nil {
		t.Fatalf("earlier Check() error = %v, want nil", err)
	}
	current.ResourceTenantID = "tenant-b"

	loaderCalls := 0
	callbackCalls := 0
	err := GuardReadDelivery(func() (Request, error) {
		loaderCalls++
		return current, nil
	}, func() error {
		callbackCalls++
		return nil
	})

	assertDeniedWithoutCallback(t, err, loaderCalls, callbackCalls)
}

func TestGuardEventDeliveryReauthorizesAtTimeOfUse(t *testing.T) {
	t.Parallel()

	current := activeRequest(RoleOBS, ResourceCanvas, ActionView)
	if err := Check(current); err != nil {
		t.Fatalf("earlier Check() error = %v, want nil", err)
	}
	current.Subscription = SubscriptionInactive

	loaderCalls := 0
	callbackCalls := 0
	err := GuardEventDelivery(func() (Request, error) {
		loaderCalls++
		return current, nil
	}, func() error {
		callbackCalls++
		return nil
	})

	assertDeniedWithoutCallback(t, err, loaderCalls, callbackCalls)
}

func TestGuardsInvokeAuthorizedCallbacksExactlyOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		guard func(RequestLoader, Callback) error
	}{
		{name: "mutation", guard: GuardMutation},
		{name: "read delivery", guard: GuardReadDelivery},
		{name: "event delivery", guard: GuardEventDelivery},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			loaderCalls := 0
			callbackCalls := 0
			err := tt.guard(func() (Request, error) {
				loaderCalls++
				return activeRequest(RoleOwner, ResourceCanvas, ActionView), nil
			}, func() error {
				callbackCalls++
				return nil
			})

			if err != nil {
				t.Fatalf("guard error = %v, want nil", err)
			}
			if loaderCalls != 1 {
				t.Fatalf("loader calls = %d, want 1", loaderCalls)
			}
			if callbackCalls != 1 {
				t.Fatalf("callback calls = %d, want 1", callbackCalls)
			}
		})
	}
}

func TestGuardsRedactLoaderFailuresAndRejectInvalidDependencies(t *testing.T) {
	t.Parallel()

	internalFailure := errors.New("database tenant record missing")
	tests := []struct {
		name     string
		loader   RequestLoader
		callback Callback
	}{
		{
			name: "loader failure",
			loader: func() (Request, error) {
				return Request{}, internalFailure
			},
			callback: func() error { return nil },
		},
		{name: "missing loader", callback: func() error { return nil }},
		{
			name: "missing callback",
			loader: func() (Request, error) {
				return activeRequest(RoleOwner, ResourceCanvas, ActionView), nil
			},
		},
	}

	guards := []struct {
		name  string
		guard func(RequestLoader, Callback) error
	}{
		{name: "mutation", guard: GuardMutation},
		{name: "read delivery", guard: GuardReadDelivery},
		{name: "event delivery", guard: GuardEventDelivery},
	}

	for _, guard := range guards {
		guard := guard
		t.Run(guard.name, func(t *testing.T) {
			t.Parallel()

			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()

					if err := guard.guard(tt.loader, tt.callback); err != ErrAccessDenied {
						t.Fatalf("guard error = %v, want shared public denial", err)
					}
				})
			}
		})
	}
}

func TestGuardsReturnCallbackFailureAfterAuthorization(t *testing.T) {
	t.Parallel()

	callbackFailure := errors.New("operation failed")
	for _, guard := range []func(RequestLoader, Callback) error{
		GuardMutation,
		GuardReadDelivery,
		GuardEventDelivery,
	} {
		err := guard(func() (Request, error) {
			return activeRequest(RoleOwner, ResourceCanvas, ActionView), nil
		}, func() error {
			return callbackFailure
		})
		if err != callbackFailure {
			t.Fatalf("guard error = %v, want callback failure", err)
		}
	}
}

func assertDeniedWithoutCallback(t *testing.T, err error, loaderCalls, callbackCalls int) {
	t.Helper()

	if err != ErrAccessDenied {
		t.Fatalf("guard error = %v, want shared public denial", err)
	}
	if loaderCalls != 1 {
		t.Fatalf("loader calls = %d, want 1", loaderCalls)
	}
	if callbackCalls != 0 {
		t.Fatalf("callback calls = %d, want 0", callbackCalls)
	}
}
