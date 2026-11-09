package auth

import (
	"sync"
	"time"
)

// FailureLimiter counts failures per key in a fixed window, for example
// failed logins per IP address and per email (D9). It is safe for
// concurrent use and its memory is bounded by maxKeys.
type FailureLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	now     func() time.Time
	entries map[string]*failureEntry
}

type failureEntry struct {
	count int
	start time.Time
}

// NewFailureLimiter allows up to max failures per key per window.
func NewFailureLimiter(maxFailures int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{
		max:     maxFailures,
		window:  window,
		maxKeys: 10_000,
		now:     time.Now,
		entries: make(map[string]*failureEntry),
	}
}

// SetClock replaces the time source. For tests.
func (l *FailureLimiter) SetClock(now func() time.Time) { l.now = now }

// Blocked reports whether key has used up its failures in the current window.
func (l *FailureLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return false
	}
	if l.now().Sub(e.start) >= l.window {
		delete(l.entries, key)
		return false
	}
	return e.count >= l.max
}

// Fail records one failure for key.
func (l *FailureLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) >= l.window {
		if !ok && len(l.entries) >= l.maxKeys {
			l.pruneLocked(now)
		}
		l.entries[key] = &failureEntry{count: 1, start: now}
		return
	}
	e.count++
}

// Reset clears the failures for key, for example after a successful login.
func (l *FailureLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// pruneLocked drops expired entries. If every entry is still live, it drops
// them all rather than grow without bound: under a flood of distinct keys
// this weakens limiting briefly but keeps memory flat.
func (l *FailureLimiter) pruneLocked(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, k)
		}
	}
	if len(l.entries) >= l.maxKeys {
		clear(l.entries)
	}
}
