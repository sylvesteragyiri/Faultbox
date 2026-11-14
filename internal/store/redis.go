package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RedisTarget is a configured Redis instance. Endpoint is sealed ciphertext;
// callers must unseal it only when they need to connect.
type RedisTarget struct {
	ID           int64
	Name         string
	Endpoint     []byte
	Enabled      bool
	AlertEnabled bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastChecked  time.Time
	LastError    string
}

// RedisSample is a compact snapshot of non-sensitive Redis INFO values.
type RedisSample struct {
	TargetID         int64
	TargetName       string
	SampledAt        time.Time
	UsedMemory       int64
	MaxMemory        int64
	ConnectedClients int64
	KeyCount         int64
	UptimeSeconds    int64
}

// RedisStatus pairs a target with its latest successful sample, when present.
type RedisStatus struct {
	Target RedisTarget
	Latest *RedisSample
}

func scanRedisTarget(row interface{ Scan(...any) error }) (RedisTarget, error) {
	var t RedisTarget
	var enabled, alerts, created, updated, checked sql.NullInt64
	err := row.Scan(&t.ID, &t.Name, &t.Endpoint, &enabled, &alerts, &created, &updated, &checked, &t.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return RedisTarget{}, ErrNotFound
	}
	t.Enabled, t.AlertEnabled = enabled.Int64 != 0, alerts.Int64 != 0
	t.CreatedAt, t.UpdatedAt = fromNullMs(created), fromNullMs(updated)
	t.LastChecked = fromNullMs(checked)
	return t, err
}

const redisTargetColumns = `id, name, endpoint, enabled, alert_enabled, created_at, updated_at, last_checked_at, last_error`

// RedisTargets lists configured targets without decrypting their endpoints.
func (s *Store) RedisTargets(ctx context.Context) ([]RedisTarget, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT `+redisTargetColumns+` FROM redis_targets ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RedisTarget
	for rows.Next() {
		t, err := scanRedisTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RedisStatuses loads all targets and their latest successful samples in one
// indexed query for the dashboard.
func (s *Store) RedisStatuses(ctx context.Context) ([]RedisStatus, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT t.id, t.name, t.endpoint, t.enabled, t.alert_enabled, t.created_at, t.updated_at,
		t.last_checked_at, t.last_error, s.sampled_at, s.used_memory, s.maxmemory, s.connected_clients, s.key_count, s.uptime_seconds
		FROM redis_targets t LEFT JOIN redis_samples s ON s.id = (
			SELECT id FROM redis_samples WHERE target_id = t.id ORDER BY sampled_at DESC, id DESC LIMIT 1
		) ORDER BY t.name COLLATE NOCASE, t.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RedisStatus
	for rows.Next() {
		var item RedisStatus
		var enabled, alerts, created, updated, checked, sampled sql.NullInt64
		var used, maxMem, clients, keys, uptime sql.NullInt64
		t := &item.Target
		if err := rows.Scan(&t.ID, &t.Name, &t.Endpoint, &enabled, &alerts, &created, &updated, &checked, &t.LastError,
			&sampled, &used, &maxMem, &clients, &keys, &uptime); err != nil {
			return nil, err
		}
		t.Enabled, t.AlertEnabled = enabled.Int64 != 0, alerts.Int64 != 0
		t.CreatedAt, t.UpdatedAt, t.LastChecked = fromNullMs(created), fromNullMs(updated), fromNullMs(checked)
		if sampled.Valid {
			item.Latest = &RedisSample{TargetID: t.ID, TargetName: t.Name, SampledAt: fromMs(sampled.Int64), UsedMemory: used.Int64,
				MaxMemory: maxMem.Int64, ConnectedClients: clients.Int64, KeyCount: keys.Int64, UptimeSeconds: uptime.Int64}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RedisTarget returns one target, including its sealed endpoint for the poller.
func (s *Store) RedisTarget(ctx context.Context, id int64) (RedisTarget, error) {
	return scanRedisTarget(s.reader.QueryRowContext(ctx, `SELECT `+redisTargetColumns+` FROM redis_targets WHERE id = ?`, id))
}

// SaveRedisTarget inserts or updates a target. An empty endpoint preserves the
// existing sealed endpoint on update.
func (s *Store) SaveRedisTarget(ctx context.Context, t RedisTarget) (int64, error) {
	now := s.nowMs()
	if t.ID == 0 {
		res, err := s.writer.ExecContext(ctx, `INSERT INTO redis_targets (name, endpoint, enabled, alert_enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, t.Name, t.Endpoint, t.Enabled, t.AlertEnabled, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, ErrConflict
			}
			return 0, err
		}
		return res.LastInsertId()
	}
	var (
		res sql.Result
		err error
	)
	if len(t.Endpoint) == 0 {
		res, err = s.writer.ExecContext(ctx, `UPDATE redis_targets SET name = ?, enabled = ?, alert_enabled = ?, updated_at = ? WHERE id = ?`, t.Name, t.Enabled, t.AlertEnabled, now, t.ID)
	} else {
		res, err = s.writer.ExecContext(ctx, `UPDATE redis_targets SET name = ?, endpoint = ?, enabled = ?, alert_enabled = ?, updated_at = ? WHERE id = ?`, t.Name, t.Endpoint, t.Enabled, t.AlertEnabled, now, t.ID)
	}
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrConflict
		}
		return 0, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		return 0, ErrNotFound
	}
	return t.ID, nil
}

// DeleteRedisTarget removes its samples by cascade.
func (s *Store) DeleteRedisTarget(ctx context.Context, id int64) error {
	return s.execOne(ctx, `DELETE FROM redis_targets WHERE id = ?`, id)
}

// RecordRedisCheck stores the latest connection result. A successful check
// also inserts one sample; a failed check never destroys chart history.
func (s *Store) RecordRedisCheck(ctx context.Context, sample RedisSample, checkErr string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.nowMs()
	if checkErr == "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO redis_samples
			(target_id, sampled_at, used_memory, maxmemory, connected_clients, key_count, uptime_seconds)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, sample.TargetID, sample.SampledAt.UTC().UnixMilli(), sample.UsedMemory,
			sample.MaxMemory, sample.ConnectedClients, sample.KeyCount, sample.UptimeSeconds); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE redis_targets SET last_checked_at = ?, last_error = ? WHERE id = ?`, now, checkErr, sample.TargetID); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestRedisSample returns the latest successful sample for a target.
func (s *Store) LatestRedisSample(ctx context.Context, targetID int64) (RedisSample, error) {
	var p RedisSample
	var at int64
	err := s.reader.QueryRowContext(ctx, `SELECT target_id, name, sampled_at, used_memory, maxmemory, connected_clients, key_count, uptime_seconds
		FROM redis_samples JOIN redis_targets ON redis_targets.id = target_id WHERE target_id = ? ORDER BY redis_samples.sampled_at DESC, redis_samples.id DESC LIMIT 1`, targetID).
		Scan(&p.TargetID, &p.TargetName, &at, &p.UsedMemory, &p.MaxMemory, &p.ConnectedClients, &p.KeyCount, &p.UptimeSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return RedisSample{}, ErrNotFound
	}
	p.SampledAt = fromMs(at)
	return p, err
}

// RedisHistory returns samples since `since`, oldest first for charting.
func (s *Store) RedisHistory(ctx context.Context, targetID int64, since time.Time) ([]RedisSample, error) {
	// Keep chart memory bounded for long retention windows while preserving
	// peak memory within each bucket. The raw samples remain stored in SQLite.
	rows, err := s.reader.QueryContext(ctx, `WITH numbered AS (
			SELECT p.target_id, t.name, p.sampled_at, p.used_memory, p.maxmemory, p.connected_clients, p.key_count, p.uptime_seconds,
		       ROW_NUMBER() OVER (ORDER BY p.sampled_at, p.id) AS rn, COUNT(*) OVER () AS total
			  FROM redis_samples p JOIN redis_targets t ON t.id = p.target_id
			 WHERE p.target_id = ? AND p.sampled_at >= ?
		)
		SELECT target_id, name, MAX(sampled_at), MAX(used_memory), MAX(maxmemory), MAX(connected_clients), MAX(key_count), MAX(uptime_seconds)
		  FROM numbered
		 GROUP BY CAST((rn - 1) * 1000 / total AS INTEGER)
	 ORDER BY MAX(sampled_at)`, targetID, since.UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RedisSample
	for rows.Next() {
		var p RedisSample
		var at int64
		if err := rows.Scan(&p.TargetID, &p.TargetName, &at, &p.UsedMemory, &p.MaxMemory, &p.ConnectedClients, &p.KeyCount, &p.UptimeSeconds); err != nil {
			return nil, err
		}
		p.SampledAt = fromMs(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneRedisSamples deletes samples older than the configured global retention.
func (s *Store) PruneRedisSamples(ctx context.Context, before time.Time) error {
	_, err := s.writer.ExecContext(ctx, `DELETE FROM redis_samples WHERE sampled_at < ?`, before.UTC().UnixMilli())
	return err
}

// SetRedisTargetEnabled toggles polling without deleting its history.
func (s *Store) SetRedisTargetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.writer.ExecContext(ctx, `UPDATE redis_targets SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, s.nowMs(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// NormalizeRedisName trims and bounds a user-supplied display name.
func NormalizeRedisName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len([]rune(name)) > 100 {
		return "", errors.New("name is required and must be at most 100 characters")
	}
	return name, nil
}
