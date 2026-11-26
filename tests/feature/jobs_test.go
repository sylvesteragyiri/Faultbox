package feature

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func jobRunItem(job, kind, queue, outcome string, attempt int, ms int64) map[string]any {
	return map[string]any{
		"id": testutil.NewEventID(), "uuid": testutil.NewEventID(), "attempt": attempt, "max_tries": 4,
		"job": job, "kind": kind, "queue": queue, "connection": "database", "outcome": outcome,
		"duration_ms": ms, "wait_ms": 250, "started_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}
}

func jobsBatch(items ...map[string]any) map[string]any {
	b := telemetryBatch("production", nil, nil, nil)
	b["jobs"] = items
	return b
}

// runResults returns just the recent-runs section.
func runResults(body string) string {
	if i := strings.Index(body, `id="run-results"`); i >= 0 {
		return body[i:]
	}
	return ""
}

func TestJobsPageBeforeAnyRuns(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	body := a.Get("/jobs").Body
	for _, want := range []string{"<h1>Jobs</h1>", "See every queue job your app runs", "FAULTBOX_JOB_RUNS_ENABLED", `href="/queue-failures/jobs"`, `href="/jobs" aria-current="page"`} {
		if !contains(body, want) {
			t.Errorf("empty Jobs page is missing %q", want)
		}
	}
	// No invented numbers before job runs arrive.
	for _, unwanted := range []string{"Success rate", "p95 duration", "Recent runs"} {
		if contains(body, unwanted) {
			t.Errorf("empty Jobs page shows %q", unwanted)
		}
	}
}

func TestJobRunsAppearOnTheJobsPage(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")
	// The terminal failure Faultbox already tracks: ProcessPodcast, job 51, attempt 4.
	a.PostEvent(queuePath, key, testutil.Stamp(testutil.Fixture(t, "max_attempts")))
	a.WaitProcessed()

	failed := jobRunItem(`App\Jobs\ProcessPodcast`, "job", "media", "failed", 4, 1800)
	failed["job_id"] = "51"
	b := jobsBatch(
		jobRunItem(`App\Jobs\ProcessPodcast`, "job", "media", "succeeded", 1, 900),
		jobRunItem(`App\Jobs\ProcessPodcast`, "job", "media", "succeeded", 1, 1100),
		jobRunItem(`App\Jobs\ProcessPodcast`, "job", "media", "released", 3, 1500),
		failed,
		jobRunItem(`App\Notifications\InvoicePaid`, "notification", "mail", "succeeded", 1, 30),
		jobRunItem(`App\Mail\Receipt`, "mail", "mail", "succeeded", 1, 20),
	)
	resp := a.Post(telemetryPath, key, testutil.MustJSON(t, b))
	if resp.StatusCode != http.StatusAccepted || !contains(resp.Body, `"count":6`) {
		t.Fatalf("accept: %d %s", resp.StatusCode, resp.Body)
	}
	if again := a.Post(telemetryPath, key, testutil.MustJSON(t, b)); !contains(again.Body, `"status":"duplicate"`) {
		t.Fatalf("retry: %s", again.Body)
	}
	a.LoginAdmin()

	body := a.Get("/jobs").Body
	for _, want := range []string{
		"Success rate", "66.7%", "1 failed · 1 retried", // 4 of 6 succeeded, counted once despite the retry
		"p95 queue wait", "250 ms", "ProcessPodcast", "InvoicePaid", "notification", "Receipt",
		"Recent runs", "Successful runs are kept for 48 hours",
	} {
		if !contains(body, want) {
			t.Errorf("Jobs page is missing %q", want)
		}
	}
	if strings.Index(body, "ProcessPodcast") > strings.Index(body, "InvoicePaid") {
		t.Error("the failing job should come first")
	}
	results := runResults(body)
	if m := regexp.MustCompile(`href="/queue-failures/(\d+)">Failed job</a> · <a href="/issues/(\d+)">Issue</a>`).FindStringSubmatch(results); m == nil {
		t.Error("the failed run should link to its failed job and issue")
	}
	if !contains(results, `href="/app-notifications">Notifications</a>`) || !contains(results, `href="/mail">Mail</a>`) {
		t.Error("queued notifications and mail should link to their pages")
	}

	// Filters work through GET parameters.
	onlyFailed := runResults(a.Get("/jobs?outcome=failed").Body)
	if strings.Count(onlyFailed, "<tr>") != 2 || !contains(onlyFailed, "Failed</span>") { // header row + one run
		t.Errorf("outcome filter: %s", onlyFailed)
	}
	byKind := runResults(a.Get("/jobs?kind=notification").Body)
	if !contains(byKind, "InvoicePaid") || contains(byKind, "ProcessPodcast") {
		t.Error("kind filter should show only the notification")
	}
	byJob := runResults(a.Get("/jobs?queue=media&job=" + url.QueryEscape(`App\Jobs\ProcessPodcast`)).Body)
	if strings.Count(byJob, "<tr>") != 5 || contains(byJob, "InvoicePaid") {
		t.Error("job and queue filters should show the four ProcessPodcast runs")
	}
	none := runResults(a.Get("/jobs?since=1h&job=" + url.QueryEscape(`App\Jobs\Missing`)).Body)
	if !contains(none, "No matching runs") || !contains(none, "Clear filters") {
		t.Error("a filter with no runs should say so and offer to clear it")
	}
	// Unknown values are ignored rather than matching nothing.
	if junk := runResults(a.Get("/jobs?outcome=bogus&kind=cron&since=forever").Body); strings.Count(junk, "<tr>") != 7 {
		t.Error("unknown filter values should be ignored")
	}

	// HTMX filter changes get only the results.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, a.Server.URL+"/jobs?outcome=released", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "run-results")
	frag := testutil.Do(t, a.Client, req).Body
	if !strings.HasPrefix(strings.TrimSpace(frag), `<section id="run-results"`) || contains(frag, "<h1>") {
		t.Errorf("HTMX should get the results fragment only, got %.200s", frag)
	}
}

func TestJobRunsAreValidatedAtTheAPI(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")
	bad := jobRunItem("send mail to jane@example.com", "job", "default", "finished", 1, -5)
	resp := a.Post(telemetryPath, key, testutil.MustJSON(t, jobsBatch(bad)))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d %s", resp.StatusCode, resp.Body)
	}
	for _, want := range []string{"jobs[0].job", "jobs[0].outcome", "jobs[0].duration_ms"} {
		if !contains(resp.Body, want) {
			t.Errorf("validation errors should mention %s: %s", want, resp.Body)
		}
	}
	if contains(resp.Body, "jane@example.com") {
		t.Error("the rejected value must not be echoed back")
	}
}

func TestOlderSuccessfulRunsExplainedInEmptyList(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, _ := testutil.NewProjectKey(t, a.Store(), "Shop")
	// Only a failed run is listed; the empty filtered list explains the window.
	if _, _, err := a.Store().IngestTelemetry(t.Context(), p.ID, store.TelemetryBatch{BatchID: testutil.NewEventID(), Jobs: []store.NewJobRun{{
		UUID: testutil.NewEventID(), Attempt: 1, Environment: "production", JobName: `App\Jobs\A`, Kind: "job",
		Outcome: store.JobFailed, DurationMs: 5, StartedAt: time.Now().Add(-time.Hour),
	}}}); err != nil {
		t.Fatal(err)
	}
	a.LoginAdmin()
	body := runResults(a.Get("/jobs?outcome=succeeded").Body)
	if !contains(body, "Successful runs older than 48 hours aren't kept") {
		t.Errorf("empty list should explain retention: %.400s", body)
	}
}
