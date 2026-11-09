package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Issue list sort orders.
const (
	SortLastSeen  = "last_seen"
	SortCount     = "count"
	SortFirstSeen = "first_seen"
)

// Issue list categories: "errors" is exceptions and messages; queue failures
// get their own view (M5).
const (
	CategoryErrors = "errors"
	CategoryQueue  = "queue_failure"
	CategoryAll    = "all"
)

// MaxIssuePage bounds OFFSET so deep pages can't turn into long scans.
const MaxIssuePage = 400

// IssueFilter selects issues for the list view. Zero values mean "any".
type IssueFilter struct {
	ProjectID     int64
	Status        string // unresolved, resolved, ignored, or "" for all
	Category      string // errors, queue_failure, all ("" = errors)
	Environment   string
	Release       string
	ExceptionType string
	Search        string
	// SeenFrom and SeenTo select issues with activity overlapping the range:
	// last seen on or after From, and first seen on or before To.
	SeenFrom, SeenTo time.Time
	Sort             string
	Page, PerPage    int
}

// IssueSummary is an issue row for lists, without the heavy sample columns.
type IssueSummary struct {
	ID              int64
	ProjectID       int64
	Fingerprint     string
	Category        string
	Level           string
	ExceptionType   string
	Message         string
	Culprit         string
	JobName         string
	Status          string
	OccurrenceCount int64
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	LastRelease     string
	// Trend holds daily counts for the last TrendDays days, oldest first.
	Trend []int64
}

// TrendDays is the length of the per-issue sparkline in lists.
const TrendDays = 14

// IssuePage is one page of results.
type IssuePage struct {
	Issues  []IssueSummary
	Page    int
	PerPage int
	HasNext bool
}

func (f *IssueFilter) normalize() {
	if f.PerPage <= 0 || f.PerPage > 100 {
		f.PerPage = 25
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Page > MaxIssuePage {
		f.Page = MaxIssuePage
	}
	switch f.Sort {
	case SortCount, SortFirstSeen:
	default:
		f.Sort = SortLastSeen
	}
	if f.Category == "" {
		f.Category = CategoryErrors
	}
	f.Search = strings.TrimSpace(f.Search)
	if len(f.Search) > 200 {
		f.Search = f.Search[:200]
	}
}

var hexPrefixRe = regexp.MustCompile(`^[0-9a-f]{6,64}$`)

// issueQuery builds the WHERE/ORDER BY for a filter. Every predicate is
// either on an indexed column or an IN over an indexed rollup table.
func issueQuery(f IssueFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, a ...any) { where = append(where, clause); args = append(args, a...) }

	if f.ProjectID > 0 {
		add("i.project_id = ?", f.ProjectID)
	}
	if f.Status != "" {
		add("i.status = ?", f.Status)
	}
	switch f.Category {
	case CategoryErrors:
		add("i.category IN ('exception', 'message')")
	case CategoryQueue:
		add("i.category = 'queue_failure'")
	}
	if f.Environment != "" {
		add("i.id IN (SELECT issue_id FROM issue_environments WHERE environment = ?)", f.Environment)
	}
	if f.Release != "" {
		add("i.id IN (SELECT issue_id FROM issue_releases WHERE release = ?)", f.Release)
	}
	if f.ExceptionType != "" {
		add("i.exception_type = ?", f.ExceptionType)
	}
	if !f.SeenFrom.IsZero() {
		add("i.last_seen_at >= ?", f.SeenFrom.UnixMilli())
	}
	if !f.SeenTo.IsZero() {
		add("i.first_seen_at <= ?", f.SeenTo.UnixMilli())
	}
	if f.Search != "" {
		clause, a := searchClause(f.Search)
		add(clause, a...)
	}

	q := " FROM issues i"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	switch f.Sort {
	case SortCount:
		q += " ORDER BY i.occurrence_count DESC, i.id"
	case SortFirstSeen:
		q += " ORDER BY i.first_seen_at DESC, i.id"
	default:
		q += " ORDER BY i.last_seen_at DESC, i.id"
	}
	return q, args
}

// searchClause matches the text (exception class, message, culprit, job
// and request path) through the trigram index, plus exact release names and
// fingerprint prefixes.
func searchClause(q string) (string, []any) {
	var (
		ors  []string
		args []any
	)
	if fts := ftsQuery(q); fts != "" {
		ors = append(ors, "i.id IN (SELECT rowid FROM issues_fts WHERE issues_fts MATCH ?)")
		args = append(args, fts)
	} else {
		// Terms under three characters can't use the trigram index; the
		// LIMIT keeps this fallback bounded.
		like := "%" + escapeLike(q) + "%"
		ors = append(ors, `(i.exception_type LIKE ? ESCAPE '\' OR i.message LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if lower := strings.ToLower(q); hexPrefixRe.MatchString(lower) {
		ors = append(ors, "(i.fingerprint >= ? AND i.fingerprint < ?)")
		args = append(args, lower, lower+"~")
	}
	ors = append(ors, "i.id IN (SELECT issue_id FROM issue_releases WHERE release = ?)")
	args = append(args, q)
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// ftsQuery turns user input into an FTS5 query: every term of 3+ characters
// must appear as a substring. Quoting makes punctuation literal.
func ftsQuery(q string) string {
	var terms []string
	for _, t := range strings.Fields(q) {
		if len([]rune(t)) < 3 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " AND ")
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const summaryColumns = `i.id, i.project_id, i.fingerprint, i.category, i.level, i.exception_type, i.message, i.culprit,
	COALESCE(i.job_name, ''), i.status, i.occurrence_count, i.first_seen_at, i.last_seen_at, COALESCE(i.last_release, '')`

// ListIssues returns one page of issues matching the filter.
func (s *Store) ListIssues(ctx context.Context, f IssueFilter) (IssuePage, error) {
	f.normalize()
	q, args := issueQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)

	// q is built only from constant fragments in issueQuery; values are bound parameters.
	rows, err := s.reader.QueryContext(ctx, "SELECT "+summaryColumns+q+" LIMIT ? OFFSET ?", args...) //nolint:gosec // G202: see above
	if err != nil {
		return IssuePage{}, err
	}
	defer func() { _ = rows.Close() }()

	page := IssuePage{Page: f.Page, PerPage: f.PerPage}
	for rows.Next() {
		var (
			is          IssueSummary
			first, last int64
		)
		if err := rows.Scan(&is.ID, &is.ProjectID, &is.Fingerprint, &is.Category, &is.Level, &is.ExceptionType,
			&is.Message, &is.Culprit, &is.JobName, &is.Status, &is.OccurrenceCount, &first, &last, &is.LastRelease); err != nil {
			return IssuePage{}, err
		}
		is.FirstSeenAt, is.LastSeenAt = fromMs(first), fromMs(last)
		page.Issues = append(page.Issues, is)
	}
	if err := rows.Err(); err != nil {
		return IssuePage{}, err
	}
	if len(page.Issues) > f.PerPage {
		page.Issues, page.HasNext = page.Issues[:f.PerPage], true
	}
	if err := s.attachTrends(ctx, page.Issues); err != nil {
		return IssuePage{}, err
	}
	return page, nil
}

// attachTrends loads the last TrendDays of daily counts for the listed
// issues in one indexed query.
func (s *Store) attachTrends(ctx context.Context, issues []IssueSummary) error {
	if len(issues) == 0 {
		return nil
	}
	today := s.now().UTC().UnixMilli() / dayMs
	first := today - TrendDays + 1
	ids := make([]any, 0, len(issues)+1)
	index := make(map[int64]int, len(issues))
	ph := make([]string, len(issues))
	for i, is := range issues {
		ids = append(ids, is.ID)
		index[is.ID] = i
		ph[i] = "?"
		issues[i].Trend = make([]int64, TrendDays)
	}
	ids = append(ids, first)
	rows, err := s.reader.QueryContext(ctx,
		`SELECT issue_id, day, count FROM issue_daily_counts WHERE issue_id IN (`+strings.Join(ph, ",")+`) AND day >= ?`, ids...) //nolint:gosec // G202: only "?" placeholders are joined
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, day, n int64
		if err := rows.Scan(&id, &day, &n); err != nil {
			return err
		}
		if i, ok := index[id]; ok && day-first < TrendDays {
			issues[i].Trend[day-first] = n
		}
	}
	return rows.Err()
}

// ExplainIssueList returns SQLite's query plan for a filter. Tests use it to
// check that list queries are served by indexes.
func (s *Store) ExplainIssueList(ctx context.Context, f IssueFilter) ([]string, error) {
	f.normalize()
	q, args := issueQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)
	rows, err := s.reader.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT "+summaryColumns+q+" LIMIT ? OFFSET ?", args...) //nolint:gosec // G202: constant fragments, bound values
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var (
			id, parent, notused int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

// ---- facets for filter controls ----

// Facets are the choices offered by the issue list's filter controls.
type Facets struct {
	Environments   []string
	Releases       []string
	ExceptionTypes []string
}

// IssueFacets lists known environments, the 50 most recent releases and the
// 50 most common exception types.
func (s *Store) IssueFacets(ctx context.Context, projectID int64) (Facets, error) {
	var f Facets
	scope, args := "", []any{}
	if projectID > 0 {
		scope, args = " WHERE i.project_id = ?", []any{projectID}
	}
	var err error
	if f.Environments, err = s.strings(ctx,
		`SELECT DISTINCT e.environment FROM issue_environments e JOIN issues i ON i.id = e.issue_id`+scope+` ORDER BY 1 LIMIT 50`, args...); err != nil {
		return f, err
	}
	if f.Releases, err = s.strings(ctx,
		`SELECT r.release FROM issue_releases r JOIN issues i ON i.id = r.issue_id`+scope+
			` GROUP BY r.release ORDER BY MAX(r.last_seen_at) DESC LIMIT 50`, args...); err != nil {
		return f, err
	}
	f.ExceptionTypes, err = s.strings(ctx,
		`SELECT i.exception_type FROM issues i`+scope+` GROUP BY i.exception_type ORDER BY COUNT(*) DESC, 1 LIMIT 50`, args...)
	return f, err
}

func (s *Store) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- overview ----

const dayMs = 24 * 60 * 60 * 1000

// DayCount is one day of the overview trend.
type DayCount struct {
	Day           time.Time
	Errors        int64
	QueueFailures int64
}

// Trend returns per-day event counts for the last n days, oldest first,
// with empty days filled in.
func (s *Store) Trend(ctx context.Context, days int, projectID int64) ([]DayCount, error) {
	today := s.now().UTC().UnixMilli() / dayMs
	first := today - int64(days) + 1
	// daily_totals is maintained at ingest, so this reads days x projects
	// rows instead of every issue's daily counts.
	query := `SELECT day, SUM(errors), SUM(queue_failures) FROM daily_totals WHERE day >= ?`
	args := []any{first}
	if projectID > 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` GROUP BY day`
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]DayCount, days)
	for i := range out {
		out[i].Day = time.UnixMilli((first + int64(i)) * dayMs).UTC()
	}
	for rows.Next() {
		var day, errs, queue int64
		if err := rows.Scan(&day, &errs, &queue); err != nil {
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(days) {
			out[i].Errors, out[i].QueueFailures = errs, queue
		}
	}
	return out, rows.Err()
}

// RecentFailure is a failed job for the overview list.
type RecentFailure struct {
	EventID  int64
	IssueID  int64
	JobName  string
	Queue    string
	Kind     string
	Message  string
	FailedAt time.Time
}

// RecentQueueFailures lists the latest failed jobs, for every project or one
// (projectID > 0).
func (s *Store) RecentQueueFailures(ctx context.Context, projectID int64, limit int) ([]RecentFailure, error) {
	query := `SELECT q.event_id, q.issue_id, q.job_name, COALESCE(q.queue_name, ''), q.kind, i.message, q.failed_at
	            FROM queue_failures q JOIN issues i ON i.id = q.issue_id
	           WHERE q.kind != 'attempt'`
	args := []any{}
	if projectID > 0 {
		query += ` AND q.project_id = ?`
		args = append(args, projectID)
	}
	args = append(args, limit)
	rows, err := s.reader.QueryContext(ctx, query+` ORDER BY q.failed_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RecentFailure
	for rows.Next() {
		var (
			r  RecentFailure
			at int64
		)
		if err := rows.Scan(&r.EventID, &r.IssueID, &r.JobName, &r.Queue, &r.Kind, &r.Message, &at); err != nil {
			return nil, err
		}
		r.FailedAt = fromMs(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- detail ----

// Breakdown is one row of an environment or release breakdown.
type Breakdown struct {
	Name       string
	Count      int64
	LastSeenAt time.Time
}

// IssueBreakdown returns the environment or release breakdown of an issue,
// most frequent first. which is "environment" or "release".
func (s *Store) IssueBreakdown(ctx context.Context, issueID int64, which string) ([]Breakdown, error) {
	table, col := "issue_environments", "environment"
	if which == "release" {
		table, col = "issue_releases", "release"
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+col+`, occurrence_count, last_seen_at FROM `+table+` WHERE issue_id = ? ORDER BY occurrence_count DESC LIMIT 20`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Breakdown
	for rows.Next() {
		var (
			b  Breakdown
			at int64
		)
		if err := rows.Scan(&b.Name, &b.Count, &at); err != nil {
			return nil, err
		}
		b.LastSeenAt = fromMs(at)
		out = append(out, b)
	}
	return out, rows.Err()
}

// IssueTrend returns an issue's daily counts for the last n days, oldest first.
func (s *Store) IssueTrend(ctx context.Context, issueID int64, days int) ([]DayCount, error) {
	counts, err := s.IssueDailyCounts(ctx, issueID)
	if err != nil {
		return nil, err
	}
	today := s.now().UTC().UnixMilli() / dayMs
	first := today - int64(days) + 1
	out := make([]DayCount, days)
	for i := range out {
		day := first + int64(i)
		out[i] = DayCount{Day: time.UnixMilli(day * dayMs).UTC(), Errors: counts[day]}
	}
	return out, nil
}

// EventDetail is one stored occurrence with everything the detail page shows.
type EventDetail struct {
	ID             int64
	UUID           string
	Level          string
	Environment    string
	Release        string
	ExceptionType  string
	Message        string
	Stacktrace     string
	RequestURL     string
	HTTPMethod     string
	HTTPStatus     int
	CorrelationID  string
	RequestContext string
	UserContext    string
	EventContext   string
	OccurredAt     time.Time
}

const eventDetailColumns = `id, event_uuid, level, environment, COALESCE(release, ''), exception_type, message, stacktrace,
	COALESCE(request_url, ''), COALESCE(http_method, ''), COALESCE(http_status, 0), COALESCE(correlation_id, ''),
	COALESCE(request_context, ''), COALESCE(user_context, ''), COALESCE(event_context, ''), occurred_at`

func scanEventDetail(row interface{ Scan(...any) error }) (EventDetail, error) {
	var (
		e  EventDetail
		at int64
	)
	err := row.Scan(&e.ID, &e.UUID, &e.Level, &e.Environment, &e.Release, &e.ExceptionType, &e.Message, &e.Stacktrace,
		&e.RequestURL, &e.HTTPMethod, &e.HTTPStatus, &e.CorrelationID, &e.RequestContext, &e.UserContext, &e.EventContext, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return EventDetail{}, ErrNotFound
	}
	e.OccurredAt = fromMs(at)
	return e, err
}

// IssueEventDetail returns one event of an issue by UUID, or the latest
// event when uuid is empty.
func (s *Store) IssueEventDetail(ctx context.Context, issueID int64, uuid string) (EventDetail, error) {
	if uuid == "" {
		return scanEventDetail(s.reader.QueryRowContext(ctx,
			`SELECT `+eventDetailColumns+` FROM events WHERE issue_id = ? ORDER BY occurred_at DESC, id DESC LIMIT 1`, issueID))
	}
	return scanEventDetail(s.reader.QueryRowContext(ctx,
		`SELECT `+eventDetailColumns+` FROM events WHERE issue_id = ? AND event_uuid = ?`, issueID, uuid))
}

// EventListItem is a row in an issue's event list.
type EventListItem struct {
	UUID        string
	Environment string
	Release     string
	RequestURL  string
	HTTPStatus  int
	OccurredAt  time.Time
}

// IssueEventList returns an issue's events, newest first, one page at a time.
func (s *Store) IssueEventList(ctx context.Context, issueID int64, limit, offset int) ([]EventListItem, bool, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT event_uuid, environment, COALESCE(release, ''), COALESCE(request_url, ''), COALESCE(http_status, 0), occurred_at
		   FROM events WHERE issue_id = ? ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`, issueID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []EventListItem
	for rows.Next() {
		var (
			e  EventListItem
			at int64
		)
		if err := rows.Scan(&e.UUID, &e.Environment, &e.Release, &e.RequestURL, &e.HTTPStatus, &at); err != nil {
			return nil, false, err
		}
		e.OccurredAt = fromMs(at)
		out = append(out, e)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, rows.Err()
}

// SortBreakdownByName orders a breakdown alphabetically (used for stable tests).
func SortBreakdownByName(b []Breakdown) {
	sort.Slice(b, func(i, j int) bool { return b[i].Name < b[j].Name })
}
