// Package ratelimit is a deterministic, in-memory fixed-window limiter for the
// single pilot instance (CTRL-RATE-01 early primitive; TE-014 owns the control).
package ratelimit

import (
	"sync"
	"time"
)

type entry struct {
	start time.Time
	count int
}

// Limiter allows at most limit attempts per key in each window. A window
// starts at a key's first attempt. The key table is bounded: when it is full
// and no entry has expired, a new key is denied, so the limiter fails closed.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	now     func() time.Time
	keys    map[string]*entry
}

// New returns a limiter. now is the time source; nil uses time.Now.
func New(limit int, window time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{limit: limit, window: window, maxKeys: maxKeys, now: now, keys: map[string]*entry{}}
}

// Allow records one attempt for key. When the attempt is denied it returns
// how long until the key's window ends.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.keys[key]
	if ok && !now.Before(w.start.Add(l.window)) {
		delete(l.keys, key)
		ok = false
	}
	if !ok {
		if len(l.keys) >= l.maxKeys {
			l.evictExpired(now)
		}
		if len(l.keys) >= l.maxKeys {
			return false, l.window
		}
		w = &entry{start: now}
		l.keys[key] = w
	}
	if w.count >= l.limit {
		return false, w.start.Add(l.window).Sub(now)
	}
	w.count++
	return true, 0
}

func (l *Limiter) evictExpired(now time.Time) {
	for key, w := range l.keys {
		if !now.Before(w.start.Add(l.window)) {
			delete(l.keys, key)
		}
	}
}
