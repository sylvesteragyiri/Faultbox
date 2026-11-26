package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// The Jobs page (M10): every queue job the monitored apps run, successful
// ones included. The summary, chart and per-class table read the daily
// rollup; the recent-runs list reads queue_job_runs, where successful runs
// are kept only for the short-lived retention period.

// runRanges are the recent-runs list's time filters.
var runRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// jobRunForm is the recent-runs filter state, from the query string.
type jobRunForm struct {
	Days    int // the summary's window: 7, 14 or 30
	Job     string
	Queue   string
	Kind    string
	Outcome string
	Since   string // a runRanges key, or "" for any time
	Page    int
}

func parseJobRunForm(r *http.Request, days int) jobRunForm {
	q := r.URL.Query()
	f := jobRunForm{
		Days:    days,
		Job:     strings.TrimSpace(q.Get("job")),
		Queue:   strings.TrimSpace(q.Get("queue")),
		Kind:    q.Get("kind"),
		Outcome: q.Get("outcome"),
		Since:   q.Get("since"),
	}
	if len(f.Job) > 255 {
		f.Job = f.Job[:255]
	}
	if len(f.Queue) > 255 {
		f.Queue = f.Queue[:255]
	}
	if !slices.Contains(store.JobKinds, f.Kind) {
		f.Kind = ""
	}
	if !slices.Contains(store.JobOutcomes, f.Outcome) {
		f.Outcome = ""
	}
	if _, ok := runRanges[f.Since]; !ok {
		f.Since = ""
	}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	f.Page = max(1, f.Page)
	return f
}

func (f jobRunForm) filter(sc store.TelemetryScope, now time.Time) store.JobRunFilter {
	sf := store.JobRunFilter{
		ProjectID: sc.ProjectID, Environment: sc.Environment,
		JobName: f.Job, Queue: f.Queue, Kind: f.Kind, Outcome: f.Outcome, Page: f.Page, PerPage: 25,
	}
	if d, ok := runRanges[f.Since]; ok {
		sf.Since = now.Add(-d)
	}
	return sf
}

// values encodes the form, leaving out defaults so URLs stay short.
func (f jobRunForm) values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	if f.Days != 30 && f.Days != 0 {
		v.Set("range", strconv.Itoa(f.Days)+"d")
	}
	set("job", f.Job)
	set("queue", f.Queue)
	set("kind", f.Kind)
	set("outcome", f.Outcome)
	set("since", f.Since)
	if f.Page > 1 {
		v.Set("page", strconv.Itoa(f.Page))
	}
	return v
}

func (f jobRunForm) url() string {
	if enc := f.values().Encode(); enc != "" {
		return "/jobs?" + enc
	}
	return "/jobs"
}

// PageURL links to another page of the same runs.
func (f jobRunForm) PageURL(page int) string { f.Page = page; return f.url() }

// JobURL filters the runs to one job class and queue.
func (f jobRunForm) JobURL(job, queue string) string {
	f.Job, f.Queue, f.Page = job, queue, 1
	return f.url() + "#runs"
}

// RangeURL changes the summary's window, keeping the filters.
func (f jobRunForm) RangeURL(days int) string { f.Days, f.Page = days, 1; return f.url() }

// Filtered reports whether any recent-runs filter is applied.
func (f jobRunForm) Filtered() bool {
	return f.Job != "" || f.Queue != "" || f.Kind != "" || f.Outcome != "" || f.Since != ""
}

// ClearURL drops the recent-runs filters, keeping the summary's window.
func (f jobRunForm) ClearURL() string { return jobRunForm{Days: f.Days}.url() + "#runs" }

// jobRunRow is one recent run with what it links to.
type jobRunRow struct {
	store.JobRun
	ProjectName string
	// DeliveryURL is the notification or mail page for a queued
	// notification or mailable.
	DeliveryURL string
}

func (h *Handler) jobsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, names, days, err := h.telemetryScope(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	form := parseJobRunForm(r, days)
	policy, err := h.retention(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	page, err := h.store.ListJobRuns(ctx, form.filter(sc, time.Now()))
	if err != nil {
		h.serverError(w, err)
		return
	}
	rows := make([]jobRunRow, len(page.Runs))
	for i, run := range page.Runs {
		rows[i] = jobRunRow{JobRun: run, ProjectName: names[run.ProjectID]}
		switch run.Kind {
		case "notification":
			rows[i].DeliveryURL = notificationsPage.Path
		case "mail":
			rows[i].DeliveryURL = mailPage.Path
		}
	}
	data := map[string]any{
		"Form":       form,
		"Range":      days,
		"Runs":       rows,
		"Page":       page,
		"First":      (page.Page-1)*page.PerPage + 1,
		"Last":       (page.Page-1)*page.PerPage + len(page.Runs),
		"ShortHours": policy.ShortHours,
		"Kinds":      store.JobKinds,
		"Outcomes":   store.JobOutcomes,
		"ScopeName":  names[sc.ProjectID],
		"Env":        sc.Environment,
		"FailedURL":  queueForm{}.url(),
	}
	// Filter changes swap only the results; the browser URL follows (hx-push-url).
	if isHTMX(r) && r.Header.Get("HX-Target") == "run-results" {
		h.render(w, http.StatusOK, "jobs", "run_results", h.newPage(r, "Jobs", "jobs", data))
		return
	}

	summary, err := h.store.JobRunSummary(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.JobRunTrend(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	stats, err := h.store.JobRunStats(ctx, sc, days, 50)
	if err != nil {
		h.serverError(w, err)
		return
	}
	facets, err := h.store.JobRunFacets(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	failed, err := h.store.QueueSummary(ctx, sc.ProjectID, queueDays)
	if err != nil {
		h.serverError(w, err)
		return
	}
	data["Summary"] = summary
	data["Chart"] = newBarChart(fmt.Sprintf("Job runs per day, last %d days", days), trend, []string{"Succeeded", "Failed or retried"})
	data["Stats"] = stats
	data["Facets"] = facets
	data["Failed"] = failed
	data["OtherName"] = store.JobOtherName
	h.render(w, http.StatusOK, "jobs", "", h.newPage(r, "Jobs", "jobs", data))
}

// outcomeLabel names a run outcome for people.
func outcomeLabel(outcome string) string {
	switch outcome {
	case store.JobSucceeded:
		return "Succeeded"
	case store.JobReleased:
		return "Retried"
	case store.JobFailed:
		return "Failed"
	case store.JobTimedOut:
		return "Timed out"
	}
	return outcome
}
