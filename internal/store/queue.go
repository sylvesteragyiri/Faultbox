package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Failed-job kinds for the queue views. A "failure" is a terminal or manual
// failure (the job will not run again); an "attempt" is a retry that failed.
const (
	QueueKindFailures = "failures" // terminal and manual (the default)
	QueueKindAttempts = "attempt"
	QueueKindAll      = "all"
)

// QueueFilter selects failed jobs for the list view. Zero values mean "any".
type QueueFilter struct {
	ProjectID   int64
	Queue       string
	Connection  string
	JobName     string
	Environment string
	Kind        string // failures, attempt or all ("" = failures)
	Since       time.Time
	Page        int
	PerPage     int
}

func (f *QueueFilter) normalize() {
	if f.PerPage <= 0 || f.PerPage > 100 {
		f.PerPage = 25
	}
	f.Page = max(1, min(f.Page, MaxIssuePage))
	switch f.Kind {
	case QueueKindAttempts, QueueKindAll:
	default:
		f.Kind = QueueKindFailures
	}
}

// FailedJob is one failed job for lists, with its issue's exception.
type FailedJob struct {
	EventID       int64
	IssueID       int64
	ProjectID     int64
	Kind          string
	JobName       string
	JobID         string
	Queue         string
	Connection    string
	Environment   string
	Attempts      int
	MaxTries      int
	FailedAt      time.Time
	ExceptionType string
	Message       string
	IssueStatus   string
}

// FailedJobPage is one page of failed jobs.
type FailedJobPage struct {
	Jobs    []FailedJob
	Page    int
	PerPage int
	HasNext bool
}

// queueQuery builds the FROM/WHERE/ORDER BY for a filter. Each equality
// filter has an index ending in failed_at, so pages read in order.
func queueQuery(f QueueFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, a ...any) { where = append(where, clause); args = append(args, a...) }
	if f.ProjectID > 0 {
		add("q.project_id = ?", f.ProjectID)
	}
	if f.Queue != "" {
		add("q.queue_name = ?", f.Queue)
	}
	if f.Connection != "" {
		add("q.connection_name = ?", f.Connection)
	}
	if f.JobName != "" {
		add("q.job_name = ?", f.JobName)
	}
	if f.Environment != "" {
		add("q.environment = ?", f.Environment)
	}
	switch f.Kind {
	case QueueKindFailures:
		add("q.kind != 'attempt'")
	case QueueKindAttempts:
		add("q.kind = 'attempt'")
	}
	if !f.Since.IsZero() {
		add("q.failed_at >= ?", f.Since.UnixMilli())
	}
	q := " FROM queue_failures q JOIN issues i ON i.id = q.issue_id"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// rowid ascending matches the implicit tail of every failed_at index.
	return q + " ORDER BY q.failed_at DESC, q.id", args
}

const failedJobColumns = `q.event_id, q.issue_id, q.project_id, q.kind, q.job_name, COALESCE(q.job_id, ''),
	COALESCE(q.queue_name, ''), COALESCE(q.connection_name, ''), q.environment, COALESCE(q.attempts, 0),
	COALESCE(q.max_tries, 0), q.failed_at, i.exception_type, i.message, i.status`

func scanFailedJob(row interface{ Scan(...any) error }) (FailedJob, error) {
	var (
		j  FailedJob
		at int64
	)
	err := row.Scan(&j.EventID, &j.IssueID, &j.ProjectID, &j.Kind, &j.JobName, &j.JobID, &j.Queue, &j.Connection,
		&j.Environment, &j.Attempts, &j.MaxTries, &at, &j.ExceptionType, &j.Message, &j.IssueStatus)
	j.FailedAt = fromMs(at)
	return j, err
}

// ListFailedJobs returns one page of failed jobs, newest first.
func (s *Store) ListFailedJobs(ctx context.Context, f QueueFilter) (FailedJobPage, error) {
	f.normalize()
	q, args := queueQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)
	// q is built only from constant fragments in queueQuery; values are bound parameters.
	rows, err := s.reader.QueryContext(ctx, "SELECT "+failedJobColumns+q+" LIMIT ? OFFSET ?", args...) //nolint:gosec // G202: see above
	if err != nil {
		return FailedJobPage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := FailedJobPage{Page: f.Page, PerPage: f.PerPage}
	for rows.Next() {
		j, err := scanFailedJob(rows)
		if err != nil {
			return FailedJobPage{}, err
		}
		page.Jobs = append(page.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return FailedJobPage{}, err
	}
	if len(page.Jobs) > f.PerPage {
		page.Jobs, page.HasNext = page.Jobs[:f.PerPage], true
	}
	return page, nil
}

// ExplainFailedJobList returns SQLite's query plan for a filter, for tests.
func (s *Store) ExplainFailedJobList(ctx context.Context, f QueueFilter) ([]string, error) {
	f.normalize()
	q, args := queueQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)
	return s.explain(ctx, "SELECT "+failedJobColumns+q+" LIMIT ? OFFSET ?", args...)
}

func (s *Store) explain(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
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

// QueueFacets are the choices offered by the failed-job filters.
type QueueFacets struct {
	Queues       []string
	Connections  []string
	JobNames     []string
	Environments []string
}

// FailedJobFacets lists the queues, connections, job classes and
// environments seen in failed jobs, most frequent first.
func (s *Store) FailedJobFacets(ctx context.Context, projectID int64) (QueueFacets, error) {
	var f QueueFacets
	scope, args := "", []any{}
	if projectID > 0 {
		scope, args = " AND project_id = ?", []any{projectID}
	}
	facet := func(col string) ([]string, error) {
		// col is one of the constant column names below.
		return s.strings(ctx, `SELECT `+col+` FROM queue_failures WHERE `+col+` IS NOT NULL AND `+col+` != ''`+scope+
			` GROUP BY `+col+` ORDER BY COUNT(*) DESC, 1 LIMIT 100`, args...)
	}
	var err error
	if f.Queues, err = facet("queue_name"); err != nil {
		return f, err
	}
	if f.Connections, err = facet("connection_name"); err != nil {
		return f, err
	}
	if f.JobNames, err = facet("job_name"); err != nil {
		return f, err
	}
	f.Environments, err = facet("environment")
	return f, err
}

// ---- dashboard ----
//
// The dashboard reads queue_daily_counts, a per-day rollup maintained by the
// ingest transaction, so its cost doesn't grow with failure volume. Windows
// are whole UTC days: the last n days including today.

// rollupScope returns the WHERE clause and arguments selecting the last
// days of the rollup, optionally for one project.
func (s *Store) rollupScope(projectID int64, days int) (string, []any) {
	first := s.now().UTC().UnixMilli()/dayMs - int64(days) + 1
	if projectID > 0 {
		return ` WHERE project_id = ? AND day >= ?`, []any{projectID, first}
	}
	return ` WHERE day >= ?`, []any{first}
}

// QueueSummary holds the queue dashboard's counters.
type QueueSummary struct {
	Failures   int64 // terminal and manual failures in the window
	Attempts   int64 // failed retry attempts in the window
	JobTypes   int64 // distinct job classes that failed in the window
	Unresolved int64 // unresolved queue failure issues, all time
}

// QueueSummary counts failed jobs over the last days, optionally for one project.
func (s *Store) QueueSummary(ctx context.Context, projectID int64, days int) (QueueSummary, error) {
	var sum QueueSummary
	where, args := s.rollupScope(projectID, days)
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(failures), 0), COALESCE(SUM(attempts), 0), COUNT(DISTINCT job_name) FROM queue_daily_counts`+where,
		args...).Scan(&sum.Failures, &sum.Attempts, &sum.JobTypes); err != nil {
		return sum, err
	}
	query := `SELECT COUNT(*) FROM issues WHERE status = 'unresolved' AND category = 'queue_failure'`
	args = nil
	if projectID > 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	err := s.reader.QueryRowContext(ctx, query, args...).Scan(&sum.Unresolved)
	return sum, err
}

// JobStat is one job class on the queue dashboard.
type JobStat struct {
	JobName      string
	Failures     int64 // terminal and manual
	Attempts     int64 // failed retries
	Issues       int64 // distinct exception groups for this job, all time
	LastFailedAt time.Time
}

// TopFailingJobs lists the job classes with the most failed runs (failures
// and retries) over the last days.
func (s *Store) TopFailingJobs(ctx context.Context, projectID int64, days, limit int) ([]JobStat, error) {
	where, args := s.rollupScope(projectID, days)
	args = append(args, limit)
	const topSelect = `SELECT job_name, SUM(failures), SUM(attempts), MAX(last_failed_at) FROM queue_daily_counts`
	const topOrder = ` GROUP BY job_name ORDER BY SUM(failures) + SUM(attempts) DESC, job_name LIMIT ?`
	query := topSelect + where + topOrder //nolint:gosec // G202: where is a constant from rollupScope; values are bound
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var (
		out   []JobStat
		names []any
	)
	for rows.Next() {
		var (
			j  JobStat
			at int64
		)
		if err := rows.Scan(&j.JobName, &j.Failures, &j.Attempts, &at); err != nil {
			return nil, err
		}
		j.LastFailedAt = fromMs(at)
		out = append(out, j)
		names = append(names, j.JobName)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	// How many exception groups each listed job has.
	query = `SELECT job_name, COUNT(*) FROM issues WHERE category = 'queue_failure' AND job_name IN (?` +
		strings.Repeat(",?", len(names)-1) + `)`
	if projectID > 0 {
		query += ` AND project_id = ?`
		names = append(names, projectID)
	}
	counts, err := s.reader.QueryContext(ctx, query+` GROUP BY job_name`, names...) //nolint:gosec // G202: only "?" placeholders are added
	if err != nil {
		return nil, err
	}
	defer func() { _ = counts.Close() }()
	byName := map[string]int64{}
	for counts.Next() {
		var (
			name string
			n    int64
		)
		if err := counts.Scan(&name, &n); err != nil {
			return nil, err
		}
		byName[name] = n
	}
	for i := range out {
		out[i].Issues = byName[out[i].JobName]
	}
	return out, counts.Err()
}

// QueueBreakdown counts failed runs over the last days by queue or
// connection, most frequent first. by is "queue" or "connection".
func (s *Store) QueueBreakdown(ctx context.Context, projectID int64, days int, by string) ([]Breakdown, error) {
	col := "queue_name"
	if by == "connection" {
		col = "connection_name"
	}
	where, args := s.rollupScope(projectID, days)
	label := `CASE WHEN ` + col + ` = '' THEN '(none)' ELSE ` + col + ` END`
	query := `SELECT ` + label + `, SUM(failures + attempts), MAX(last_failed_at) FROM queue_daily_counts` + //nolint:gosec // G202: col is one of two constants, where comes from rollupScope
		where + ` GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 10`
	rows, err := s.reader.QueryContext(ctx, query, args...)
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

// EnvironmentRate is the share of an environment's events that are failed jobs.
type EnvironmentRate struct {
	Environment   string
	QueueFailures int64
	Events        int64
}

// Percent is QueueFailures as a whole-number percentage of Events.
func (r EnvironmentRate) Percent() int64 {
	if r.Events <= 0 {
		return 0
	}
	return (r.QueueFailures*100 + r.Events/2) / r.Events
}

// QueueFailureRates returns, per environment, how many events were failed
// jobs out of all events. It reads the issue_environments rollup, so it
// covers every event ever received, not only those still retained.
func (s *Store) QueueFailureRates(ctx context.Context, projectID int64) ([]EnvironmentRate, error) {
	query := `SELECT e.environment,
	                 SUM(CASE WHEN i.category = 'queue_failure' THEN e.occurrence_count ELSE 0 END),
	                 SUM(e.occurrence_count)
	            FROM issue_environments e JOIN issues i ON i.id = e.issue_id`
	var args []any
	if projectID > 0 {
		query += ` WHERE i.project_id = ?`
		args = append(args, projectID)
	}
	query += ` GROUP BY e.environment ORDER BY 2 DESC, 3 DESC, 1 LIMIT 10`
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EnvironmentRate
	for rows.Next() {
		var r EnvironmentRate
		if err := rows.Scan(&r.Environment, &r.QueueFailures, &r.Events); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueueTrend returns per-day failed jobs for the last n days, oldest first:
// terminal and manual failures in Errors, failed retries in QueueFailures,
// so newBarChart can stack them.
func (s *Store) QueueTrend(ctx context.Context, projectID int64, days int) ([]DayCount, error) {
	first := s.now().UTC().UnixMilli()/dayMs - int64(days) + 1
	where, args := s.rollupScope(projectID, days)
	query := `SELECT day, SUM(failures), SUM(attempts) FROM queue_daily_counts` + where + ` GROUP BY day` //nolint:gosec // G202: where is a constant from rollupScope; values are bound
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
		var day, failures, attempts int64
		if err := rows.Scan(&day, &failures, &attempts); err != nil {
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(days) {
			out[i].Errors, out[i].QueueFailures = failures, attempts
		}
	}
	return out, rows.Err()
}

// ---- detail ----

// FailedJobDetail is one failed job with its event.
type FailedJobDetail struct {
	FailedJob
	BatchID string
	Payload string // sanitized JSON, or ""
	Event   EventDetail
}

// FailedJob returns one failed job by its event ID.
func (s *Store) FailedJob(ctx context.Context, eventID int64) (FailedJobDetail, error) {
	var d FailedJobDetail
	row := s.reader.QueryRowContext(ctx,
		`SELECT `+failedJobColumns+`, COALESCE(q.batch_id, ''), COALESCE(q.payload_metadata, '')
		   FROM queue_failures q JOIN issues i ON i.id = q.issue_id WHERE q.event_id = ?`, eventID)
	var (
		j  FailedJob
		at int64
	)
	err := row.Scan(&j.EventID, &j.IssueID, &j.ProjectID, &j.Kind, &j.JobName, &j.JobID, &j.Queue, &j.Connection,
		&j.Environment, &j.Attempts, &j.MaxTries, &at, &j.ExceptionType, &j.Message, &j.IssueStatus, &d.BatchID, &d.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	j.FailedAt = fromMs(at)
	d.FailedJob = j
	d.Event, err = scanEventDetail(s.reader.QueryRowContext(ctx, `SELECT `+eventDetailColumns+` FROM events WHERE id = ?`, eventID))
	return d, err
}

// JobFailures lists recent failures of one job class in a project, newest
// first, for the detail page's history.
func (s *Store) JobFailures(ctx context.Context, projectID int64, jobName string, limit int) ([]FailedJob, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+failedJobColumns+` FROM queue_failures q JOIN issues i ON i.id = q.issue_id
		  WHERE q.project_id = ? AND q.job_name = ? ORDER BY q.failed_at DESC, q.id LIMIT ?`, projectID, jobName, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FailedJob
	for rows.Next() {
		j, err := scanFailedJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
