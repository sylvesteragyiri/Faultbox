package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Cache telemetry: counters per day, project, environment, store and key
// pattern. Never a row per operation.

// CacheKeyCap bounds the distinct (store, key) pairs kept per project and
// day. Past it, new patterns are counted under CacheOtherKey, so an app that
// leaks IDs into its key patterns can't grow the table without limit.
const (
	CacheKeyCap   = 1000
	CacheOtherKey = "(other)"
)

// NewCacheCount is what one app did with one key pattern since its last
// flush.
type NewCacheCount struct {
	Environment string
	Store       string
	Key         string
	Hits        int64
	Misses      int64
	Writes      int64
	Deletes     int64
	Failures    int64
	At          time.Time
}

func ingestCache(ctx context.Context, tx *sql.Tx, projectID int64, c NewCacheCount) error {
	at := c.At.UnixMilli()
	day := at / dayMs
	key := c.Key
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM app_cache_daily_counts WHERE day = ? AND project_id = ? AND environment = ? AND store = ? AND key = ?`,
		day, projectID, c.Environment, c.Store, key).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM app_cache_daily_counts WHERE day = ? AND project_id = ?`, day, projectID).Scan(&n); err != nil {
			return err
		}
		if n >= CacheKeyCap {
			key = CacheOtherKey
		}
	case err != nil:
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO app_cache_daily_counts (day, project_id, environment, store, key, hits, misses, writes, deletes, failures, last_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (day, project_id, environment, store, key) DO UPDATE SET
		   hits = hits + excluded.hits, misses = misses + excluded.misses, writes = writes + excluded.writes,
		   deletes = deletes + excluded.deletes, failures = failures + excluded.failures,
		   last_at = MAX(last_at, excluded.last_at)`,
		day, projectID, c.Environment, c.Store, key, c.Hits, c.Misses, c.Writes, c.Deletes, c.Failures, at)
	return err
}

// CacheTotals counts cache operations.
type CacheTotals struct {
	Hits     int64
	Misses   int64
	Writes   int64
	Deletes  int64
	Failures int64
}

// Reads is hits plus misses.
func (c CacheTotals) Reads() int64 { return c.Hits + c.Misses }

// HitPercent is hits as a percentage of reads, to one decimal.
func (c CacheTotals) HitPercent() float64 { return percent1(c.Hits, c.Reads()) }

// Operations is every counted operation.
func (c CacheTotals) Operations() int64 { return c.Reads() + c.Writes + c.Deletes + c.Failures }

// CacheSummary totals a window.
type CacheSummary struct {
	CacheTotals
	Keys   int64 // distinct (store, key) patterns
	Stores int64
}

// CacheSummary totals the last `days` days.
func (s *Store) CacheSummary(ctx context.Context, sc TelemetryScope, days int) (CacheSummary, error) {
	var out CacheSummary
	where, args := sc.where("day", s.firstDay(days))
	query := `SELECT COALESCE(SUM(hits), 0), COALESCE(SUM(misses), 0), COALESCE(SUM(writes), 0), COALESCE(SUM(deletes), 0), COALESCE(SUM(failures), 0), COUNT(DISTINCT store || char(0) || key), COUNT(DISTINCT store) FROM app_cache_daily_counts` + where //nolint:gosec // G202: where is built from constants
	err := s.reader.QueryRowContext(ctx, query, args...).Scan(&out.Hits, &out.Misses, &out.Writes, &out.Deletes, &out.Failures, &out.Keys, &out.Stores)
	return out, err
}

// CacheTrend returns hits (Errors) and misses (QueueFailures) per day,
// oldest first, so newBarChart can stack them.
func (s *Store) CacheTrend(ctx context.Context, sc TelemetryScope, days int) ([]DayCount, error) {
	first := s.firstDay(days)
	where, args := sc.where("day", first)
	query := `SELECT day, SUM(hits), SUM(misses) FROM app_cache_daily_counts` + where + ` GROUP BY day` //nolint:gosec // G202: where is built from constants
	return s.dayCounts(ctx, query, args, first, days)
}

// CacheStat is one store and key pattern over a window.
type CacheStat struct {
	CacheTotals
	Store  string
	Key    string
	LastAt time.Time
}

// CacheSort orders CacheStats.
type CacheSort string

// The orders the cache page offers.
const (
	CacheByOperations CacheSort = "operations" // the busiest patterns
	CacheByMisses     CacheSort = "misses"     // where the cache helps least
	CacheByFailures   CacheSort = "failures"
)

// CacheStats lists key patterns over the last `days` days.
func (s *Store) CacheStats(ctx context.Context, sc TelemetryScope, days int, sort CacheSort, limit int) ([]CacheStat, error) {
	where, args := sc.where("day", s.firstDay(days))
	args = append(args, limit)
	order := ` ORDER BY SUM(hits + misses + writes + deletes + failures) DESC`
	switch sort {
	case CacheByMisses:
		order = ` ORDER BY SUM(misses) DESC, SUM(hits) ASC`
	case CacheByFailures:
		order = ` ORDER BY SUM(failures) DESC, SUM(writes + deletes) DESC`
	}
	const statsSelect = `SELECT store, key, SUM(hits), SUM(misses), SUM(writes), SUM(deletes), SUM(failures), MAX(last_at) FROM app_cache_daily_counts`
	query := statsSelect + where + ` GROUP BY store, key` + order + `, store, key LIMIT ?` //nolint:gosec // G202: where and order are built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CacheStat
	for rows.Next() {
		var (
			c  CacheStat
			at int64
		)
		if err := rows.Scan(&c.Store, &c.Key, &c.Hits, &c.Misses, &c.Writes, &c.Deletes, &c.Failures, &at); err != nil {
			return nil, err
		}
		c.LastAt = fromMs(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CacheStoreStat totals one cache store over a window.
type CacheStoreStat struct {
	CacheTotals
	Store string
	Keys  int64
}

// CacheStores totals each store over the last `days` days, busiest first.
func (s *Store) CacheStores(ctx context.Context, sc TelemetryScope, days int) ([]CacheStoreStat, error) {
	where, args := sc.where("day", s.firstDay(days))
	query := `SELECT store, SUM(hits), SUM(misses), SUM(writes), SUM(deletes), SUM(failures), COUNT(DISTINCT key) FROM app_cache_daily_counts` + where + ` GROUP BY store ORDER BY SUM(hits + misses + writes + deletes + failures) DESC, store LIMIT 20` //nolint:gosec // G202: where is built from constants
	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CacheStoreStat
	for rows.Next() {
		var c CacheStoreStat
		if err := rows.Scan(&c.Store, &c.Hits, &c.Misses, &c.Writes, &c.Deletes, &c.Failures, &c.Keys); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
