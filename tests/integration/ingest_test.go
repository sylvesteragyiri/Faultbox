package integration

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/api"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func newEvent(projectID int64, fingerprint string, at time.Time) store.NewEvent {
	return store.NewEvent{
		ProjectID: projectID, UUID: testutil.NewEventID(), Category: "exception", Level: "error",
		Environment: "production", Fingerprint: fingerprint, ExceptionType: "RuntimeException",
		Message: "boom", Stacktrace: `{"frames":[]}`, SampleContext: `{}`, OccurredAt: at,
	}
}

func TestIngestBatchGroupsAndCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "App")
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	var batch []store.NewEvent
	for i := range 5 {
		e := newEvent(p.ID, "fp-a", t0.Add(time.Duration(i)*time.Minute))
		e.Release = []string{"1.0", "1.0", "1.1", "1.1", "1.1"}[i]
		batch = append(batch, e)
	}
	batch = append(batch, newEvent(p.ID, "fp-b", t0))

	results, err := st.IngestBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].NewIssue || results[1].NewIssue || results[4].Count != 5 || !results[5].NewIssue {
		t.Fatalf("results = %+v", results)
	}

	issues, err := st.ProjectIssues(ctx, p.ID, 10)
	if err != nil || len(issues) != 2 {
		t.Fatalf("issues = %d, %v; want 2", len(issues), err)
	}
	a, _ := st.Issue(ctx, results[0].IssueID)
	if a.OccurrenceCount != 5 || !a.FirstSeenAt.Equal(t0) || !a.LastSeenAt.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("issue a: count %d, first %v, last %v", a.OccurrenceCount, a.FirstSeenAt, a.LastSeenAt)
	}
	if a.FirstRelease != "1.0" || a.LastRelease != "1.1" {
		t.Fatalf("releases: first %q, last %q", a.FirstRelease, a.LastRelease)
	}
	if events, _ := st.IssueEvents(ctx, a.ID, 10); len(events) != 5 {
		t.Fatalf("issue a has %d events, want 5", len(events))
	}
	rel, _ := st.IssueReleases(ctx, a.ID)
	if rel["1.0"] != 2 || rel["1.1"] != 3 {
		t.Fatalf("release counts = %v", rel)
	}
	days, _ := st.IssueDailyCounts(ctx, a.ID)
	var total int64
	for _, n := range days {
		total += n
	}
	if total != 5 {
		t.Fatalf("daily counts sum to %d, want 5", total)
	}
	if got, _ := st.Project(ctx, p.ID); !got.LastEventAt.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("project last_event_at = %v", got.LastEventAt)
	}
}

func TestIngestBatchSkipsDuplicateEventIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "App")

	e := newEvent(p.ID, "fp", time.Now())
	first, err := st.IngestBatch(ctx, []store.NewEvent{e, e}) // duplicate inside one batch
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.IngestBatch(ctx, []store.NewEvent{e}) // and across batches
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Duplicate || !first[1].Duplicate || !again[0].Duplicate {
		t.Fatalf("duplicate flags: %+v %+v", first, again)
	}
	issue, _ := st.Issue(ctx, first[0].IssueID)
	if issue.OccurrenceCount != 1 {
		t.Fatalf("count = %d, want 1", issue.OccurrenceCount)
	}

	// The same event_id in another project is a different event.
	other, _ := testutil.NewProjectKey(t, st, "Other")
	e.ProjectID = other.ID
	r, err := st.IngestBatch(ctx, []store.NewEvent{e})
	if err != nil || r[0].Duplicate {
		t.Fatalf("other project: %+v, %v", r, err)
	}
}

func TestResolvedIssueReopensButIgnoredStaysIgnored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "App")

	r, _ := st.IngestBatch(ctx, []store.NewEvent{newEvent(p.ID, "fp-r", time.Now())})
	resolvedID := r[0].IssueID
	if err := st.SetIssueStatus(ctx, resolvedID, store.IssueResolved); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Issue(ctx, resolvedID); got.ResolvedAt.IsZero() {
		t.Fatal("resolved_at not set")
	}

	again := newEvent(p.ID, "fp-r", time.Now())
	again.Stacktrace = `{"frames":[{"function":"regressed"}]}`
	r, _ = st.IngestBatch(ctx, []store.NewEvent{again})
	if !r[0].Reopened || r[0].Status != store.IssueUnresolved {
		t.Fatalf("regression result = %+v", r[0])
	}
	got, _ := st.Issue(ctx, resolvedID)
	if got.Status != store.IssueUnresolved || !got.ResolvedAt.IsZero() || got.SampleEventUUID != again.UUID ||
		!strings.Contains(got.SampleStacktrace, "regressed") {
		t.Fatalf("after regression: %+v", got)
	}

	r, _ = st.IngestBatch(ctx, []store.NewEvent{newEvent(p.ID, "fp-i", time.Now())})
	ignoredID := r[0].IssueID
	_ = st.SetIssueStatus(ctx, ignoredID, store.IssueIgnored)
	r, _ = st.IngestBatch(ctx, []store.NewEvent{newEvent(p.ID, "fp-i", time.Now())})
	if r[0].Reopened || r[0].Status != store.IssueIgnored {
		t.Fatalf("ignored issue result = %+v", r[0])
	}
	if got, _ := st.Issue(ctx, ignoredID); got.Status != store.IssueIgnored || got.OccurrenceCount != 2 {
		t.Fatalf("ignored issue: %+v", got)
	}
}

func TestQueueFailureRowsAreStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Workers")

	attempts := 3
	e := newEvent(p.ID, "fp-job", time.Now())
	e.Category = "queue_failure"
	e.Job = &store.NewQueueFailure{Kind: "terminal", Name: "App\\Jobs\\Sync", Queue: "default", Connection: "redis", Attempts: &attempts}
	r, err := st.IngestBatch(ctx, []store.NewEvent{e})
	if err != nil {
		t.Fatal(err)
	}
	qf, err := st.IssueQueueFailures(ctx, r[0].IssueID, 10)
	if err != nil || len(qf) != 1 || qf[0].JobName != "App\\Jobs\\Sync" || qf[0].Attempts != 3 || qf[0].Queue != "default" {
		t.Fatalf("queue failures = %+v, %v", qf, err)
	}
	issue, _ := st.Issue(ctx, r[0].IssueID)
	if issue.Category != "queue_failure" || issue.JobName != "App\\Jobs\\Sync" {
		t.Fatalf("issue = %+v", issue)
	}
	if stats, _ := st.Stats(ctx, 0); stats.QueueFailures != 1 || stats.Unresolved != 1 || stats.NewToday != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

// ---- processor ----

func validatedEvent(t *testing.T, projectID int64, fixture string) *ingest.Event {
	t.Helper()
	p := testutil.Stamp(testutil.Fixture(t, fixture))
	payload, err := ingest.Decode(bytes.NewReader(testutil.MustJSON(t, p)))
	if err != nil {
		t.Fatal(err)
	}
	kind := ingest.KindException
	if _, ok := p["job"]; ok {
		kind = ingest.KindQueueFailure
	}
	ev, err := ingest.Validate(payload, kind, projectID, nil, time.Now(), 2000)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestProcessorBatchesAndScrubs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "App")

	q := ingest.NewQueue(500, 32<<20)
	m := &ingest.Metrics{}
	var hooked int
	proc := ingest.NewProcessor(q, st, m, testutil.Logger(t), ingest.ProcessorOptions{
		BatchSize: 50, OnResults: func(r []store.IngestResult) { hooked += len(r) },
	})
	proc.Start()

	const n = 120
	for range n {
		if err := q.Enqueue(validatedEvent(t, p.ID, "unique_violation")); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Enqueue(validatedEvent(t, p.ID, "job_timeout")); err != nil {
		t.Fatal(err)
	}
	if err := proc.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	s := m.Snapshot(q)
	if s.Processed != n+1 || s.Batches < 3 || s.QueueDepth != 0 || s.QueueBytes != 0 || hooked != n+1 {
		t.Fatalf("snapshot = %+v, hooked %d", s, hooked)
	}
	issues, _ := st.ProjectIssues(ctx, p.ID, 10)
	if len(issues) != 2 {
		t.Fatalf("%d issues, want 2 (all unique_violation events group together)", len(issues))
	}
	for _, is := range issues {
		if is.Category == "exception" && is.OccurrenceCount != n {
			t.Fatalf("exception issue count = %d, want %d", is.OccurrenceCount, n)
		}
		for _, secret := range []string{"hunter2hunter2", "secret-token", "laravel_session", "xero-secret-token"} {
			if strings.Contains(is.SampleContext, secret) {
				t.Fatalf("sample context of %s still contains %q: %s", is.ExceptionType, secret, is.SampleContext)
			}
		}
	}

	if err := proc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(validatedEvent(t, p.ID, "type_error")); !errors.Is(err, ingest.ErrQueueClosed) {
		t.Fatalf("enqueue after shutdown: %v", err)
	}
}

func TestShutdownDrainsQueuedEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "App")

	q := ingest.NewQueue(500, 32<<20)
	m := &ingest.Metrics{}
	// A long batch delay: events would sit in memory without the shutdown drain.
	proc := ingest.NewProcessor(q, st, m, testutil.Logger(t), ingest.ProcessorOptions{BatchSize: 1000, BatchDelay: time.Hour})
	proc.Start()
	for range 40 {
		if err := q.Enqueue(validatedEvent(t, p.ID, "model_not_found")); err != nil {
			t.Fatal(err)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := proc.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(q); s.Processed != 40 || s.Lost != 0 {
		t.Fatalf("after drain: processed %d, lost %d", s.Processed, s.Lost)
	}
}

// ---- API backpressure ----

func TestSaturatedQueueRejectsWith503(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	_, key := testutil.NewProjectKey(t, st, "Busy")

	// No processor: the queue only fills.
	q := ingest.NewQueue(2, 32<<20)
	m := &ingest.Metrics{}
	h := api.New(st, testutil.Logger(t), api.IngestOptions{
		Queue: q, Metrics: m, Limiter: ingest.NewRateLimiter(1000, 1000), MaxBody: 256 << 10,
	})

	post := func() *httptest.ResponseRecorder {
		body := testutil.MustJSON(t, testutil.Stamp(testutil.Fixture(t, "model_not_found")))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/events", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := range 2 {
		if rec := post(); rec.Code != http.StatusAccepted {
			t.Fatalf("event %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	for i := range 5 {
		rec := post()
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" ||
			!strings.Contains(rec.Body.String(), `"queue_full"`) {
			t.Fatalf("overflow %d: %d %v %s", i, rec.Code, rec.Header(), rec.Body)
		}
	}
	s := m.Snapshot(q)
	if s.Accepted != 2 || s.Rejected[ingest.RejectQueueFull] != 5 || s.QueueDepth != 2 {
		t.Fatalf("metrics = %+v", s)
	}
}

func TestByteBudgetKeepsMemoryBounded(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	_, key := testutil.NewProjectKey(t, st, "Big")

	// Room for many events by count, but only ~1 MB of payload.
	q := ingest.NewQueue(10_000, 1<<20)
	h := api.New(st, testutil.Logger(t), api.IngestOptions{
		Queue: q, Metrics: &ingest.Metrics{}, Limiter: ingest.NewRateLimiter(100000, 100000), MaxBody: 256 << 10,
	})
	big := testutil.Stamp(testutil.Fixture(t, "model_not_found"))
	big["context"] = map[string]any{"blob": strings.Repeat("x", 30_000)}

	var accepted, rejected int
	for range 100 {
		big["event_id"] = testutil.NewEventID()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/events", bytes.NewReader(testutil.MustJSON(t, big)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusServiceUnavailable:
			rejected++
		default:
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body)
		}
	}
	if q.Bytes() > q.MaxBytes() {
		t.Fatalf("queue holds %d bytes, over its %d budget", q.Bytes(), q.MaxBytes())
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("accepted %d, rejected %d; want the byte budget to cut in", accepted, rejected)
	}
	t.Logf("accepted %d, rejected %d, queued %d KB", accepted, rejected, q.Bytes()>>10)
}
