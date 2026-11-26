package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestRetentionSettingsOverrideTheEnvironment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)

	// Nothing saved: the environment's values, or the defaults when unset.
	r, err := st.Retention(ctx, 24, 14)
	if err != nil || r != (store.Retention{ShortHours: 24, IssueDays: 14}) {
		t.Fatalf("environment values = %+v, %v", r, err)
	}
	if r, _ := st.Retention(ctx, 0, 0); r.ShortHours != 48 || r.IssueDays != 30 {
		t.Errorf("defaults = %+v, want 48 hours and 30 days", r)
	}

	if err := st.SaveRetention(ctx, 12, 7); err != nil {
		t.Fatal(err)
	}
	r, _ = st.Retention(ctx, 24, 14)
	if r != (store.Retention{ShortHours: 12, IssueDays: 7, ShortSaved: true, IssueSaved: true}) {
		t.Errorf("saved values should win: %+v", r)
	}

	for _, bad := range []struct{ short, days int }{
		{0, 30}, {169, 30}, {48, 0}, {48, 3651}, {49, 2}, // short-lived longer than issue retention
	} {
		if err := st.SaveRetention(ctx, bad.short, bad.days); !errors.Is(err, store.ErrRetentionInvalid) {
			t.Errorf("SaveRetention(%d, %d) = %v, want invalid", bad.short, bad.days, err)
		}
	}
	if err := st.SaveRetention(ctx, 48, 2); err != nil {
		t.Errorf("48 hours fits in 2 days: %v", err)
	}

	if err := st.ResetRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Retention(ctx, 24, 14); r.ShortSaved || r.IssueSaved || r.ShortHours != 24 {
		t.Errorf("after reset the environment applies again: %+v", r)
	}

	// Short-lived retention never outlasts issue retention, whatever the
	// environment says.
	if r, _ := st.Retention(ctx, 168, 1); r.ShortHours != 24 || r.IssueDays != 1 {
		t.Errorf("168 hours with 1 day of issues = %+v, want capped at 24 hours", r)
	}
	// Saving again overrides the environment again.
	if err := st.SaveRetention(ctx, 12, 7); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Retention(ctx, 24, 14); !r.ShortSaved || r.ShortHours != 12 {
		t.Errorf("saved again = %+v", r)
	}
}

func TestSweepDeletesOnlyExpiredRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Shop")
	now := time.Now().UTC()
	policy := store.Retention{ShortHours: 48, IssueDays: 30}

	// Events and failed jobs: two old, one recent, in one issue.
	attempts := 1
	var events []store.NewEvent
	for _, ago := range []time.Duration{40 * 24 * time.Hour, 35 * 24 * time.Hour, time.Hour} {
		e := newEvent(p.ID, "q-sync", now.Add(-ago))
		e.Category = store.CategoryQueue
		e.Job = &store.NewQueueFailure{Kind: "terminal", Name: `App\Jobs\SyncInvoice`, Attempts: &attempts}
		events = append(events, e)
	}
	results, err := st.IngestBatch(ctx, events)
	if err != nil {
		t.Fatal(err)
	}
	issueID := results[0].IssueID

	// Job runs: successful ones expire after 48 hours, the rest after 30 days.
	ingestRuns(t, st, p.ID,
		jobRun(`App\Jobs\A`, store.JobSucceeded, 1, now.Add(-72*time.Hour)), // expired
		jobRun(`App\Jobs\A`, store.JobSucceeded, 1, now.Add(-73*time.Hour)), // expired
		jobRun(`App\Jobs\A`, store.JobSucceeded, 1, now.Add(-time.Hour)),
		jobRun(`App\Jobs\A`, store.JobFailed, 1, now.Add(-72*time.Hour)),
		jobRun(`App\Jobs\A`, store.JobReleased, 1, now.Add(-40*24*time.Hour)), // expired, with its rollup day
	)
	// A command run and a telemetry batch ID from 40 days ago.
	st.SetClock(func() time.Time { return now.Add(-40 * 24 * time.Hour) })
	if _, _, err := st.IngestTelemetry(ctx, p.ID, store.TelemetryBatch{BatchID: testutil.NewEventID(), Commands: []store.NewCommandRun{{
		UUID: testutil.NewEventID(), Environment: "production", Command: "migrate", StartedAt: now.Add(-40 * 24 * time.Hour),
	}}}); err != nil {
		t.Fatal(err)
	}
	st.SetClock(time.Now)

	impact, err := st.RetentionImpact(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"Successful job runs": 2, "Telemetry batch IDs": 1, "Failed and retried job runs": 1,
		"Events and failed jobs": 2, "Command runs": 1, "Job run rollup": 1, "Failed job rollup": 2, "Command rollup": 1,
	}
	got := map[string]int64{}
	for _, c := range impact {
		got[c.Label] = c.Rows
	}
	for label, n := range want {
		if got[label] != n {
			t.Errorf("impact %q = %d, want %d (all: %v)", label, got[label], n, got)
		}
	}

	// Two rows per transaction, so the batching loop runs more than once.
	deleted, err := st.Sweep(ctx, policy, store.SweepOptions{Batch: 2, Pause: -1})
	if err != nil {
		t.Fatal(err)
	}
	if deleted["queue_job_runs"] != 3 || deleted["events"] != 2 || deleted["app_command_runs"] != 1 || deleted["telemetry_batches"] != 1 {
		t.Errorf("deleted = %v", deleted)
	}
	if again, _ := st.RetentionImpact(ctx, policy); len(again) != 0 {
		t.Errorf("nothing should be left to delete: %+v", again)
	}

	page, err := st.ListJobRuns(ctx, store.JobRunFilter{ProjectID: p.ID})
	if err != nil || len(page.Runs) != 2 {
		t.Fatalf("kept runs = %+v, %v", page.Runs, err)
	}
	for _, r := range page.Runs {
		if r.Outcome == store.JobSucceeded && r.FinishedAt.Before(now.Add(-48*time.Hour)) {
			t.Error("an expired successful run survived")
		}
	}
	jobs, err := st.ListFailedJobs(ctx, store.QueueFilter{ProjectID: p.ID})
	if err != nil || len(jobs.Jobs) != 1 {
		t.Errorf("failed jobs go with their events: %+v, %v", jobs.Jobs, err)
	}
	// The issue summary survives its events.
	is, err := st.Issue(ctx, issueID)
	if err != nil || is.OccurrenceCount != 3 {
		t.Errorf("issue summary = %+v, %v", is, err)
	}
	if evs, _ := st.IssueEvents(ctx, issueID, 10); len(evs) != 1 {
		t.Errorf("issue keeps %d events, want 1", len(evs))
	}
	// The rollup's recent days stay.
	if sum, _ := st.JobRunSummary(ctx, store.TelemetryScope{ProjectID: p.ID}, 30); sum.Runs != 4 {
		t.Errorf("recent rollup days should stay: %d runs", sum.Runs)
	}

	// A cancelled sweep stops and says so.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.Sweep(cancelled, policy, store.SweepOptions{}); err == nil {
		t.Error("a cancelled sweep should report the cancellation")
	}
}

func TestPrunedTableSizes(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Shop")
	var runs []store.NewJobRun
	for range 300 {
		runs = append(runs, jobRun(`App\Jobs\A`, store.JobSucceeded, 1, time.Now().Add(-time.Minute)))
	}
	ingestRuns(t, st, p.ID, runs...)
	sizes, err := st.PrunedTableSizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) < 10 || sizes[0].Table != "queue_job_runs" || sizes[0].Label != "Job runs" || sizes[0].Bytes < 10_000 {
		t.Errorf("job runs should be the largest pruned table: %+v", sizes)
	}
}
