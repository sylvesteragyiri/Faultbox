package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/grouping"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// issueForm is the list's filter state, as submitted in the query string.
type issueForm struct {
	Project  string
	Status   string
	Category string
	Env      string
	Release  string
	Type     string
	Q        string
	Range    string
	Sort     string
	Page     int
}

var rangeDurations = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

func parseIssueForm(r *http.Request) issueForm {
	q := r.URL.Query()
	f := issueForm{
		Project:  q.Get("project"),
		Status:   q.Get("status"),
		Category: q.Get("category"),
		Env:      strings.TrimSpace(q.Get("env")),
		Release:  strings.TrimSpace(q.Get("release")),
		Type:     strings.TrimSpace(q.Get("type")),
		Q:        strings.TrimSpace(q.Get("q")),
		Range:    q.Get("range"),
		Sort:     q.Get("sort"),
	}
	if !q.Has("status") {
		f.Status = store.IssueUnresolved
	}
	switch f.Status {
	case store.IssueUnresolved, store.IssueResolved, store.IssueIgnored, "all":
	default:
		f.Status = store.IssueUnresolved
	}
	switch f.Category {
	case store.CategoryQueue, store.CategoryAll:
	default:
		f.Category = store.CategoryErrors
	}
	if _, ok := rangeDurations[f.Range]; !ok {
		f.Range = ""
	}
	switch f.Sort {
	case store.SortCount, store.SortFirstSeen:
	default:
		f.Sort = store.SortLastSeen
	}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	if f.Page < 1 {
		f.Page = 1
	}
	return f
}

func (f issueForm) filter(now time.Time) store.IssueFilter {
	sf := store.IssueFilter{
		Category: f.Category, Environment: f.Env, Release: f.Release, ExceptionType: f.Type,
		Search: f.Q, Sort: f.Sort, Page: f.Page, PerPage: 25,
	}
	sf.ProjectID, _ = strconv.ParseInt(f.Project, 10, 64)
	if f.Status != "all" {
		sf.Status = f.Status
	}
	if d, ok := rangeDurations[f.Range]; ok {
		sf.SeenFrom = now.Add(-d)
	}
	return sf
}

// values encodes the form, leaving out defaults so URLs stay short.
func (f issueForm) values() url.Values {
	v := url.Values{}
	set := func(k, val, def string) {
		if val != "" && val != def {
			v.Set(k, val)
		}
	}
	set("project", f.Project, "")
	if f.Status != store.IssueUnresolved {
		v.Set("status", f.Status)
	}
	set("category", f.Category, store.CategoryErrors)
	set("env", f.Env, "")
	set("release", f.Release, "")
	set("type", f.Type, "")
	set("q", f.Q, "")
	set("range", f.Range, "")
	set("sort", f.Sort, store.SortLastSeen)
	if f.Page > 1 {
		v.Set("page", strconv.Itoa(f.Page))
	}
	return v
}

func (f issueForm) url() string {
	if enc := f.values().Encode(); enc != "" {
		return "/issues?" + enc
	}
	return "/issues"
}

// ListURL is the current list, filters and page included.
func (f issueForm) ListURL() string { return f.url() }

// PageURL links to another page of the same results.
func (f issueForm) PageURL(page int) string { f.Page = page; return f.url() }

// SortURL re-sorts the same results from page 1.
func (f issueForm) SortURL(sort string) string { f.Sort, f.Page = sort, 1; return f.url() }

// Filtered reports whether anything beyond the defaults is applied.
func (f issueForm) Filtered() bool {
	return f.Project != "" || f.Env != "" || f.Release != "" || f.Type != "" || f.Q != "" || f.Range != "" ||
		f.Status != store.IssueUnresolved || f.Category != store.CategoryErrors
}

type issueRow struct {
	store.IssueSummary
	ProjectName string
	Spark       string
	Bars        []SparkBar
}

func (h *Handler) projectNames(r *http.Request) (map[int64]string, []store.Project, error) {
	projects, err := h.store.Projects(r.Context())
	if err != nil {
		return nil, nil, err
	}
	names := make(map[int64]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	return names, projects, nil
}

func (h *Handler) issueRows(names map[int64]string, issues []store.IssueSummary) []issueRow {
	rows := make([]issueRow, len(issues))
	for i, is := range issues {
		rows[i] = issueRow{IssueSummary: is, ProjectName: names[is.ProjectID], Spark: sparkline(is.Trend), Bars: sparkBars(is.Trend)}
	}
	return rows
}

func (h *Handler) issueList(w http.ResponseWriter, r *http.Request) {
	form := parseIssueForm(r)
	f := form.filter(time.Now())
	page, err := h.store.ListIssues(r.Context(), f)
	if err != nil {
		h.serverError(w, err)
		return
	}
	names, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	facets, err := h.store.IssueFacets(r.Context(), f.ProjectID)
	if err != nil {
		h.serverError(w, err)
		return
	}

	data := map[string]any{
		"Form":     form,
		"Rows":     h.issueRows(names, page.Issues),
		"Page":     page,
		"Projects": projects,
		"Facets":   facets,
		"First":    (page.Page-1)*page.PerPage + 1,
		"Last":     (page.Page-1)*page.PerPage + len(page.Issues),
	}
	pg := h.newPage(r, "Issues", "issues", data)
	// Filter changes swap only the results; the browser URL follows (hx-push-url).
	if isHTMX(r) && r.Header.Get("HX-Target") == "issue-results" {
		h.render(w, http.StatusOK, "issues", "issue_results", pg)
		return
	}
	h.render(w, http.StatusOK, "issues", "", pg)
}

// ---- detail ----

type frameView struct {
	Function string
	Location string
	InApp    bool
}

type frameGroup struct {
	InApp     bool
	Collapsed bool
	Frames    []frameView
}

type traceView struct {
	Groups  []frameGroup
	Omitted int
	Empty   bool
}

func buildTrace(raw string) traceView {
	var st struct {
		Frames        []grouping.Frame `json:"frames"`
		FramesOmitted int              `json:"frames_omitted"`
	}
	_ = json.Unmarshal([]byte(raw), &st)
	tv := traceView{Omitted: st.FramesOmitted, Empty: len(st.Frames) == 0}

	anyInApp := false
	for _, f := range st.Frames {
		if grouping.IsInApp(f) {
			anyInApp = true
			break
		}
	}
	for _, f := range st.Frames {
		inApp := grouping.IsInApp(f)
		fv := frameView{InApp: inApp}
		switch {
		case f.Class != "" && f.Function != "":
			fv.Function = f.Class + "::" + f.Function
		case f.Function != "":
			fv.Function = f.Function
		}
		if f.File != "" {
			fv.Location = grouping.NormalizePath(f.File)
			if f.Line > 0 {
				fv.Location += ":" + strconv.Itoa(f.Line)
			}
		}
		n := len(tv.Groups)
		if n == 0 || tv.Groups[n-1].InApp != inApp {
			// Dependency frames fold away unless they are all there is.
			tv.Groups = append(tv.Groups, frameGroup{InApp: inApp, Collapsed: !inApp && anyInApp})
			n++
		}
		tv.Groups[n-1].Frames = append(tv.Groups[n-1].Frames, fv)
	}
	return tv
}

type requestView struct {
	URL     string
	Method  string
	Route   string
	Status  int
	Headers [][2]string
}

func buildRequest(raw string) *requestView {
	if raw == "" {
		return nil
	}
	var rq struct {
		URL        string            `json:"url"`
		Method     string            `json:"method"`
		StatusCode int               `json:"status_code"`
		Route      string            `json:"route"`
		Headers    map[string]string `json:"headers"`
	}
	if json.Unmarshal([]byte(raw), &rq) != nil {
		return nil
	}
	v := &requestView{URL: rq.URL, Method: rq.Method, Route: rq.Route, Status: rq.StatusCode}
	for k, val := range rq.Headers {
		v.Headers = append(v.Headers, [2]string{k, val})
	}
	sort.Slice(v.Headers, func(i, j int) bool { return v.Headers[i][0] < v.Headers[j][0] })
	return v
}

// prettyJSON indents a JSON blob for display; empty or null gives "".
func prettyJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "{}" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, []byte(raw), "", "  ") != nil {
		return raw
	}
	return buf.String()
}

func (h *Handler) loadIssue(w http.ResponseWriter, r *http.Request) (store.Issue, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return store.Issue{}, false
	}
	is, err := h.store.Issue(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return store.Issue{}, false
	}
	if err != nil {
		h.serverError(w, err)
		return store.Issue{}, false
	}
	return is, true
}

func (h *Handler) issueShow(w http.ResponseWriter, r *http.Request) {
	is, ok := h.loadIssue(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	project, err := h.store.Project(ctx, is.ProjectID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	envs, err := h.store.IssueBreakdown(ctx, is.ID, "environment")
	if err != nil {
		h.serverError(w, err)
		return
	}
	releases, err := h.store.IssueBreakdown(ctx, is.ID, "release")
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.IssueTrend(ctx, is.ID, 30)
	if err != nil {
		h.serverError(w, err)
		return
	}
	eventsOffset, _ := strconv.Atoi(r.URL.Query().Get("events"))
	eventsOffset = max(0, min(eventsOffset, 10_000))
	events, moreEvents, err := h.store.IssueEventList(ctx, is.ID, 20, eventsOffset)
	if err != nil {
		h.serverError(w, err)
		return
	}

	// The selected event, or the latest one. After retention removes every
	// event, the issue's stored sample still shows a trace (D13).
	selected := r.URL.Query().Get("event")
	ev, err := h.store.IssueEventDetail(ctx, is.ID, selected)
	var sample map[string]json.RawMessage
	_ = json.Unmarshal([]byte(is.SampleContext), &sample)
	var view struct {
		Event    *store.EventDetail
		Trace    traceView
		Request  *requestView
		User     string
		Context  string
		Job      string
		IsSample bool
	}
	switch {
	case err == nil:
		view.Event = &ev
		view.Trace = buildTrace(ev.Stacktrace)
		view.Request = buildRequest(ev.RequestContext)
		view.User = prettyJSON(ev.UserContext)
		view.Context = prettyJSON(ev.EventContext)
	case errors.Is(err, store.ErrNotFound):
		if selected != "" {
			h.notFound(w, r)
			return
		}
		view.IsSample = true
		view.Trace = buildTrace(is.SampleStacktrace)
		view.Request = buildRequest(string(sample["request"]))
		view.User = prettyJSON(string(sample["user"]))
		view.Context = prettyJSON(string(sample["context"]))
	default:
		h.serverError(w, err)
		return
	}
	if job, ok := sample["job"]; ok {
		view.Job = prettyJSON(string(job))
	}

	var failures []store.QueueFailure
	if is.Category == store.CategoryQueue {
		if failures, err = h.store.IssueQueueFailures(ctx, is.ID, 20); err != nil {
			h.serverError(w, err)
			return
		}
	}

	h.render(w, http.StatusOK, "issue", "", h.newPage(r, is.ExceptionType, "issues", map[string]any{
		"Issue":      is,
		"Project":    project,
		"Envs":       envs,
		"Releases":   releases,
		"Chart":      newBarChart("Events per day, last 30 days", trend, []string{"Events"}),
		"Events":     events,
		"MoreEvents": moreEvents,
		"EventsNext": eventsOffset + 20,
		"EventsPrev": max(0, eventsOffset-20),
		"EventsAt":   eventsOffset,
		"View":       view,
		"Failures":   failures,
	}))
}

func (h *Handler) issueSetStatus(w http.ResponseWriter, r *http.Request) {
	is, ok := h.loadIssue(w, r)
	if !ok {
		return
	}
	status := r.PostFormValue("status")
	if status != store.IssueResolved && status != store.IssueIgnored && status != store.IssueUnresolved {
		h.errorPage(w, r, http.StatusBadRequest, "Unknown issue status.")
		return
	}
	if err := h.store.SetIssueStatus(r.Context(), is.ID, status); err != nil {
		h.serverError(w, err)
		return
	}
	h.logger.Info("issue status changed", "issue_id", is.ID, "status", status, "user_id", h.userID(r))
	// A card in the issue list re-renders itself.
	if target := r.Header.Get("HX-Target"); isHTMX(r) && strings.HasPrefix(target, "issue-card-") {
		h.renderIssueCard(w, r, is.ID)
		return
	}
	if !isHTMX(r) {
		// statusReturnPath only returns local issue or failed-job paths.
		http.Redirect(w, r, statusReturnPath(r.PostFormValue("next"), is.ID), http.StatusSeeOther) //nolint:gosec // G710: local paths only, see statusReturnPath
		return
	}
	is, err := h.store.Issue(r.Context(), is.ID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "issue", "issue_status", h.newPage(r, is.ExceptionType, "issues", map[string]any{"Issue": is}))
}

// statusReturnPath is where a plain (non-HTMX) status form returns to: the
// failed-job page or issue list it was sent from, or the issue. Only these
// local paths are accepted, so the field can't redirect off-site.
func statusReturnPath(next string, issueID int64) string {
	if rest, ok := strings.CutPrefix(next, "/queue-failures/"); ok && rest != "" {
		if _, err := strconv.ParseInt(rest, 10, 64); err == nil {
			return next
		}
	}
	if u, err := url.Parse(next); err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && u.Path == "/issues" {
		return (&url.URL{Path: "/issues", RawQuery: u.Query().Encode()}).String()
	}
	return "/issues/" + strconv.FormatInt(issueID, 10)
}

// renderIssueCard renders one issue-list card, after a status change.
func (h *Handler) renderIssueCard(w http.ResponseWriter, r *http.Request, id int64) {
	ctx := r.Context()
	is, err := h.store.Issue(ctx, id)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.IssueTrend(ctx, id, store.TrendDays)
	if err != nil {
		h.serverError(w, err)
		return
	}
	project, err := h.store.Project(ctx, is.ProjectID)
	if err != nil {
		h.serverError(w, err)
		return
	}
	counts := make([]int64, len(trend))
	for i, d := range trend {
		counts[i] = d.Errors
	}
	sum := store.IssueSummary{
		ID: is.ID, ProjectID: is.ProjectID, Fingerprint: is.Fingerprint, Category: is.Category, Level: is.Level,
		ExceptionType: is.ExceptionType, Message: is.Message, Culprit: is.Culprit, JobName: is.JobName,
		Status: is.Status, OccurrenceCount: is.OccurrenceCount, FirstSeenAt: is.FirstSeenAt, LastSeenAt: is.LastSeenAt,
		LastRelease: is.LastRelease, Trend: counts,
	}
	row := issueRow{IssueSummary: sum, ProjectName: project.Name, Spark: sparkline(counts), Bars: sparkBars(counts)}
	h.renderPartial(w, "issues", "issue_card", map[string]any{
		"Row": row, "CSRF": csrfFrom(ctx), "Next": r.PostFormValue("next"),
	})
}
