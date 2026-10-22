// Package ratelimit is a deterministic, in-memory fixed-window limiter for the
// single pilot instance (CTRL-RATE-01 early primitive; TE-014 owns the control).
package ratelimit

import (
	"sync"
	"time"
)

// Rate is a number of attempts allowed per window.
type Rate struct {
	Limit  int
	Window time.Duration
}

// Result is the outcome of one attempt. Full reports that the attempt was
// denied because the key table is full, not because the key is over its limit.
type Result struct {
	Allowed    bool
	RetryAfter time.Duration
	Full       bool
}

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
	res := l.Check(key)
	return res.Allowed, res.RetryAfter
}

// Check records one attempt for key like Allow and also reports whether a
// denial came from a full key table.
func (l *Limiter) Check(key string) Result {
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
			return Result{RetryAfter: l.window, Full: true}
		}
		w = &entry{start: now}
		l.keys[key] = w
	}
	if w.count >= l.limit {
		return Result{RetryAfter: w.start.Add(l.window).Sub(now)}
	}
	w.count++
	return Result{Allowed: true}
}

func (l *Limiter) evictExpired(now time.Time) {
	for key, w := range l.keys {
		if !now.Before(w.start.Add(l.window)) {
			delete(l.keys, key)
		}
	}
}
