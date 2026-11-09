package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// NewAppNotification is one Laravel notification on one channel, as sent by
// a monitored app. Not to be confused with Faultbox's own alerts (Channel,
// Notification).
type NewAppNotification struct {
	UUID          string
	Environment   string
	Release       string
	Notification  string // class
	Channel       string
	Notifiable    string // class
	Failed        bool
	ErrorType     string
	ErrorMessage  string
	CorrelationID string
	OccurredAt    time.Time
}

// IngestAppNotifications stores a batch in one transaction: daily counts
// for every notification, plus a row for each failure. A batch ID that was
// already stored is skipped and reported as a duplicate, so SDK retries
// never double-count.
func (s *Store) IngestAppNotifications(ctx context.Context, projectID int64, batchID string, items []NewAppNotification) (duplicate bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO telemetry_batches (project_id, batch_id, received_at) VALUES (?, ?, ?)
			 ON CONFLICT (project_id, batch_id) DO NOTHING`, projectID, strings.ToLower(batchID), s.nowMs())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			duplicate = true
			return nil
		}
		for _, n := range items {
			at := n.OccurredAt.UnixMilli()
			sent, failed := 1, 0
			if n.Failed {
				sent, failed = 0, 1
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO app_notification_daily_counts (day, project_id, environment, notification, channel, sent, failed, last_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT (day, project_id, environment, notification, channel) DO UPDATE SET
				   sent = sent + excluded.sent,
				   failed = failed + excluded.failed,
				   last_at = MAX(last_at, excluded.last_at)`,
				at/dayMs, projectID, n.Environment, n.Notification, n.Channel, sent, failed, at); err != nil {
				return err
			}
			if n.Failed {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO app_notification_failures (project_id, uuid, environment, release, notification, channel,
					                                       notifiable, error_type, error_message, correlation_id, occurred_at)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					 ON CONFLICT (project_id, uuid) DO NOTHING`,
					projectID, n.UUID, n.Environment, nullString(n.Release), n.Notification, n.Channel,
					n.Notifiable, n.ErrorType, n.ErrorMessage, nullString(n.CorrelationID), at); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return duplicate, err
}

// AppNotificationScope selects notifications by project and environment.
// Zero values mean "all".
type AppNotificationScope struct {
	ProjectID   int64
	Environment string
}

func (sc AppNotificationScope) where(column string, first int64) (string, []any) {
	clause := ` WHERE ` + column + ` >= ?`
	args := []any{first}
	if sc.ProjectID > 0 {
		clause += ` AND project_id = ?`
		args = append(args, sc.ProjectID)
	}
	if sc.Environment != "" {
		clause += ` AND environment = ?`
		args = append(args, sc.Environment)
	}
	return clause, args
}

// AppNotificationSummary counts notifications over a window.
type AppNotificationSummary struct {
	Sent    int64
	Failed  int64
	Classes int64 // distinct notification classes
}

// FailurePercent is Failed as a percentage of all attempts, rounded.
func (s AppNotificationSummary) FailurePercent() float64 {
	total := s.Sent + s.Failed
	if total == 0 {
		return 0
	}
	return float64(int64(float64(s.Failed)/float64(total)*1000+0.5)) / 10
}

func (s *Store) firstDay(days int) int64 { return s.now().UTC().UnixMilli()/dayMs - int64(days) + 1 }

// AppNotificationSummary totals the last `days` days.
func (s *Store) AppNotificationSummary(ctx context.Context, sc AppNotificationScope, days int) (AppNotificationSummary, error) {
	var out AppNotificationSummary
	where, args := sc.where("day", s.firstDay(days))
	err := s.reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(sent), 0), COALESCE(SUM(failed), 0), COUNT(DISTINCT notification)
		   FROM app_notification_daily_counts`+where, args...). //nolint:gosec // G202: where is built from constants
		Scan(&out.Sent, &out.Failed, &out.Classes)
	return out, err
}

// AppNotificationTrend returns sent (Errors) and failed (QueueFailures) per
// day, oldest first, so newBarChart can stack them.
func (s *Store) AppNotificationTrend(ctx context.Context, sc AppNotificationScope, days int) ([]DayCount, error) {
	first := s.firstDay(days)
	where, args := sc.where("day", first)
	rows, err := s.reader.QueryContext(ctx,
		`SELECT day, SUM(sent), SUM(failed) FROM app_notification_daily_counts`+where+` GROUP BY day`, args...) //nolint:gosec // G202: where is built from constants
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]DayCount, days)
	for i := range out {
		out[i].Day = time.UnixMilli((first + int64(i)) * dayMs).UTC()
	}
	for rows.Next() {
		var day, sent, failed int64
		if err := rows.Scan(&day, &sent, &failed); err != nil {
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(days) {
			out[i].Errors, out[i].QueueFailures = sent, failed
		}
	}
	return out, rows.Err()
}

// AppNotificationStat is one notification class on one channel.
type AppNotificationStat struct {
	Notification string
	Channel      string
	Sent         int64
	Failed       int64
	LastAt       time.Time
}

// FailurePercent is Failed as a percentage of all attempts, rounded.
func (a AppNotificationStat) FailurePercent() float64 {
	return AppNotificationSummary{Sent: a.Sent, Failed: a.Failed}.FailurePercent()
}

// AppNotificationTypes lists notification classes per channel over the last
// `days` days: the ones failing most first, then the busiest.
func (s *Store) AppNotificationTypes(ctx context.Context, sc AppNotificationScope, days, limit int) ([]AppNotificationStat, error) {
	where, args := sc.where("day", s.firstDay(days))
	args = append(args, limit)
	const typesSelect = `SELECT notification, channel, SUM(sent), SUM(failed), MAX(last_at) FROM app_notification_daily_counts`
	const typesOrder = ` GROUP BY notification, channel ORDER BY SUM(failed) DESC, SUM(sent) DESC, notification, channel LIMIT ?`
	query := typesSelect + where + typesOrder //nolint:gosec // G202: where is built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AppNotificationStat
	for rows.Next() {
		var (
			a  AppNotificationStat
			at int64
		)
		if err := rows.Scan(&a.Notification, &a.Channel, &a.Sent, &a.Failed, &at); err != nil {
			return nil, err
		}
		a.LastAt = fromMs(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AppNotificationFailure is one failed notification.
type AppNotificationFailure struct {
	ProjectID     int64
	Environment   string
	Release       string
	Notification  string
	Channel       string
	Notifiable    string
	ErrorType     string
	ErrorMessage  string
	CorrelationID string
	OccurredAt    time.Time
}

// AppNotificationFailures lists the latest failures, newest first.
func (s *Store) AppNotificationFailures(ctx context.Context, sc AppNotificationScope, limit int) ([]AppNotificationFailure, error) {
	where, args := sc.where("occurred_at", 0)
	args = append(args, limit)
	const failuresSelect = `SELECT project_id, environment, COALESCE(release, ''), notification, channel, notifiable,
	       error_type, error_message, COALESCE(correlation_id, ''), occurred_at FROM app_notification_failures`
	query := failuresSelect + where + ` ORDER BY occurred_at DESC, id DESC LIMIT ?` //nolint:gosec // G202: where is built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AppNotificationFailure
	for rows.Next() {
		var (
			f  AppNotificationFailure
			at int64
		)
		if err := rows.Scan(&f.ProjectID, &f.Environment, &f.Release, &f.Notification, &f.Channel, &f.Notifiable,
			&f.ErrorType, &f.ErrorMessage, &f.CorrelationID, &at); err != nil {
			return nil, err
		}
		f.OccurredAt = fromMs(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// KnownEnvironments lists the environments seen for a project (or every
// project when projectID is 0): in issues, in app notifications, and in the
// project's configured list. Used by the scope switcher.
func (s *Store) KnownEnvironments(ctx context.Context, projectID int64) ([]string, error) {
	query := `SELECT environment FROM (
	            SELECT e.environment FROM issue_environments e JOIN issues i ON i.id = e.issue_id WHERE ?1 = 0 OR i.project_id = ?1
	            UNION SELECT environment FROM app_notification_daily_counts WHERE ?1 = 0 OR project_id = ?1
	            UNION SELECT name FROM project_environments WHERE ?1 = 0 OR project_id = ?1
	          ) ORDER BY 1 LIMIT 50`
	return s.strings(ctx, query, projectID)
}
