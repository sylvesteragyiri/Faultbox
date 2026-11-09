package ingest

import (
	"math"
	"sync"
	"time"
)

// RateLimiter is a token bucket per project: rate events per second
// sustained, with bursts up to burst (D14).
type RateLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[int64]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows rate events/s per project with the given burst.
func NewRateLimiter(rate, burst int) *RateLimiter {
	return &RateLimiter{
		rate:    float64(rate),
		burst:   float64(burst),
		now:     time.Now,
		buckets: make(map[int64]*bucket),
	}
}

// SetClock replaces the time source. For tests.
func (l *RateLimiter) SetClock(now func() time.Time) { l.now = now }

// Allow takes one token for the project. If none is left it returns false
// and how long until the next token is available.
func (l *RateLimiter) Allow(projectID int64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[projectID]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[projectID] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}
