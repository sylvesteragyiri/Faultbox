package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Retention: how long Faultbox keeps what it stores (queue-job-runs.md, J0).
//
//   - Short-lived data is high-volume detail only useful for a day or two:
//     successful job runs and telemetry batch IDs.
//   - Issue and history data is everything else that grows with use: events
//     (and with them failed jobs), failed notifications and mail, command
//     runs, failed and retried job runs, Redis samples and the telemetry
//     rollups.
//
// Issue summaries survive: an issue keeps its title, counts, first and last
// seen and sample after its events go, and so do the per-issue and per-day
// totals behind the overview's charts (D13).
//
// Each value comes from the settings table when saved on the Settings page,
// otherwise from the environment (FAULTBOX_SHORT_RETENTION_HOURS and
// FAULTBOX_RETENTION_DAYS).

// Bounds for the two retention settings.
const (
	MinShortRetentionHours = 1
	MaxShortRetentionHours = 168
	MinIssueRetentionDays  = 1
	MaxIssueRetentionDays  = 3650

	DefaultShortRetentionHours = 48
	DefaultIssueRetentionDays  = 30

	settingShortHours = "retention.short_hours"
	settingIssueDays  = "retention.issue_days"
)

// ErrRetentionInvalid is wrapped by ValidateRetention's errors.
var ErrRetentionInvalid = errors.New("invalid retention")

// Retention is the policy in force.
type Retention struct {
	ShortHours int
	IssueDays  int
	// ShortSaved and IssueSaved report whether the value was saved on the
	// Settings page rather than taken from the environment.
	ShortSaved bool
	IssueSaved bool
}

// ValidateRetention checks two values a person entered.
func ValidateRetention(shortHours, issueDays int) error {
	if shortHours < MinShortRetentionHours || shortHours > MaxShortRetentionHours {
		return fmt.Errorf("%w: short-lived data retention must be from %d to %d hours", ErrRetentionInvalid, MinShortRetentionHours, MaxShortRetentionHours)
	}
	if issueDays < MinIssueRetentionDays || issueDays > MaxIssueRetentionDays {
		return fmt.Errorf("%w: issue and history retention must be from %d to %d days", ErrRetentionInvalid, MinIssueRetentionDays, MaxIssueRetentionDays)
	}
	if shortHours > issueDays*24 {
		return fmt.Errorf("%w: short-lived data can't be kept longer than issues and history (%d hours is more than %d days)", ErrRetentionInvalid, shortHours, issueDays)
	}
	return nil
}

// Retention returns the policy in force: saved values over the defaults
// from the environment. Short-lived retention never exceeds issue
// retention, even when the two come from different places.
func (s *Store) Retention(ctx context.Context, defaultShortHours, defaultIssueDays int) (Retention, error) {
	r := Retention{ShortHours: defaultShortHours, IssueDays: defaultIssueDays}
	if r.ShortHours < MinShortRetentionHours || r.ShortHours > MaxShortRetentionHours {
		r.ShortHours = DefaultShortRetentionHours
	}
	if r.IssueDays < MinIssueRetentionDays || r.IssueDays > MaxIssueRetentionDays {
		r.IssueDays = DefaultIssueRetentionDays
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN (?, ?)`, settingShortHours, settingIssueDays)
	if err != nil {
		return r, fmt.Errorf("read retention settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return r, err
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			continue // a hand-edited value; fall back to the environment
		}
		switch key {
		case settingShortHours:
			if n >= MinShortRetentionHours && n <= MaxShortRetentionHours {
				r.ShortHours, r.ShortSaved = n, true
			}
		case settingIssueDays:
			if n >= MinIssueRetentionDays && n <= MaxIssueRetentionDays {
				r.IssueDays, r.IssueSaved = n, true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	r.ShortHours = min(r.ShortHours, r.IssueDays*24)
	return r, nil
}

// SaveRetention stores both values; they override the environment.
func (s *Store) SaveRetention(ctx context.Context, shortHours, issueDays int) error {
	if err := ValidateRetention(shortHours, issueDays); err != nil {
		return err
	}
	now := s.nowMs()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for key, value := range map[string]int{settingShortHours: shortHours, settingIssueDays: issueDays} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				key, strconv.Itoa(value), now); err != nil {
				return fmt.Errorf("save %s: %w", key, err)
			}
		}
		return nil
	})
}

// ResetRetention forgets the saved values, so the environment applies again.
func (s *Store) ResetRetention(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, `DELETE FROM settings WHERE key IN (?, ?)`, settingShortHours, settingIssueDays)
	return err
}

// ---- what retention covers ----

// pruned is one kind of row the sweep deletes.
type pruned struct {
	label string // shown on the Settings page
	table string
	short bool   // short-lived retention; otherwise issue retention
	where string // rows to delete, with one ? for the cutoff
	// key selects rows for a batched delete: "id" for rowid tables, a row
	// column list for WITHOUT ROWID ones. "" means a daily rollup, deleted one day
	// at a time (where then compares day).
	key string
}

// Every pruned set, in sweep order. Each where clause has an index whose
// first columns match it, so a sweep reads only the rows it deletes.
var prunedSets = []pruned{
	{"Successful job runs", "queue_job_runs", true, `outcome = 'succeeded' AND finished_at < ?`, "id"},
	{"Telemetry batch IDs", "telemetry_batches", true, `received_at < ?`, "project_id, batch_id"},
	{"Failed and retried job runs", "queue_job_runs", false, `outcome IN ('released', 'failed', 'timed_out') AND finished_at < ?`, "id"},
	{"Events and failed jobs", "events", false, `occurred_at < ?`, "id"}, // queue_failures rows go with their event
	{"Failed notifications", "app_notification_failures", false, `occurred_at < ?`, "id"},
	{"Failed mail", "app_mail_failures", false, `occurred_at < ?`, "id"},
	{"Command runs", "app_command_runs", false, `started_at < ?`, "id"},
	{"Redis samples", "redis_samples", false, `sampled_at < ?`, "id"},
	{"Job run rollup", "queue_job_daily_counts", false, `day < ?`, ""},
	{"Failed job rollup", "queue_daily_counts", false, `day < ?`, ""},
	{"Notification rollup", "app_notification_daily_counts", false, `day < ?`, ""},
	{"Mail rollup", "app_mail_daily_counts", false, `day < ?`, ""},
	{"Command rollup", "app_command_daily_counts", false, `day < ?`, ""},
	{"Cache rollup", "app_cache_daily_counts", false, `day < ?`, ""},
}

// cutoff is the first value a pruned set keeps: a time in milliseconds, or
// a day for rollups (the last IssueDays days, today included, are kept).
func (s *Store) cutoff(p pruned, r Retention) int64 {
	now := s.now()
	switch {
	case p.short:
		return now.Add(-time.Duration(r.ShortHours) * time.Hour).UnixMilli()
	case p.key == "":
		return s.firstDay(r.IssueDays)
	default:
		return now.AddDate(0, 0, -r.IssueDays).UnixMilli()
	}
}

// PrunedCount is how many rows of one kind a policy would delete.
type PrunedCount struct {
	Label string
	Rows  int64
}

// RetentionImpact counts the rows a policy would delete if it applied now,
// for the Settings page to show before a change is saved. Kinds with nothing
// to delete are left out.
func (s *Store) RetentionImpact(ctx context.Context, r Retention) ([]PrunedCount, error) {
	var out []PrunedCount
	for _, p := range prunedSets {
		var n int64
		query := `SELECT COUNT(*) FROM ` + p.table + ` WHERE ` + p.where //nolint:gosec // G202: constant identifiers
		if err := s.reader.QueryRowContext(ctx, query, s.cutoff(p, r)).Scan(&n); err != nil {
			return nil, fmt.Errorf("count expired %s: %w", p.table, err)
		}
		if n > 0 {
			out = append(out, PrunedCount{Label: p.label, Rows: n})
		}
	}
	return out, nil
}

// TableSize is the disk used by one pruned table and its indexes.
type TableSize struct {
	Table string
	Label string
	Bytes int64
}

// PrunedTableSizes reports the disk used by each table retention prunes,
// largest first, so a busy app can see what job runs cost.
func (s *Store) PrunedTableSizes(ctx context.Context) ([]TableSize, error) {
	var out []TableSize
	seen := map[string]bool{}
	for _, p := range prunedSets {
		if seen[p.table] {
			continue
		}
		seen[p.table] = true
		names, err := s.strings(ctx, `SELECT name FROM sqlite_schema WHERE tbl_name = ? AND type IN ('table', 'index')`, p.table)
		if err != nil {
			return nil, err
		}
		ts := TableSize{Table: p.table, Label: p.label}
		if p.table == "queue_job_runs" {
			ts.Label = "Job runs"
		}
		for _, name := range names {
			var n int64
			// aggregate=TRUE totals a b-tree without returning a row per page.
			if err := s.reader.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat WHERE name = ? AND aggregate = TRUE`, name).Scan(&n); err != nil {
				return nil, fmt.Errorf("size of %s: %w", name, err)
			}
			ts.Bytes += n
		}
		out = append(out, ts)
	}
	for i := 1; i < len(out); i++ { // insertion sort: a dozen entries
		for j := i; j > 0 && out[j].Bytes > out[j-1].Bytes; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// ---- sweep ----

// SweepOptions tunes a sweep. Zero values take the defaults.
type SweepOptions struct {
	Batch int           // rows per delete transaction (default 1,000)
	Pause time.Duration // between transactions, so ingestion gets the writer (default 20 ms; negative for none)
}

// Sweep deletes everything older than the policy keeps, one table at a
// time, in short transactions of at most Batch rows, pausing between them
// so the single writer is never held for long. It returns how many rows of
// each table it deleted. A cancelled context stops it between batches.
func (s *Store) Sweep(ctx context.Context, r Retention, opts SweepOptions) (map[string]int64, error) {
	if opts.Batch <= 0 {
		opts.Batch = 1000
	}
	if opts.Pause == 0 {
		opts.Pause = 20 * time.Millisecond
	}
	deleted := map[string]int64{}
	for _, p := range prunedSets {
		cut := s.cutoff(p, r)
		for {
			var (
				res sql.Result
				err error
			)
			if p.key == "" {
				// WITHOUT ROWID rollups: the oldest expired day per transaction.
				// A day holds at most a few thousand rows.
				res, err = s.writer.ExecContext(ctx,
					`DELETE FROM `+p.table+` WHERE day = (SELECT MIN(day) FROM `+p.table+`) AND day < ?`, cut) //nolint:gosec // G202: constant identifiers
			} else {
				res, err = s.writer.ExecContext(ctx,
					`DELETE FROM `+p.table+` WHERE (`+p.key+`) IN (SELECT `+p.key+` FROM `+p.table+` WHERE `+p.where+` LIMIT ?)`, //nolint:gosec // G202: constant identifiers
					cut, opts.Batch)
			}
			if err != nil {
				return deleted, fmt.Errorf("sweep %s: %w", p.table, err)
			}
			n, _ := res.RowsAffected()
			deleted[p.table] += n
			if n == 0 || (p.key != "" && n < int64(opts.Batch)) {
				break
			}
			if opts.Pause > 0 {
				select {
				case <-ctx.Done():
					return deleted, ctx.Err()
				case <-time.After(opts.Pause):
				}
			}
		}
	}
	return deleted, nil
}
