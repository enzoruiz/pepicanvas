package outbox

import "time"

// Stats describes the outbox records of one tenant or of every tenant.
type Stats struct {
	// Pending counts undelivered records.
	Pending int
	// OldestPending is when the oldest pending record was created; it is
	// zero when nothing is pending.
	OldestPending time.Time
	// Retrying counts pending records with at least one failed delivery.
	Retrying int
	// PendingAttempts sums the failed deliveries of pending records.
	PendingAttempts int
	// Rejected counts retained records settled as rejected.
	Rejected int
}

// Age is how long the oldest pending record has waited at now. It is zero
// when nothing is pending and never negative.
func (s Stats) Age(now time.Time) time.Duration {
	if s.Pending == 0 || s.OldestPending.IsZero() {
		return 0
	}
	return max(now.Sub(s.OldestPending), 0)
}
