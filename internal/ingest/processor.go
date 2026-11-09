package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/grouping"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Batching defaults (D14).
const (
	DefaultBatchSize  = 100
	DefaultBatchDelay = 250 * time.Millisecond
	writeTimeout      = 10 * time.Second
)

// ResultHook receives what each stored batch did. M6 notifications plug in here.
type ResultHook func([]store.IngestResult)

// Processor drains the queue into the database in short batched
// transactions on a single goroutine, so it never competes with itself for
// SQLite's write lock.
type Processor struct {
	queue      *Queue
	store      *store.Store
	metrics    *Metrics
	logger     *slog.Logger
	batchSize  int
	batchDelay time.Duration
	onResults  ResultHook

	flushReq chan chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
}

// ProcessorOptions configures a Processor.
type ProcessorOptions struct {
	BatchSize  int
	BatchDelay time.Duration
	OnResults  ResultHook
}

// NewProcessor builds a processor; call Start to run it.
func NewProcessor(q *Queue, st *store.Store, m *Metrics, logger *slog.Logger, opts ProcessorOptions) *Processor {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.BatchDelay <= 0 {
		opts.BatchDelay = DefaultBatchDelay
	}
	return &Processor{
		queue: q, store: st, metrics: m, logger: logger,
		batchSize: opts.BatchSize, batchDelay: opts.BatchDelay, onResults: opts.OnResults,
		flushReq: make(chan chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start runs the processing loop in the background.
func (p *Processor) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.run(ctx)
}

// Flush blocks until every event queued before the call has been written.
// Tests use it instead of sleeping.
func (p *Processor) Flush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case p.flushReq <- ack:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown closes the queue and drains what is left. If ctx expires first,
// the remaining events are counted as lost (D14: 10 s drain by default).
func (p *Processor) Shutdown(ctx context.Context) error {
	p.queue.Close()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		p.cancel()
		<-p.done
		lost := int64(p.queue.Len())
		for ev := range p.queue.ch {
			p.queue.release(ev)
		}
		p.metrics.Lost.Add(lost)
		p.logger.Error("shutdown drain timed out; queued events were lost", "lost", lost)
		return ctx.Err()
	}
}

func (p *Processor) run(ctx context.Context) {
	defer close(p.done)
	batch := make([]*Event, 0, p.batchSize)
	enqueued := make([]time.Time, 0, p.batchSize)
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.write(ctx, batch, enqueued)
		clear(batch)
		batch, enqueued = batch[:0], enqueued[:0]
	}
	add := func(ev *Event) {
		if len(batch) == 0 {
			timer.Reset(p.batchDelay)
		}
		batch = append(batch, ev)
		enqueued = append(enqueued, time.Now())
		p.queue.release(ev)
		if len(batch) >= p.batchSize {
			timer.Stop()
			flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			p.metrics.Lost.Add(int64(len(batch)))
			return
		case ev, ok := <-p.queue.ch:
			if !ok {
				timer.Stop()
				flush()
				return
			}
			add(ev)
		case <-timer.C:
			flush()
		case ack := <-p.flushReq:
			// Take everything already queued, then write it.
		drain:
			for {
				select {
				case ev, ok := <-p.queue.ch:
					if !ok {
						break drain
					}
					add(ev)
				default:
					break drain
				}
			}
			timer.Stop()
			flush()
			close(ack)
		}
	}
}

// write stores a batch in one transaction. If that fails, it retries each
// event on its own, so one bad event can't take the rest of the batch with it.
func (p *Processor) write(ctx context.Context, batch []*Event, enqueued []time.Time) {
	events := make([]store.NewEvent, 0, len(batch))
	for _, ev := range batch {
		events = append(events, p.prepare(ev))
	}

	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	results, err := p.store.IngestBatch(wctx, events)
	cancel()
	if err != nil {
		p.logger.Warn("batch write failed; retrying events individually", "events", len(events), "error", err)
		results = results[:0]
		for i := range events {
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			r, err := p.store.IngestBatch(wctx, events[i:i+1])
			cancel()
			if err != nil {
				p.metrics.Failed.Add(1)
				p.logger.Error("event could not be stored", "event_id", events[i].UUID, "project_id", events[i].ProjectID, "error", err)
				continue
			}
			results = append(results, r...)
		}
	}
	elapsed := time.Since(start)

	p.metrics.Batches.Add(1)
	latencies := make([]time.Duration, 0, len(results))
	byUUID := make(map[string]time.Time, len(batch))
	for i, ev := range batch {
		byUUID[ev.UUID] = enqueued[i]
	}
	for _, r := range results {
		if r.Duplicate {
			p.metrics.Duplicates.Add(1)
		} else {
			p.metrics.Processed.Add(1)
		}
		if t, ok := byUUID[r.UUID]; ok {
			latencies = append(latencies, time.Since(t))
		}
	}
	p.metrics.observeBatch(elapsed, latencies)

	if p.onResults != nil && len(results) > 0 {
		p.onResults(results)
	}
}

// prepare turns a validated event into a database row: fingerprint,
// culprit, scrubbed JSON blobs (PRD §4.3, §8).
func (p *Processor) prepare(ev *Event) store.NewEvent {
	fp := grouping.Fingerprint(ev.ExceptionType, ev.Message, ev.Frames)
	if ev.Job != nil {
		fp = grouping.QueueFingerprint(ev.Job.Name, fp)
	}

	frames := ev.Frames
	if frames == nil {
		frames = []grouping.Frame{}
	}
	stack := mustJSON(struct {
		Frames        []grouping.Frame `json:"frames"`
		FramesOmitted int              `json:"frames_omitted,omitempty"`
	}{frames, ev.FramesOmitted})

	ne := store.NewEvent{
		ProjectID:     ev.ProjectID,
		UUID:          ev.UUID,
		Category:      ev.Kind,
		Level:         ev.Level,
		Environment:   ev.Environment,
		Release:       ev.Release,
		Fingerprint:   fp,
		ExceptionType: ev.ExceptionType,
		Message:       ev.Message,
		Culprit:       grouping.Culprit(ev.Frames),
		Stacktrace:    stack,
		CorrelationID: ev.CorrelationID,
		OccurredAt:    ev.OccurredAt,
	}

	sample := map[string]json.RawMessage{}
	if r := ev.Request; r != nil {
		r.Headers = ScrubHeaders(r.Headers)
		ne.RequestURL, ne.HTTPMethod = r.URL, r.Method
		if r.StatusCode != nil {
			ne.HTTPStatus = *r.StatusCode
		}
		ne.RequestContext = mustJSON(r)
		sample["request"] = json.RawMessage(ne.RequestContext)
	}
	ne.UserContext = p.scrub(ev, ev.User)
	ne.EventContext = p.scrub(ev, ev.Context)
	if ne.UserContext != "" {
		sample["user"] = json.RawMessage(ne.UserContext)
	}
	if ne.EventContext != "" {
		sample["context"] = json.RawMessage(ne.EventContext)
	}

	if j := ev.Job; j != nil {
		ne.Job = &store.NewQueueFailure{
			Kind: j.Kind, Name: j.Name, ID: j.ID, Queue: j.Queue, Connection: j.Connection,
			Attempts: j.Attempts, MaxTries: j.MaxTries, BatchID: j.BatchID,
			Payload: p.scrub(ev, j.Payload),
		}
		j.Payload = json.RawMessage(nullIfEmpty(ne.Job.Payload))
		sample["job"] = json.RawMessage(mustJSON(j))
	}
	ne.SampleContext = mustJSON(sample)
	return ne
}

func (p *Processor) scrub(ev *Event, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	out, err := ScrubJSON(raw)
	if err != nil {
		// Validation already checked the JSON, so this should not happen;
		// drop the blob rather than store it unscrubbed.
		p.logger.Warn("could not scrub event data; dropping it", "event_id", ev.UUID, "error", err)
		return ""
	}
	return string(out)
}

func nullIfEmpty(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
