package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User is a dashboard user. In V1 every user is an admin (D9).
type User struct {
	ID          int64
	Email       string
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// Session is a logged-in dashboard session.
type Session struct {
	ID         int64
	UserID     int64
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CreateUser adds a dashboard user. Emails are unique, ignoring case.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	email = strings.TrimSpace(email)
	now := s.nowMs()
	res, err := s.writer.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)`, email, passwordHash, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Email: email, CreatedAt: fromMs(now)}, nil
}

// CountUsers returns the number of dashboard users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserByEmail returns a user and their password hash.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, string, error) {
	var (
		u         User
		hash      string
		created   int64
		lastLogin sql.NullInt64
	)
	err := s.reader.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at, last_login_at FROM users WHERE email = ?`,
		strings.TrimSpace(email),
	).Scan(&u.ID, &u.Email, &hash, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", err
	}
	u.CreatedAt, u.LastLoginAt = fromMs(created), fromNullMs(lastLogin)
	return u, hash, nil
}

// CreateSession starts a session for a user and records the login time.
// Expired sessions are swept at the same time.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash []byte, ttl time.Duration) (Session, error) {
	now := s.now()
	nowMs := now.UnixMilli()
	expires := now.Add(ttl).UnixMilli()
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, nowMs); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (user_id, token_hash, expires_at, last_seen_at, created_at) VALUES (?, ?, ?, ?, ?)`,
			userID, tokenHash, expires, nowMs, nowMs)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, nowMs, userID)
		return err
	})
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return Session{ID: id, UserID: userID, ExpiresAt: fromMs(expires), LastSeenAt: fromMs(nowMs)}, nil
}

// SessionUser resolves a session token hash to its unexpired session and user.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (Session, User, error) {
	var (
		sess                        Session
		u                           User
		expires, lastSeen, uCreated int64
		lastLogin                   sql.NullInt64
	)
	err := s.reader.QueryRowContext(ctx,
		`SELECT s.id, s.user_id, s.expires_at, s.last_seen_at, u.email, u.created_at, u.last_login_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, s.nowMs(),
	).Scan(&sess.ID, &sess.UserID, &expires, &lastSeen, &u.Email, &uCreated, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, ErrNotFound
	}
	if err != nil {
		return Session{}, User{}, err
	}
	sess.ExpiresAt, sess.LastSeenAt = fromMs(expires), fromMs(lastSeen)
	u.ID, u.CreatedAt, u.LastLoginAt = sess.UserID, fromMs(uCreated), fromNullMs(lastLogin)
	return sess, u, nil
}

// ExtendSession slides a session's expiry forward after activity.
func (s *Store) ExtendSession(ctx context.Context, id int64, ttl time.Duration) error {
	now := s.now()
	return s.execOne(ctx, `UPDATE sessions SET expires_at = ?, last_seen_at = ? WHERE id = ?`,
		now.Add(ttl).UnixMilli(), now.UnixMilli(), id)
}

// DeleteSession ends a session (logout). Missing sessions are not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.writer.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}
