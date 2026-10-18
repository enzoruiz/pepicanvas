package subscription_test

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription/subscriptiontest"
)

const (
	operator  = access.AccountID("0000000000000000000000000000000a")
	streamerA = access.AccountID("000000000000000000000000000000a1")
	streamerB = access.AccountID("000000000000000000000000000000b1")
	stranger  = access.AccountID("00000000000000000000000000000005")
	opID      = "operation-0000001"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func fixture(t *testing.T) (*subscription.Service, *subscriptiontest.MemoryStore, *clock) {
	t.Helper()
	store := subscriptiontest.NewMemoryStore()
	store.AddAccount(operator, "operator@example.com")
	store.AddAccount(streamerA, "a@example.com")
	store.AddAccount(streamerB, "b@example.com")
	store.AddAccount(stranger, "stranger@example.com")
	store.GrantOperator(operator)
	c := &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	return subscription.NewService(store, subscription.WithClock(c.now)), store, c
}

func activate(op, email string) subscription.ActivateRequest {
	return subscription.ActivateRequest{OperatorID: operator, OperationID: op, Email: email, CorrelationID: "corr-" + op}
}

func onlyTenant(t *testing.T, store *subscriptiontest.MemoryStore, owner access.AccountID) subscription.Tenant {
	t.Helper()
	var owned []subscription.Tenant
	for _, tenant := range store.Tenants() {
		if tenant.Owner == owner {
			owned = append(owned, tenant)
		}
	}
	if len(owned) != 1 {
		t.Fatalf("tenants owned by %s = %d, want 1", owner, len(owned))
	}
	return owned[0]
}

func TestActivationCreatesOneActiveTenantWithOneDefaultCanvas(t *testing.T) {
	service, store, _ := fixture(t)

	got, err := service.Activate(context.Background(), activate(opID, "  A@Example.com "))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got.AccountID != streamerA || !got.Created || !hex32.MatchString(got.TenantID) || !hex32.MatchString(got.CanvasID) {
		t.Fatalf("activation = %+v", got)
	}
	tenant := onlyTenant(t, store, streamerA)
	if tenant.ID != got.TenantID || tenant.CanvasID != got.CanvasID || tenant.Status != access.SubscriptionActive {
		t.Fatalf("tenant = %+v, activation = %+v", tenant, got)
	}
	key := canvas.Key{TenantID: got.TenantID, CanvasID: got.CanvasID}
	if canvases := store.Canvases(); len(canvases) != 1 || canvases[key] != canvas.DefaultDimensions {
		t.Fatalf("canvases = %v, want only %v at 1920x1080", canvases, key)
	}
	if role, ok := store.Membership(got.TenantID, streamerA); !ok || role != access.RoleOwner {
		t.Fatalf("owner membership = %q, %v", role, ok)
	}
	if records := store.Records(); len(records) != 1 || records[opID].CorrelationID != "corr-"+opID || records[opID].OperatorID != operator {
		t.Fatalf("records = %+v, want one with the operator and the request correlation", records)
	}
}

func TestRepeatedActivationKeepsTheSameCanvas(t *testing.T) {
	service, store, _ := fixture(t)
	first, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}

	again, err := service.Activate(context.Background(), activate("operation-0000002", "A@EXAMPLE.COM"))
	if err != nil {
		t.Fatalf("second Activate: %v", err)
	}
	if again.TenantID != first.TenantID || again.CanvasID != first.CanvasID || again.Created {
		t.Fatalf("second activation = %+v, first = %+v", again, first)
	}
	onlyTenant(t, store, streamerA)
	if n := len(store.Canvases()); n != 1 {
		t.Fatalf("canvases = %d, want 1", n)
	}
}

func TestActivatingAnInactiveTenantEnablesItsExistingCanvas(t *testing.T) {
	service, store, _ := fixture(t)
	first, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetStatus(first.TenantID, access.SubscriptionInactive)

	again, err := service.Activate(context.Background(), activate("operation-0000002", "a@example.com"))
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	tenant := onlyTenant(t, store, streamerA)
	if again.CanvasID != first.CanvasID || again.Created || tenant.Status != access.SubscriptionActive {
		t.Fatalf("reactivation = %+v, tenant = %+v", again, tenant)
	}
	if n := len(store.Canvases()); n != 1 {
		t.Fatalf("canvases = %d, want 1", n)
	}
}

func TestConcurrentActivationsProduceOneCanvas(t *testing.T) {
	service, store, _ := fixture(t)
	const n = 16
	results := make([]subscription.Activation, n)
	ops := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := opID
			if i%2 == 1 {
				op = "operation-other-" + string(rune('a'+i))
			}
			ops[i] = op
			results[i], errs[i] = service.Activate(context.Background(), activate(op, "a@example.com"))
		}()
	}
	wg.Wait()

	creators := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("activation %d: %v", i, errs[i])
		}
		if results[i].CanvasID != results[0].CanvasID || results[i].TenantID != results[0].TenantID {
			t.Fatalf("activation %d = %+v, want canvas of %+v", i, results[i], results[0])
		}
		if results[i].Created {
			creators[ops[i]] = true
		}
	}
	onlyTenant(t, store, streamerA)
	if len(store.Canvases()) != 1 {
		t.Fatalf("canvases = %d, want 1", len(store.Canvases()))
	}
	// Only one operation created the tenant; its replays answer created=true
	// and every other operation found the tenant already there.
	if len(creators) != 1 {
		t.Fatalf("operations answering created = %v, want exactly one", creators)
	}
}

func TestNonOperatorsCannotActivateAndLearnNothing(t *testing.T) {
	service, store, _ := fixture(t)
	for _, actor := range []access.AccountID{streamerB, stranger, "ffffffffffffffffffffffffffffffff"} {
		for _, email := range []string{"a@example.com", "nobody@example.com"} {
			request := activate(opID, email)
			request.OperatorID = actor
			_, err := service.Activate(context.Background(), request)
			if !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("%s activating %s: err = %v, want ErrAccessDenied", actor, email, err)
			}
		}
	}
	if store.AccountLookups() != 0 {
		t.Fatalf("account lookups = %d, want 0 before authorization", store.AccountLookups())
	}
	if len(store.Tenants()) != 0 || len(store.Canvases()) != 0 || len(store.Records()) != 0 {
		t.Fatal("a denied activation left state behind")
	}
}

func TestAuthorizationIsReevaluatedOnEveryActivationIncludingReplays(t *testing.T) {
	service, store, _ := fixture(t)
	if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	store.RevokeOperator(operator)

	for _, op := range []string{opID, "operation-0000002"} {
		if _, err := service.Activate(context.Background(), activate(op, "b@example.com")); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("revoked operator, %s: err = %v, want ErrAccessDenied", op, err)
		}
	}
	if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("revoked operator replay: err = %v, want ErrAccessDenied", err)
	}
	if len(store.Tenants()) != 1 {
		t.Fatalf("tenants = %d, want 1", len(store.Tenants()))
	}
}

func TestUnknownAccountIsNotFoundAndLeavesNothing(t *testing.T) {
	service, store, _ := fixture(t)
	_, err := service.Activate(context.Background(), activate(opID, "nobody@example.com"))
	if !errors.Is(err, subscription.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
	if len(store.Tenants()) != 0 || len(store.Records()) != 0 {
		t.Fatal("an unknown account left state behind")
	}
	// The operation ID was not consumed: the same ID works for a real account.
	if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); err != nil {
		t.Fatalf("reuse after not found: %v", err)
	}
}

func TestMalformedActivationsAreRejectedBeforeStorage(t *testing.T) {
	service, store, _ := fixture(t)
	cases := map[string]subscription.ActivateRequest{
		"empty email":          activate(opID, ""),
		"email without at":     activate(opID, "a.example.com"),
		"short operation ID":   activate("short", "a@example.com"),
		"operation ID symbols": activate("operation-000000!", "a@example.com"),
		"long operation ID":    activate(string(make([]byte, 129)), "a@example.com"),
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Activate(context.Background(), request); !errors.Is(err, subscription.ErrInvalidActivation) {
				t.Fatalf("err = %v, want ErrInvalidActivation", err)
			}
		})
	}
	if store.Transactions() != 0 {
		t.Fatalf("transactions = %d, want 0", store.Transactions())
	}
}

func TestAFailureAtAnyStepLeavesNoTenantCanvasOrRecord(t *testing.T) {
	steps := []string{"IsOperator", "Activation", "LockAccount", "TenantOwnedBy", "CreateTenant", "RecordActivation", "commit"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			service, store, _ := fixture(t)
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if len(store.Tenants()) != 0 || len(store.Canvases()) != 0 || len(store.Records()) != 0 {
				t.Fatal("a failed activation left state behind")
			}
			got, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
			if err != nil || !got.Created {
				t.Fatalf("retry = %+v, %v; want one created activation", got, err)
			}
			onlyTenant(t, store, streamerA)
		})
	}
}

func TestAFailedReactivationKeepsTheTenantInactive(t *testing.T) {
	service, store, _ := fixture(t)
	first, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetStatus(first.TenantID, access.SubscriptionInactive)
	boom := errors.New("injected failure")
	store.FailOnce("RecordActivation", boom)

	if _, err := service.Activate(context.Background(), activate("operation-0000002", "a@example.com")); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if tenant := onlyTenant(t, store, streamerA); tenant.Status != access.SubscriptionInactive {
		t.Fatalf("status = %s, want inactive after a failed reactivation", tenant.Status)
	}
}

func TestRetriesReplayTheSameResultWhileRetained(t *testing.T) {
	service, store, c := fixture(t)
	first, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}

	c.advance(subscription.ActivationRetention - time.Nanosecond)
	retry := activate(opID, " A@example.com")
	retry.CorrelationID = "corr-retry"
	replayed, err := service.Activate(context.Background(), retry)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed != first {
		t.Fatalf("replay = %+v, want the first answer %+v", replayed, first)
	}
	if records := store.Records(); len(records) != 1 || records[opID].CorrelationID != "corr-"+opID {
		t.Fatalf("records = %+v, want the first correlation kept", records)
	}
}

func TestAReusedOperationIDWithADifferentPayloadIsRejected(t *testing.T) {
	service, store, _ := fixture(t)
	if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	store.GrantOperator(stranger)

	otherEmail := activate(opID, "b@example.com")
	otherOperator := activate(opID, "a@example.com")
	otherOperator.OperatorID = stranger
	for name, request := range map[string]subscription.ActivateRequest{"other email": otherEmail, "other operator": otherOperator} {
		if _, err := service.Activate(context.Background(), request); !errors.Is(err, subscription.ErrOperationConflict) {
			t.Fatalf("%s: err = %v, want ErrOperationConflict", name, err)
		}
	}
	if len(store.Tenants()) != 1 {
		t.Fatalf("tenants = %d, want 1 (b was not activated)", len(store.Tenants()))
	}
}

func TestAnOperationIDStartsANewOperationAtRetentionExpiry(t *testing.T) {
	service, store, c := fixture(t)
	first, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}

	c.advance(subscription.ActivationRetention)
	got, err := service.Activate(context.Background(), activate(opID, "b@example.com"))
	if err != nil {
		t.Fatalf("expired ID reused: %v", err)
	}
	if got.AccountID != streamerB || !got.Created || got.CanvasID == first.CanvasID {
		t.Fatalf("activation = %+v", got)
	}
	records := store.Records()
	if len(records) != 1 || records[opID].Result != got {
		t.Fatalf("records = %+v, want only the new operation", records)
	}
}

func TestExpiredRecordsArePurged(t *testing.T) {
	service, store, c := fixture(t)
	if _, err := service.Activate(context.Background(), activate(opID, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	c.advance(subscription.ActivationRetention)
	if _, err := service.Activate(context.Background(), activate("operation-0000002", "b@example.com")); err != nil {
		t.Fatal(err)
	}
	if records := store.Records(); len(records) != 1 {
		t.Fatalf("records = %d, want 1 after the purge", len(records))
	}
}

func TestTwoStreamersGetDistinctCanvasesAndOnlyTheirOwn(t *testing.T) {
	service, _, _ := fixture(t)
	a, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.Activate(context.Background(), activate("operation-0000002", "b@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if a.TenantID == b.TenantID || a.CanvasID == b.CanvasID {
		t.Fatalf("streamers share a tenant or canvas: %+v %+v", a, b)
	}

	for _, c := range []struct {
		account access.AccountID
		want    subscription.Activation
	}{{streamerA, a}, {streamerB, b}} {
		key, err := service.CanvasOf(context.Background(), c.account)
		if err != nil {
			t.Fatalf("CanvasOf(%s): %v", c.account, err)
		}
		if key != (canvas.Key{TenantID: c.want.TenantID, CanvasID: c.want.CanvasID}) {
			t.Fatalf("CanvasOf(%s) = %+v, want %+v", c.account, key, c.want)
		}
	}
}

func TestCanvasOfDeniesAccountsWithoutAnActiveSubscription(t *testing.T) {
	service, store, _ := fixture(t)
	if _, err := service.CanvasOf(context.Background(), streamerA); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("never activated: err = %v, want ErrAccessDenied", err)
	}
	a, err := service.Activate(context.Background(), activate(opID, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetStatus(a.TenantID, access.SubscriptionInactive)
	if _, err := service.CanvasOf(context.Background(), streamerA); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("inactive: err = %v, want ErrAccessDenied", err)
	}
	if _, err := service.CanvasOf(context.Background(), operator); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("operator: err = %v, want ErrAccessDenied (operators get no canvas)", err)
	}
}

func TestIsOperatorReadsTheCurrentGrant(t *testing.T) {
	service, store, _ := fixture(t)
	if ok, err := service.IsOperator(context.Background(), operator); err != nil || !ok {
		t.Fatalf("operator: %v, %v", ok, err)
	}
	if ok, err := service.IsOperator(context.Background(), streamerA); err != nil || ok {
		t.Fatalf("streamer: %v, %v", ok, err)
	}
	store.RevokeOperator(operator)
	if ok, _ := service.IsOperator(context.Background(), operator); ok {
		t.Fatal("revoked operator still reported as operator")
	}
}
