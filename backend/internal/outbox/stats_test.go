package outbox_test

import (
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
)

func TestStatsAgeIsTheWaitOfTheOldestPendingRecord(t *testing.T) {
	cases := []struct {
		name  string
		stats outbox.Stats
		now   time.Time
		want  time.Duration
	}{
		{"no pending", outbox.Stats{}, t0, 0},
		{"no pending with a stale oldest", outbox.Stats{OldestPending: t0}, t0.Add(time.Hour), 0},
		{"oldest pending", outbox.Stats{Pending: 2, OldestPending: t0}, t0.Add(90 * time.Second), 90 * time.Second},
		{"created now", outbox.Stats{Pending: 1, OldestPending: t0}, t0, 0},
		{"clock behind the record", outbox.Stats{Pending: 1, OldestPending: t0}, t0.Add(-time.Second), 0},
	}
	for _, c := range cases {
		if got := c.stats.Age(c.now); got != c.want {
			t.Errorf("%s: Age = %v, want %v", c.name, got, c.want)
		}
	}
}
