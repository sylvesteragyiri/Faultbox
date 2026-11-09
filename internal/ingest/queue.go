package ingest

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrQueueFull means the queue is at its event or byte limit. The API
// answers 503 with Retry-After so SDKs back off (D14).
var ErrQueueFull = errors.New("ingestion queue is full")

// ErrQueueClosed means the server is shutting down.
var ErrQueueClosed = errors.New("ingestion queue is closed")

// Queue is a bounded, non-blocking FIFO between the HTTP handlers and the
// processor. It is bounded twice: by event count and by estimated bytes,
// because 1,000 events of up to 256 KB each would otherwise far exceed the
// 128 MB memory target.
type Queue struct {
	ch       chan *Event
	maxBytes int64
	bytes    atomic.Int64

	mu     sync.RWMutex
	closed bool
}

// NewQueue creates a queue holding at most capacity events and maxBytes of
// estimated event size.
func NewQueue(capacity int, maxBytes int64) *Queue {
	return &Queue{ch: make(chan *Event, capacity), maxBytes: maxBytes}
}

// Enqueue adds an event without blocking.
func (q *Queue) Enqueue(ev *Event) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrQueueClosed
	}
	if q.bytes.Add(ev.Size) > q.maxBytes {
		q.bytes.Add(-ev.Size)
		return ErrQueueFull
	}
	select {
	case q.ch <- ev:
		return nil
	default:
		q.bytes.Add(-ev.Size)
		return ErrQueueFull
	}
}

// release returns an event's bytes to the budget once it has left the queue.
func (q *Queue) release(ev *Event) { q.bytes.Add(-ev.Size) }

// Close stops new events. Queued events can still be drained.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
}

// Len is the number of queued events.
func (q *Queue) Len() int { return len(q.ch) }

// Cap is the maximum number of queued events.
func (q *Queue) Cap() int { return cap(q.ch) }

// Bytes is the estimated size of the queued events.
func (q *Queue) Bytes() int64 { return q.bytes.Load() }

// MaxBytes is the byte budget.
func (q *Queue) MaxBytes() int64 { return q.maxBytes }

// Accepting reports whether the queue is open and has room. /ready uses it.
func (q *Queue) Accepting() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return !q.closed && len(q.ch) < cap(q.ch) && q.bytes.Load() < q.maxBytes
}
