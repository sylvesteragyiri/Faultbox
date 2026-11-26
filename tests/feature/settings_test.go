package feature

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestSettingsRetention(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	cfg.ShortRetentionHours = 48
	a := testutil.NewAppWithConfig(t, cfg)
	p, _ := testutil.NewProjectKey(t, a.Store(), "Shop")
	// A successful run 30 hours old: kept at 48 hours, deleted at 24.
	if _, _, err := a.Store().IngestTelemetry(t.Context(), p.ID, store.TelemetryBatch{BatchID: testutil.NewEventID(), Jobs: []store.NewJobRun{{
		UUID: testutil.NewEventID(), Attempt: 1, Environment: "production", JobName: `App\Jobs\A`, Kind: "job",
		Outcome: store.JobSucceeded, DurationMs: 5, StartedAt: time.Now().Add(-30 * time.Hour),
	}}}); err != nil {
		t.Fatal(err)
	}
	a.LoginAdmin()

	page := a.Get("/settings").Body
	for _, want := range []string{
		"<h2>Retention</h2>", `<option value="48" selected>48 hours</option>`, `<option value="24" >24 hours</option>`,
		"From <code>FAULTBOX_SHORT_RETENTION_HOURS</code>: 48 hours", "From <code>FAULTBOX_RETENTION_DAYS</code>: 30 days",
		`name="issue_days" value="30"`, "Disk used by pruned data", "<dt>Job runs</dt>",
	} {
		if !contains(page, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}
	if contains(page, "Use the environment's values") {
		t.Error("nothing saved yet, so there's nothing to reset")
	}
	csrf := a.CSRF("/settings")
	post := func(form url.Values) testutil.Response { return a.PostForm("/settings/retention", form, csrf) }

	// Invalid values are rejected with a message and change nothing.
	for _, bad := range []url.Values{
		{"short_hours": {"48"}, "issue_days": {"1"}},
		{"short_hours": {"0"}, "issue_days": {"30"}},
		{"short_hours": {"48"}, "issue_days": {"ten"}},
	} {
		resp := post(bad)
		if resp.StatusCode != http.StatusUnprocessableEntity || !contains(resp.Body, `class="alert alert-error"`) {
			t.Errorf("POST %v = %d, want 422 with an error", bad, resp.StatusCode)
		}
	}
	if !contains(post(url.Values{"short_hours": {"48"}, "issue_days": {"1"}}).Body, "can&#39;t be kept longer than issues and history") {
		t.Error("the short-lived > issue retention message should explain the rule")
	}

	// A change that deletes data asks first and saves nothing yet.
	preview := post(url.Values{"short_hours": {"24"}, "issue_days": {"30"}})
	if preview.StatusCode != http.StatusOK || !contains(preview.Body, "This change deletes about 1 row at the next sweep") ||
		!contains(preview.Body, "Successful job runs: 1") || !contains(preview.Body, `name="confirm" value="1"`) {
		t.Fatalf("preview = %d %.600s", preview.StatusCode, preview.Body)
	}
	if r, _ := a.Store().Retention(t.Context(), 48, 30); r.ShortSaved {
		t.Fatal("the preview must not save")
	}
	saved := post(url.Values{"short_hours": {"24"}, "issue_days": {"30"}, "confirm": {"1"}})
	if saved.StatusCode != http.StatusOK || !contains(saved.Body, "Retention saved") || !contains(saved.Body, "Saved here: 24 hours") {
		t.Fatalf("after confirming: %d %.300s", saved.StatusCode, saved.Body)
	}
	if !contains(saved.Body, "Use the environment's values") {
		t.Error("saved values can be reset")
	}

	// The sweep applies the new value.
	if _, err := a.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runs, _ := a.Store().ListJobRuns(t.Context(), store.JobRunFilter{}); len(runs.Runs) != 0 {
		t.Error("the sweep should delete the 30-hour-old run at 24 hours")
	}

	// A change that deletes nothing saves straight away.
	if resp := post(url.Values{"short_hours": {"48"}, "issue_days": {"60"}}); !contains(resp.Body, "Retention saved") {
		t.Errorf("lengthening retention needs no confirmation: %.300s", resp.Body)
	}
	if r, _ := a.Store().Retention(t.Context(), 48, 30); r.IssueDays != 60 || !r.IssueSaved {
		t.Errorf("retention = %+v", r)
	}

	reset := post(url.Values{"reset": {"1"}})
	if !contains(reset.Body, "Retention reset to the environment") || !contains(reset.Body, "From <code>FAULTBOX_RETENTION_DAYS</code>: 30 days") {
		t.Errorf("reset: %.300s", reset.Body)
	}

	// Without the CSRF token nothing is saved.
	if resp := a.PostForm("/settings/retention", url.Values{"short_hours": {"24"}, "issue_days": {"30"}}, ""); resp.StatusCode < 400 {
		t.Errorf("missing CSRF token: status %d", resp.StatusCode)
	}
}
