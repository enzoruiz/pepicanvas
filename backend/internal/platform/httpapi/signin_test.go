package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// faults makes the API's account and subscription ports fail on demand.
type faults struct {
	authenticate error
	isOperator   error
	signOut      error
}

type faultyAccounts struct {
	httpapi.Accounts
	faults *faults
}

func (a faultyAccounts) Authenticate(ctx context.Context, token secret.Value) (access.Principal, error) {
	if a.faults.authenticate != nil {
		return access.Principal{}, a.faults.authenticate
	}
	return a.Accounts.Authenticate(ctx, token)
}

func (a faultyAccounts) SignOut(ctx context.Context, token secret.Value) error {
	if a.faults.signOut != nil {
		return a.faults.signOut
	}
	return a.Accounts.SignOut(ctx, token)
}

type faultySubscriptions struct {
	httpapi.Subscriptions
	faults *faults
}

func (s faultySubscriptions) IsOperator(ctx context.Context, account access.AccountID) (bool, error) {
	if s.faults.isOperator != nil {
		return false, s.faults.isOperator
	}
	return s.Subscriptions.IsOperator(ctx, account)
}

func TestSignInThatFailsAfterCreatingTheSessionLeavesNoSession(t *testing.T) {
	for name, fault := range map[string]faults{
		"authenticate": {authenticate: errors.New("session read failed")},
		"is operator":  {isOperator: errors.New("operator read failed")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, httpapi.DefaultLimits)
			e.register("op-http-failsign-1", "alice@example.com")
			presented := e.signIn("alice@example.com")

			*e.faults = fault
			res := e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password), withSession(presented.token))
			if sessionCookie(res) != nil {
				t.Fatal("a failed sign-in set a session cookie")
			}
			expectError(t, res, http.StatusInternalServerError, "internal")
			// Neither the presented session nor the undelivered one survives.
			if _, sessions := e.store.Counts(); sessions != 0 {
				t.Fatalf("sessions = %d, want 0", sessions)
			}

			// Without a presented session the undelivered one is ended too.
			res = e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password))
			if sessionCookie(res) != nil {
				t.Fatal("a failed sign-in set a session cookie")
			}
			expectError(t, res, http.StatusInternalServerError, "internal")
			if _, sessions := e.store.Counts(); sessions != 0 {
				t.Fatalf("sessions = %d, want 0", sessions)
			}

			*e.faults = faults{}
			s := e.signIn("alice@example.com")
			if res := e.do(http.MethodGet, "/api/session", nil, withSession(s.token)); res.StatusCode != http.StatusOK {
				t.Fatalf("sign-in after the failure = %d", res.StatusCode)
			}
		})
	}
}

func TestSignInCleanupFailureIsLoggedWithoutSecrets(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	e.register("op-http-failsign-2", "alice@example.com")
	e.faults.isOperator = errors.New("operator read failed")
	e.faults.signOut = errors.New("session delete failed")

	res := e.do(http.MethodPost, "/api/sessions", credentials("alice@example.com", password))
	if sessionCookie(res) != nil {
		t.Fatal("a failed sign-in set a session cookie")
	}
	expectError(t, res, http.StatusInternalServerError, "internal")
	logs := e.logs.String()
	if !strings.Contains(logs, "session delete failed") {
		t.Fatalf("the cleanup failure was not logged: %s", logs)
	}
	for _, leaked := range []string{password, "alice@example.com"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("logs contain %q: %s", leaked, logs)
		}
	}
}
