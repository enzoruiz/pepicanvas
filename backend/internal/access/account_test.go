package access_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/access/accesstest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	opA      = "op-registration-0001"
	opB      = "op-registration-0002"
	password = "correct horse battery"
)

type fixture struct {
	store  *accesstest.MemoryStore
	hasher *accesstest.PlainHasher
	clock  *accesstest.Clock
	svc    *access.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{store: accesstest.NewMemoryStore(), hasher: &accesstest.PlainHasher{}, clock: accesstest.NewClock()}
	svc, err := access.NewService(f.store, f.store, f.hasher, access.WithClock(f.clock.Now))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.svc = svc
	return f
}

func pw(s string) secret.Value { return secret.New(secret.KindPassword, s) }

func (f fixture) register(t *testing.T, op, email, pass string) access.AccountID {
	t.Helper()
	id, err := f.svc.Register(context.Background(), access.RegisterRequest{OperationID: op, Email: email, Password: pw(pass)})
	if err != nil {
		t.Fatalf("Register(%q): %v", email, err)
	}
	return id
}

func TestRegisteredIdentityCanSignIn(t *testing.T) {
	f := newFixture(t)
	id := f.register(t, opA, "  Alice@Example.COM ", password)
	if id == "" {
		t.Fatal("Register returned an empty account ID")
	}

	session, err := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if session.AccountID != id {
		t.Fatalf("SignIn account = %q, want %q", session.AccountID, id)
	}
	if session.Token.Kind() != secret.KindSessionToken || !strings.HasPrefix(session.Token.Reveal(), "pcs_") {
		t.Fatalf("session token kind %q is not a generated session token", session.Token.Kind())
	}
	principal, err := f.svc.Authenticate(context.Background(), session.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.AccountID != id || principal.Email != "alice@example.com" {
		t.Fatalf("principal = %+v", principal)
	}
}

func TestInvalidRegistrationIsRejectedWithoutEffect(t *testing.T) {
	cases := map[string]access.RegisterRequest{
		"empty email":        {OperationID: opA, Email: "", Password: pw(password)},
		"no at sign":         {OperationID: opA, Email: "alice.example.com", Password: pw(password)},
		"empty local part":   {OperationID: opA, Email: "@example.com", Password: pw(password)},
		"empty domain":       {OperationID: opA, Email: "alice@", Password: pw(password)},
		"two at signs":       {OperationID: opA, Email: "a@b@example.com", Password: pw(password)},
		"inner space":        {OperationID: opA, Email: "al ice@example.com", Password: pw(password)},
		"control character":  {OperationID: opA, Email: "alice\x00@example.com", Password: pw(password)},
		"email too long":     {OperationID: opA, Email: strings.Repeat("a", 243) + "@example.com", Password: pw(password)},
		"password too short": {OperationID: opA, Email: "alice@example.com", Password: pw(strings.Repeat("x", 11))},
		"password too long":  {OperationID: opA, Email: "alice@example.com", Password: pw(strings.Repeat("x", 129))},
		"zero password":      {OperationID: opA, Email: "alice@example.com"},
		"empty operation":    {OperationID: "", Email: "alice@example.com", Password: pw(password)},
		"short operation":    {OperationID: "op-1", Email: "alice@example.com", Password: pw(password)},
		"operation symbols":  {OperationID: "op registration 0001", Email: "alice@example.com", Password: pw(password)},
		"operation too long": {OperationID: strings.Repeat("o", 129), Email: "alice@example.com", Password: pw(password)},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Register(context.Background(), request)
			if !errors.Is(err, access.ErrInvalidRegistration) {
				t.Fatalf("Register error = %v, want ErrInvalidRegistration", err)
			}
			if accounts, _ := f.store.Counts(); accounts != 0 {
				t.Fatalf("accounts = %d, want 0", accounts)
			}
		})
	}
}

func TestRegistrationBoundariesAreAccepted(t *testing.T) {
	f := newFixture(t)
	f.register(t, strings.Repeat("o", 16), strings.Repeat("a", 242)+"@example.com", strings.Repeat("x", 12))
	f.register(t, strings.Repeat("p", 128), "bob@example.com", strings.Repeat("y", 128))
	if accounts, _ := f.store.Counts(); accounts != 2 {
		t.Fatalf("accounts = %d, want 2", accounts)
	}
}

func TestRegisteredIdentityDoesNotProduceASecondAccount(t *testing.T) {
	f := newFixture(t)
	first := f.register(t, opA, "alice@example.com", password)

	for _, email := range []string{"alice@example.com", " ALICE@example.com"} {
		_, err := f.svc.Register(context.Background(), access.RegisterRequest{OperationID: opB, Email: email, Password: pw("another password!")})
		if !errors.Is(err, access.ErrIdentityTaken) {
			t.Fatalf("Register(%q) error = %v, want ErrIdentityTaken", email, err)
		}
	}
	if accounts, _ := f.store.Counts(); accounts != 1 {
		t.Fatalf("accounts = %d, want 1", accounts)
	}
	session, err := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))
	if err != nil || session.AccountID != first {
		t.Fatalf("original credentials must still sign in: %v", err)
	}
	if _, err := f.svc.SignIn(context.Background(), "alice@example.com", pw("another password!")); !errors.Is(err, access.ErrInvalidCredentials) {
		t.Fatalf("rejected registration password must not sign in: %v", err)
	}
}

func TestInvalidCredentialsCreateNoSessionAndLookTheSame(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)

	cases := map[string]struct{ email, password string }{
		"wrong password":     {"alice@example.com", "wrong password!!"},
		"unknown email":      {"nobody@example.com", password},
		"malformed email":    {"not-an-email", password},
		"empty password":     {"alice@example.com", ""},
		"oversized password": {"alice@example.com", strings.Repeat("x", 129)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before := f.hasher.Verifications()
			session, err := f.svc.SignIn(context.Background(), c.email, pw(c.password))
			if !errors.Is(err, access.ErrInvalidCredentials) {
				t.Fatalf("SignIn error = %v, want ErrInvalidCredentials", err)
			}
			if err.Error() != access.ErrInvalidCredentials.Error() {
				t.Fatalf("SignIn error text %q reveals more than the shared denial", err)
			}
			if !session.Token.IsZero() {
				t.Fatal("failed SignIn returned a token")
			}
			if _, sessions := f.store.Counts(); sessions != 0 {
				t.Fatalf("sessions = %d, want 0", sessions)
			}
			if got := f.hasher.Verifications() - before; got != 1 {
				t.Fatalf("password verifications = %d, want exactly 1 for every failure path", got)
			}
		})
	}
}

func TestSessionExpiresAfterIdleTimeout(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)
	session, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))

	f.clock.Advance(access.SessionIdleTimeout - time.Nanosecond)
	if _, err := f.svc.Authenticate(context.Background(), session.Token); err != nil {
		t.Fatalf("session just before idle timeout: %v", err)
	}
	// Use renews the idle window.
	f.clock.Advance(access.SessionIdleTimeout - time.Nanosecond)
	if _, err := f.svc.Authenticate(context.Background(), session.Token); err != nil {
		t.Fatalf("session renewed by use: %v", err)
	}
	f.clock.Advance(access.SessionIdleTimeout)
	if _, err := f.svc.Authenticate(context.Background(), session.Token); !errors.Is(err, access.ErrSessionInvalid) {
		t.Fatalf("idle session error = %v, want ErrSessionInvalid", err)
	}
	if _, sessions := f.store.Counts(); sessions != 0 {
		t.Fatalf("expired session must be deleted, sessions = %d", sessions)
	}
}

func TestSessionExpiresAtAbsoluteLifetimeEvenWhenUsed(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)
	session, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))

	step := access.SessionIdleTimeout - time.Hour
	elapsed := time.Duration(0)
	for elapsed+step < access.SessionAbsoluteLifetime {
		f.clock.Advance(step)
		elapsed += step
		if _, err := f.svc.Authenticate(context.Background(), session.Token); err != nil {
			t.Fatalf("session used at %v: %v", elapsed, err)
		}
	}
	f.clock.Advance(access.SessionAbsoluteLifetime - elapsed - time.Nanosecond)
	if _, err := f.svc.Authenticate(context.Background(), session.Token); err != nil {
		t.Fatalf("session just before absolute lifetime: %v", err)
	}
	f.clock.Advance(time.Nanosecond)
	if _, err := f.svc.Authenticate(context.Background(), session.Token); !errors.Is(err, access.ErrSessionInvalid) {
		t.Fatalf("session at absolute lifetime error = %v, want ErrSessionInvalid", err)
	}
}

func TestExpiryConstantsMatchTheDecision(t *testing.T) {
	if access.SessionIdleTimeout != 24*time.Hour || access.SessionAbsoluteLifetime != 7*24*time.Hour {
		t.Fatalf("idle %v absolute %v, want 24h and 168h", access.SessionIdleTimeout, access.SessionAbsoluteLifetime)
	}
}

func TestSignOutEndsTheSessionAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)
	session, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))
	other, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))

	for range 2 {
		if err := f.svc.SignOut(context.Background(), session.Token); err != nil {
			t.Fatalf("SignOut: %v", err)
		}
	}
	if _, err := f.svc.Authenticate(context.Background(), session.Token); !errors.Is(err, access.ErrSessionInvalid) {
		t.Fatalf("signed-out session error = %v, want ErrSessionInvalid", err)
	}
	if _, err := f.svc.Authenticate(context.Background(), other.Token); err != nil {
		t.Fatalf("another session of the same account must survive: %v", err)
	}
}

func TestMalformedTokensAreInvalidSessions(t *testing.T) {
	f := newFixture(t)
	for _, token := range []secret.Value{
		{},
		secret.New(secret.KindSessionToken, ""),
		secret.New(secret.KindSessionToken, "pcs_short"),
		secret.New(secret.KindSessionToken, "pci_"+strings.Repeat("A", 43)),
		secret.New(secret.KindInvitationToken, "pcs_"+strings.Repeat("A", 43)),
	} {
		if _, err := f.svc.Authenticate(context.Background(), token); !errors.Is(err, access.ErrSessionInvalid) {
			t.Fatalf("Authenticate(%v) error = %v, want ErrSessionInvalid", token, err)
		}
	}
}

func TestCSRFTokenIsBoundToTheSession(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)
	one, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))
	two, _ := f.svc.SignIn(context.Background(), "alice@example.com", pw(password))

	csrf := access.CSRFToken(one.Token)
	if csrf.Kind() != secret.KindCSRFToken {
		t.Fatalf("CSRF kind = %q", csrf.Kind())
	}
	if csrf.Reveal() != one.CSRF.Reveal() {
		t.Fatal("SignIn must return the session's CSRF token")
	}
	if strings.Contains(csrf.Reveal(), one.Token.Reveal()) {
		t.Fatal("CSRF token must not contain the session token")
	}
	if !access.VerifyCSRF(one.Token, csrf.Reveal()) {
		t.Fatal("valid CSRF token rejected")
	}
	for name, presented := range map[string]string{
		"missing":       "",
		"other session": access.CSRFToken(two.Token).Reveal(),
		"tampered":      csrf.Reveal()[:len(csrf.Reveal())-1] + "x",
		"session token": one.Token.Reveal(),
	} {
		if access.VerifyCSRF(one.Token, presented) {
			t.Fatalf("%s CSRF token accepted", name)
		}
	}
	if access.VerifyCSRF(secret.Value{}, access.CSRFToken(secret.Value{}).Reveal()) {
		t.Fatal("CSRF without a session must be rejected")
	}
}

func TestRegistrationRetryReplaysTheSameAccount(t *testing.T) {
	f := newFixture(t)
	first, err := f.svc.Register(correlated(t), access.RegisterRequest{OperationID: opA, Email: "alice@example.com", Password: pw(password)})
	if err != nil {
		t.Fatal(err)
	}
	firstCorrelation := f.store.Correlation(opA)
	if firstCorrelation == "" {
		t.Fatal("registration must retain the request correlation ID")
	}

	// A retry after a timeout or reconnection may use another correlation ID
	// and a differently cased email.
	f.clock.Advance(access.RegistrationRetention - time.Nanosecond)
	again, err := f.svc.Register(correlated(t), access.RegisterRequest{OperationID: opA, Email: " ALICE@example.com", Password: pw(password)})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again != first {
		t.Fatalf("retry account = %q, want %q", again, first)
	}
	if accounts, _ := f.store.Counts(); accounts != 1 {
		t.Fatalf("accounts = %d, want 1", accounts)
	}
	if f.store.Correlation(opA) != firstCorrelation {
		t.Fatal("retry must not overwrite the retained operation")
	}
}

func TestReusedOperationIDWithADifferentPayloadIsRejected(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)

	for name, request := range map[string]access.RegisterRequest{
		"other email":    {OperationID: opA, Email: "bob@example.com", Password: pw(password)},
		"other password": {OperationID: opA, Email: "alice@example.com", Password: pw("another password!")},
	} {
		_, err := f.svc.Register(context.Background(), request)
		if !errors.Is(err, access.ErrOperationConflict) {
			t.Fatalf("%s: error = %v, want ErrOperationConflict", name, err)
		}
	}
	if accounts, _ := f.store.Counts(); accounts != 1 {
		t.Fatalf("accounts = %d, want 1", accounts)
	}
	if _, ok, _ := f.store.AccountByEmail(context.Background(), "bob@example.com"); ok {
		t.Fatal("conflicting payload created an account")
	}
}

func TestRegistrationRetentionExpires(t *testing.T) {
	f := newFixture(t)
	f.register(t, opA, "alice@example.com", password)

	f.clock.Advance(access.RegistrationRetention)
	// The operation ID expired: the same ID now starts a new operation.
	bob := f.register(t, opA, "bob@example.com", password)
	if bob == "" {
		t.Fatal("expired operation ID must start a new operation")
	}
	// The expired retry of the first payload is a new registration of a taken identity.
	f.clock.Advance(access.RegistrationRetention)
	_, err := f.svc.Register(context.Background(), access.RegisterRequest{OperationID: opA, Email: "alice@example.com", Password: pw(password)})
	if !errors.Is(err, access.ErrIdentityTaken) {
		t.Fatalf("expired retry error = %v, want ErrIdentityTaken", err)
	}
	if access.RegistrationRetention != 10*time.Minute {
		t.Fatalf("retention = %v, want 10m", access.RegistrationRetention)
	}
}

func TestFailedRegistrationLeavesNoTraceAndRetryAppliesOnce(t *testing.T) {
	f := newFixture(t)
	f.store.FailCreate(errors.New("connection reset before commit"))
	if _, err := f.svc.Register(context.Background(), access.RegisterRequest{OperationID: opA, Email: "alice@example.com", Password: pw(password)}); err == nil {
		t.Fatal("Register must surface the store failure")
	}
	if accounts, _ := f.store.Counts(); accounts != 0 {
		t.Fatalf("accounts = %d, want 0", accounts)
	}
	f.store.FailCreate(nil)
	first := f.register(t, opA, "alice@example.com", password)
	second := f.register(t, opA, "alice@example.com", password)
	if first != second {
		t.Fatalf("retries returned %q and %q", first, second)
	}
}

func TestConcurrentDuplicateRegistrationsProduceOneAccount(t *testing.T) {
	f := newFixture(t)
	const n = 16
	ids := make([]access.AccountID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i], errs[i] = f.svc.Register(context.Background(), access.RegisterRequest{OperationID: opA, Email: "alice@example.com", Password: pw(password)})
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("call %d: id %q err %v, want %q", i, ids[i], errs[i], ids[0])
		}
	}
	if accounts, _ := f.store.Counts(); accounts != 1 {
		t.Fatalf("accounts = %d, want 1", accounts)
	}
}

func correlated(t *testing.T) context.Context {
	t.Helper()
	id, err := correlation.New()
	if err != nil {
		t.Fatal(err)
	}
	return correlation.WithID(context.Background(), id)
}
