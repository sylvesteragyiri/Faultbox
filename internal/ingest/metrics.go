package ingest

import (
	"sync"
	"sync/atomic"
	"time"
)

// Rejection reasons counted by Metrics.
const (
	RejectInvalidJSON   = "invalid_json"
	RejectValidation    = "validation_failed"
	RejectTooLarge      = "payload_too_large"
	RejectMediaType     = "unsupported_media_type"
	RejectRateLimited   = "rate_limited"
	RejectQueueFull     = "queue_full"
	RejectShuttingDown  = "shutting_down"
	rejectReasonsLength = 7
)

var rejectReasons = [rejectReasonsLength]string{
	RejectInvalidJSON, RejectValidation, RejectTooLarge, RejectMediaType,
	RejectRateLimited, RejectQueueFull, RejectShuttingDown,
}

// Metrics counts what happens to events. It is safe for concurrent use and
// exposed on the Settings page now and at /metrics in M7.
type Metrics struct {
	Accepted   atomic.Int64 // queued after validation
	Processed  atomic.Int64 // stored as a new event
	Duplicates atomic.Int64 // dropped because the event_id was already stored
	Failed     atomic.Int64 // could not be stored
	Lost       atomic.Int64 // still queued when the shutdown drain timed out
	Batches    atomic.Int64

	rejected [rejectReasonsLength]atomic.Int64

	mu           sync.Mutex
	latencyTotal time.Duration // queue wait + write, summed over processed events
	latencyMax   time.Duration
	lastBatch    time.Duration
}

// Reject counts one rejected request.
func (m *Metrics) Reject(reason string) {
	for i, r := range rejectReasons {
		if r == reason {
			m.rejected[i].Add(1)
			return
		}
	}
}

func (m *Metrics) observeBatch(write time.Duration, latencies []time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastBatch = write
	for _, l := range latencies {
		m.latencyTotal += l
		if l > m.latencyMax {
			m.latencyMax = l
		}
	}
}

// Snapshot is a point-in-time copy of the metrics.
type Snapshot struct {
	Accepted, Processed, Duplicates, Failed, Lost, Batches int64
	Rejected                                               map[string]int64
	RejectedTotal                                          int64
	QueueDepth, QueueCapacity                              int
	QueueBytes, QueueMaxBytes                              int64
	AvgLatency, MaxLatency, LastBatchWrite                 time.Duration
}

// Snapshot reads the counters together with the queue's current state.
func (m *Metrics) Snapshot(q *Queue) Snapshot {
	s := Snapshot{
		Accepted:   m.Accepted.Load(),
		Processed:  m.Processed.Load(),
		Duplicates: m.Duplicates.Load(),
		Failed:     m.Failed.Load(),
		Lost:       m.Lost.Load(),
		Batches:    m.Batches.Load(),
		Rejected:   make(map[string]int64, rejectReasonsLength),
	}
	for i, r := range rejectReasons {
		n := m.rejected[i].Load()
		s.Rejected[r] = n
		s.RejectedTotal += n
	}
	if q != nil {
		s.QueueDepth, s.QueueCapacity = q.Len(), q.Cap()
		s.QueueBytes, s.QueueMaxBytes = q.Bytes(), q.MaxBytes()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.MaxLatency, s.LastBatchWrite = m.latencyMax, m.lastBatch
	if done := s.Processed + s.Duplicates; done > 0 {
		s.AvgLatency = m.latencyTotal / time.Duration(done)
	}
	return s
}
