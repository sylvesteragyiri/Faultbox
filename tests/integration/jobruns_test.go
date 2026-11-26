package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// jobRun builds one attempt that finished durationMs after start.
func jobRun(name, outcome string, durationMs int64, start time.Time) store.NewJobRun {
	return store.NewJobRun{
		UUID: testutil.NewEventID(), JobUUID: testutil.NewEventID(), Attempt: 1, Environment: "production",
		JobName: name, Kind: "job", Queue: "default", Connection: "redis", Outcome: outcome,
		DurationMs: durationMs, StartedAt: start,
	}
}

func ingestRuns(t *testing.T, st *store.Store, projectID int64, runs ...store.NewJobRun) (batchID string) {
	t.Helper()
	batchID = testutil.NewEventID()
	if _, _, err := st.IngestTelemetry(context.Background(), projectID, store.TelemetryBatch{BatchID: batchID, Jobs: runs}); err != nil {
		t.Fatalf("ingest job runs: %v", err)
	}
	return batchID
}

func TestJobRunsAreStoredAndRolledUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	shop, _ := testutil.NewProjectKey(t, st, "Shop")
	billing, _ := testutil.NewProjectKey(t, st, "Billing")
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	wait := func(ms int64) *int64 { return &ms }

	const sync, notify = `App\Jobs\SyncInvoice`, `App\Notifications\InvoicePaid`
	ok1, ok2, ok3 := jobRun(sync, store.JobSucceeded, 5, start), jobRun(sync, store.JobSucceeded, 40, start.Add(time.Second)), jobRun(sync, store.JobSucceeded, 2000, start.Add(2*time.Second))
	ok1.WaitMs, ok2.WaitMs = wait(100), wait(9000)
	released := jobRun(sync, store.JobReleased, 300, start.Add(3*time.Second))
	failed := jobRun(sync, store.JobFailed, 310, start.Add(4*time.Second))
	failed.Attempt = 2
	n := jobRun(notify, store.JobSucceeded, 15, start.Add(5*time.Second))
	n.Kind, n.Queue = "notification", "mail"
	batch := store.TelemetryBatch{BatchID: testutil.NewEventID(), Jobs: []store.NewJobRun{ok1, ok2, ok3, released, failed, n}}
	if dup, _, err := st.IngestTelemetry(ctx, shop.ID, batch); err != nil || dup {
		t.Fatalf("ingest: dup=%v err=%v", dup, err)
	}
	// A retried post with the same batch ID counts once.
	if dup, _, err := st.IngestTelemetry(ctx, shop.ID, batch); err != nil || !dup {
		t.Fatalf("retry: dup=%v err=%v", dup, err)
	}
	ingestRuns(t, st, billing.ID, jobRun(`App\Jobs\ChargeCard`, store.JobTimedOut, 60_000, start))

	sum, err := st.JobRunSummary(ctx, store.TelemetryScope{ProjectID: shop.ID}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Runs != 6 || sum.Succeeded != 4 || sum.Released != 1 || sum.Failed != 1 || sum.TimedOut != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.SuccessPercent() != 66.7 || sum.MaxMs != 2000 || sum.Waited != 2 || sum.AvgWaitMs() != 4550 {
		t.Errorf("success %v%%, max %d, waited %d, avg wait %d", sum.SuccessPercent(), sum.MaxMs, sum.Waited, sum.AvgWaitMs())
	}
	// Durations 5, 15, 40, 300, 310, 2000: p50 is the 3rd (≤ 50 ms bucket),
	// p95 the 6th, capped at the slowest run.
	if sum.P50Ms() != 50 || sum.P95Ms() != 2000 || sum.P95WaitMs() != 9000 {
		t.Errorf("p50 %d, p95 %d, p95 wait %d", sum.P50Ms(), sum.P95Ms(), sum.P95WaitMs())
	}
	all, err := st.JobRunSummary(ctx, store.TelemetryScope{}, 30)
	if err != nil || all.Runs != 7 || all.TimedOut != 1 {
		t.Fatalf("every project: %+v, %v", all, err)
	}
	if env, _ := st.JobRunSummary(ctx, store.TelemetryScope{Environment: "staging"}, 30); env.Runs != 0 {
		t.Errorf("staging should have no runs, got %d", env.Runs)
	}

	trend, err := st.JobRunTrend(ctx, store.TelemetryScope{ProjectID: shop.ID}, 7)
	if err != nil || len(trend) != 7 {
		t.Fatalf("trend = %v, %v", trend, err)
	}
	if last := trend[6]; last.Errors+trend[5].Errors != 4 || last.QueueFailures+trend[5].QueueFailures != 2 {
		t.Errorf("trend should stack 4 succeeded and 2 unsuccessful runs: %+v", trend[5:])
	}

	stats, err := st.JobRunStats(ctx, store.TelemetryScope{ProjectID: shop.ID}, 30, 10)
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if s := stats[0]; s.JobName != sync || s.Runs != 5 || s.Failed != 1 || s.Released != 1 || s.LastOutcome != store.JobFailed || s.Kind != "job" || s.Queue != "default" {
		t.Errorf("failing job first, with its last outcome: %+v", s)
	}
	if s := stats[1]; s.JobName != notify || s.Kind != "notification" || s.Queue != "mail" || s.LastOutcome != store.JobSucceeded {
		t.Errorf("notification row: %+v", s)
	}

	facets, err := st.JobRunFacets(ctx, store.TelemetryScope{ProjectID: shop.ID}, 30)
	if err != nil || strings.Join(facets.JobNames, ",") != sync+","+notify || strings.Join(facets.Queues, ",") != "default,mail" {
		t.Errorf("facets = %+v, %v", facets, err)
	}

	list := func(f store.JobRunFilter) []string {
		t.Helper()
		page, err := st.ListJobRuns(ctx, f)
		if err != nil {
			t.Fatalf("ListJobRuns(%+v): %v", f, err)
		}
		var out []string
		for _, r := range page.Runs {
			out = append(out, fmt.Sprintf("%s/%s/%d", r.JobName[strings.LastIndex(r.JobName, `\`)+1:], r.Outcome, r.DurationMs))
		}
		return out
	}
	if got := strings.Join(list(store.JobRunFilter{ProjectID: shop.ID}), " "); got !=
		"InvoicePaid/succeeded/15 SyncInvoice/failed/310 SyncInvoice/succeeded/2000 SyncInvoice/released/300 SyncInvoice/succeeded/40 SyncInvoice/succeeded/5" {
		t.Errorf("newest finish first: %s", got)
	}
	for f, want := range map[*store.JobRunFilter]string{
		{ProjectID: shop.ID, Outcome: store.JobFailed}:                  "SyncInvoice/failed/310",
		{ProjectID: shop.ID, Kind: "notification"}:                      "InvoicePaid/succeeded/15",
		{ProjectID: shop.ID, Queue: "mail"}:                             "InvoicePaid/succeeded/15",
		{JobName: `App\Jobs\ChargeCard`}:                                "ChargeCard/timed_out/60000",
		{ProjectID: shop.ID, JobName: sync, Outcome: store.JobReleased}: "SyncInvoice/released/300",
		{ProjectID: shop.ID, Since: time.Now().Add(time.Minute)}:        "",
		{ProjectID: shop.ID, Environment: "staging"}:                    "",
	} {
		if got := strings.Join(list(*f), " "); got != want {
			t.Errorf("filter %+v = %q, want %q", *f, got, want)
		}
	}
	page, err := st.ListJobRuns(ctx, store.JobRunFilter{ProjectID: shop.ID, PerPage: 4, Page: 2})
	if err != nil || len(page.Runs) != 2 || page.HasNext {
		t.Errorf("page 2 of 4 per page: %d runs, next %v, %v", len(page.Runs), page.HasNext, err)
	}
	first, _ := st.ListJobRuns(ctx, store.JobRunFilter{ProjectID: shop.ID, Outcome: store.JobSucceeded, PerPage: 1})
	if r := first.Runs[0]; r.HasWait || r.Kind != "notification" || !r.FinishedAt.Equal(r.StartedAt.Add(15*time.Millisecond)) {
		t.Errorf("run without a wait, finished after its duration: %+v", r)
	}
}

func TestJobNamesAreCappedPerDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Dynamic")
	start := time.Now().UTC().Add(-time.Minute)

	runs := make([]store.NewJobRun, 0, store.JobNameCap)
	for i := range store.JobNameCap {
		runs = append(runs, jobRun(fmt.Sprintf(`App\Jobs\Job%d`, i), store.JobSucceeded, 1, start))
		if len(runs) == 500 { // the API's per-list limit
			ingestRuns(t, st, p.ID, runs...)
			runs = runs[:0]
		}
	}
	// Past the cap: two new names go to (other); a known name still counts under itself.
	ingestRuns(t, st, p.ID, jobRun(`App\Jobs\Extra1`, store.JobSucceeded, 1, start),
		jobRun(`App\Jobs\Extra2`, store.JobFailed, 1, start), jobRun(`App\Jobs\Job7`, store.JobSucceeded, 1, start))

	stats, err := st.JobRunStats(ctx, store.TelemetryScope{ProjectID: p.ID}, 1, 2000)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int64{}
	for _, s := range stats {
		byName[s.JobName] = s.Runs
	}
	if len(byName) != store.JobNameCap+1 || byName[store.JobOtherName] != 2 || byName[`App\Jobs\Job7`] != 2 || byName[`App\Jobs\Extra1`] != 0 {
		t.Errorf("%d names, (other) %d, Job7 %d, Extra1 %d", len(byName), byName[store.JobOtherName], byName[`App\Jobs\Job7`], byName[`App\Jobs\Extra1`])
	}
	if stats[0].JobName != store.JobOtherName {
		t.Errorf("(other) holds the only failure and should come first, got %s", stats[0].JobName)
	}
	// Run rows keep the real name.
	page, err := st.ListJobRuns(ctx, store.JobRunFilter{ProjectID: p.ID, JobName: `App\Jobs\Extra2`})
	if err != nil || len(page.Runs) != 1 {
		t.Errorf("run rows keep names past the cap: %+v, %v", page.Runs, err)
	}
	facets, _ := st.JobRunFacets(ctx, store.TelemetryScope{ProjectID: p.ID}, 1)
	for _, n := range facets.JobNames {
		if n == store.JobOtherName {
			t.Error("(other) can't be used as a filter, so it isn't offered")
		}
	}
}

func TestFailedJobRunsLinkToTheirFailedJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Shop")
	now := time.Now().UTC()

	attempts := 2
	e := newEvent(p.ID, "q-sync", now.Add(-time.Minute))
	e.Category = store.CategoryQueue
	e.Job = &store.NewQueueFailure{Kind: "terminal", Name: `App\Jobs\SyncInvoice`, ID: "42", Queue: "default", Attempts: &attempts}
	results, err := st.IngestBatch(ctx, []store.NewEvent{e})
	if err != nil {
		t.Fatal(err)
	}

	first := jobRun(`App\Jobs\SyncInvoice`, store.JobReleased, 10, now.Add(-3*time.Minute))
	first.JobID = "42"
	last := jobRun(`App\Jobs\SyncInvoice`, store.JobFailed, 10, now.Add(-2*time.Minute))
	last.JobID, last.Attempt = "42", 2
	other := jobRun(`App\Jobs\SyncInvoice`, store.JobSucceeded, 10, now.Add(-90*time.Second))
	other.JobID, other.Attempt = "42", 2 // a success never links to a failure
	ingestRuns(t, st, p.ID, first, last, other)

	page, err := st.ListJobRuns(ctx, store.JobRunFilter{ProjectID: p.ID})
	if err != nil || len(page.Runs) != 3 {
		t.Fatalf("runs = %+v, %v", page.Runs, err)
	}
	links := map[string][2]int64{}
	for _, r := range page.Runs {
		links[r.UUID] = [2]int64{r.FailureEventID, r.IssueID}
	}
	if got := links[last.UUID]; got != [2]int64{results[0].EventID, results[0].IssueID} {
		t.Errorf("failed attempt 2 should link to event %d / issue %d, got %v", results[0].EventID, results[0].IssueID, got)
	}
	if got := links[first.UUID]; got != [2]int64{} {
		t.Errorf("attempt 1 has no failed-job row of its own, got %v", got)
	}
	if got := links[other.UUID]; got != [2]int64{} {
		t.Errorf("successful run linked to %v", got)
	}
}

func TestJobRunListQueryPlansUseIndexes(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Shop")
	ingestRuns(t, st, p.ID, jobRun(`App\Jobs\A`, store.JobSucceeded, 1, time.Now().Add(-time.Minute)))
	for _, f := range []store.JobRunFilter{
		{},
		{ProjectID: p.ID},
		{ProjectID: p.ID, JobName: `App\Jobs\A`},
		{ProjectID: p.ID, Outcome: store.JobFailed},
		{ProjectID: p.ID, Queue: "default", Kind: "job", Environment: "production"},
		{Since: time.Now().Add(-time.Hour)},
	} {
		plan, err := st.ExplainJobRunList(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if strings.Contains(joined, "SCAN r\n") || strings.HasSuffix(joined, "SCAN r") || strings.Contains(joined, "USE TEMP B-TREE") {
			t.Errorf("filter %+v doesn't read an index in order:\n%s", f, joined)
		}
	}
}
