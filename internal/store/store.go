// Package store owns the SQLite database: connections, migrations and queries.
//
// It keeps two pools over the same file. The writer has exactly one
// connection, so writes are serialized inside the process and never wait on
// SQLite's busy timeout against each other. The reader pool is read-only and
// thanks to WAL mode never blocks, or is blocked by, the writer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/sylvesteragyiri/faultbox/migrations"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write breaks a uniqueness rule.
var ErrConflict = errors.New("already exists")

// Store is the database handle.
type Store struct {
	path   string
	writer *sql.DB
	reader *sql.DB
	now    func() time.Time
}

// Open opens (creating if needed) the database at path, applies pending
// migrations and restricts the file permissions.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	pragmas := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		pragmas.Add("_pragma", p)
	}

	wq := cloneValues(pragmas)
	wq.Set("_txlock", "immediate") // take the write lock up front, never upgrade mid-transaction
	writer, err := sql.Open("sqlite", path+"?"+wq.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	s := &Store{path: path, writer: writer, now: time.Now}

	// Migrate before opening readers so the file and its WAL mode exist.
	if err := s.migrate(ctx); err != nil {
		_ = writer.Close()
		return nil, err
	}

	rq := cloneValues(pragmas)
	rq.Add("_pragma", "query_only(1)")
	reader, err := sql.Open("sqlite", path+"?"+rq.Encode())
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open database reader: %w", err)
	}
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)
	s.reader = reader

	if err := s.Ping(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.restrictPermissions()
	return s, nil
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func (s *Store) migrate(ctx context.Context) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.writer, migrations.FS)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return s.backfillNotificationIssues(ctx)
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.writer, migrations.FS)
	if err != nil {
		return 0, err
	}
	return provider.GetDBVersion(ctx)
}

// restrictPermissions makes the database readable by the owner only (PRD §8).
// It is a no-op on filesystems without Unix permissions.
func (s *Store) restrictPermissions() {
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		_ = os.Chmod(p, 0o600) // best effort; missing -wal/-shm files are fine
	}
}

// Ping checks that both pools can run a query.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.writer.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("database writer: %w", err)
	}
	if err := s.reader.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("database reader: %w", err)
	}
	return nil
}

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	errs = append(errs, s.writer.Close())
	return errors.Join(errs...)
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// SetClock replaces the time source. For tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) nowMs() int64 { return s.now().UnixMilli() }

func fromMs(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMs(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return fromMs(ms.Int64)
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// inTx runs fn in a write transaction on the single writer connection.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
