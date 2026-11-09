package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// Notification channel kinds.
const (
	ChannelEmail   = "email"
	ChannelWebhook = "webhook"
)

// Notification triggers (PRD §4.7).
const (
	TriggerNewIssue     = "new_issue"
	TriggerRegression   = "regression"
	TriggerQueueFailure = "queue_failure"
	TriggerThreshold    = "threshold"
	TriggerTest         = "test"
)

// Notification delivery statuses.
const (
	NotificationPending = "pending"
	NotificationSent    = "sent"
	NotificationFailed  = "failed"
)

// SMTP connection security modes.
const (
	SMTPStartTLS = "starttls" // plain connection upgraded with STARTTLS (port 587)
	SMTPTLS      = "tls"      // implicit TLS (port 465)
	SMTPNone     = "none"     // no encryption: only for a local relay
)

// ChannelConfig holds a channel's non-secret settings, stored as JSON.
type ChannelConfig struct {
	// Email
	Host     string   `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	Security string   `json:"security,omitempty"`
	Username string   `json:"username,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	// Webhook
	URL string `json:"url,omitempty"`
}

// Channel is a notification destination and the rules for when it fires.
type Channel struct {
	ID           int64
	Name         string
	Kind         string
	Enabled      bool
	Config       ChannelConfig
	Secret       []byte // sealed with auth.Seal; nil when none is stored
	ProjectID    int64  // 0: every project
	Environments []string
	// Triggers
	OnNewIssue      bool
	OnRegression    bool
	OnQueueFailure  bool
	ThresholdCount  int // 0 = off
	ThresholdWindow int // minutes
	// Cooldown is the minimum gap, in minutes, between alerts for the same
	// issue and trigger on this channel.
	Cooldown  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasSecret reports whether a sealed secret is stored.
func (c Channel) HasSecret() bool { return len(c.Secret) > 0 }

// Matches reports whether an event from this project and environment is in
// the channel's scope.
func (c Channel) Matches(projectID int64, environment string) bool {
	if c.ProjectID != 0 && c.ProjectID != projectID {
		return false
	}
	if len(c.Environments) == 0 {
		return true
	}
	for _, e := range c.Environments {
		if e == environment {
			return true
		}
	}
	return false
}

// Limits on channel settings.
const (
	MaxChannelNameLen  = 100
	MaxRecipients      = 20
	MaxCooldownMinutes = 7 * 24 * 60
	MaxThresholdWindow = 24 * 60
	MaxThresholdCount  = 1_000_000
)

// NormalizeChannel trims and checks a channel's settings. It doesn't look
// at the secret.
func NormalizeChannel(c *Channel) error {
	c.Name = strings.Join(strings.Fields(c.Name), " ")
	switch {
	case c.Name == "":
		return invalid("Channel name is required.")
	case len([]rune(c.Name)) > MaxChannelNameLen:
		return invalid("Channel name must be at most %d characters.", MaxChannelNameLen)
	}
	cfg := &c.Config
	switch c.Kind {
	case ChannelEmail:
		cfg.URL = ""
		cfg.Host = strings.TrimSpace(cfg.Host)
		cfg.Username = strings.TrimSpace(cfg.Username)
		if cfg.Host == "" || strings.ContainsAny(cfg.Host, " /:") {
			return invalid("Enter the SMTP server's host name, such as smtp.example.com.")
		}
		if cfg.Port < 1 || cfg.Port > 65535 {
			return invalid("SMTP port must be between 1 and 65535.")
		}
		switch cfg.Security {
		case SMTPStartTLS, SMTPTLS, SMTPNone:
		default:
			return invalid("Choose STARTTLS, TLS or no encryption.")
		}
		from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
		if err != nil {
			return invalid("The From address isn't a valid email address.")
		}
		cfg.From = from.String()
		var to []string
		for _, raw := range cfg.To {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			a, err := mail.ParseAddress(raw)
			if err != nil {
				return invalid("%q isn't a valid email address.", raw)
			}
			to = append(to, a.Address)
		}
		if len(to) == 0 {
			return invalid("Add at least one recipient.")
		}
		if len(to) > MaxRecipients {
			return invalid("A channel can have at most %d recipients.", MaxRecipients)
		}
		cfg.To = to
	case ChannelWebhook:
		*cfg = ChannelConfig{URL: strings.TrimSpace(cfg.URL)}
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(cfg.URL) > 2000 {
			return invalid("The webhook URL must be a full http:// or https:// URL.")
		}
		if u.User != nil {
			return invalid("Don't put credentials in the webhook URL; requests are signed with the channel's secret instead.")
		}
	default:
		return invalid("Unknown channel type.")
	}
	if c.Cooldown < 1 || c.Cooldown > MaxCooldownMinutes {
		return invalid("Cooldown must be between 1 minute and 7 days.")
	}
	if c.ThresholdCount < 0 || c.ThresholdCount > MaxThresholdCount {
		return invalid("The occurrence threshold must be between 0 (off) and %d.", MaxThresholdCount)
	}
	if c.ThresholdCount > 0 && (c.ThresholdWindow < 1 || c.ThresholdWindow > MaxThresholdWindow) {
		return invalid("The threshold window must be between 1 minute and 24 hours.")
	}
	if c.ThresholdWindow < 1 {
		c.ThresholdWindow = 60
	}
	if !c.OnNewIssue && !c.OnRegression && !c.OnQueueFailure && c.ThresholdCount == 0 {
		return invalid("Turn on at least one trigger.")
	}
	return nil
}

const channelColumns = `id, name, kind, enabled, config, secret, COALESCE(project_id, 0), environments,
	on_new_issue, on_regression, on_queue_failure, threshold_count, threshold_window_minutes, cooldown_minutes,
	created_at, updated_at`

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var (
		c                Channel
		config, envs     string
		created, updated int64
	)
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Enabled, &config, &c.Secret, &c.ProjectID, &envs,
		&c.OnNewIssue, &c.OnRegression, &c.OnQueueFailure, &c.ThresholdCount, &c.ThresholdWindow, &c.Cooldown,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, err
	}
	if err := json.Unmarshal([]byte(config), &c.Config); err != nil {
		return Channel{}, err
	}
	if envs != "" {
		c.Environments = strings.Split(envs, ",")
	}
	c.CreatedAt, c.UpdatedAt = fromMs(created), fromMs(updated)
	return c, nil
}

func channelArgs(c Channel) (config string, project any, envs string, err error) {
	b, err := json.Marshal(c.Config)
	if err != nil {
		return "", nil, "", err
	}
	if c.ProjectID > 0 {
		project = c.ProjectID
	}
	return string(b), project, strings.Join(c.Environments, ","), nil
}

// CreateChannel stores a new channel. Callers run NormalizeChannel first.
func (s *Store) CreateChannel(ctx context.Context, c Channel) (int64, error) {
	config, project, envs, err := channelArgs(c)
	if err != nil {
		return 0, err
	}
	now := s.nowMs()
	res, err := s.writer.ExecContext(ctx,
		`INSERT INTO notification_channels (name, kind, enabled, config, secret, project_id, environments,
		   on_new_issue, on_regression, on_queue_failure, threshold_count, threshold_window_minutes, cooldown_minutes,
		   created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Kind, c.Enabled, config, c.Secret, project, envs,
		c.OnNewIssue, c.OnRegression, c.OnQueueFailure, c.ThresholdCount, c.ThresholdWindow, c.Cooldown, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateChannel saves a channel's settings. The stored secret is replaced
// only when replaceSecret is true; the kind never changes.
func (s *Store) UpdateChannel(ctx context.Context, c Channel, replaceSecret bool) error {
	config, project, envs, err := channelArgs(c)
	if err != nil {
		return err
	}
	query := `UPDATE notification_channels SET name = ?, enabled = ?, config = ?, project_id = ?, environments = ?,
	            on_new_issue = ?, on_regression = ?, on_queue_failure = ?, threshold_count = ?,
	            threshold_window_minutes = ?, cooldown_minutes = ?, updated_at = ?`
	args := []any{c.Name, c.Enabled, config, project, envs, c.OnNewIssue, c.OnRegression, c.OnQueueFailure,
		c.ThresholdCount, c.ThresholdWindow, c.Cooldown, s.nowMs()}
	if replaceSecret {
		query += `, secret = ?`
		args = append(args, c.Secret)
	}
	args = append(args, c.ID)
	return s.execOne(ctx, query+` WHERE id = ?`, args...)
}

// SetChannelEnabled turns a channel on or off.
func (s *Store) SetChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return s.execOne(ctx, `UPDATE notification_channels SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, s.nowMs(), id)
}

// DeleteChannel removes a channel and its delivery history.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	return s.execOne(ctx, `DELETE FROM notification_channels WHERE id = ?`, id)
}

// Channel returns one channel.
func (s *Store) Channel(ctx context.Context, id int64) (Channel, error) {
	return scanChannel(s.reader.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = ?`, id))
}

// Channels lists every channel, by name.
func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- delivery history ----

// Notification is one alert for one channel, and its delivery state.
type Notification struct {
	ID            int64
	ChannelID     int64
	IssueID       int64 // 0 for test notifications
	ProjectID     int64
	Trigger       string
	Status        string
	Subject       string
	Payload       string // JSON message
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	SentAt        time.Time
	// Joined for lists.
	ChannelName string
	ChannelKind string
}

// InsertNotification records a pending alert, due immediately.
func (s *Store) InsertNotification(ctx context.Context, n Notification) (int64, error) {
	now := s.nowMs()
	var issue, project any
	if n.IssueID > 0 {
		issue = n.IssueID
	}
	if n.ProjectID > 0 {
		project = n.ProjectID
	}
	res, err := s.writer.ExecContext(ctx,
		`INSERT INTO notification_history (channel_id, issue_id, project_id, trigger, status, subject, payload,
		                                   attempts, next_attempt_at, created_at)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?, 0, ?, ?)`,
		n.ChannelID, issue, project, n.Trigger, n.Subject, n.Payload, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const notificationColumns = `h.id, h.channel_id, COALESCE(h.issue_id, 0), COALESCE(h.project_id, 0), h.trigger, h.status,
	h.subject, h.payload, h.attempts, COALESCE(h.next_attempt_at, 0), COALESCE(h.last_error, ''), h.created_at,
	COALESCE(h.sent_at, 0), c.name, c.kind`

func scanNotification(row interface{ Scan(...any) error }) (Notification, error) {
	var (
		n                   Notification
		next, created, sent int64
	)
	err := row.Scan(&n.ID, &n.ChannelID, &n.IssueID, &n.ProjectID, &n.Trigger, &n.Status, &n.Subject, &n.Payload,
		&n.Attempts, &next, &n.LastError, &created, &sent, &n.ChannelName, &n.ChannelKind)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	n.NextAttemptAt, n.CreatedAt, n.SentAt = fromMs(next), fromMs(created), fromMs(sent)
	if next == 0 {
		n.NextAttemptAt = time.Time{}
	}
	if sent == 0 {
		n.SentAt = time.Time{}
	}
	return n, err
}

// Notification returns one history row.
func (s *Store) Notification(ctx context.Context, id int64) (Notification, error) {
	return scanNotification(s.reader.QueryRowContext(ctx,
		`SELECT `+notificationColumns+` FROM notification_history h JOIN notification_channels c ON c.id = h.channel_id
		  WHERE h.id = ?`, id))
}

// DueNotifications returns pending alerts whose next attempt is due, oldest first.
func (s *Store) DueNotifications(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id FROM notification_history WHERE status = 'pending' AND next_attempt_at <= ?
		  ORDER BY next_attempt_at, id LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkNotificationSent records a successful delivery.
func (s *Store) MarkNotificationSent(ctx context.Context, id int64, attempts int) error {
	now := s.nowMs()
	return s.execOne(ctx,
		`UPDATE notification_history SET status = 'sent', attempts = ?, sent_at = ?, next_attempt_at = NULL, last_error = NULL
		  WHERE id = ?`, attempts, now, id)
}

// MarkNotificationRetry records a failed attempt that will be retried at next.
func (s *Store) MarkNotificationRetry(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error {
	return s.execOne(ctx,
		`UPDATE notification_history SET status = 'pending', attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		attempts, next.UnixMilli(), truncateError(lastErr), id)
}

// MarkNotificationFailed records a delivery that has used up its attempts.
func (s *Store) MarkNotificationFailed(ctx context.Context, id int64, attempts int, lastErr string) error {
	return s.execOne(ctx,
		`UPDATE notification_history SET status = 'failed', attempts = ?, next_attempt_at = NULL, last_error = ? WHERE id = ?`,
		attempts, truncateError(lastErr), id)
}

// RequeueNotification makes a failed delivery pending again, due now.
func (s *Store) RequeueNotification(ctx context.Context, id int64) error {
	return s.execOne(ctx,
		`UPDATE notification_history SET status = 'pending', next_attempt_at = ? WHERE id = ? AND status = 'failed'`,
		s.nowMs(), id)
}

func truncateError(msg string) string {
	if r := []rune(msg); len(r) > 500 {
		return string(r[:500]) + "…"
	}
	return msg
}

// NotificationFilter selects history rows. Zero values mean "any".
type NotificationFilter struct {
	ChannelID int64
	Status    string
	Limit     int
}

// ListNotifications returns history rows, newest first.
func (s *Store) ListNotifications(ctx context.Context, f NotificationFilter) ([]Notification, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	query := `SELECT ` + notificationColumns + ` FROM notification_history h JOIN notification_channels c ON c.id = h.channel_id`
	var (
		where []string
		args  []any
	)
	if f.ChannelID > 0 {
		where, args = append(where, "h.channel_id = ?"), append(args, f.ChannelID)
	}
	switch f.Status {
	case NotificationPending, NotificationSent, NotificationFailed:
		where, args = append(where, "h.status = ?"), append(args, f.Status)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, f.Limit)
	rows, err := s.reader.QueryContext(ctx, query+` ORDER BY h.created_at DESC, h.id DESC LIMIT ?`, args...) //nolint:gosec // G202: constant fragments, bound values
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotificationCounts summarises deliveries created since a time.
type NotificationCounts struct {
	Sent, Failed, Pending int64
}

// NotificationStats counts deliveries created since a time, plus every
// delivery still pending.
func (s *Store) NotificationStats(ctx context.Context, since time.Time) (NotificationCounts, error) {
	var c NotificationCounts
	err := s.reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(status = 'sent' AND created_at >= ?1), 0),
		        COALESCE(SUM(status = 'failed' AND created_at >= ?1), 0),
		        COALESCE(SUM(status = 'pending'), 0)
		   FROM notification_history WHERE created_at >= ?1 OR status = 'pending'`, since.UnixMilli()).
		Scan(&c.Sent, &c.Failed, &c.Pending)
	return c, err
}

// AlertKey identifies alerts for cooldown purposes.
type AlertKey struct {
	ChannelID int64
	IssueID   int64
	Trigger   string
}

// RecentAlerts returns the latest alert time for each channel, issue and
// trigger since a time, to seed the cooldown cache after a restart.
func (s *Store) RecentAlerts(ctx context.Context, since time.Time) (map[AlertKey]time.Time, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT channel_id, issue_id, trigger, MAX(created_at) FROM notification_history
		  WHERE created_at >= ? AND issue_id IS NOT NULL GROUP BY 1, 2, 3`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[AlertKey]time.Time{}
	for rows.Next() {
		var (
			k  AlertKey
			at int64
		)
		if err := rows.Scan(&k.ChannelID, &k.IssueID, &k.Trigger, &at); err != nil {
			return nil, err
		}
		out[k] = fromMs(at)
	}
	return out, rows.Err()
}

// IssueEventsSince counts an issue's events that occurred at or after a time.
func (s *Store) IssueEventsSince(ctx context.Context, issueID int64, since time.Time) (int64, error) {
	var n int64
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE issue_id = ? AND occurred_at >= ?`, issueID, since.UnixMilli()).Scan(&n)
	return n, err
}
