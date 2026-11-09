package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// queueDays is how many days back the queue dashboard looks. It matches the
// default event retention, so the numbers cover every failed job still stored.
const queueDays = 30

// ---- dashboard ----

func (h *Handler) queueDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project"), 10, 64)

	names, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	var project *store.Project
	for i := range projects {
		if projects[i].ID == projectID {
			project = &projects[i]
		}
	}
	if project == nil {
		projectID = 0
	}

	summary, err := h.store.QueueSummary(ctx, projectID, queueDays)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.QueueTrend(ctx, projectID, queueDays)
	if err != nil {
		h.serverError(w, err)
		return
	}
	top, err := h.store.TopFailingJobs(ctx, projectID, queueDays, 8)
	if err != nil {
		h.serverError(w, err)
		return
	}
	queues, err := h.store.QueueBreakdown(ctx, projectID, queueDays, "queue")
	if err != nil {
		h.serverError(w, err)
		return
	}
	connections, err := h.store.QueueBreakdown(ctx, projectID, queueDays, "connection")
	if err != nil {
		h.serverError(w, err)
		return
	}
	rates, err := h.store.QueueFailureRates(ctx, projectID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	recent, err := h.store.ListFailedJobs(ctx, store.QueueFilter{ProjectID: projectID, PerPage: 8})
	if err != nil {
		h.serverError(w, err)
		return
	}

	var queueTotal int64
	for _, b := range queues {
		queueTotal += b.Count
	}
	var connTotal int64
	for _, b := range connections {
		connTotal += b.Count
	}
	project0 := ""
	if projectID > 0 {
		project0 = strconv.FormatInt(projectID, 10)
	}
	h.render(w, http.StatusOK, "queue", "", h.newPage(r, "Queue Failures", "queue", map[string]any{
		"Projects":    projects,
		"Project":     project,
		"ProjectID":   project0,
		"Summary":     summary,
		"Chart":       newBarChart("Failed jobs per day, last 30 days", trend, []string{"Failed jobs", "Failed retries"}),
		"Top":         top,
		"Queues":      queues,
		"QueueTotal":  queueTotal,
		"Conns":       connections,
		"ConnTotal":   connTotal,
		"Rates":       rates,
		"Recent":      h.failedJobRows(names, recent.Jobs),
		"ListURL":     queueForm{Project: project0}.url(),
		"ProjectLink": queueToolLinks(projects, projectID),
	}))
}

// queueTool is a project's link to its own queue dashboard.
type queueTool struct {
	Project string
	URL     string
}

// queueToolLinks returns the queue tool links to offer: the selected
// project's, or every project's that has one.
func queueToolLinks(projects []store.Project, projectID int64) []queueTool {
	var out []queueTool
	for _, p := range projects {
		if p.QueueDashboardURL != "" && (projectID == 0 || p.ID == projectID) {
			out = append(out, queueTool{Project: p.Name, URL: p.QueueDashboardURL})
		}
	}
	return out
}

// ---- list ----

// queueForm is the failed-job list's filter state, from the query string.
type queueForm struct {
	Project    string
	Queue      string
	Connection string
	Job        string
	Env        string
	Kind       string
	Range      string
	Page       int
}

func parseQueueForm(r *http.Request) queueForm {
	q := r.URL.Query()
	f := queueForm{
		Project:    q.Get("project"),
		Queue:      strings.TrimSpace(q.Get("queue")),
		Connection: strings.TrimSpace(q.Get("connection")),
		Job:        strings.TrimSpace(q.Get("job")),
		Env:        strings.TrimSpace(q.Get("env")),
		Kind:       q.Get("kind"),
		Range:      q.Get("range"),
	}
	if _, err := strconv.ParseInt(f.Project, 10, 64); err != nil {
		f.Project = ""
	}
	switch f.Kind {
	case store.QueueKindAttempts, store.QueueKindAll:
	default:
		f.Kind = store.QueueKindFailures
	}
	if _, ok := rangeDurations[f.Range]; !ok {
		f.Range = ""
	}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	f.Page = max(1, f.Page)
	return f
}

func (f queueForm) filter(now time.Time) store.QueueFilter {
	sf := store.QueueFilter{
		Queue: f.Queue, Connection: f.Connection, JobName: f.Job, Environment: f.Env,
		Kind: f.Kind, Page: f.Page, PerPage: 25,
	}
	sf.ProjectID, _ = strconv.ParseInt(f.Project, 10, 64)
	if d, ok := rangeDurations[f.Range]; ok {
		sf.Since = now.Add(-d)
	}
	return sf
}

// values encodes the form, leaving out defaults so URLs stay short.
func (f queueForm) values() url.Values {
	v := url.Values{}
	set := func(k, val, def string) {
		if val != "" && val != def {
			v.Set(k, val)
		}
	}
	set("project", f.Project, "")
	set("queue", f.Queue, "")
	set("connection", f.Connection, "")
	set("job", f.Job, "")
	set("env", f.Env, "")
	set("kind", f.Kind, store.QueueKindFailures)
	set("range", f.Range, "")
	if f.Page > 1 {
		v.Set("page", strconv.Itoa(f.Page))
	}
	return v
}

func (f queueForm) url() string {
	if enc := f.values().Encode(); enc != "" {
		return "/queue-failures/jobs?" + enc
	}
	return "/queue-failures/jobs"
}

// PageURL links to another page of the same results.
func (f queueForm) PageURL(page int) string { f.Page = page; return f.url() }

// JobURL filters the same results to one job class.
func (f queueForm) JobURL(job string) string { f.Job, f.Page = job, 1; return f.url() }

// Filtered reports whether anything beyond the defaults is applied.
func (f queueForm) Filtered() bool {
	return f.Project != "" || f.Queue != "" || f.Connection != "" || f.Job != "" || f.Env != "" ||
		f.Range != "" || f.Kind != store.QueueKindFailures
}

type failedJobRow struct {
	store.FailedJob
	ProjectName string
}

func (h *Handler) failedJobRows(names map[int64]string, jobs []store.FailedJob) []failedJobRow {
	rows := make([]failedJobRow, len(jobs))
	for i, j := range jobs {
		rows[i] = failedJobRow{FailedJob: j, ProjectName: names[j.ProjectID]}
	}
	return rows
}

func (h *Handler) failedJobList(w http.ResponseWriter, r *http.Request) {
	form := parseQueueForm(r)
	f := form.filter(time.Now())
	page, err := h.store.ListFailedJobs(r.Context(), f)
	if err != nil {
		h.serverError(w, err)
		return
	}
	names, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	facets, err := h.store.FailedJobFacets(r.Context(), f.ProjectID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	pg := h.newPage(r, "Failed jobs", "queue", map[string]any{
		"Form":     form,
		"Rows":     h.failedJobRows(names, page.Jobs),
		"Page":     page,
		"Projects": projects,
		"Facets":   facets,
		"First":    (page.Page-1)*page.PerPage + 1,
		"Last":     (page.Page-1)*page.PerPage + len(page.Jobs),
	})
	// Filter changes swap only the results; the browser URL follows (hx-push-url).
	if isHTMX(r) && r.Header.Get("HX-Target") == "job-results" {
		h.render(w, http.StatusOK, "queue_jobs", "job_results", pg)
		return
	}
	h.render(w, http.StatusOK, "queue_jobs", "", pg)
}

// ---- detail ----

func (h *Handler) failedJobShow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return
	}
	ctx := r.Context()
	job, err := h.store.FailedJob(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, err)
		return
	}
	is, err := h.store.Issue(ctx, job.IssueID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	project, err := h.store.Project(ctx, job.ProjectID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	history, err := h.store.JobFailures(ctx, job.ProjectID, job.JobName, 10)
	if err != nil {
		h.serverError(w, err)
		return
	}

	h.render(w, http.StatusOK, "queue_job", "", h.newPage(r, job.JobName, "queue", map[string]any{
		"Job":     job,
		"Issue":   is,
		"Project": project,
		"Trace":   buildTrace(job.Event.Stacktrace),
		"Payload": prettyJSON(job.Payload),
		"User":    prettyJSON(job.Event.UserContext),
		"Context": prettyJSON(job.Event.EventContext),
		"History": history,
		"ListURL": queueForm{Project: strconv.FormatInt(job.ProjectID, 10), Job: job.JobName}.url(),
		"Next":    "/queue-failures/" + strconv.FormatInt(job.EventID, 10),
	}))
}
