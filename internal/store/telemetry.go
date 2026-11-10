package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Telemetry is what the monitored apps do besides throwing: the Laravel
// notifications and mail they send, the artisan commands they run and how
// they use the cache (cache.go). It
// arrives in batches (POST /api/v1/telemetry); daily rollups keep the
// dashboard cheap, and failures (plus every command run) are kept one by one.

// NewAppNotification is one Laravel notification on one channel, as sent by
// a monitored app. Not to be confused with Faultbox's own alerts.
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

// NewAppMail is one email sent directly: a Mailable or Mail::raw.
type NewAppMail struct {
	UUID          string
	Environment   string
	Release       string
	Mailable      string // class, or "raw"
	Mailer        string
	Recipients    int
	Failed        bool
	ErrorType     string
	ErrorMessage  string
	CorrelationID string
	OccurredAt    time.Time
}

// NewCommandRun is one artisan command run.
type NewCommandRun struct {
	UUID         string
	Environment  string
	Release      string
	Command      string
	ExitCode     int
	DurationMs   int64
	ErrorType    string
	ErrorMessage string
	StartedAt    time.Time
}

// TelemetryBatch is one validated batch.
type TelemetryBatch struct {
	BatchID       string
	Notifications []NewAppNotification
	Mails         []NewAppMail
	Commands      []NewCommandRun
	Cache         []NewCacheCount
}

// IngestTelemetry stores a batch in one transaction. A batch ID that was
// already stored is skipped and reported as a duplicate, so SDK retries
// never double-count. Each failed notification is also stored as an event
// of a notification issue; issues says what that did, for alerts.
func (s *Store) IngestTelemetry(ctx context.Context, projectID int64, b TelemetryBatch) (duplicate bool, issues []IngestResult, err error) {
	now := s.nowMs()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO telemetry_batches (project_id, batch_id, received_at) VALUES (?, ?, ?)
			 ON CONFLICT (project_id, batch_id) DO NOTHING`, projectID, b.BatchID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			duplicate = true
			return nil
		}
		var lastFailure int64
		for _, n := range b.Notifications {
			stored, err := ingestDelivery(ctx, tx, NotificationDeliveries, projectID, delivery{
				uuid: n.UUID, env: n.Environment, release: n.Release, name: n.Notification, channel: n.Channel,
				extra: n.Notifiable, failed: n.Failed, errType: n.ErrorType, errMsg: n.ErrorMessage,
				correlation: n.CorrelationID, at: n.OccurredAt,
			})
			if err != nil {
				return err
			}
			if !stored {
				continue
			}
			r, err := linkNotificationIssue(ctx, tx, projectID, n, now)
			if err != nil {
				return fmt.Errorf("notification %s: %w", n.UUID, err)
			}
			if !r.Duplicate {
				issues = append(issues, r)
				lastFailure = max(lastFailure, n.OccurredAt.UnixMilli())
			}
		}
		if lastFailure > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE projects SET last_event_at = MAX(COALESCE(last_event_at, 0), ?) WHERE id = ?`,
				lastFailure, projectID); err != nil {
				return err
			}
		}
		for _, m := range b.Mails {
			if _, err := ingestDelivery(ctx, tx, MailDeliveries, projectID, delivery{
				uuid: m.UUID, env: m.Environment, release: m.Release, name: m.Mailable, channel: m.Mailer,
				extra: m.Recipients, failed: m.Failed, errType: m.ErrorType, errMsg: m.ErrorMessage,
				correlation: m.CorrelationID, at: m.OccurredAt,
			}); err != nil {
				return err
			}
		}
		for _, c := range b.Commands {
			if err := ingestCommand(ctx, tx, projectID, c); err != nil {
				return err
			}
		}
		for _, c := range b.Cache {
			if err := ingestCache(ctx, tx, projectID, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || duplicate {
		return duplicate, nil, err
	}
	return false, issues, nil
}

// ---- notification issues ----

// NotificationFingerprint groups failed notifications into issues: one per
// notification class, channel and error type, the way an exception groups
// by type and culprit. The message is left out because it often carries
// addresses or IDs.
func NotificationFingerprint(notification, channel, errType string) string {
	sum := sha256.Sum256([]byte("notification\x00" + notification + "\x00" + channel + "\x00" + errType))
	return hex.EncodeToString(sum[:])
}

// notificationContext is what the issue page shows about a failed
// notification, where an exception would show its request.
type notificationContext struct {
	Notification string `json:"notification"`
	Channel      string `json:"channel"`
	Notifiable   string `json:"notifiable"`
}

// notificationEvent turns a failed notification into an issue event. The
// notification class goes in job_name, which the issue page uses as its
// heading and search indexes.
func notificationEvent(projectID int64, n NewAppNotification) (NewEvent, error) {
	details, err := json.Marshal(notificationContext{Notification: n.Notification, Channel: n.Channel, Notifiable: n.Notifiable})
	if err != nil {
		return NewEvent{}, err
	}
	sample, err := json.Marshal(map[string]json.RawMessage{"notification": details})
	if err != nil {
		return NewEvent{}, err
	}
	return NewEvent{
		ProjectID:     projectID,
		UUID:          n.UUID,
		Category:      CategoryNotification,
		Level:         "error",
		Environment:   n.Environment,
		Release:       n.Release,
		Fingerprint:   NotificationFingerprint(n.Notification, n.Channel, n.ErrorType),
		ExceptionType: n.ErrorType,
		Message:       n.ErrorMessage,
		Culprit:       "via " + n.Channel + " to " + n.Notifiable,
		Stacktrace:    `{"frames":[]}`,
		CorrelationID: n.CorrelationID,
		EventContext:  string(details),
		SampleContext: string(sample),
		OccurredAt:    n.OccurredAt,
		Notification:  n.Notification,
	}, nil
}

// linkNotificationIssue stores a failed notification as an issue event and
// records the issue on the failure row.
func linkNotificationIssue(ctx context.Context, tx *sql.Tx, projectID int64, n NewAppNotification, now int64) (IngestResult, error) {
	ev, err := notificationEvent(projectID, n)
	if err != nil {
		return IngestResult{}, err
	}
	r, err := ingestOne(ctx, tx, &ev, now)
	if err != nil {
		return r, err
	}
	if r.Duplicate { // an event with this ID was already stored
		if err := tx.QueryRowContext(ctx, `SELECT issue_id FROM events WHERE project_id = ? AND event_uuid = ?`,
			projectID, n.UUID).Scan(&r.IssueID); err != nil {
			return r, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE app_notification_failures SET issue_id = ? WHERE project_id = ? AND uuid = ?`,
		r.IssueID, projectID, n.UUID)
	return r, err
}

// unlinkedFailure is a failed notification stored without an issue.
type unlinkedFailure struct {
	projectID int64
	n         NewAppNotification
}

// backfillNotificationIssues links failures stored before notification
// issues existed (migration 00010). It runs when the store opens and only
// reads failures without an issue, so after the first run it finds none.
func (s *Store) backfillNotificationIssues(ctx context.Context) error {
	const batch = 500
	for {
		todo, err := s.unlinkedNotificationFailures(ctx, batch)
		if err != nil || len(todo) == 0 {
			return err
		}
		now := s.nowMs()
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			for _, u := range todo {
				if _, err := linkNotificationIssue(ctx, tx, u.projectID, u.n, now); err != nil {
					return fmt.Errorf("notification %s: %w", u.n.UUID, err)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("link notification failures to issues: %w", err)
		}
		if len(todo) < batch {
			return nil
		}
	}
}

func (s *Store) unlinkedNotificationFailures(ctx context.Context, limit int) ([]unlinkedFailure, error) {
	rows, err := s.writer.QueryContext(ctx,
		`SELECT project_id, uuid, environment, COALESCE(release, ''), notification, channel, notifiable,
		        error_type, error_message, COALESCE(correlation_id, ''), occurred_at
		   FROM app_notification_failures WHERE issue_id IS NULL ORDER BY occurred_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []unlinkedFailure
	for rows.Next() {
		var (
			u  unlinkedFailure
			at int64
		)
		if err := rows.Scan(&u.projectID, &u.n.UUID, &u.n.Environment, &u.n.Release, &u.n.Notification, &u.n.Channel,
			&u.n.Notifiable, &u.n.ErrorType, &u.n.ErrorMessage, &u.n.CorrelationID, &at); err != nil {
			return nil, err
		}
		u.n.OccurredAt, u.n.Failed = fromMs(at), true
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- notifications and mail: deliveries ----

// DeliveryKind describes a delivered signal's tables: sent and failed
// counts per name and channel, plus one row per failure.
type DeliveryKind struct {
	counts   string // daily rollup table
	failures string // failures table
	name     string // notification / mailable
	channel  string // channel / mailer
	extra    string // notifiable / recipients
	issues   bool   // failures are linked to issues (notifications only)
}

// The two delivered signals.
var (
	NotificationDeliveries = DeliveryKind{"app_notification_daily_counts", "app_notification_failures", "notification", "channel", "notifiable", true}
	MailDeliveries         = DeliveryKind{"app_mail_daily_counts", "app_mail_failures", "mailable", "mailer", "recipients", false}
)

type delivery struct {
	uuid, env, release, name, channel string
	extra                             any
	failed                            bool
	errType, errMsg, correlation      string
	at                                time.Time
}

// ingestDelivery counts a delivery and stores it if it failed. stored
// reports whether a new failure row was written.
func ingestDelivery(ctx context.Context, tx *sql.Tx, k DeliveryKind, projectID int64, d delivery) (stored bool, err error) {
	at := d.at.UnixMilli()
	sent, failed := 1, 0
	if d.failed {
		sent, failed = 0, 1
	}
	// Table and column names come from the two constant kinds above.
	cols := k.name + ", " + k.channel
	countQuery := "INSERT INTO " + k.counts + " (day, project_id, environment, " + cols + ", sent, failed, last_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (day, project_id, environment, " + cols + ") DO UPDATE SET sent = sent + excluded.sent, failed = failed + excluded.failed, last_at = MAX(last_at, excluded.last_at)" //nolint:gosec // G202: constant identifiers
	if _, err := tx.ExecContext(ctx, countQuery, at/dayMs, projectID, d.env, d.name, d.channel, sent, failed, at); err != nil {
		return false, err
	}
	if !d.failed {
		return false, nil
	}
	failureQuery := "INSERT INTO " + k.failures + " (project_id, uuid, environment, release, " + cols + ", " + k.extra + ", error_type, error_message, correlation_id, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, uuid) DO NOTHING" //nolint:gosec // G202: constant identifiers
	res, err := tx.ExecContext(ctx, failureQuery,
		projectID, d.uuid, d.env, nullString(d.release), d.name, d.channel, d.extra,
		d.errType, d.errMsg, nullString(d.correlation), at)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TelemetryScope selects telemetry by project and environment. Zero values
// mean "all".
type TelemetryScope struct {
	ProjectID   int64
	Environment string
}

func (sc TelemetryScope) where(column string, first int64) (string, []any) {
	return sc.whereOn("", column, first)
}

// whereOn is where for a query that names its table: alias is prefixed
// to every column, such as "f.".
func (sc TelemetryScope) whereOn(alias, column string, first int64) (string, []any) {
	clause := ` WHERE ` + alias + column + ` >= ?`
	args := []any{first}
	if sc.ProjectID > 0 {
		clause += ` AND ` + alias + `project_id = ?`
		args = append(args, sc.ProjectID)
	}
	if sc.Environment != "" {
		clause += ` AND ` + alias + `environment = ?`
		args = append(args, sc.Environment)
	}
	return clause, args
}

func (s *Store) firstDay(days int) int64 { return s.now().UTC().UnixMilli()/dayMs - int64(days) + 1 }

// DeliverySummary counts deliveries over a window.
type DeliverySummary struct {
	Sent   int64
	Failed int64
	Names  int64 // distinct notification classes / mailables
}

// FailurePercent is Failed as a percentage of all attempts, to one decimal.
func (d DeliverySummary) FailurePercent() float64 { return percent1(d.Failed, d.Sent+d.Failed) }

func percent1(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(int64(float64(part)/float64(total)*1000+0.5)) / 10
}

// DeliverySummary totals the last `days` days.
func (s *Store) DeliverySummary(ctx context.Context, k DeliveryKind, sc TelemetryScope, days int) (DeliverySummary, error) {
	var out DeliverySummary
	where, args := sc.where("day", s.firstDay(days))
	query := `SELECT COALESCE(SUM(sent), 0), COALESCE(SUM(failed), 0), COUNT(DISTINCT ` + k.name + `) FROM ` + k.counts + where //nolint:gosec // G202: constant identifiers
	err := s.reader.QueryRowContext(ctx, query, args...).Scan(&out.Sent, &out.Failed, &out.Names)
	return out, err
}

// DeliveryTrend returns sent (Errors) and failed (QueueFailures) per day,
// oldest first, so newBarChart can stack them.
func (s *Store) DeliveryTrend(ctx context.Context, k DeliveryKind, sc TelemetryScope, days int) ([]DayCount, error) {
	first := s.firstDay(days)
	where, args := sc.where("day", first)
	query := `SELECT day, SUM(sent), SUM(failed) FROM ` + k.counts + where + ` GROUP BY day` //nolint:gosec // G202: constant identifiers
	return s.dayCounts(ctx, query, args, first, days)
}

func (s *Store) dayCounts(ctx context.Context, query string, args []any, first int64, days int) ([]DayCount, error) {
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
		var day, a, b int64
		if err := rows.Scan(&day, &a, &b); err != nil {
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(days) {
			out[i].Errors, out[i].QueueFailures = a, b
		}
	}
	return out, rows.Err()
}

// DeliveryStat is one notification class or mailable on one channel or mailer.
type DeliveryStat struct {
	Name    string
	Channel string
	Sent    int64
	Failed  int64
	LastAt  time.Time
}

// FailurePercent is Failed as a percentage of all attempts, to one decimal.
func (d DeliveryStat) FailurePercent() float64 { return percent1(d.Failed, d.Sent+d.Failed) }

// DeliveryStats lists names per channel over the last `days` days: the ones
// failing most first, then the busiest.
func (s *Store) DeliveryStats(ctx context.Context, k DeliveryKind, sc TelemetryScope, days, limit int) ([]DeliveryStat, error) {
	where, args := sc.where("day", s.firstDay(days))
	args = append(args, limit)
	query := `SELECT ` + k.name + `, ` + k.channel + `, SUM(sent), SUM(failed), MAX(last_at) FROM ` + k.counts + where + //nolint:gosec // G202: constant identifiers
		` GROUP BY 1, 2 ORDER BY SUM(failed) DESC, SUM(sent) DESC, 1, 2 LIMIT ?`
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryStat
	for rows.Next() {
		var (
			d  DeliveryStat
			at int64
		)
		if err := rows.Scan(&d.Name, &d.Channel, &d.Sent, &d.Failed, &at); err != nil {
			return nil, err
		}
		d.LastAt = fromMs(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeliveryFailure is one failed notification or mail.
type DeliveryFailure struct {
	UUID          string
	ProjectID     int64
	Environment   string
	Release       string
	Name          string
	Channel       string
	Extra         string // the notifiable's class, or the number of recipients
	ErrorType     string
	ErrorMessage  string
	CorrelationID string
	OccurredAt    time.Time
	// IssueID and IssueStatus: the issue a failed notification belongs to;
	// 0 and "" for mail, and for a failure not linked yet.
	IssueID     int64
	IssueStatus string
}

// DeliveryFailures lists the latest failures, newest first.
func (s *Store) DeliveryFailures(ctx context.Context, k DeliveryKind, sc TelemetryScope, limit int) ([]DeliveryFailure, error) {
	where, args := sc.whereOn("f.", "occurred_at", 0)
	args = append(args, limit)
	issue, join := `0, ''`, ``
	if k.issues {
		issue, join = `COALESCE(f.issue_id, 0), COALESCE(i.status, '')`, ` LEFT JOIN issues i ON i.id = f.issue_id`
	}
	query := `SELECT f.uuid, f.project_id, f.environment, COALESCE(f.release, ''), f.` + k.name + `, f.` + k.channel + `, CAST(f.` + k.extra + ` AS TEXT), ` + //nolint:gosec // G202: constant identifiers
		`f.error_type, f.error_message, COALESCE(f.correlation_id, ''), f.occurred_at, ` + issue + ` FROM ` + k.failures + ` f` + join + where +
		` ORDER BY f.occurred_at DESC, f.id DESC LIMIT ?`
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryFailure
	for rows.Next() {
		var (
			f  DeliveryFailure
			at int64
		)
		if err := rows.Scan(&f.UUID, &f.ProjectID, &f.Environment, &f.Release, &f.Name, &f.Channel, &f.Extra,
			&f.ErrorType, &f.ErrorMessage, &f.CorrelationID, &at, &f.IssueID, &f.IssueStatus); err != nil {
			return nil, err
		}
		f.OccurredAt = fromMs(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- commands ----

func ingestCommand(ctx context.Context, tx *sql.Tx, projectID int64, c NewCommandRun) error {
	at := c.StartedAt.UnixMilli()
	failed := 0
	if c.ExitCode != 0 {
		failed = 1
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO app_command_runs (project_id, uuid, environment, release, command, exit_code, duration_ms, failed,
		                               error_type, error_message, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (project_id, uuid) DO NOTHING`,
		projectID, c.UUID, c.Environment, nullString(c.Release), c.Command, c.ExitCode, c.DurationMs, failed,
		nullString(c.ErrorType), nullString(c.ErrorMessage), at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // a run already stored in another batch
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO app_command_daily_counts (day, project_id, environment, command, runs, failed, total_ms, max_ms, last_at, last_exit)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
		 ON CONFLICT (day, project_id, environment, command) DO UPDATE SET
		   runs = runs + 1, failed = failed + excluded.failed, total_ms = total_ms + excluded.total_ms,
		   max_ms = MAX(max_ms, excluded.max_ms),
		   last_exit = CASE WHEN excluded.last_at >= last_at THEN excluded.last_exit ELSE last_exit END,
		   last_at = MAX(last_at, excluded.last_at)`,
		at/dayMs, projectID, c.Environment, c.Command, failed, c.DurationMs, c.DurationMs, at, c.ExitCode)
	return err
}

// CommandSummary counts command runs over a window.
type CommandSummary struct {
	Runs     int64
	Failed   int64
	Commands int64 // distinct commands
	TotalMs  int64
}

// FailurePercent is Failed as a percentage of Runs, to one decimal.
func (c CommandSummary) FailurePercent() float64 { return percent1(c.Failed, c.Runs) }

// AvgMs is the mean run time in milliseconds.
func (c CommandSummary) AvgMs() int64 {
	if c.Runs == 0 {
		return 0
	}
	return c.TotalMs / c.Runs
}

// CommandSummary totals the last `days` days.
func (s *Store) CommandSummary(ctx context.Context, sc TelemetryScope, days int) (CommandSummary, error) {
	var out CommandSummary
	where, args := sc.where("day", s.firstDay(days))
	query := `SELECT COALESCE(SUM(runs), 0), COALESCE(SUM(failed), 0), COUNT(DISTINCT command), COALESCE(SUM(total_ms), 0) FROM app_command_daily_counts` + where //nolint:gosec // G202: where is built from constants
	err := s.reader.QueryRowContext(ctx, query, args...).Scan(&out.Runs, &out.Failed, &out.Commands, &out.TotalMs)
	return out, err
}

// CommandTrend returns successful runs (Errors) and failed runs
// (QueueFailures) per day, oldest first, so newBarChart can stack them.
func (s *Store) CommandTrend(ctx context.Context, sc TelemetryScope, days int) ([]DayCount, error) {
	first := s.firstDay(days)
	where, args := sc.where("day", first)
	query := `SELECT day, SUM(runs - failed), SUM(failed) FROM app_command_daily_counts` + where + ` GROUP BY day` //nolint:gosec // G202: where is built from constants
	return s.dayCounts(ctx, query, args, first, days)
}

// CommandStat is one command over a window.
type CommandStat struct {
	Command  string
	Runs     int64
	Failed   int64
	TotalMs  int64
	MaxMs    int64
	LastAt   time.Time
	LastExit int
}

// AvgMs is the mean run time in milliseconds.
func (c CommandStat) AvgMs() int64 {
	if c.Runs == 0 {
		return 0
	}
	return c.TotalMs / c.Runs
}

// FailurePercent is Failed as a percentage of Runs, to one decimal.
func (c CommandStat) FailurePercent() float64 { return percent1(c.Failed, c.Runs) }

// CommandStats lists commands over the last `days` days: failing first,
// then the most run.
func (s *Store) CommandStats(ctx context.Context, sc TelemetryScope, days, limit int) ([]CommandStat, error) {
	where, args := sc.where("day", s.firstDay(days))
	args = append(args, limit)
	// The last exit code is the one from the latest day with runs.
	const statsSelect = `SELECT command, SUM(runs), SUM(failed), SUM(total_ms), MAX(max_ms), MAX(last_at),
	       (SELECT d2.last_exit FROM app_command_daily_counts d2 WHERE d2.command = d.command AND d2.project_id = d.project_id
	         ORDER BY d2.last_at DESC LIMIT 1)
	  FROM app_command_daily_counts d`
	query := statsSelect + where + ` GROUP BY command ORDER BY SUM(failed) DESC, SUM(runs) DESC, command LIMIT ?` //nolint:gosec // G202: where is built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CommandStat
	for rows.Next() {
		var (
			c  CommandStat
			at int64
		)
		if err := rows.Scan(&c.Command, &c.Runs, &c.Failed, &c.TotalMs, &c.MaxMs, &at, &c.LastExit); err != nil {
			return nil, err
		}
		c.LastAt = fromMs(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CommandRun is one stored run.
type CommandRun struct {
	ProjectID    int64
	Environment  string
	Release      string
	Command      string
	ExitCode     int
	DurationMs   int64
	Failed       bool
	ErrorType    string
	ErrorMessage string
	StartedAt    time.Time
}

// RecentCommandRuns lists the latest runs, newest first; only failed ones
// when failedOnly is set.
func (s *Store) RecentCommandRuns(ctx context.Context, sc TelemetryScope, failedOnly bool, limit int) ([]CommandRun, error) {
	where, args := sc.where("started_at", 0)
	if failedOnly {
		where += ` AND failed = 1`
	}
	args = append(args, limit)
	const runsSelect = `SELECT project_id, environment, COALESCE(release, ''), command, exit_code, duration_ms, failed,
	       COALESCE(error_type, ''), COALESCE(error_message, ''), started_at FROM app_command_runs`
	query := runsSelect + where + ` ORDER BY started_at DESC, id DESC LIMIT ?` //nolint:gosec // G202: where is built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CommandRun
	for rows.Next() {
		var (
			c  CommandRun
			at int64
		)
		if err := rows.Scan(&c.ProjectID, &c.Environment, &c.Release, &c.Command, &c.ExitCode, &c.DurationMs, &c.Failed,
			&c.ErrorType, &c.ErrorMessage, &at); err != nil {
			return nil, err
		}
		c.StartedAt = fromMs(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// KnownEnvironments lists the environments seen for a project (or every
// project when projectID is 0): in issues, telemetry, and the project's
// configured list. Used by the scope switcher.
func (s *Store) KnownEnvironments(ctx context.Context, projectID int64) ([]string, error) {
	query := `SELECT environment FROM (
	            SELECT e.environment FROM issue_environments e JOIN issues i ON i.id = e.issue_id WHERE ?1 = 0 OR i.project_id = ?1
	            UNION SELECT environment FROM app_notification_daily_counts WHERE ?1 = 0 OR project_id = ?1
	            UNION SELECT environment FROM app_mail_daily_counts WHERE ?1 = 0 OR project_id = ?1
	            UNION SELECT environment FROM app_command_daily_counts WHERE ?1 = 0 OR project_id = ?1
	            UNION SELECT environment FROM app_cache_daily_counts WHERE ?1 = 0 OR project_id = ?1
	            UNION SELECT name FROM project_environments WHERE ?1 = 0 OR project_id = ?1
	          ) ORDER BY 1 LIMIT 50`
	return s.strings(ctx, query, projectID)
}
