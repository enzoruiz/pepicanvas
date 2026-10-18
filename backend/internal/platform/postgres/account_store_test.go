package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const accountPassword = "correct horse battery"

// fastHasher keeps argon2id but with cheap parameters, so integration tests
// exercise the real PHC strings without the production cost.
var fastHasher = password.Argon2id{Memory: 8192, Iterations: 1, Parallelism: 1}

type accountClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *accountClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *accountClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newAccountClock() *accountClock {
	return &accountClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func openAccounts(t *testing.T, server *pgtest.Server, clock *accountClock) (*access.Service, *postgres.AccountStore, *pgxpool.Pool) {
	t.Helper()
	_, pool := openStore(t, server)
	store := postgres.NewAccountStore(pool)
	svc, err := access.NewService(store, store, fastHasher, access.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store, pool
}

func secretPassword(s string) secret.Value { return secret.New(secret.KindPassword, s) }

func register(ctx context.Context, svc *access.Service, op, email string) (access.AccountID, error) {
	return svc.Register(ctx, access.RegisterRequest{OperationID: op, Email: email, Password: secretPassword(accountPassword)})
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestAccountAndSessionSurviveRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	svc, _, _ := openAccounts(t, server, clock)
	ctx := context.Background()

	id, err := register(ctx, svc, "op-restart-registration", "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.SignIn(ctx, "alice@example.com", secretPassword(accountPassword))
	if err != nil {
		t.Fatal(err)
	}

	server.Restart(t)
	svc, _, pool := openAccounts(t, server, clock)

	clock.Advance(time.Hour)
	principal, err := svc.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("session after restart: %v", err)
	}
	if principal.AccountID != id || principal.Email != "alice@example.com" {
		t.Fatalf("principal = %+v", principal)
	}
	if _, err := svc.SignIn(ctx, "alice@example.com", secretPassword(accountPassword)); err != nil {
		t.Fatalf("sign in after restart: %v", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE token_hash = sha256($1::bytea)", []byte(session.Token.Reveal())); n != 1 {
		t.Fatalf("stored sessions for the token hash = %d, want 1", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE position($1::bytea IN token_hash) > 0", []byte(session.Token.Reveal())); n != 0 {
		t.Fatal("the raw session token is stored")
	}
	if n := count(t, pool, "SELECT count(*) FROM accounts WHERE password_hash LIKE '$argon2id$%' AND position($1 IN password_hash) = 0", accountPassword); n != 1 {
		t.Fatalf("argon2id hashes without the raw password = %d, want 1", n)
	}
}

func TestLostRegistrationAnswerReplaysAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	svc, _, _ := openAccounts(t, server, clock)

	first, err := register(correlatedContext(t), svc, "op-lost-answer-0001", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The answer is lost; the client reconnects after a server restart.
	server.Restart(t)
	svc, _, pool := openAccounts(t, server, clock)
	clock.Advance(access.RegistrationRetention - time.Microsecond)

	again, err := register(correlatedContext(t), svc, "op-lost-answer-0001", "ALICE@example.com")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again != first {
		t.Fatalf("retry account = %q, want %q", again, first)
	}
	if n := count(t, pool, "SELECT count(*) FROM accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM account_registrations WHERE correlation_id ~ '^[0-9a-f]{32}$'"); n != 1 {
		t.Fatalf("registrations with a correlation ID = %d, want 1", n)
	}
	if _, err := svc.Register(context.Background(), access.RegisterRequest{OperationID: "op-lost-answer-0001", Email: "bob@example.com", Password: secretPassword(accountPassword)}); !errors.Is(err, access.ErrOperationConflict) {
		t.Fatalf("different payload error = %v, want ErrOperationConflict", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM accounts"); n != 1 {
		t.Fatalf("accounts after conflict = %d, want 1", n)
	}
}

func TestRegistrationRetentionExpiresInPostgres(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	svc, _, pool := openAccounts(t, server, clock)
	ctx := context.Background()

	if _, err := register(ctx, svc, "op-retention-0001", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(access.RegistrationRetention)
	bob, err := register(ctx, svc, "op-retention-0001", "bob@example.com")
	if err != nil {
		t.Fatalf("expired operation ID must start a new operation: %v", err)
	}
	var owner string
	if err := pool.QueryRow(ctx, "SELECT account_id FROM account_registrations WHERE operation_id = 'op-retention-0001'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != string(bob) {
		t.Fatal("the expired registration row was not replaced")
	}
	if n := count(t, pool, "SELECT count(*) FROM account_registrations"); n != 1 {
		t.Fatalf("registrations = %d, want only the unexpired one", n)
	}
}

func TestConcurrentRegistrationsInPostgres(t *testing.T) {
	server := pgtest.Start(t)
	svc, _, pool := openAccounts(t, server, newAccountClock())
	ctx := context.Background()

	const n = 12
	ids := make([]access.AccountID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ids[i], errs[i] = register(ctx, svc, "op-concurrent-same", "alice@example.com") })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("duplicate %d: %q, %v; want %q", i, ids[i], errs[i], ids[0])
		}
	}

	// Different operations racing for one email: exactly one account.
	var created, taken int
	var mu sync.Mutex
	for i := range n {
		wg.Go(func() {
			_, err := register(ctx, svc, "op-concurrent-bob-"+string(rune('a'+i)), "bob@example.com")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, access.ErrIdentityTaken):
				taken++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if created != 1 || taken != n-1 {
		t.Fatalf("created %d, taken %d; want 1 and %d", created, taken, n-1)
	}
	if got := count(t, pool, "SELECT count(*) FROM accounts"); got != 2 {
		t.Fatalf("accounts = %d, want 2", got)
	}
}

func TestStoreReportsRetainedOperationsAndTakenIdentities(t *testing.T) {
	server := pgtest.Start(t)
	_, pool := openStore(t, server)
	store := postgres.NewAccountStore(pool)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	account := func(id, email, op string) access.NewAccount {
		return access.NewAccount{
			Account:      access.Account{ID: access.AccountID(id), Email: email, PasswordHash: "$argon2id$stub"},
			Registration: access.Registration{OperationID: op, AccountID: access.AccountID(id), AcceptedAt: now},
			PurgeBefore:  now.Add(-access.RegistrationRetention),
		}
	}
	if err := store.CreateAccount(ctx, account("0123456789abcdef0123456789abcdef", "alice@example.com", "op-store-errors-01")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, account("1123456789abcdef0123456789abcdef", "alice@example.com", "op-store-errors-02")); !errors.Is(err, access.ErrIdentityTaken) {
		t.Fatalf("duplicate email error = %v, want ErrIdentityTaken", err)
	}
	if err := store.CreateAccount(ctx, account("2123456789abcdef0123456789abcdef", "bob@example.com", "op-store-errors-01")); !errors.Is(err, access.ErrOperationRetained) {
		t.Fatalf("retained operation error = %v, want ErrOperationRetained", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM accounts"); n != 1 {
		t.Fatalf("failed creations left accounts behind: %d", n)
	}
}

func TestExpiredSessionsArePurged(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	svc, _, pool := openAccounts(t, server, clock)
	ctx := context.Background()
	if _, err := register(ctx, svc, "op-session-purge-01", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	old, _ := svc.SignIn(ctx, "alice@example.com", secretPassword(accountPassword))
	stale, _ := svc.SignIn(ctx, "alice@example.com", secretPassword(accountPassword))

	clock.Advance(access.SessionIdleTimeout)
	if _, err := svc.Authenticate(ctx, old.Token); !errors.Is(err, access.ErrSessionInvalid) {
		t.Fatalf("idle session error = %v, want ErrSessionInvalid", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions"); n != 1 {
		t.Fatalf("sessions after expired use = %d, want 1", n)
	}
	// A new sign-in removes the account's other expired sessions.
	if _, err := svc.SignIn(ctx, "alice@example.com", secretPassword(accountPassword)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions"); n != 1 {
		t.Fatalf("sessions after a new sign-in = %d, want only the new one", n)
	}
	if _, err := svc.Authenticate(ctx, stale.Token); !errors.Is(err, access.ErrSessionInvalid) {
		t.Fatalf("purged session error = %v", err)
	}
}

func TestMembershipFactsAreTenantScoped(t *testing.T) {
	server := pgtest.Start(t)
	svc, store, pool := openAccounts(t, server, newAccountClock())
	ctx := context.Background()

	owner, _ := register(ctx, svc, "op-membership-owner", "owner@example.com")
	moderator, _ := register(ctx, svc, "op-membership-moder", "moderator@example.com")
	inactiveOwner, _ := register(ctx, svc, "op-membership-inact", "inactive@example.com")
	stranger, _ := register(ctx, svc, "op-membership-stran", "stranger@example.com")
	seedTenant(t, pool, "tenant-a", access.SubscriptionActive, map[access.AccountID]access.Role{owner: access.RoleOwner, moderator: access.RoleModerator})
	seedTenant(t, pool, "tenant-b", access.SubscriptionActive, nil)
	seedTenant(t, pool, "tenant-c", access.SubscriptionInactive, map[access.AccountID]access.Role{inactiveOwner: access.RoleOwner})

	cases := []struct {
		account access.AccountID
		tenant  string
		want    access.Membership
		found   bool
	}{
		{owner, "tenant-a", access.Membership{TenantID: "tenant-a", Role: access.RoleOwner, Subscription: access.SubscriptionActive}, true},
		{moderator, "tenant-a", access.Membership{TenantID: "tenant-a", Role: access.RoleModerator, Subscription: access.SubscriptionActive}, true},
		{inactiveOwner, "tenant-c", access.Membership{TenantID: "tenant-c", Role: access.RoleOwner, Subscription: access.SubscriptionInactive}, true},
		{owner, "tenant-b", access.Membership{}, false},
		{owner, "tenant-c", access.Membership{}, false},
		{stranger, "tenant-a", access.Membership{}, false},
		{owner, "tenant-missing", access.Membership{}, false},
	}
	for _, c := range cases {
		got, found, err := store.MembershipOf(ctx, c.account, c.tenant)
		if err != nil || found != c.found || got != c.want {
			t.Fatalf("MembershipOf(%s, %s) = %+v, %v, %v; want %+v, %v", c.account, c.tenant, got, found, err, c.want, c.found)
		}
	}
}

func seedTenant(t *testing.T, pool *pgxpool.Pool, tenant string, status access.SubscriptionStatus, members map[access.AccountID]access.Role) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO tenants (tenant_id, subscription_status) VALUES ($1, $2)", tenant, string(status)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for account, role := range members {
		if _, err := pool.Exec(ctx, "INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, $3)", tenant, string(account), string(role)); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
}

func correlatedContext(t *testing.T) context.Context {
	t.Helper()
	id, err := correlation.New()
	if err != nil {
		t.Fatal(err)
	}
	return correlation.WithID(context.Background(), id)
}
