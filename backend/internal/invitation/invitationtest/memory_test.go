package invitationtest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation/invitationtest"
)

// The fake refuses a limit PostgreSQL refuses, so the relay's batch size is
// checked the same way in memory.
func TestPendingCutsRefusesANonPositiveLimit(t *testing.T) {
	store := invitationtest.NewMemoryStore()
	for _, limit := range []int{0, -1} {
		if cuts, err := store.PendingCuts(context.Background(), limit); err == nil {
			t.Fatalf("PendingCuts(%d) = %v, nil; want an error", limit, cuts)
		}
	}
}

// The fake refuses a correlation longer than PostgreSQL stores, on the cut
// and on the revocation record.
func TestARevocationWithAnOverlongCorrelationIsRefused(t *testing.T) {
	store := invitationtest.NewMemoryStore()
	long := strings.Repeat("c", 129)
	for name, write := range map[string]func(invitation.Tx) error{
		"cut": func(tx invitation.Tx) error {
			return tx.RecordCut(context.Background(), invitation.RevocationCut{TenantID: "tenant", AccountID: "account", Version: 2, CorrelationID: long, CreatedAt: time.Now()})
		},
		"record": func(tx invitation.Tx) error {
			return tx.RecordMemberRevocation(context.Background(), invitation.MemberRevocationRecord{OperationID: "operation-000000000001", CorrelationID: long, AcceptedAt: time.Now()}, time.Time{})
		},
	} {
		if err := store.InTx(context.Background(), write); err == nil {
			t.Fatalf("%s with a 129-octet correlation was stored", name)
		}
	}
}
