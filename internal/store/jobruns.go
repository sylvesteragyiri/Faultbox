package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Queue job runs (M10): one row per attempt of every job the monitored
// apps' workers run, successful ones included, plus a daily rollup per job
// class, queue and connection that the dashboard reads. Run rows are
// short-lived for successful attempts (retention.go); the rollup is kept as
// long as issues are.

// Job run outcomes.
const (
	JobSucceeded = "succeeded"
	JobReleased  = "released" // threw and will be retried
	JobFailed    = "failed"   // terminal: no retries left, or failed manually
	JobTimedOut  = "timed_out"
)

// JobOutcomes lists every outcome, for validation and filters.
var JobOutcomes = []string{JobSucceeded, JobReleased, JobFailed, JobTimedOut}

// JobKinds lists what a queued job can be: a plain job, or one of Laravel's
// wrappers for a queued notification, mailable, listener or broadcast.
var JobKinds = []string{"job", "notification", "mail", "listener", "broadcast"}

// JobNameCap bounds the distinct job names kept in the rollup per project
// and day. Past it, new names are counted under JobOtherName, so an app with
// dynamic job names can't grow the rollup without limit.
const (
	JobNameCap   = 1000
	JobOtherName = "(other)"
)

// JobBucketBounds are the upper bounds, in milliseconds, of the duration and
// queue-wait buckets (d0..d12 and w0..w12). A last bucket (d13, w13) holds
// everything slower. Percentiles are read from these, so they are
// approximate: "p95 ≤ 2.5 s".
var JobBucketBounds = [...]int64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10_000, 30_000, 60_000, 300_000}

const jobBuckets = len(JobBucketBounds) + 1

func jobBucket(ms int64) int {
	for i, b := range JobBucketBounds {
		if ms <= b {
			return i
		}
	}
	return len(JobBucketBounds)
}

// NewJobRun is one attempt of one queued job.
type NewJobRun struct {
	UUID          string
	JobUUID       string
	JobID         string
	Attempt       int
	MaxTries      *int
	Environment   string
	Release       string
	JobName       string
	Kind          string
	Queue         string
	Connection    string
	Outcome       string
	DurationMs    int64
	WaitMs        *int64 // nil when the job wasn't stamped at dispatch
	CorrelationID string
	BatchID       string // Laravel job batch
	StartedAt     time.Time
}

// jobAgg is one rollup key's totals within a telemetry batch.
type jobAgg struct {
	day                          int64
	env, name, queue, conn, kind string
	runs, succeeded, released    int64
	failed, timedOut             int64
	totalMs, maxMs               int64
	waited, totalWait, maxWait   int64
	d, w                         [jobBuckets]int64
	lastAt                       int64
	lastOutcome                  string
}

type jobAggKey struct {
	day                    int64
	env, name, queue, conn string
}

// jobRollupUpsert adds one jobAgg to queue_job_daily_counts.
var jobRollupUpsert = func() string {
	var cols, vals, sets []string
	for _, p := range []string{"d", "w"} {
		for i := range jobBuckets {
			c := p + strconv.Itoa(i)
			cols, vals = append(cols, c), append(vals, "?")
			sets = append(sets, c+" = "+c+" + excluded."+c)
		}
	}
	return `INSERT INTO queue_job_daily_counts (day, project_id, environment, job_name, queue_name, connection_name, kind,
	          runs, succeeded, released, failed, timed_out, total_ms, max_ms, waited, total_wait_ms, max_wait_ms,
	          last_run_at, last_outcome, ` + strings.Join(cols, ", ") + `)
	        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + strings.Join(vals, ", ") + `)
	        ON CONFLICT (day, project_id, environment, job_name, queue_name, connection_name) DO UPDATE SET
	          kind = CASE WHEN excluded.last_run_at >= last_run_at THEN excluded.kind ELSE kind END,
	          runs = runs + excluded.runs, succeeded = succeeded + excluded.succeeded,
	          released = released + excluded.released, failed = failed + excluded.failed,
	          timed_out = timed_out + excluded.timed_out, total_ms = total_ms + excluded.total_ms,
	          max_ms = MAX(max_ms, excluded.max_ms), waited = waited + excluded.waited,
	          total_wait_ms = total_wait_ms + excluded.total_wait_ms, max_wait_ms = MAX(max_wait_ms, excluded.max_wait_ms),
	          last_outcome = CASE WHEN excluded.last_run_at >= last_run_at THEN excluded.last_outcome ELSE last_outcome END,
	          last_run_at = MAX(last_run_at, excluded.last_run_at), ` + strings.Join(sets, ", ")
}()

// ingestJobRuns stores a batch's runs and adds them to the rollup: one
// insert per run through a prepared statement and one upsert per rollup key.
func ingestJobRuns(ctx context.Context, tx *sql.Tx, projectID int64, runs []NewJobRun) error {
	if len(runs) == 0 {
		return nil
	}
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO queue_job_runs (project_id, uuid, job_uuid, job_id, attempt, max_tries, environment, release, job_name, kind,
		                             queue_name, connection_name, outcome, duration_ms, wait_ms, correlation_id, batch_id,
		                             started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = insert.Close() }()

	names := newJobNameCap(tx, projectID)
	aggs := map[jobAggKey]*jobAgg{}
	var order []jobAggKey
	for _, r := range runs {
		started := r.StartedAt.UnixMilli()
		finished := started + r.DurationMs
		var wait any
		if r.WaitMs != nil {
			wait = *r.WaitMs
		}
		if _, err := insert.ExecContext(ctx,
			projectID, r.UUID, nullString(r.JobUUID), nullString(r.JobID), r.Attempt, nullInt(r.MaxTries), r.Environment,
			nullString(r.Release), r.JobName, r.Kind, r.Queue, r.Connection, r.Outcome, r.DurationMs, wait,
			nullString(r.CorrelationID), nullString(r.BatchID), started, finished); err != nil {
			return fmt.Errorf("insert job run %s: %w", r.UUID, err)
		}

		day := finished / dayMs
		name, err := names.resolve(ctx, day, r.JobName)
		if err != nil {
			return err
		}
		key := jobAggKey{day, r.Environment, name, r.Queue, r.Connection}
		a := aggs[key]
		if a == nil {
			a = &jobAgg{day: day, env: r.Environment, name: name, queue: r.Queue, conn: r.Connection}
			aggs[key] = a
			order = append(order, key)
		}
		a.runs++
		switch r.Outcome {
		case JobSucceeded:
			a.succeeded++
		case JobReleased:
			a.released++
		case JobFailed:
			a.failed++
		case JobTimedOut:
			a.timedOut++
		}
		a.totalMs += r.DurationMs
		a.maxMs = max(a.maxMs, r.DurationMs)
		a.d[jobBucket(r.DurationMs)]++
		if r.WaitMs != nil {
			a.waited++
			a.totalWait += *r.WaitMs
			a.maxWait = max(a.maxWait, *r.WaitMs)
			a.w[jobBucket(*r.WaitMs)]++
		}
		if finished >= a.lastAt {
			a.lastAt, a.lastOutcome, a.kind = finished, r.Outcome, r.Kind
		}
	}

	upsert, err := tx.PrepareContext(ctx, jobRollupUpsert)
	if err != nil {
		return err
	}
	defer func() { _ = upsert.Close() }()
	for _, key := range order {
		a := aggs[key]
		args := []any{a.day, projectID, a.env, a.name, a.queue, a.conn, a.kind,
			a.runs, a.succeeded, a.released, a.failed, a.timedOut, a.totalMs, a.maxMs, a.waited, a.totalWait, a.maxWait,
			a.lastAt, a.lastOutcome}
		for _, n := range a.d {
			args = append(args, n)
		}
		for _, n := range a.w {
			args = append(args, n)
		}
		if _, err := upsert.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("job run rollup: %w", err)
		}
	}
	return nil
}

// jobNameCap applies JobNameCap within one batch, reading each day's names
// at most once.
type jobNameCap struct {
	tx        *sql.Tx
	projectID int64
	days      map[int64]map[string]bool // names already in the rollup, or added by this batch
}

func newJobNameCap(tx *sql.Tx, projectID int64) *jobNameCap {
	return &jobNameCap{tx: tx, projectID: projectID, days: map[int64]map[string]bool{}}
}

// load reads the job names already in one day's rollup.
func (c *jobNameCap) load(ctx context.Context, day int64) (map[string]bool, error) {
	rows, err := c.tx.QueryContext(ctx,
		`SELECT DISTINCT job_name FROM queue_job_daily_counts WHERE day = ? AND project_id = ?`, day, c.projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	known := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		known[n] = true
	}
	return known, rows.Err()
}

func (c *jobNameCap) resolve(ctx context.Context, day int64, name string) (string, error) {
	known, ok := c.days[day]
	if !ok {
		var err error
		if known, err = c.load(ctx, day); err != nil {
			return "", fmt.Errorf("job names for day %d: %w", day, err)
		}
		c.days[day] = known
	}
	if known[name] {
		return name, nil
	}
	// (other) itself doesn't count against the cap.
	n := len(known)
	if known[JobOtherName] {
		n--
	}
	if n >= JobNameCap {
		known[JobOtherName] = true
		return JobOtherName, nil
	}
	known[name] = true
	return name, nil
}

// ---- rollup reads ----

// JobRunTotals are the rollup's counters over a window.
type JobRunTotals struct {
	Runs        int64
	Succeeded   int64
	Released    int64
	Failed      int64
	TimedOut    int64
	TotalMs     int64
	MaxMs       int64
	Waited      int64
	TotalWaitMs int64
	MaxWaitMs   int64
	D           [jobBuckets]int64
	W           [jobBuckets]int64
}

// SuccessPercent is Succeeded as a percentage of Runs, to one decimal.
func (t JobRunTotals) SuccessPercent() float64 { return percent1(t.Succeeded, t.Runs) }

// Unsuccessful counts runs that threw, failed or timed out.
func (t JobRunTotals) Unsuccessful() int64 { return t.Released + t.Failed + t.TimedOut }

// AvgMs is the mean run time.
func (t JobRunTotals) AvgMs() int64 {
	if t.Runs == 0 {
		return 0
	}
	return t.TotalMs / t.Runs
}

// AvgWaitMs is the mean queue wait of the runs that reported one.
func (t JobRunTotals) AvgWaitMs() int64 {
	if t.Waited == 0 {
		return 0
	}
	return t.TotalWaitMs / t.Waited
}

// P50Ms and P95Ms are approximate duration percentiles: the upper bound of
// the bucket holding that rank, capped at the slowest run seen.
func (t JobRunTotals) P50Ms() int64 { return bucketPercentile(t.D, t.Runs, 50, t.MaxMs) }

// P95Ms is the approximate 95th percentile duration.
func (t JobRunTotals) P95Ms() int64 { return bucketPercentile(t.D, t.Runs, 95, t.MaxMs) }

// P95WaitMs is the approximate 95th percentile queue wait.
func (t JobRunTotals) P95WaitMs() int64 { return bucketPercentile(t.W, t.Waited, 95, t.MaxWaitMs) }

func bucketPercentile(buckets [jobBuckets]int64, total int64, pct int64, maxSeen int64) int64 {
	if total <= 0 {
		return 0
	}
	rank := (total*pct + 99) / 100 // the ceil'd rank of the percentile
	var seen int64
	for i, n := range buckets {
		seen += n
		if seen >= rank {
			if i < len(JobBucketBounds) {
				return min(JobBucketBounds[i], maxSeen)
			}
			return maxSeen
		}
	}
	return maxSeen
}

// jobTotalsSelect sums every counter, in JobRunTotals.scan order.
var jobTotalsSelect = func() string {
	cols := []string{"COALESCE(SUM(runs), 0)", "COALESCE(SUM(succeeded), 0)", "COALESCE(SUM(released), 0)",
		"COALESCE(SUM(failed), 0)", "COALESCE(SUM(timed_out), 0)", "COALESCE(SUM(total_ms), 0)", "COALESCE(MAX(max_ms), 0)",
		"COALESCE(SUM(waited), 0)", "COALESCE(SUM(total_wait_ms), 0)", "COALESCE(MAX(max_wait_ms), 0)"}
	for _, p := range []string{"d", "w"} {
		for i := range jobBuckets {
			cols = append(cols, "COALESCE(SUM("+p+strconv.Itoa(i)+"), 0)")
		}
	}
	return strings.Join(cols, ", ")
}()

func (t *JobRunTotals) dest() []any {
	d := []any{&t.Runs, &t.Succeeded, &t.Released, &t.Failed, &t.TimedOut, &t.TotalMs, &t.MaxMs, &t.Waited, &t.TotalWaitMs, &t.MaxWaitMs}
	for i := range t.D {
		d = append(d, &t.D[i])
	}
	for i := range t.W {
		d = append(d, &t.W[i])
	}
	return d
}

// JobRunSummary totals the last `days` days.
func (s *Store) JobRunSummary(ctx context.Context, sc TelemetryScope, days int) (JobRunTotals, error) {
	var t JobRunTotals
	where, args := sc.where("day", s.firstDay(days))
	query := `SELECT ` + jobTotalsSelect + ` FROM queue_job_daily_counts` + where //nolint:gosec // G202: built from constants
	err := s.reader.QueryRowContext(ctx, query, args...).Scan(t.dest()...)
	return t, err
}

// JobRunTrend returns successful runs (Errors) and the rest (QueueFailures)
// per day, oldest first, so newBarChart can stack them.
func (s *Store) JobRunTrend(ctx context.Context, sc TelemetryScope, days int) ([]DayCount, error) {
	first := s.firstDay(days)
	where, args := sc.where("day", first)
	query := `SELECT day, SUM(succeeded), SUM(released + failed + timed_out) FROM queue_job_daily_counts` + where + ` GROUP BY day` //nolint:gosec // G202: built from constants
	return s.dayCounts(ctx, query, args, first, days)
}

// JobRunStat is one job class on one queue and connection over a window.
type JobRunStat struct {
	JobRunTotals
	JobName     string
	Kind        string
	Queue       string
	Connection  string
	LastRunAt   time.Time
	LastOutcome string
}

// JobRunStats lists job classes per queue over the last `days` days:
// those failing most first, then the busiest.
func (s *Store) JobRunStats(ctx context.Context, sc TelemetryScope, days, limit int) ([]JobRunStat, error) {
	where, args := sc.whereOn("d.", "day", s.firstDay(days))
	args = append(args, limit)
	// The last outcome and kind come from the row with the latest run: the
	// zero-padded time sorts as text, so MAX picks that row's suffix.
	query := `SELECT d.job_name, d.queue_name, d.connection_name, MAX(d.last_run_at), ` + jobTotalsSelect + `, ` + //nolint:gosec // G202: built from constants
		`SUBSTR(MAX(printf('%015d', d.last_run_at) || d.last_outcome || ' ' || d.kind), 16)
	  FROM queue_job_daily_counts d` + where +
		` GROUP BY d.job_name, d.queue_name, d.connection_name
	   ORDER BY SUM(d.failed + d.timed_out) DESC, SUM(d.released) DESC, SUM(d.runs) DESC, 1, 2, 3 LIMIT ?`
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []JobRunStat
	for rows.Next() {
		var (
			j    JobRunStat
			at   int64
			last sql.NullString
		)
		dest := append([]any{&j.JobName, &j.Queue, &j.Connection, &at}, j.dest()...)
		if err := rows.Scan(append(dest, &last)...); err != nil {
			return nil, err
		}
		j.LastRunAt = fromMs(at)
		j.LastOutcome, j.Kind, _ = strings.Cut(last.String, " ")
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobRunFacets are the choices the recent-runs filters offer: job names and
// queues seen in the rollup's window, busiest first.
type JobRunFacets struct {
	JobNames []string
	Queues   []string
}

// JobRunFacets reads the filter choices from the rollup, which is small.
func (s *Store) JobRunFacets(ctx context.Context, sc TelemetryScope, days int) (JobRunFacets, error) {
	var f JobRunFacets
	where, args := sc.where("day", s.firstDay(days))
	var err error
	if f.JobNames, err = s.strings(ctx, `SELECT job_name FROM queue_job_daily_counts`+where+` AND job_name != '`+JobOtherName+ //nolint:gosec // G202: constants
		`' GROUP BY job_name ORDER BY SUM(runs) DESC, 1 LIMIT 200`, args...); err != nil {
		return f, err
	}
	f.Queues, err = s.strings(ctx, `SELECT queue_name FROM queue_job_daily_counts`+where+ //nolint:gosec // G202: constants
		` AND queue_name != '' GROUP BY queue_name ORDER BY SUM(runs) DESC, 1 LIMIT 100`, args...)
	return f, err
}

// ---- recent runs ----

// JobRunFilter selects runs for the recent-runs list. Zero values mean "any".
type JobRunFilter struct {
	ProjectID   int64
	Environment string
	JobName     string
	Queue       string
	Kind        string
	Outcome     string
	Since       time.Time
	Page        int
	PerPage     int
}

func (f *JobRunFilter) normalize() {
	if f.PerPage <= 0 || f.PerPage > 100 {
		f.PerPage = 25
	}
	f.Page = max(1, min(f.Page, MaxIssuePage))
}

// JobRun is one stored attempt.
type JobRun struct {
	ID            int64
	ProjectID     int64
	UUID          string
	JobID         string
	Attempt       int
	MaxTries      int
	Environment   string
	JobName       string
	Kind          string
	Queue         string
	Connection    string
	Outcome       string
	DurationMs    int64
	WaitMs        int64
	HasWait       bool
	CorrelationID string
	StartedAt     time.Time
	FinishedAt    time.Time
	// FailureEventID and IssueID link a failed or released run to its
	// failed-job row and issue; 0 when Faultbox has no matching failure.
	FailureEventID int64
	IssueID        int64
}

// JobRunPage is one page of runs.
type JobRunPage struct {
	Runs    []JobRun
	Page    int
	PerPage int
	HasNext bool
}

func jobRunQuery(f JobRunFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, a ...any) { where = append(where, clause); args = append(args, a...) }
	if f.ProjectID > 0 {
		add("r.project_id = ?", f.ProjectID)
	}
	if f.JobName != "" {
		add("r.job_name = ?", f.JobName)
	}
	if f.Outcome != "" {
		add("r.outcome = ?", f.Outcome)
	}
	if f.Queue != "" {
		add("r.queue_name = ?", f.Queue)
	}
	if f.Kind != "" {
		add("r.kind = ?", f.Kind)
	}
	if f.Environment != "" {
		add("r.environment = ?", f.Environment)
	}
	if !f.Since.IsZero() {
		add("r.finished_at >= ?", f.Since.UnixMilli())
	}
	q := " FROM queue_job_runs r"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	return q + " ORDER BY r.finished_at DESC, r.id DESC", args
}

// The failure link is a lookup per listed run (at most a page), through
// idx_queue_failures_project_job_id.
const jobRunColumns = `r.id, r.project_id, r.uuid, COALESCE(r.job_id, ''), r.attempt, COALESCE(r.max_tries, 0), r.environment,
	r.job_name, r.kind, r.queue_name, r.connection_name, r.outcome, r.duration_ms, r.wait_ms, COALESCE(r.correlation_id, ''),
	r.started_at, r.finished_at,
	(SELECT q.event_id || ' ' || q.issue_id FROM queue_failures q
	  WHERE r.outcome != 'succeeded' AND r.job_id IS NOT NULL
	    AND q.project_id = r.project_id AND q.job_id = r.job_id AND q.attempts = r.attempt
	  ORDER BY q.failed_at DESC LIMIT 1)`

// ListJobRuns returns one page of runs, newest first.
func (s *Store) ListJobRuns(ctx context.Context, f JobRunFilter) (JobRunPage, error) {
	f.normalize()
	q, args := jobRunQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)
	rows, err := s.reader.QueryContext(ctx, "SELECT "+jobRunColumns+q+" LIMIT ? OFFSET ?", args...) //nolint:gosec // G202: constant fragments
	if err != nil {
		return JobRunPage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := JobRunPage{Page: f.Page, PerPage: f.PerPage}
	for rows.Next() {
		var (
			r                 JobRun
			wait              sql.NullInt64
			link              sql.NullString
			started, finished int64
		)
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.UUID, &r.JobID, &r.Attempt, &r.MaxTries, &r.Environment, &r.JobName, &r.Kind,
			&r.Queue, &r.Connection, &r.Outcome, &r.DurationMs, &wait, &r.CorrelationID, &started, &finished, &link); err != nil {
			return JobRunPage{}, err
		}
		r.WaitMs, r.HasWait = wait.Int64, wait.Valid
		r.StartedAt, r.FinishedAt = fromMs(started), fromMs(finished)
		if ev, is, ok := strings.Cut(link.String, " "); ok {
			r.FailureEventID, _ = strconv.ParseInt(ev, 10, 64)
			r.IssueID, _ = strconv.ParseInt(is, 10, 64)
		}
		page.Runs = append(page.Runs, r)
	}
	if err := rows.Err(); err != nil {
		return JobRunPage{}, err
	}
	if len(page.Runs) > f.PerPage {
		page.Runs, page.HasNext = page.Runs[:f.PerPage], true
	}
	return page, nil
}

// ExplainJobRunList returns SQLite's query plan for a filter, for tests.
func (s *Store) ExplainJobRunList(ctx context.Context, f JobRunFilter) ([]string, error) {
	f.normalize()
	q, args := jobRunQuery(f)
	args = append(args, f.PerPage+1, (f.Page-1)*f.PerPage)
	return s.explain(ctx, "SELECT "+jobRunColumns+q+" LIMIT ? OFFSET ?", args...)
}

// OldestSuccessfulRun returns when the oldest stored successful run
// finished, or the zero time when none is stored. The recent-runs list
// uses it to explain where older successful runs went.
func (s *Store) OldestSuccessfulRun(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	err := s.reader.QueryRowContext(ctx,
		`SELECT MIN(finished_at) FROM queue_job_runs WHERE outcome = 'succeeded'`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return fromNullMs(at), err
}
