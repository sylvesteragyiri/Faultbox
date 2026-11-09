package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Issue statuses (PRD §4.4).
const (
	IssueUnresolved = "unresolved"
	IssueResolved   = "resolved"
	IssueIgnored    = "ignored"
)

// maxIssueMessageLen bounds the message copied onto the issue row.
const maxIssueMessageLen = 1000

// NewEvent is a processed event ready to store.
type NewEvent struct {
	ProjectID      int64
	UUID           string
	Category       string // exception, message or queue_failure
	Level          string
	Environment    string
	Release        string
	Fingerprint    string
	ExceptionType  string
	Message        string
	Culprit        string
	Stacktrace     string // JSON
	RequestURL     string
	HTTPMethod     string
	HTTPStatus     int // 0 when unknown
	CorrelationID  string
	RequestContext string // JSON or ""
	UserContext    string // JSON or ""
	EventContext   string // JSON or ""
	SampleContext  string // JSON copied to the issue (D13)
	OccurredAt     time.Time
	Job            *NewQueueFailure
}

// NewQueueFailure is the queue-specific part of a NewEvent.
type NewQueueFailure struct {
	Kind       string
	Name       string
	ID         string
	Queue      string
	Connection string
	Attempts   *int
	MaxTries   *int
	BatchID    string
	Payload    string // JSON or ""
}

// IngestResult says what storing one event did. Notifications (M6) use it.
type IngestResult struct {
	UUID      string
	IssueID   int64
	EventID   int64
	Duplicate bool // event_id already stored; nothing changed
	NewIssue  bool // first event of a new issue
	Reopened  bool // a resolved issue occurred again (regression)
	Status    string
	Count     int64 // issue occurrence count after this event
}

// IngestBatch stores events in one transaction. Duplicate event IDs are
// skipped (D11). Either every event in the batch is stored or none is.
func (s *Store) IngestBatch(ctx context.Context, events []NewEvent) ([]IngestResult, error) {
	results := make([]IngestResult, len(events))
	now := s.nowMs()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		lastEvent := map[int64]int64{}
		for i := range events {
			r, err := ingestOne(ctx, tx, &events[i], now)
			if err != nil {
				return fmt.Errorf("event %s: %w", events[i].UUID, err)
			}
			results[i] = r
			if !r.Duplicate {
				if ms := events[i].OccurredAt.UnixMilli(); ms > lastEvent[events[i].ProjectID] {
					lastEvent[events[i].ProjectID] = ms
				}
			}
		}
		for projectID, ms := range lastEvent {
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects SET last_event_at = MAX(COALESCE(last_event_at, 0), ?) WHERE id = ?`,
				ms, projectID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func ingestOne(ctx context.Context, tx *sql.Tx, e *NewEvent, now int64) (IngestResult, error) {
	res := IngestResult{UUID: e.UUID}

	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE project_id = ? AND event_uuid = ?`, e.ProjectID, e.UUID).Scan(&exists)
	if err == nil {
		res.Duplicate = true
		return res, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}

	occurred := e.OccurredAt.UnixMilli()
	issueMessage := e.Message
	if len(issueMessage) > maxIssueMessageLen {
		issueMessage = issueMessage[:maxIssueMessageLen]
	}
	var jobName any
	if e.Job != nil {
		jobName = e.Job.Name
	}

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT id, status, occurrence_count FROM issues WHERE project_id = ? AND fingerprint = ?`,
		e.ProjectID, e.Fingerprint).Scan(&res.IssueID, &status, &res.Count)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r, err := tx.ExecContext(ctx,
			`INSERT INTO issues (project_id, fingerprint, category, level, exception_type, message, culprit, job_name,
			                     status, occurrence_count, first_seen_at, last_seen_at, first_release, last_release,
			                     sample_event_uuid, sample_stacktrace, sample_context, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'unresolved', 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ProjectID, e.Fingerprint, e.Category, e.Level, e.ExceptionType, issueMessage, e.Culprit, jobName,
			occurred, occurred, nullString(e.Release), nullString(e.Release),
			e.UUID, e.Stacktrace, e.SampleContext, now, now)
		if err != nil {
			return res, err
		}
		if res.IssueID, err = r.LastInsertId(); err != nil {
			return res, err
		}
		res.NewIssue, res.Status, res.Count = true, IssueUnresolved, 1
	case err != nil:
		return res, err
	default:
		res.Reopened = status == IssueResolved
		res.Status = status
		if res.Reopened {
			res.Status = IssueUnresolved
		}
		res.Count++
		// A regression refreshes the sample so the issue shows the trace
		// that brought it back (D13).
		_, err = tx.ExecContext(ctx,
			`UPDATE issues SET
			   occurrence_count = occurrence_count + 1,
			   first_seen_at    = MIN(first_seen_at, ?1),
			   last_seen_at     = MAX(last_seen_at, ?1),
			   last_release     = COALESCE(?2, last_release),
			   first_release    = COALESCE(first_release, ?2),
			   status           = CASE WHEN status = 'resolved' THEN 'unresolved' ELSE status END,
			   resolved_at      = CASE WHEN status = 'resolved' THEN NULL ELSE resolved_at END,
			   sample_event_uuid = CASE WHEN ?3 THEN ?4 ELSE sample_event_uuid END,
			   sample_stacktrace = CASE WHEN ?3 THEN ?5 ELSE sample_stacktrace END,
			   sample_context    = CASE WHEN ?3 THEN ?6 ELSE sample_context END,
			   updated_at       = ?7
			 WHERE id = ?8`,
			occurred, nullString(e.Release), res.Reopened, e.UUID, e.Stacktrace, e.SampleContext, now, res.IssueID)
		if err != nil {
			return res, err
		}
	}

	var httpStatus any
	if e.HTTPStatus > 0 {
		httpStatus = e.HTTPStatus
	}
	r, err := tx.ExecContext(ctx,
		`INSERT INTO events (project_id, issue_id, event_uuid, event_type, level, environment, release, fingerprint,
		                     exception_type, message, stacktrace, request_url, http_method, http_status, correlation_id,
		                     request_context, user_context, event_context, occurred_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ProjectID, res.IssueID, e.UUID, e.Category, e.Level, e.Environment, nullString(e.Release), e.Fingerprint,
		e.ExceptionType, e.Message, e.Stacktrace, nullString(e.RequestURL), nullString(e.HTTPMethod), httpStatus,
		nullString(e.CorrelationID), nullString(e.RequestContext), nullString(e.UserContext), nullString(e.EventContext),
		occurred, now)
	if err != nil {
		return res, err
	}
	if res.EventID, err = r.LastInsertId(); err != nil {
		return res, err
	}

	if j := e.Job; j != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO queue_failures (event_id, project_id, issue_id, kind, job_name, job_id, queue_name, connection_name,
			                             attempts, max_tries, batch_id, payload_metadata, failed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			res.EventID, e.ProjectID, res.IssueID, j.Kind, j.Name, nullString(j.ID), nullString(j.Queue),
			nullString(j.Connection), nullInt(j.Attempts), nullInt(j.MaxTries), nullString(j.BatchID),
			nullString(j.Payload), occurred); err != nil {
			return res, err
		}
	}

	if e.Release != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_releases (issue_id, release, first_seen_at, last_seen_at, occurrence_count)
			 VALUES (?1, ?2, ?3, ?3, 1)
			 ON CONFLICT (issue_id, release) DO UPDATE SET
			   occurrence_count = occurrence_count + 1,
			   first_seen_at = MIN(first_seen_at, excluded.first_seen_at),
			   last_seen_at  = MAX(last_seen_at, excluded.last_seen_at)`,
			res.IssueID, e.Release, occurred); err != nil {
			return res, err
		}
	}

	day := occurred / (24 * 60 * 60 * 1000)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO issue_daily_counts (issue_id, day, count) VALUES (?, ?, 1)
		 ON CONFLICT (issue_id, day) DO UPDATE SET count = count + 1`,
		res.IssueID, day); err != nil {
		return res, err
	}
	return res, nil
}
