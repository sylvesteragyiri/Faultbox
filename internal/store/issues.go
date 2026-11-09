package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Issue is a group of events with the same fingerprint (PRD §4.3).
type Issue struct {
	ID               int64
	ProjectID        int64
	Fingerprint      string
	Category         string
	Level            string
	ExceptionType    string
	Message          string
	Culprit          string
	JobName          string
	Status           string
	OccurrenceCount  int64
	FirstSeenAt      time.Time
	LastSeenAt       time.Time
	ResolvedAt       time.Time
	FirstRelease     string
	LastRelease      string
	SampleEventUUID  string
	SampleStacktrace string
	SampleContext    string
}

const issueColumns = `id, project_id, fingerprint, category, level, exception_type, message, culprit,
	COALESCE(job_name, ''), status, occurrence_count, first_seen_at, last_seen_at, resolved_at,
	COALESCE(first_release, ''), COALESCE(last_release, ''), sample_event_uuid, sample_stacktrace, sample_context`

func scanIssue(row interface{ Scan(...any) error }) (Issue, error) {
	var (
		i           Issue
		first, last int64
		resolved    sql.NullInt64
	)
	err := row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Category, &i.Level, &i.ExceptionType, &i.Message,
		&i.Culprit, &i.JobName, &i.Status, &i.OccurrenceCount, &first, &last, &resolved,
		&i.FirstRelease, &i.LastRelease, &i.SampleEventUUID, &i.SampleStacktrace, &i.SampleContext)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, err
	}
	i.FirstSeenAt, i.LastSeenAt, i.ResolvedAt = fromMs(first), fromMs(last), fromNullMs(resolved)
	return i, nil
}

// Issue returns one issue.
func (s *Store) Issue(ctx context.Context, id int64) (Issue, error) {
	return scanIssue(s.reader.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
}

// ProjectIssues lists a project's issues, most recently seen first. M4 adds
// filters and pagination on top.
func (s *Store) ProjectIssues(ctx context.Context, projectID int64, limit int) ([]Issue, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues WHERE project_id = ? ORDER BY last_seen_at DESC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// SetIssueStatus resolves, ignores or reopens an issue.
func (s *Store) SetIssueStatus(ctx context.Context, id int64, status string) error {
	if status != IssueUnresolved && status != IssueResolved && status != IssueIgnored {
		return invalid("unknown issue status %q", status)
	}
	now := s.nowMs()
	var resolvedAt any
	if status == IssueResolved {
		resolvedAt = now
	}
	return s.execOne(ctx, `UPDATE issues SET status = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
		status, resolvedAt, now, id)
}

// IssueStats are the overview counters.
type IssueStats struct {
	Total, Unresolved, NewToday int64
	Events, QueueFailures       int64
}

// Stats counts issues and events across all projects.
func (s *Store) Stats(ctx context.Context) (IssueStats, error) {
	var st IssueStats
	dayStart := s.now().UTC().Truncate(24 * time.Hour).UnixMilli()
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(status = 'unresolved'), 0), COALESCE(SUM(first_seen_at >= ?), 0),
		        COALESCE(SUM(occurrence_count), 0), COALESCE(SUM(CASE WHEN category = 'queue_failure' THEN occurrence_count ELSE 0 END), 0)
		   FROM issues`, dayStart,
	).Scan(&st.Total, &st.Unresolved, &st.NewToday, &st.Events, &st.QueueFailures)
	return st, err
}

// StoredEvent is one occurrence, as stored.
type StoredEvent struct {
	ID             int64
	IssueID        int64
	UUID           string
	Type           string
	Environment    string
	Release        string
	Message        string
	Stacktrace     string
	RequestURL     string
	HTTPStatus     int
	UserContext    string
	EventContext   string
	RequestContext string
	OccurredAt     time.Time
}

// IssueEvents lists an issue's events, newest first.
func (s *Store) IssueEvents(ctx context.Context, issueID int64, limit int) ([]StoredEvent, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, issue_id, event_uuid, event_type, environment, COALESCE(release, ''), message, stacktrace,
		        COALESCE(request_url, ''), COALESCE(http_status, 0), COALESCE(user_context, ''),
		        COALESCE(event_context, ''), COALESCE(request_context, ''), occurred_at
		   FROM events WHERE issue_id = ? ORDER BY occurred_at DESC, id DESC LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StoredEvent
	for rows.Next() {
		var (
			e  StoredEvent
			at int64
		)
		if err := rows.Scan(&e.ID, &e.IssueID, &e.UUID, &e.Type, &e.Environment, &e.Release, &e.Message,
			&e.Stacktrace, &e.RequestURL, &e.HTTPStatus, &e.UserContext, &e.EventContext, &e.RequestContext, &at); err != nil {
			return nil, err
		}
		e.OccurredAt = fromMs(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueueFailure is the queue-specific record of a failed job event.
type QueueFailure struct {
	EventID    int64
	IssueID    int64
	Kind       string
	JobName    string
	JobID      string
	Queue      string
	Connection string
	Attempts   int
	FailedAt   time.Time
	Payload    string
}

// IssueQueueFailures lists the failed-job records of a queue issue.
func (s *Store) IssueQueueFailures(ctx context.Context, issueID int64, limit int) ([]QueueFailure, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT event_id, issue_id, kind, job_name, COALESCE(job_id, ''), COALESCE(queue_name, ''),
		        COALESCE(connection_name, ''), COALESCE(attempts, 0), failed_at, COALESCE(payload_metadata, '')
		   FROM queue_failures WHERE issue_id = ? ORDER BY failed_at DESC LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QueueFailure
	for rows.Next() {
		var (
			q  QueueFailure
			at int64
		)
		if err := rows.Scan(&q.EventID, &q.IssueID, &q.Kind, &q.JobName, &q.JobID, &q.Queue, &q.Connection,
			&q.Attempts, &at, &q.Payload); err != nil {
			return nil, err
		}
		q.FailedAt = fromMs(at)
		out = append(out, q)
	}
	return out, rows.Err()
}

// IssueReleases returns per-release counts for an issue.
func (s *Store) IssueReleases(ctx context.Context, issueID int64) (map[string]int64, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT release, occurrence_count FROM issue_releases WHERE issue_id = ?`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var (
			r string
			n int64
		)
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		out[r] = n
	}
	return out, rows.Err()
}

// IssueDailyCounts returns counts per UTC day (days since the epoch).
func (s *Store) IssueDailyCounts(ctx context.Context, issueID int64) (map[int64]int64, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT day, count FROM issue_daily_counts WHERE issue_id = ?`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var d, n int64
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}
