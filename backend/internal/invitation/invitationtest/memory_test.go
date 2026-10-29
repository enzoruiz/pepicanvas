package invitationtest_test

import (
	"context"
	"testing"

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
