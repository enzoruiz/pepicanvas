package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func TestLimitIsReachedAndRecoversAfterTheWindow(t *testing.T) {
	c := newClock()
	l := ratelimit.New(3, time.Minute, 100, c.Now)
	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("attempt %d denied within the limit", i+1)
		}
	}
	c.Advance(20 * time.Second)
	ok, retry := l.Allow("alice")
	if ok {
		t.Fatal("attempt over the limit allowed")
	}
	if retry != 40*time.Second {
		t.Fatalf("retry after = %v, want the rest of the window (40s)", retry)
	}
	c.Advance(40*time.Second - time.Nanosecond)
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("allowed before the window ended")
	}
	c.Advance(time.Nanosecond)
	if ok, _ := l.Allow("alice"); !ok {
		t.Fatal("denied after the window ended")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := ratelimit.New(1, time.Minute, 100, c.Now)
	l.Allow("alice")
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("alice over the limit allowed")
	}
	if ok, _ := l.Allow("bob"); !ok {
		t.Fatal("bob denied because of alice")
	}
}

func TestFullKeyTableFailsClosedUntilEntriesExpire(t *testing.T) {
	c := newClock()
	l := ratelimit.New(5, time.Minute, 2, c.Now)
	l.Allow("a")
	l.Allow("b")
	ok, retry := l.Allow("c")
	if ok || retry != time.Minute {
		t.Fatalf("new key with a full table = %v, %v; want denied for one window", ok, retry)
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("known key denied because the table is full")
	}
	c.Advance(time.Minute)
	if ok, _ := l.Allow("c"); !ok {
		t.Fatal("new key denied after expired entries could be evicted")
	}
}

func TestConcurrentAttemptsNeverExceedTheLimit(t *testing.T) {
	l := ratelimit.New(10, time.Minute, 100, newClock().Now)
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if ok, _ := l.Allow("alice"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed = %d, want 10", allowed)
	}
}
