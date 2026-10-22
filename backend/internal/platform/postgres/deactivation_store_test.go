package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const deactivationCorrelation = "fedcba9876543210fedcba9876543210"

func deactivateIn(s subscriptions, operator access.AccountID, op, email string) (subscription.Deactivation, error) {
	return s.svc.Deactivate(context.Background(), subscription.DeactivateRequest{
		OperatorID: operator, OperationID: op, Email: email, CorrelationID: deactivationCorrelation,
	})
}

func mustDeactivate(t *testing.T, s subscriptions, operator access.AccountID, op, email string) subscription.Deactivation {
	t.Helper()
	deactivation, err := deactivateIn(s, operator, op, email)
	if err != nil {
		t.Fatalf("Deactivate(%s, %s): %v", op, email, err)
	}
	return deactivation
}

// tenantFacts is the stored subscription state of one tenant.
type tenantFacts struct {
	status  string
	version int64
}

func factsOf(t *testing.T, pool *pgxpool.Pool, tenantID string) tenantFacts {
	t.Helper()
	var facts tenantFacts
	if err := pool.QueryRow(context.Background(), "SELECT subscription_status, access_version FROM tenants WHERE tenant_id = $1", tenantID).
		Scan(&facts.status, &facts.version); err != nil {
		t.Fatalf("read tenant %s: %v", tenantID, err)
	}
	return facts
}

// deactivationRows counts every table a deactivation writes.
type deactivationRows struct{ records, cuts, pending int }

func countDeactivationRows(t *testing.T, pool *pgxpool.Pool) deactivationRows {
	t.Helper()
	return deactivationRows{
		records: count(t, pool, "SELECT count(*) FROM subscription_deactivations"),
		cuts:    count(t, pool, "SELECT count(*) FROM access_cuts"),
		pending: count(t, pool, "SELECT count(*) FROM access_cuts WHERE applied_at IS NULL"),
	}
}

// canvasWrites counts what an accepted canvas mutation writes.
type canvasWrites struct{ operations, outbox int }

func countCanvasWrites(t *testing.T, pool *pgxpool.Pool, key canvas.Key) canvasWrites {
	t.Helper()
	return canvasWrites{
		operations: count(t, pool, "SELECT count(*) FROM canvas_operations WHERE tenant_id = $1 AND canvas_id = $2", key.TenantID, key.CanvasID),
		outbox:     count(t, pool, "SELECT count(*) FROM canvas_outbox WHERE tenant_id = $1 AND canvas_id = $2", key.TenantID, key.CanvasID),
	}
}

// waitForLockWaiters waits until n sessions wait for a lock. It polls a
// condition; it never sleeps in place of synchronization.
func waitForLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for count(t, pool, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'") < n {
		if time.Now().After(deadline) {
			t.Fatalf("no %d session(s) waiting for a lock", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamer seeds an operator and activates the account of email.
func streamer(t *testing.T, s subscriptions, email string) (access.AccountID, subscription.Activation, canvas.Key) {
	t.Helper()
	operator := seedOperator(t, s, "operator@example.com")
	seedAccount(t, s, email)
	a := mustActivate(t, s, operator, "op-activate-streamer", email)
	return operator, a, canvas.Key{TenantID: a.TenantID, CanvasID: a.CanvasID}
}

func TestDeactivationCommitsTheTenantRecordAndOneCut(t *testing.T) {
	clock := newAccountClock()
	s := openSubscriptions(t, pgtest.Start(t), clock)
	ctx := context.Background()
	operator, a, key := streamer(t, s, "alice@example.com")
	seedAccount(t, s, "bob@example.com")
	b := mustActivate(t, s, operator, "op-activate-bob-0001", "bob@example.com")

	got := mustDeactivate(t, s, operator, "op-deactivate-alice1", " Alice@Example.com ")
	want := subscription.Deactivation{AccountID: a.AccountID, TenantID: a.TenantID, CanvasID: a.CanvasID, Deactivated: true, Version: 2}
	if got != want {
		t.Fatalf("deactivation = %+v, want %+v", got, want)
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"inactive", 2}) {
		t.Fatalf("tenant = %+v, want inactive at version 2", facts)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{1, 1, 1}) {
		t.Fatalf("rows = %+v, want one record and one pending cut", rows)
	}
	if n := count(t, s.pool, `SELECT count(*) FROM subscription_deactivations WHERE operation_id = 'op-deactivate-alice1'
		AND operator_account_id = $1 AND account_id = $2 AND tenant_id = $3 AND canvas_id = $4 AND deactivated AND version = 2
		AND correlation_id = $5 AND accepted_at = $6`,
		string(operator), string(a.AccountID), a.TenantID, a.CanvasID, deactivationCorrelation, clock.Now()); n != 1 {
		t.Fatal("the deactivation record does not keep the answer, operator, and correlation ID")
	}
	pending, err := s.store.PendingCuts(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingCuts = %+v, %v; want one", pending, err)
	}
	wantCut := subscription.AccessCut{TenantID: a.TenantID, Version: 2, CorrelationID: deactivationCorrelation, CreatedAt: clock.Now()}
	if cut := pending[0]; cut.ID < 1 || cut.TenantID != wantCut.TenantID || cut.Version != wantCut.Version || cut.CorrelationID != wantCut.CorrelationID || !cut.CreatedAt.Equal(wantCut.CreatedAt) {
		t.Fatalf("cut = %+v, want %+v", cut, wantCut)
	}
	// The deactivated owner reads no canvas and no grant; the rows survive.
	if _, err := s.svc.CanvasOf(ctx, a.AccountID); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("CanvasOf = %v, want ErrAccessDenied", err)
	}
	if _, err := s.svc.EditorGrant(ctx, a.AccountID, "session-alice"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("EditorGrant = %v, want ErrAccessDenied", err)
	}
	if m, found, err := s.accounts.MembershipOf(ctx, a.AccountID, a.TenantID); err != nil || !found || m.Subscription != access.SubscriptionInactive {
		t.Fatalf("MembershipOf = %+v, %v, %v; want an inactive owner", m, found, err)
	}
	if loaded := mustLoad(t, s.canvases, key); loaded.Dimensions != canvas.DefaultDimensions {
		t.Fatalf("canvas = %+v", loaded)
	}
	// Tenant B is untouched.
	if facts := factsOf(t, s.pool, b.TenantID); facts != (tenantFacts{"active", 1}) {
		t.Fatalf("tenant B = %+v, want active at version 1", facts)
	}

	// Deactivating again changes nothing and writes no cut.
	again := mustDeactivate(t, s, operator, "op-deactivate-alice2", "alice@example.com")
	if again != (subscription.Deactivation{AccountID: a.AccountID, TenantID: a.TenantID, CanvasID: a.CanvasID, Deactivated: false, Version: 2}) {
		t.Fatalf("second deactivation = %+v", again)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{2, 1, 1}) {
		t.Fatalf("rows = %+v, want two records and still one cut", rows)
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"inactive", 2}) {
		t.Fatalf("tenant after a second deactivation = %+v", facts)
	}
}

func TestFailedDeactivationLeavesNothingAndRetryCutsOnce(t *testing.T) {
	injected := errors.New("injected fault")
	for _, step := range []string{"RecordCut", "commit"} {
		t.Run(step, func(t *testing.T) {
			s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
			operator, a, _ := streamer(t, s, "alice@example.com")

			postgres.SetSubscriptionFault(s.store, func(_ context.Context, at string) error {
				if at == step {
					return injected
				}
				return nil
			})
			if _, err := deactivateIn(s, operator, "op-failed-deactivate", "alice@example.com"); !errors.Is(err, injected) {
				t.Fatalf("deactivation with a fault at %s: error = %v", step, err)
			}
			if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{}) {
				t.Fatalf("failed deactivation left %+v", rows)
			}
			if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"active", 1}) {
				t.Fatalf("tenant after a failed deactivation = %+v", facts)
			}

			postgres.SetSubscriptionFault(s.store, nil)
			first := mustDeactivate(t, s, operator, "op-failed-deactivate", "alice@example.com")
			second := mustDeactivate(t, s, operator, "op-failed-deactivate", "alice@example.com")
			if !first.Deactivated || first.Version != 2 || second != first {
				t.Fatalf("retry = %+v then %+v, want one cut replayed", first, second)
			}
			if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{1, 1, 1}) {
				t.Fatalf("rows after retry = %+v, want one record and one cut", rows)
			}
		})
	}
}

func TestLostDeactivationAnswerReplaysAfterRestart(t *testing.T) {
	server := pgtest.Start(t)
	clock := newAccountClock()
	s := openSubscriptions(t, server, clock)
	ctx := context.Background()
	operator, a, _ := streamer(t, s, "alice@example.com")
	seedAccount(t, s, "bob@example.com")
	mustActivate(t, s, operator, "op-activate-bob-0001", "bob@example.com")

	lost := mustDeactivate(t, s, operator, "op-lost-deactivate01", "alice@example.com")

	server.Restart(t)
	s = openSubscriptions(t, server, clock)
	// A reactivation in between does not change the retained answer.
	mustActivate(t, s, operator, "op-reactivate-alice1", "alice@example.com")
	clock.Advance(subscription.ActivationRetention - time.Microsecond)

	again, err := s.svc.Deactivate(ctx, subscription.DeactivateRequest{
		OperatorID: operator, OperationID: "op-lost-deactivate01", Email: "ALICE@example.com", CorrelationID: "0000000000000000ffffffffffffffff",
	})
	if err != nil || again != lost {
		t.Fatalf("retry after restart = %+v, %v; want %+v", again, err, lost)
	}
	if _, err := deactivateIn(s, operator, "op-lost-deactivate01", "bob@example.com"); !errors.Is(err, subscription.ErrOperationConflict) {
		t.Fatalf("different payload error = %v, want ErrOperationConflict", err)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{1, 1, 1}) {
		t.Fatalf("rows = %+v, want the single first effect", rows)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_deactivations WHERE correlation_id = $1", deactivationCorrelation); n != 1 {
		t.Fatal("the record lost the first correlation ID")
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"active", 2}) {
		t.Fatalf("tenant = %+v, want the reactivation kept at version 2", facts)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM tenants WHERE subscription_status = 'active' AND access_version = 1"); n != 1 {
		t.Fatal("tenant B changed")
	}
}

func TestDeactivationRetentionExpiresInPostgres(t *testing.T) {
	clock := newAccountClock()
	s := openSubscriptions(t, pgtest.Start(t), clock)
	operator, _, _ := streamer(t, s, "alice@example.com")
	bob := seedAccount(t, s, "bob@example.com")
	seedAccount(t, s, "carol@example.com")
	b := mustActivate(t, s, operator, "op-activate-bob-0001", "bob@example.com")
	mustActivate(t, s, operator, "op-activate-carol001", "carol@example.com")

	mustDeactivate(t, s, operator, "op-retention-deact01", "alice@example.com")
	mustDeactivate(t, s, operator, "op-retention-other01", "carol@example.com")
	clock.Advance(subscription.ActivationRetention)

	replaced := mustDeactivate(t, s, operator, "op-retention-deact01", "bob@example.com")
	if replaced.AccountID != bob || replaced.TenantID != b.TenantID || !replaced.Deactivated {
		t.Fatalf("expired operation ID = %+v, want a new deactivation of %q", replaced, bob)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM subscription_deactivations WHERE operation_id = 'op-retention-deact01' AND account_id = $1", string(bob)); n != 1 {
		t.Fatal("the expired deactivation row was not replaced")
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{1, 3, 3}) {
		t.Fatalf("rows = %+v, want only the unexpired record and every cut", rows)
	}
}

func TestOnlyOperatorsDeactivate(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, a, _ := streamer(t, s, "alice@example.com")
	other := seedAccount(t, s, "streamer@example.com")

	if _, err := deactivateIn(s, other, "op-denied-streamer01", "alice@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("non-operator error = %v, want ErrAccessDenied", err)
	}
	if _, err := deactivateIn(s, a.AccountID, "op-denied-owner00001", "alice@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("owner error = %v, want ErrAccessDenied", err)
	}
	if _, err := deactivateIn(s, "ffffffffffffffffffffffffffffffff", "op-denied-stranger01", "nobody@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("unknown actor error = %v, want ErrAccessDenied", err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM operators WHERE account_id = $1", string(operator)); err != nil {
		t.Fatal(err)
	}
	if _, err := deactivateIn(s, operator, "op-denied-revoked01", "alice@example.com"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("revoked operator error = %v, want ErrAccessDenied", err)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{}) {
		t.Fatalf("denied deactivations wrote %+v", rows)
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"active", 1}) {
		t.Fatalf("tenant = %+v, want untouched", facts)
	}
}

func TestDeactivatingAnAccountWithoutATenantIsNotFound(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, _, _ := streamer(t, s, "alice@example.com")
	seedAccount(t, s, "never@example.com")
	for _, email := range []string{"never@example.com", "nobody@example.com"} {
		if _, err := deactivateIn(s, operator, "op-notfound-deact01", email); !errors.Is(err, subscription.ErrAccountNotFound) {
			t.Fatalf("%s: error = %v, want ErrAccountNotFound", email, err)
		}
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{}) {
		t.Fatalf("not found wrote %+v", rows)
	}
	if got := mustDeactivate(t, s, operator, "op-notfound-deact01", "alice@example.com"); !got.Deactivated {
		t.Fatalf("reuse after not found = %+v", got)
	}
}

func TestDeactivationHoldsTheOperatorGrantUntilCommit(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, _, _ := streamer(t, s, "alice@example.com")

	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	postgres.SetSubscriptionFault(s.store, func(_ context.Context, step string) error {
		if step == "commit" {
			close(reached)
			<-release
		}
		return nil
	})
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, err := deactivateIn(s, operator, "op-held-grant-deact1", "alice@example.com")
		done <- err
	}()
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("deactivation ended before commit: %v", err)
	}

	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM operators WHERE account_id = $1", string(operator))
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("revocation during deactivation: error = %v, want lock_not_available", err)
	}
	releaseHeld()
	if err := <-done; err != nil {
		t.Fatalf("deactivation: %v", err)
	}
}

// S3: a mutation whose transaction holds the tenant row before the
// deactivation commits persists; the deactivation waits for it.
func TestAMutationInFlightBeforeTheDeactivationPersists(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, a, key := streamer(t, s, "alice@example.com")

	held := postgres.NewCanvasStore(s.pool)
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	postgres.SetBeforeCommit(held, func(context.Context) error {
		close(reached)
		<-release
		return nil
	})
	type applied struct {
		result canvas.Result
		err    error
	}
	mutation := make(chan applied, 1)
	go func() {
		result, err := held.Apply(context.Background(), key, canvas.Operation{ID: "op-in-flight-mutation"}, createText)
		mutation <- applied{result, err}
	}()
	deactivation := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case m := <-mutation:
		close(finished)
		t.Fatalf("mutation ended before commit: %+v", m)
	}
	go func() {
		defer close(finished)
		_, err := deactivateIn(s, operator, "op-deactivate-race01", "alice@example.com")
		deactivation <- err
	}()

	// The deactivation waits for the tenant row the mutation holds.
	waitForLockWaiters(t, s.pool, 1)
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"active", 1}) {
		t.Fatalf("tenant while the mutation is in flight = %+v, want active", facts)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{}) {
		t.Fatalf("deactivation committed before the mutation: %+v", rows)
	}
	releaseHeld()
	m := <-mutation
	if m.err != nil || m.result.Revision != 1 {
		t.Fatalf("in-flight mutation = %+v, %v; want revision 1", m.result, m.err)
	}
	if err := <-deactivation; err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"inactive", 2}) {
		t.Fatalf("tenant = %+v, want inactive at version 2", facts)
	}
	loaded := mustLoad(t, s.canvases, key)
	if loaded.Revision != 1 || len(loaded.Objects) != 1 || loaded.Objects[0].ID != m.result.ObjectID {
		t.Fatalf("canvas = %+v, want the in-flight object", loaded)
	}
	if writes := countCanvasWrites(t, s.pool, key); writes != (canvasWrites{1, 1}) {
		t.Fatalf("writes = %+v, want one operation and one outbox record", writes)
	}
}

// S3: a mutation that reaches the tenant row while the deactivation holds it
// waits, and is rejected once the deactivation commits.
func TestAMutationWaitingBehindTheDeactivationIsRejected(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, _, key := streamer(t, s, "alice@example.com")

	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	postgres.SetSubscriptionFault(s.store, func(_ context.Context, step string) error {
		if step == "commit" {
			close(reached)
			<-release
		}
		return nil
	})
	deactivation, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, err := deactivateIn(s, operator, "op-deactivate-first1", "alice@example.com")
		deactivation <- err
	}()
	t.Cleanup(func() {
		releaseHeld()
		<-finished
	})
	select {
	case <-reached:
	case err := <-deactivation:
		t.Fatalf("deactivation ended before commit: %v", err)
	}

	mutation := make(chan error, 1)
	go func() {
		_, err := s.canvases.Apply(context.Background(), key, canvas.Operation{ID: "op-after-the-cut-01"}, createText)
		mutation <- err
	}()
	waitForLockWaiters(t, s.pool, 1)
	releaseHeld()
	if err := <-deactivation; err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	if err := <-mutation; !errors.Is(err, canvas.ErrInactive) {
		t.Fatalf("mutation after the cut: error = %v, want ErrInactive", err)
	}
	if loaded := mustLoad(t, s.canvases, key); loaded.Revision != 0 || len(loaded.Objects) != 0 {
		t.Fatalf("canvas = %+v, want it unchanged", loaded)
	}
	if writes := countCanvasWrites(t, s.pool, key); writes != (canvasWrites{}) {
		t.Fatalf("rejected mutation wrote %+v", writes)
	}
}

// S1: from the deactivation commit every new mutation is rejected and writes
// nothing; a retry of a mutation committed before it replays its result
// without an effect; reactivation accepts mutations again.
func TestMutationsAfterTheDeactivationAreRejected(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	operator, _, key := streamer(t, s, "alice@example.com")
	before := canvas.Operation{ID: "op-before-the-cut-01"}
	committed, err := s.canvases.Apply(context.Background(), key, before, createText)
	if err != nil {
		t.Fatal(err)
	}
	mustDeactivate(t, s, operator, "op-deactivate-gate01", "alice@example.com")
	state := mustLoad(t, s.canvases, key)
	writes := countCanvasWrites(t, s.pool, key)

	mutations := map[string]canvas.Mutation{
		"dimensions": canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}},
		"create":     createText,
		"content":    canvas.SetContent{ObjectID: committed.ObjectID, Content: "edited"},
		"transform":  canvas.SetTransform{ObjectID: committed.ObjectID, Transform: canvas.Transform{Width: 5, Height: 5}},
		"delete":     canvas.DeleteObject{ObjectID: committed.ObjectID},
		// The retained ID of the committed mutation with another payload.
		"reused ID": canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 800, Height: 600}},
	}
	for name, mutation := range mutations {
		operation := nextOperation(t)
		if name == "reused ID" {
			operation = before
		}
		if _, err := s.canvases.Apply(context.Background(), key, operation, mutation); !errors.Is(err, canvas.ErrInactive) {
			t.Fatalf("%s: error = %v, want ErrInactive", name, err)
		}
	}
	if got := mustLoad(t, s.canvases, key); !reflect.DeepEqual(got, state) {
		t.Fatalf("canvas = %+v, want %+v", got, state)
	}
	if got := countCanvasWrites(t, s.pool, key); got != writes {
		t.Fatalf("writes = %+v, want %+v", got, writes)
	}

	replay, err := s.canvases.Apply(context.Background(), key, before, createText)
	if err != nil || replay != committed {
		t.Fatalf("replay of a mutation committed before the cut = %+v, %v; want %+v", replay, err, committed)
	}
	if got := countCanvasWrites(t, s.pool, key); got != writes {
		t.Fatalf("replay wrote %+v, want %+v", got, writes)
	}

	mustActivate(t, s, operator, "op-reactivate-gate01", "alice@example.com")
	if result := apply(t, s.canvases, key, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}}); result.Revision != state.Revision+1 {
		t.Fatalf("mutation after reactivation = %+v, want revision %d", result, state.Revision+1)
	}
}

// canvasRows renders every stored row of a canvas and its media.
func canvasRows(t *testing.T, pool *pgxpool.Pool, key canvas.Key) string {
	t.Helper()
	var rows string
	err := pool.QueryRow(context.Background(), `SELECT concat_ws(E'\n',
		(SELECT row_to_json(c)::text FROM canvases c WHERE tenant_id = $1 AND canvas_id = $2),
		(SELECT json_agg(o ORDER BY object_id)::text FROM canvas_objects o WHERE tenant_id = $1 AND canvas_id = $2),
		(SELECT row_to_json(u)::text FROM media_usage u WHERE tenant_id = $1 AND canvas_id = $2),
		(SELECT json_agg(a ORDER BY asset_id)::text FROM media_assets a WHERE tenant_id = $1 AND canvas_id = $2),
		(SELECT json_agg(r ORDER BY reference_id)::text FROM media_references r WHERE tenant_id = $1 AND canvas_id = $2))`,
		key.TenantID, key.CanvasID).Scan(&rows)
	if err != nil {
		t.Fatalf("read canvas rows: %v", err)
	}
	return rows
}

// S5: deactivation and reactivation keep the canvas, its dimensions, its
// objects, and its media byte for byte.
func TestReactivationRestoresTheSameCanvasObjectsAndMedia(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, a, key := streamer(t, s, "alice@example.com")
	mediaStore := postgres.NewMediaStore(s.pool)
	mediaKey := media.CanvasKey{TenantID: key.TenantID, CanvasID: key.CanvasID}

	apply(t, s.canvases, key, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}})
	text := apply(t, s.canvases, key, canvas.CreateObject{Kind: canvas.KindText, Content: "hello\nworld", Transform: canvas.Transform{X: -40, Y: 25, Width: 300, Height: 80, Rotation: 12.5}})
	image := upload(t, mediaStore, mediaKey, 4096, "reference-image")
	apply(t, s.canvases, key, canvas.CreateObject{Kind: canvas.KindImage, Content: "reference-image", Transform: canvas.Transform{X: 5, Y: 5, Width: 64, Height: 64}})
	upload(t, mediaStore, mediaKey, 2048, "reference-audio")
	apply(t, s.canvases, key, canvas.CreateObject{Kind: canvas.KindAudio, Content: "reference-audio", Transform: canvas.Transform{Width: 1, Height: 1}})
	apply(t, s.canvases, key, canvas.SetTransform{ObjectID: text.ObjectID, Transform: canvas.Transform{X: 7, Y: -9, Width: 120, Height: 40, Rotation: 359.5}})

	state := mustLoad(t, s.canvases, key)
	rows := canvasRows(t, s.pool, key)
	usage := usageOf(t, mediaStore, mediaKey)
	if len(state.Objects) != 3 || state.Dimensions != (canvas.Dimensions{Width: 1280, Height: 720}) || usage.UsedBytes != 6144 {
		t.Fatalf("seeded canvas = %+v, usage %+v", state, usage)
	}

	mustDeactivate(t, s, operator, "op-deactivate-keep01", "alice@example.com")
	if got := canvasRows(t, s.pool, key); got != rows {
		t.Fatalf("rows after deactivation:\n%s\nwant\n%s", got, rows)
	}
	again := mustActivate(t, s, operator, "op-reactivate-keep01", "alice@example.com")
	if again.TenantID != a.TenantID || again.CanvasID != a.CanvasID || again.Created {
		t.Fatalf("reactivation = %+v, want the canvas of %+v", again, a)
	}
	if got := mustLoad(t, s.canvases, key); !reflect.DeepEqual(got, state) {
		t.Fatalf("canvas after reactivation = %+v, want %+v", got, state)
	}
	if got := canvasRows(t, s.pool, key); got != rows {
		t.Fatalf("rows after reactivation:\n%s\nwant\n%s", got, rows)
	}
	if got := usageOf(t, mediaStore, mediaKey); got != usage {
		t.Fatalf("usage = %+v, want %+v", got, usage)
	}
	if ready, err := mediaStore.Ready(ctx, mediaKey, image); err != nil || ready.ID != image {
		t.Fatalf("Ready(%s) = %+v, %v", image, ready, err)
	}
	own, err := s.svc.CanvasOf(ctx, a.AccountID)
	if err != nil || own != key {
		t.Fatalf("CanvasOf = %+v, %v; want %+v", own, err, key)
	}
	if facts := factsOf(t, s.pool, a.TenantID); facts != (tenantFacts{"active", 2}) {
		t.Fatalf("tenant = %+v, want active at version 2", facts)
	}
	grant, err := s.svc.EditorGrant(ctx, a.AccountID, "session-alice")
	if err != nil || grant.Version != 2 || grant.TenantID != a.TenantID {
		t.Fatalf("EditorGrant = %+v, %v; want version 2", grant, err)
	}
}

// S8: deactivating tenant A leaves tenant B working, and A's moderator loses
// access with A's owner.
func TestDeactivatingOneTenantLeavesTheOtherWorking(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, a, keyA := streamer(t, s, "alice@example.com")
	bob := seedAccount(t, s, "bob@example.com")
	moderator := seedAccount(t, s, "moderator@example.com")
	b := mustActivate(t, s, operator, "op-activate-bob-0001", "bob@example.com")
	keyB := canvas.Key{TenantID: b.TenantID, CanvasID: b.CanvasID}
	exec(t, s.pool, "INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", a.TenantID, string(moderator))
	allowed := func(account access.AccountID, tenantID string) error {
		m, found, err := s.accounts.MembershipOf(ctx, account, tenantID)
		if err != nil {
			t.Fatalf("MembershipOf: %v", err)
		}
		return access.Check(access.CanvasRequest(m, found, tenantID, access.ActionEdit))
	}
	if err := allowed(moderator, a.TenantID); err != nil {
		t.Fatalf("moderator before the cut: %v", err)
	}
	grantB, err := s.svc.EditorGrant(ctx, bob, "session-bob")
	if err != nil {
		t.Fatal(err)
	}

	mustDeactivate(t, s, operator, "op-deactivate-two-01", "alice@example.com")
	if err := allowed(moderator, a.TenantID); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("moderator of the deactivated tenant: %v, want ErrAccessDenied", err)
	}
	if err := allowed(a.AccountID, a.TenantID); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("owner of the deactivated tenant: %v, want ErrAccessDenied", err)
	}
	if _, err := s.canvases.Apply(ctx, keyA, nextOperation(t), createText); !errors.Is(err, canvas.ErrInactive) {
		t.Fatalf("mutation of A: %v, want ErrInactive", err)
	}

	if facts := factsOf(t, s.pool, b.TenantID); facts != (tenantFacts{"active", 1}) {
		t.Fatalf("tenant B = %+v, want active at version 1", facts)
	}
	if err := allowed(bob, b.TenantID); err != nil {
		t.Fatalf("owner of B: %v", err)
	}
	if again, err := s.svc.EditorGrant(ctx, bob, "session-bob"); err != nil || again != grantB {
		t.Fatalf("EditorGrant(B) = %+v, %v; want %+v", again, err, grantB)
	}
	if result := apply(t, s.canvases, keyB, createText); result.Revision != 1 {
		t.Fatalf("mutation of B = %+v, want revision 1", result)
	}
	if own, err := s.svc.CanvasOf(ctx, bob); err != nil || own != keyB {
		t.Fatalf("CanvasOf(B) = %+v, %v", own, err)
	}
	pending, err := s.store.PendingCuts(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].TenantID != a.TenantID {
		t.Fatalf("PendingCuts = %+v, %v; want only A's cut", pending, err)
	}
}

func TestRetainedDeactivationIsReportedByTheStore(t *testing.T) {
	s := openSubscriptions(t, pgtest.Start(t), newAccountClock())
	ctx := context.Background()
	operator, _, _ := streamer(t, s, "alice@example.com")
	first := mustDeactivate(t, s, operator, "op-store-retained-d1", "alice@example.com")

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	err := s.store.InTx(ctx, func(tx subscription.Tx) error {
		return tx.RecordDeactivation(ctx, subscription.DeactivationRecord{
			OperationID: "op-store-retained-d1", OperatorID: operator, Result: first, AcceptedAt: now,
		}, now.Add(-subscription.ActivationRetention))
	})
	if !errors.Is(err, subscription.ErrOperationRetained) {
		t.Fatalf("RecordDeactivation of a retained ID: error = %v, want ErrOperationRetained", err)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{1, 1, 1}) {
		t.Fatalf("rows = %+v, want the first only", rows)
	}
}

func TestPendingCutsAreOldestFirstAndSettleOnce(t *testing.T) {
	clock := newAccountClock()
	s := openSubscriptions(t, pgtest.Start(t), clock)
	ctx := context.Background()
	operator := seedOperator(t, s, "operator@example.com")
	var tenants []string
	for i, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		seedAccount(t, s, email)
		tenants = append(tenants, mustActivate(t, s, operator, fmt.Sprintf("op-pending-activate-%d", i), email).TenantID)
	}
	for i, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		mustDeactivate(t, s, operator, fmt.Sprintf("op-pending-deactivate-%d", i), email)
		clock.Advance(time.Second)
	}

	first, err := s.store.PendingCuts(ctx, 2)
	if err != nil || len(first) != 2 || first[0].TenantID != tenants[0] || first[1].TenantID != tenants[1] || first[0].ID >= first[1].ID {
		t.Fatalf("PendingCuts(2) = %+v, %v; want A then B", first, err)
	}
	if !first[1].CreatedAt.Equal(first[0].CreatedAt.Add(time.Second)) || first[0].Version != 2 || first[0].CorrelationID != deactivationCorrelation {
		t.Fatalf("cuts = %+v", first)
	}
	settledAt := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	if err := s.store.SettleCut(ctx, first[0].ID, settledAt); err != nil {
		t.Fatalf("SettleCut: %v", err)
	}
	// Settling again or settling an unknown cut changes nothing.
	if err := s.store.SettleCut(ctx, first[0].ID, settledAt.Add(time.Hour)); err != nil {
		t.Fatalf("SettleCut again: %v", err)
	}
	if err := s.store.SettleCut(ctx, 1<<40, settledAt); err != nil {
		t.Fatalf("SettleCut unknown: %v", err)
	}
	if n := count(t, s.pool, "SELECT count(*) FROM access_cuts WHERE id = $1 AND applied_at = $2", first[0].ID, settledAt); n != 1 {
		t.Fatal("the settled cut does not keep its first applied time")
	}
	rest, err := s.store.PendingCuts(ctx, 10)
	if err != nil || len(rest) != 2 || rest[0] != first[1] || rest[1].TenantID != tenants[2] {
		t.Fatalf("PendingCuts after settle = %+v, %v; want B then C", rest, err)
	}
	if rows := countDeactivationRows(t, s.pool); rows != (deactivationRows{3, 3, 2}) {
		t.Fatalf("rows = %+v", rows)
	}
}
