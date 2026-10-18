package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const activationCorrelation = "0123456789abcdef0123456789abcdef"

type subscriptions struct {
	svc      *subscription.Service
	store    *postgres.SubscriptionStore
	accounts *postgres.AccountStore
	canvases *postgres.CanvasStore
	pool     *pgxpool.Pool
}

func openSubscriptions(t *testing.T, server *pgtest.Server, clock *accountClock) subscriptions {
	t.Helper()
	canvases, pool := openStore(t, server)
	store := postgres.NewSubscriptionStore(pool)
	return subscriptions{
		svc:      subscription.NewService(store, subscription.WithClock(clock.Now)),
		store:    store,
		accounts: postgres.NewAccountStore(pool),
		canvases: canvases,
		pool:     pool,
	}
}

var seededAccounts atomic.Int64

// seedAccount registers an account through the account store.
func seedAccount(t *testing.T, s subscriptions, email string) access.AccountID {
	t.Helper()
	n := seededAccounts.Add(1)
	id := access.AccountID(fmt.Sprintf("%032x", n))
	now := time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)
	err := s.accounts.CreateAccount(context.Background(), access.NewAccount{
		Account:      access.Account{ID: id, Email: email, PasswordHash: "$argon2id$stub"},
		Registration: access.Registration{OperationID: fmt.Sprintf("op-seed-account-%04d", n), AccountID: id, AcceptedAt: now},
		PurgeBefore:  now.Add(-access.RegistrationRetention),
	})
	if err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
	return id
}

// seedOperator seeds an account and grants it the operator role.
func seedOperator(t *testing.T, s subscriptions, email string) access.AccountID {
	t.Helper()
	id := seedAccount(t, s, email)
	granted, err := s.store.GrantOperator(context.Background(), email)
	if err != nil || granted != id {
		t.Fatalf("GrantOperator(%s) = %q, %v; want %q", email, granted, err, id)
	}
	return id
}

func activate(s subscriptions, operator access.AccountID, op, email string) (subscription.Activation, error) {
	return s.svc.Activate(context.Background(), subscription.ActivateRequest{
		OperatorID: operator, OperationID: op, Email: email, CorrelationID: activationCorrelation,
	})
}

func mustActivate(t *testing.T, s subscriptions, operator access.AccountID, op, email string) subscription.Activation {
	t.Helper()
	activation, err := activate(s, operator, op, email)
	if err != nil {
		t.Fatalf("Activate(%s, %s): %v", op, email, err)
	}
	return activation
}

// activationRows counts every table an activation writes.
type activationRows struct{ tenants, memberships, canvases, records int }

func countActivationRows(t *testing.T, pool *pgxpool.Pool) activationRows {
	t.Helper()
	return activationRows{
		tenants:     count(t, pool, "SELECT count(*) FROM tenants"),
		memberships: count(t, pool, "SELECT count(*) FROM memberships"),
		canvases:    count(t, pool, "SELECT count(*) FROM canvases"),
		records:     count(t, pool, "SELECT count(*) FROM subscription_activations"),
	}
}

func TestActivationCreatesOneTenantMembershipAndCanvas(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	alice := seedAccount(t, s, "alice@example.com")

	got := mustActivate(t, s, operator, "op-activate-alice-01", " Alice@Example.com ")
	if got.AccountID != alice || !got.Created || got.TenantID == "" || got.CanvasID == "" {
		t.Fatalf("activation = %+v, want a new tenant for %q", got, alice)
	}
	if rows := countActivationRows(t, s.pool); rows != (activationRows{1, 1, 1, 1}) {
		t.Fatalf("rows = %+v, want one tenant, membership, canvas, and record", rows)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM tenants WHERE tenant_id = $1 AND owner_account_id = $2 AND canvas_id = $3 AND subscription_status = 'active'", got.TenantID, string(alice), got.CanvasID); n != 1 {
		t.Fatal("the tenant is not active, owned by the account, and bound to its canvas")
	}
	membership, found, err := s.accounts.MembershipOf(ctx, alice, got.TenantID)
	if err != nil || !found || membership.Role != access.RoleOwner || membership.Subscription != access.SubscriptionActive {
		t.Fatalf("MembershipOf = %+v, %v, %v; want an active owner", membership, found, err)
	}
	loaded := mustLoad(t, s.canvases, canvas.Key{TenantID: got.TenantID, CanvasID: got.CanvasID})
	if loaded.Dimensions != (canvas.Dimensions{Width: 1920, Height: 1080}) || loaded.Revision != 0 || len(loaded.Objects) != 0 {
		t.Fatalf("canvas = %+v, want an empty 1920x1080 canvas", loaded)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_activations WHERE operation_id = 'op-activate-alice-01' AND correlation_id = $1 AND operator_account_id = $2 AND account_id = $3 AND tenant_id = $4 AND canvas_id = $5 AND created",
		activationCorrelation, string(operator), string(alice), got.TenantID, got.CanvasID); n != 1 {
		t.Fatal("the activation record does not keep the answer and the correlation ID")
	}
}

func TestRepeatedActivationKeepsTheCanvas(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, "alice@example.com")

	first := mustActivate(t, s, operator, "op-repeat-alice-0001", "alice@example.com")
	key := canvas.Key{TenantID: first.TenantID, CanvasID: first.CanvasID}
	text := apply(t, s.canvases, key, createText)

	again := mustActivate(t, s, operator, "op-repeat-alice-0002", "alice@example.com")
	if again.TenantID != first.TenantID || again.CanvasID != first.CanvasID || again.Created {
		t.Fatalf("repeat = %+v, want the existing tenant and canvas of %+v", again, first)
	}

	if _, err := s.pool.Exec(ctx, "UPDATE tenants SET subscription_status = 'inactive' WHERE tenant_id = $1", first.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.CanvasOf(ctx, first.AccountID); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("CanvasOf an inactive tenant: error = %v, want ErrAccessDenied", err)
	}
	enabled := mustActivate(t, s, operator, "op-repeat-alice-0003", "alice@example.com")
	if enabled.TenantID != first.TenantID || enabled.CanvasID != first.CanvasID || enabled.Created {
		t.Fatalf("reactivation = %+v, want the existing tenant and canvas of %+v", enabled, first)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM tenants WHERE tenant_id = $1 AND subscription_status = 'active'", first.TenantID); n != 1 {
		t.Fatal("reactivation did not set the tenant active")
	}
	if rows := countActivationRows(t, s.pool); rows != (activationRows{1, 1, 1, 3}) {
		t.Fatalf("rows = %+v, want one tenant, membership, and canvas with three records", rows)
	}
	loaded := mustLoad(t, s.canvases, key)
	if len(loaded.Objects) != 1 || loaded.Objects[0].ID != text.ObjectID {
		t.Fatalf("reactivated canvas = %+v, want the object it held", loaded)
	}
	own, err := s.svc.CanvasOf(ctx, first.AccountID)
	if err != nil || own != key {
		t.Fatalf("CanvasOf = %+v, %v; want %+v", own, err, key)
	}
}

func TestConcurrentActivationsGiveOneCanvas(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, "alice@example.com")

	// Twelve distinct operations and twelve retries of one operation race.
	const n = 12
	results := make([]subscription.Activation, 2*n)
	errs := make([]error, 2*n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 2 * n {
		op := "op-concurrent-shared"
		if i < n {
			op = fmt.Sprintf("op-concurrent-own-%02d", i)
		}
		wg.Go(func() {
			<-start
			results[i], errs[i] = activate(s, operator, op, "alice@example.com")
		})
	}
	close(start)
	wg.Wait()
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("activation %d: %v", i, errs[i])
		}
		if results[i].TenantID != results[0].TenantID || results[i].CanvasID != results[0].CanvasID {
			t.Fatalf("activation %d = %+v, want the canvas of %+v", i, results[i], results[0])
		}
		if i < n && results[i].Created {
			created++
		}
	}
	if shared := results[n]; shared.Created {
		created++
	}
	for i := n; i < 2*n; i++ {
		if results[i] != results[n] {
			t.Fatalf("retry %d = %+v, want the shared answer %+v", i, results[i], results[n])
		}
	}
	if created != 1 {
		t.Fatalf("operations that created the tenant = %d, want 1", created)
	}
	if rows := countActivationRows(t, s.pool); rows != (activationRows{1, 1, 1, n + 1}) {
		t.Fatalf("rows = %+v, want one tenant, membership, and canvas with %d records", rows, n+1)
	}

	// Two accounts activated at once get their own tenants.
	seedAccount(t, s, "bob@example.com")
	seedAccount(t, s, "carol@example.com")
	pair := make([]subscription.Activation, 2)
	pairErrs := make([]error, 2)
	for i, email := range []string{"bob@example.com", "carol@example.com"} {
		wg.Go(func() { pair[i], pairErrs[i] = activate(s, operator, fmt.Sprintf("op-concurrent-pair-%d", i), email) })
	}
	wg.Wait()
	if pairErrs[0] != nil || pairErrs[1] != nil {
		t.Fatalf("pair errors: %v, %v", pairErrs[0], pairErrs[1])
	}
	if pair[0].TenantID == pair[1].TenantID || pair[0].CanvasID == pair[1].CanvasID || !pair[0].Created || !pair[1].Created {
		t.Fatalf("pair = %+v, want two new distinct tenants", pair)
	}
	if rows := countActivationRows(t, s.pool); rows.tenants != 3 || rows.canvases != 3 {
		t.Fatalf("rows = %+v, want three tenants and canvases", rows)
	}
}

func TestOnlyOperatorsActivate(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	streamer := seedAccount(t, s, "streamer@example.com")
	seedAccount(t, s, "alice@example.com")

	if _, err := activate(s, streamer, "op-denied-streamer01", "alice@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("non-operator error = %v, want ErrAccessDenied", err)
	}
	if _, err := activate(s, "ffffffffffffffffffffffffffffffff", "op-denied-stranger01", "nobody@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("unknown actor error = %v, want ErrAccessDenied", err)
	}
	operator := seedOperator(t, s, "operator@example.com")
	if _, err := s.pool.Exec(ctx, "DELETE FROM operators WHERE account_id = $1", string(operator)); err != nil {
		t.Fatal(err)
	}
	if _, err := activate(s, operator, "op-denied-revoked01", "alice@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("revoked operator error = %v, want ErrAccessDenied", err)
	}
	if rows := countActivationRows(t, s.pool); rows != (activationRows{}) {
		t.Fatalf("denied activations wrote %+v", rows)
	}
	if ok, err := s.svc.IsOperator(ctx, operator); err != nil || ok {
		t.Fatalf("IsOperator after revocation = %v, %v; want false", ok, err)
	}
}

func TestOperatorGrantIsHeldUntilCommit(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, "alice@example.com")

	// While the activation waits to commit, a revocation cannot remove the
	// grant the activation was authorized with.
	reached, release := make(chan struct{}), make(chan struct{})
	postgres.SetSubscriptionFault(s.store, func(_ context.Context, step string) error {
		if step == "commit" {
			close(reached)
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, err := activate(s, operator, "op-held-grant-00001", "alice@example.com"); done <- err }()
	<-reached

	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM operators WHERE account_id = $1", string(operator))
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("revocation during activation: error = %v, want lock_not_available", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("activation: %v", err)
	}
}

func TestFailedActivationLeavesNothingAndRetryCreatesOnce(t *testing.T) {
	injected := errors.New("injected fault")
	for _, step := range []string{"CreateTenant", "commit"} {
		t.Run(step, func(t *testing.T) {
			s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
			operator := seedOperator(t, s, "operator@example.com")
			seedAccount(t, s, "alice@example.com")

			postgres.SetSubscriptionFault(s.store, func(_ context.Context, at string) error {
				if at == step {
					return injected
				}
				return nil
			})
			if _, err := activate(s, operator, "op-failed-activation", "alice@example.com"); !errors.Is(err, injected) {
				t.Fatalf("activation with a fault at %s: error = %v", step, err)
			}
			if rows := countActivationRows(t, s.pool); rows != (activationRows{}) {
				t.Fatalf("failed activation left %+v", rows)
			}

			postgres.SetSubscriptionFault(s.store, nil)
			first := mustActivate(t, s, operator, "op-failed-activation", "alice@example.com")
			second := mustActivate(t, s, operator, "op-failed-activation", "alice@example.com")
			if !first.Created || second != first {
				t.Fatalf("retry = %+v then %+v, want one creation replayed", first, second)
			}
			if rows := countActivationRows(t, s.pool); rows != (activationRows{1, 1, 1, 1}) {
				t.Fatalf("rows after retry = %+v, want one of each", rows)
			}
		})
	}
}

func TestLostActivationAnswerReplaysAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	s := openSubscriptions(t, server, clock)
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	alice := seedAccount(t, s, "alice@example.com")
	seedAccount(t, s, "bob@example.com")

	lost := mustActivate(t, s, operator, "op-lost-activation01", "alice@example.com")

	// The answer is lost; the operator retries after a server restart over a
	// new pool, with a new correlation ID.
	server.Restart(t)
	s = openSubscriptions(t, server, clock)
	clock.Advance(subscription.ActivationRetention - time.Microsecond)

	again, err := s.svc.Activate(ctx, subscription.ActivateRequest{
		OperatorID: operator, OperationID: "op-lost-activation01", Email: "ALICE@example.com", CorrelationID: "fedcba9876543210fedcba9876543210",
	})
	if err != nil || again != lost {
		t.Fatalf("retry after restart = %+v, %v; want %+v", again, err, lost)
	}
	if _, err := activate(s, operator, "op-lost-activation01", "bob@example.com"); !errors.Is(err, subscription.ErrOperationConflict) {
		t.Fatalf("different payload error = %v, want ErrOperationConflict", err)
	}
	if rows := countActivationRows(t, s.pool); rows != (activationRows{1, 1, 1, 1}) {
		t.Fatalf("rows = %+v, want the single first effect", rows)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_activations WHERE correlation_id = $1", activationCorrelation); n != 1 {
		t.Fatal("the record lost the first correlation ID")
	}

	// The committed state is read back by a pool that never saw it written.
	own, err := s.svc.CanvasOf(ctx, alice)
	if err != nil || own != (canvas.Key{TenantID: lost.TenantID, CanvasID: lost.CanvasID}) {
		t.Fatalf("CanvasOf after restart = %+v, %v; want the activated canvas", own, err)
	}
	if loaded := mustLoad(t, s.canvases, own); loaded.Dimensions != canvas.DefaultDimensions {
		t.Fatalf("canvas after restart = %+v", loaded)
	}
	if ok, err := s.svc.IsOperator(ctx, operator); err != nil || !ok {
		t.Fatalf("IsOperator after restart = %v, %v; want true", ok, err)
	}
}

func TestActivationRetentionExpiresInPostgres(t *testing.T) {
	clock := newAccountClock()
	s := openSubscriptions(t, pgtest.Start(t), clock)
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, "alice@example.com")
	bob := seedAccount(t, s, "bob@example.com")
	seedAccount(t, s, "carol@example.com")

	mustActivate(t, s, operator, "op-retention-activate", "alice@example.com")
	mustActivate(t, s, operator, "op-retention-other01", "carol@example.com")
	clock.Advance(subscription.ActivationRetention)

	replaced := mustActivate(t, s, operator, "op-retention-activate", "bob@example.com")
	if replaced.AccountID != bob || !replaced.Created {
		t.Fatalf("expired operation ID = %+v, want a new activation of %q", replaced, bob)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_activations WHERE operation_id = 'op-retention-activate' AND account_id = $1", string(bob)); n != 1 {
		t.Fatal("the expired activation row was not replaced")
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_activations"); n != 1 {
		t.Fatalf("activation records = %d, want only the unexpired one", n)
	}
}

func TestTwoStreamersAreIsolated(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	alice := seedAccount(t, s, "alice@example.com")
	bob := seedAccount(t, s, "bob@example.com")

	a := mustActivate(t, s, operator, "op-isolated-alice001", "alice@example.com")
	b := mustActivate(t, s, operator, "op-isolated-bob00001", "bob@example.com")
	if a.TenantID == b.TenantID || a.CanvasID == b.CanvasID {
		t.Fatalf("streamers share a tenant or canvas: %+v, %+v", a, b)
	}
	for _, streamer := range []struct {
		account access.AccountID
		own     subscription.Activation
		other   subscription.Activation
	}{{alice, a, b}, {bob, b, a}} {
		key, err := s.svc.CanvasOf(ctx, streamer.account)
		if err != nil || key != (canvas.Key{TenantID: streamer.own.TenantID, CanvasID: streamer.own.CanvasID}) {
			t.Fatalf("CanvasOf(%s) = %+v, %v; want its own canvas", streamer.account, key, err)
		}
		if _, found, err := s.accounts.MembershipOf(ctx, streamer.account, streamer.other.TenantID); err != nil || found {
			t.Fatalf("MembershipOf(%s, other tenant) found = %v, %v; want none", streamer.account, found, err)
		}
		if m, found, err := s.accounts.MembershipOf(ctx, streamer.account, streamer.own.TenantID); err != nil || !found || m.Role != access.RoleOwner {
			t.Fatalf("MembershipOf(%s, own tenant) = %+v, %v, %v; want owner", streamer.account, m, found, err)
		}
	}
	for _, account := range []access.AccountID{operator, "ffffffffffffffffffffffffffffffff"} {
		if _, err := s.svc.CanvasOf(ctx, account); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("CanvasOf(%s) error = %v, want ErrAccessDenied", account, err)
		}
	}
}

func TestRetainedActivationIsReportedByTheStore(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, "alice@example.com")
	first := mustActivate(t, s, operator, "op-store-retained-01", "alice@example.com")

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	err := s.store.InTx(ctx, func(tx subscription.Tx) error {
		if ok, err := tx.IsOperator(ctx, operator); err != nil || !ok {
			return fmt.Errorf("operator = %v: %w", ok, err)
		}
		return tx.RecordActivation(ctx, subscription.Record{
			OperationID: "op-store-retained-01", Result: first, AcceptedAt: now,
		}, now.Add(-subscription.ActivationRetention))
	})
	if !errors.Is(err, subscription.ErrOperationRetained) {
		t.Fatalf("RecordActivation of a retained ID: error = %v, want ErrOperationRetained", err)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_activations"); n != 1 {
		t.Fatalf("activation records = %d, want 1", n)
	}
}

func TestGrantOperatorIsIdempotent(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	id := seedAccount(t, s, "operator@example.com")

	for range 2 {
		granted, err := s.store.GrantOperator(ctx, "Operator@Example.com")
		if err != nil || granted != id {
			t.Fatalf("GrantOperator = %q, %v; want %q", granted, err, id)
		}
	}
	if n := count(t, s.pool, "SELECT count(*) FROM operators WHERE account_id = $1", string(id)); n != 1 {
		t.Fatalf("operator rows = %d, want 1", n)
	}
	if ok, err := s.store.IsOperator(ctx, id); err != nil || !ok {
		t.Fatalf("IsOperator = %v, %v; want true", ok, err)
	}
	for _, email := range []string{"nobody@example.com", "not an email"} {
		if _, err := s.store.GrantOperator(ctx, email); !errors.Is(err, subscription.ErrAccountNotFound) {
			t.Fatalf("GrantOperator(%q) error = %v, want ErrAccountNotFound", email, err)
		}
	}
	if n := count(t, s.pool, "SELECT count(*) FROM operators"); n != 1 {
		t.Fatalf("operator rows = %d, want 1", n)
	}
}
