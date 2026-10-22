package subscription_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription/subscriptiontest"
)

const deactivateOp = "deactivation-00001"

func deactivate(op, email string) subscription.DeactivateRequest {
	return subscription.DeactivateRequest{OperatorID: operator, OperationID: op, Email: email, CorrelationID: "corr-" + op}
}

// activated activates a@example.com and b@example.com.
func activated(t *testing.T) (*subscription.Service, *subscriptiontest.MemoryStore, *clock, subscription.Activation, subscription.Activation) {
	t.Helper()
	service, store, c := fixture(t)
	a, err := service.Activate(context.Background(), activate("activation-a-0001", "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.Activate(context.Background(), activate("activation-b-0001", "b@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	return service, store, c, a, b
}

// snapshot is every fact a deactivation can change.
type snapshot struct {
	tenants       map[string]subscription.Tenant
	cuts          []subscription.PendingCut
	deactivations map[string]subscription.DeactivationRecord
	activations   int
}

func snap(store *subscriptiontest.MemoryStore) snapshot {
	tenants := map[string]subscription.Tenant{}
	for _, tenant := range store.Tenants() {
		tenants[tenant.ID] = tenant
	}
	return snapshot{tenants: tenants, cuts: store.Cuts(), deactivations: store.Deactivations(), activations: len(store.Records())}
}

func requireUnchanged(t *testing.T, store *subscriptiontest.MemoryStore, before snapshot) {
	t.Helper()
	if after := snap(store); !reflect.DeepEqual(after, before) {
		t.Fatalf("state changed:\n got %+v\nwant %+v", after, before)
	}
}

func TestDeactivationCutsAnActiveTenantInOneCommit(t *testing.T) {
	service, store, c, a, b := activated(t)

	got, err := service.Deactivate(context.Background(), deactivate(deactivateOp, " A@Example.com "))
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	want := subscription.Deactivation{AccountID: streamerA, TenantID: a.TenantID, CanvasID: a.CanvasID, Deactivated: true, Version: 2}
	if got != want {
		t.Fatalf("deactivation = %+v, want %+v", got, want)
	}
	tenant := onlyTenant(t, store, streamerA)
	if tenant.Status != access.SubscriptionInactive || tenant.Version != 2 || tenant.CanvasID != a.CanvasID {
		t.Fatalf("tenant = %+v, want inactive at version 2 with its canvas", tenant)
	}
	wantCuts := []subscription.PendingCut{{ID: 1, AccessCut: subscription.AccessCut{
		TenantID: a.TenantID, Version: 2, CorrelationID: "corr-" + deactivateOp, CreatedAt: c.now(),
	}}}
	if cuts := store.Cuts(); !reflect.DeepEqual(cuts, wantCuts) {
		t.Fatalf("cuts = %+v, want %+v", cuts, wantCuts)
	}
	records := store.Deactivations()
	record := records[deactivateOp]
	if len(records) != 1 || record.Result != want || record.OperatorID != operator || record.CorrelationID != "corr-"+deactivateOp || !record.AcceptedAt.Equal(c.now()) {
		t.Fatalf("records = %+v, want one with the answer, operator, and correlation", records)
	}
	if other := onlyTenant(t, store, streamerB); other.Status != access.SubscriptionActive || other.Version != 1 || other.ID != b.TenantID {
		t.Fatalf("tenant B = %+v, want it active at version 1", other)
	}
	if pending, err := store.PendingCuts(context.Background(), 10); err != nil || !reflect.DeepEqual(pending, wantCuts) {
		t.Fatalf("PendingCuts = %+v, %v; want %+v", pending, err, wantCuts)
	}
}

func TestDeactivatingAnInactiveTenantChangesNothing(t *testing.T) {
	service, store, _, a, _ := activated(t)
	first, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	before := snap(store)

	again, err := service.Deactivate(context.Background(), deactivate("deactivation-00002", "a@example.com"))
	if err != nil {
		t.Fatalf("second Deactivate: %v", err)
	}
	want := subscription.Deactivation{AccountID: streamerA, TenantID: a.TenantID, CanvasID: a.CanvasID, Deactivated: false, Version: first.Version}
	if again != want {
		t.Fatalf("second deactivation = %+v, want %+v", again, want)
	}
	if after := snap(store); !reflect.DeepEqual(after.tenants, before.tenants) || !reflect.DeepEqual(after.cuts, before.cuts) {
		t.Fatalf("a second deactivation changed the tenant or wrote a cut: %+v", after)
	}
	if records := store.Deactivations(); len(records) != 2 || records["deactivation-00002"].Result != want {
		t.Fatalf("records = %+v, want the second answer retained too", records)
	}
}

func TestADeactivationRetryReplaysTheFirstAnswerEvenAfterReactivation(t *testing.T) {
	service, store, c, a, _ := activated(t)
	first, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Activate(context.Background(), activate("activation-a-0002", "a@example.com")); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	before := snap(store)

	c.advance(subscription.ActivationRetention - time.Nanosecond)
	retry := deactivate(deactivateOp, "A@EXAMPLE.COM ")
	retry.CorrelationID = "corr-retry"
	replayed, err := service.Deactivate(context.Background(), retry)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed != first {
		t.Fatalf("replay = %+v, want the first answer %+v", replayed, first)
	}
	// The replay has no effect: the reactivated tenant stays active.
	requireUnchanged(t, store, before)
	if tenant := onlyTenant(t, store, streamerA); tenant.Status != access.SubscriptionActive || tenant.ID != a.TenantID {
		t.Fatalf("tenant = %+v, want it still active", tenant)
	}
}

func TestAReusedDeactivationIDWithADifferentPayloadIsRejected(t *testing.T) {
	service, store, _, _, _ := activated(t)
	if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	store.GrantOperator(stranger)
	before := snap(store)

	otherEmail := deactivate(deactivateOp, "b@example.com")
	otherOperator := deactivate(deactivateOp, "a@example.com")
	otherOperator.OperatorID = stranger
	for name, request := range map[string]subscription.DeactivateRequest{"other email": otherEmail, "other operator": otherOperator} {
		if _, err := service.Deactivate(context.Background(), request); !errors.Is(err, subscription.ErrOperationConflict) {
			t.Fatalf("%s: err = %v, want ErrOperationConflict", name, err)
		}
	}
	requireUnchanged(t, store, before)
}

// An activation and a deactivation never share an operation answer: the
// deactivation fingerprint covers the action.
func TestAnActivationIDDoesNotReplayAsADeactivation(t *testing.T) {
	service, store, _, a, _ := activated(t)
	got, err := service.Deactivate(context.Background(), deactivate("activation-a-0001", "a@example.com"))
	if err != nil {
		t.Fatalf("Deactivate under an activation ID: %v", err)
	}
	if !got.Deactivated || got.TenantID != a.TenantID {
		t.Fatalf("deactivation = %+v, want a real cut", got)
	}
	if len(store.Cuts()) != 1 {
		t.Fatalf("cuts = %d, want 1", len(store.Cuts()))
	}
}

func TestADeactivationIDStartsANewOperationAtRetentionExpiry(t *testing.T) {
	service, store, c, _, b := activated(t)
	if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); err != nil {
		t.Fatal(err)
	}

	c.advance(subscription.ActivationRetention)
	got, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "b@example.com"))
	if err != nil {
		t.Fatalf("expired ID reused: %v", err)
	}
	if got.AccountID != streamerB || got.TenantID != b.TenantID || !got.Deactivated || got.Version != 2 {
		t.Fatalf("deactivation = %+v, want B cut", got)
	}
	records := store.Deactivations()
	if len(records) != 1 || records[deactivateOp].Result != got {
		t.Fatalf("records = %+v, want only the new operation", records)
	}
	if n := len(store.Cuts()); n != 2 {
		t.Fatalf("cuts = %d, want 2", n)
	}
}

func TestOnlyOperatorsDeactivateAndOthersLearnNothing(t *testing.T) {
	service, store, _, _, _ := activated(t)
	before := snap(store)
	lookups := store.AccountLookups()
	for _, actor := range []access.AccountID{streamerA, stranger, "ffffffffffffffffffffffffffffffff"} {
		for _, email := range []string{"a@example.com", "nobody@example.com", "stranger@example.com"} {
			request := deactivate(deactivateOp, email)
			request.OperatorID = actor
			if _, err := service.Deactivate(context.Background(), request); !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("%s deactivating %s: err = %v, want ErrAccessDenied", actor, email, err)
			}
		}
	}
	if n := store.AccountLookups() - lookups; n != 0 {
		t.Fatalf("account lookups = %d, want 0 before authorization", n)
	}
	requireUnchanged(t, store, before)
}

func TestARevokedOperatorCannotDeactivateOrReplay(t *testing.T) {
	service, store, _, _, _ := activated(t)
	if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	store.RevokeOperator(operator)
	before := snap(store)
	lookups := store.AccountLookups()

	for _, request := range []subscription.DeactivateRequest{deactivate(deactivateOp, "a@example.com"), deactivate("deactivation-00002", "b@example.com")} {
		if _, err := service.Deactivate(context.Background(), request); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("revoked operator, %s: err = %v, want ErrAccessDenied", request.OperationID, err)
		}
	}
	if n := store.AccountLookups() - lookups; n != 0 {
		t.Fatalf("account lookups = %d, want 0", n)
	}
	requireUnchanged(t, store, before)
}

func TestDeactivatingAnUnknownOrNeverActivatedAccountIsNotFound(t *testing.T) {
	service, store, _, _, _ := activated(t)
	before := snap(store)
	for _, email := range []string{"nobody@example.com", "stranger@example.com"} {
		if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, email)); !errors.Is(err, subscription.ErrAccountNotFound) {
			t.Fatalf("%s: err = %v, want ErrAccountNotFound", email, err)
		}
	}
	requireUnchanged(t, store, before)
	// The operation ID was not consumed.
	if got, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); err != nil || !got.Deactivated {
		t.Fatalf("reuse after not found = %+v, %v", got, err)
	}
}

func TestMalformedDeactivationsAreRejectedBeforeStorage(t *testing.T) {
	service, store, _ := fixture(t)
	cases := map[string]subscription.DeactivateRequest{
		"empty email":          deactivate(deactivateOp, ""),
		"email without at":     deactivate(deactivateOp, "a.example.com"),
		"short operation ID":   deactivate("short", "a@example.com"),
		"operation ID symbols": deactivate("deactivation-0000!", "a@example.com"),
		"long operation ID":    deactivate(string(make([]byte, 129)), "a@example.com"),
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Deactivate(context.Background(), request); !errors.Is(err, subscription.ErrInvalidDeactivation) {
				t.Fatalf("err = %v, want ErrInvalidDeactivation", err)
			}
		})
	}
	if store.Transactions() != 0 {
		t.Fatalf("transactions = %d, want 0", store.Transactions())
	}
}

func TestAFailureAtAnyDeactivationStepLeavesNothing(t *testing.T) {
	steps := []string{"IsOperator", "Deactivation", "LockAccount", "TenantOwnedBy", "DeactivateTenant", "RecordCut", "RecordDeactivation", "commit"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			service, store, _, a, _ := activated(t)
			before := snap(store)
			boom := errors.New("injected failure")
			store.FailOnce(step, boom)

			if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			requireUnchanged(t, store, before)
			got, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com"))
			if err != nil || !got.Deactivated || got.Version != 2 || got.TenantID != a.TenantID {
				t.Fatalf("retry = %+v, %v; want one cut at version 2", got, err)
			}
			if cuts := store.Cuts(); len(cuts) != 1 || cuts[0].ID != 1 || cuts[0].Version != 2 {
				t.Fatalf("cuts = %+v, want one", cuts)
			}
		})
	}
}

func TestReactivationRestoresTheSameCanvasAtTheNewVersion(t *testing.T) {
	service, store, _, a, _ := activated(t)
	ctx := context.Background()
	key := canvas.Key{TenantID: a.TenantID, CanvasID: a.CanvasID}
	if own, err := service.CanvasOf(ctx, streamerA); err != nil || own != key {
		t.Fatalf("CanvasOf before = %+v, %v", own, err)
	}
	if _, err := service.Deactivate(ctx, deactivate(deactivateOp, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CanvasOf(ctx, streamerA); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("CanvasOf while inactive: err = %v, want ErrAccessDenied", err)
	}

	again, err := service.Activate(ctx, activate("activation-a-0002", "a@example.com"))
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if again.TenantID != a.TenantID || again.CanvasID != a.CanvasID || again.Created {
		t.Fatalf("reactivation = %+v, want the tenant and canvas of %+v", again, a)
	}
	tenant := onlyTenant(t, store, streamerA)
	if tenant.Status != access.SubscriptionActive || tenant.Version != 2 {
		t.Fatalf("tenant = %+v, want active at the deactivation's version 2", tenant)
	}
	if n := len(store.Canvases()); n != 2 {
		t.Fatalf("canvases = %d, want 2 (A and B)", n)
	}
	if n := len(store.Cuts()); n != 1 {
		t.Fatalf("cuts = %d, want only the deactivation's", n)
	}
	if own, err := service.CanvasOf(ctx, streamerA); err != nil || own != key {
		t.Fatalf("CanvasOf after reactivation = %+v, %v; want %+v", own, err, key)
	}
}

func TestEditorGrantFollowsTheTenantAccessVersion(t *testing.T) {
	service, _, _, a, b := activated(t)
	ctx := context.Background()

	before, err := service.EditorGrant(ctx, streamerA, "session-a")
	want := revocation.Grant{TenantID: a.TenantID, AccountID: string(streamerA), SessionID: "session-a", Client: revocation.ClientEditor, Version: 1}
	if err != nil || before != want {
		t.Fatalf("EditorGrant before = %+v, %v; want %+v", before, err, want)
	}
	if _, err := service.Deactivate(ctx, deactivate(deactivateOp, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EditorGrant(ctx, streamerA, "session-a"); !errors.Is(err, access.ErrAccessDenied) {
		t.Fatalf("EditorGrant while inactive: err = %v, want ErrAccessDenied", err)
	}
	// Tenant B keeps its grant.
	if grant, err := service.EditorGrant(ctx, streamerB, "session-b"); err != nil || grant.TenantID != b.TenantID || grant.Version != 1 {
		t.Fatalf("EditorGrant(B) = %+v, %v; want B at version 1", grant, err)
	}

	if _, err := service.Activate(ctx, activate("activation-a-0002", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	after, err := service.EditorGrant(ctx, streamerA, "session-a")
	if err != nil || after.TenantID != a.TenantID || after.Version != 2 || after.Version <= before.Version {
		t.Fatalf("EditorGrant after reactivation = %+v, %v; want version 2", after, err)
	}
	for _, account := range []access.AccountID{operator, stranger, "ffffffffffffffffffffffffffffffff"} {
		if _, err := service.EditorGrant(ctx, account, "session-x"); !errors.Is(err, access.ErrAccessDenied) {
			t.Fatalf("EditorGrant(%s): err = %v, want ErrAccessDenied", account, err)
		}
	}
}

// A concurrent deactivation that commits the same operation ID between this
// deactivation's record lookup and its record insert is replayed.
func TestAConcurrentCommitOfTheSameDeactivationIDIsReplayed(t *testing.T) {
	service, store, _, a, _ := activated(t)
	transactions := store.Transactions()
	var winner subscription.Deactivation
	store.BeforeRecordOnce(func() {
		var err error
		if winner, err = service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); err != nil {
			t.Errorf("concurrent Deactivate: %v", err)
		}
	})

	got, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "A@example.com"))
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if got != winner || !got.Deactivated || got.TenantID != a.TenantID {
		t.Fatalf("replay = %+v, want the committed answer %+v", got, winner)
	}
	if cuts := store.Cuts(); len(cuts) != 1 {
		t.Fatalf("cuts = %+v, want one", cuts)
	}
	if records := store.Deactivations(); len(records) != 1 || records[deactivateOp].Result != got {
		t.Fatalf("records = %+v", records)
	}
	if n := store.Transactions() - transactions; n != 3 {
		t.Fatalf("transactions = %d, want 3", n)
	}
}

func TestAConcurrentCommitOfTheSameDeactivationIDWithAnotherPayloadIsAConflict(t *testing.T) {
	service, store, _, a, b := activated(t)
	store.BeforeRecordOnce(func() {
		if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "b@example.com")); err != nil {
			t.Errorf("concurrent Deactivate: %v", err)
		}
	})

	if _, err := service.Deactivate(context.Background(), deactivate(deactivateOp, "a@example.com")); !errors.Is(err, subscription.ErrOperationConflict) {
		t.Fatalf("err = %v, want ErrOperationConflict", err)
	}
	if tenant := onlyTenant(t, store, streamerA); tenant.ID != a.TenantID || tenant.Status != access.SubscriptionActive || tenant.Version != 1 {
		t.Fatalf("tenant A = %+v, want untouched", tenant)
	}
	if cuts := store.Cuts(); len(cuts) != 1 || cuts[0].TenantID != b.TenantID {
		t.Fatalf("cuts = %+v, want only B's", cuts)
	}
}

func TestPendingCutsAreReadOldestFirstAndSettled(t *testing.T) {
	service, store, _, a, b := activated(t)
	ctx := context.Background()
	if _, err := service.Deactivate(ctx, deactivate("deactivation-a-01", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deactivate(ctx, deactivate("deactivation-b-01", "b@example.com")); err != nil {
		t.Fatal(err)
	}
	first, err := store.PendingCuts(ctx, 1)
	if err != nil || len(first) != 1 || first[0].TenantID != a.TenantID {
		t.Fatalf("PendingCuts(1) = %+v, %v; want A's cut", first, err)
	}
	if err := store.SettleCut(ctx, first[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	rest, err := store.PendingCuts(ctx, 10)
	if err != nil || len(rest) != 1 || rest[0].TenantID != b.TenantID {
		t.Fatalf("PendingCuts after settle = %+v, %v; want B's cut", rest, err)
	}
}
