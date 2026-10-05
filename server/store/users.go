package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UpsertUser creates the operator account or replaces its password hash.
func (s *Store) UpsertUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash`,
		username, passwordHash, nowString())
	return err
}

// GetUser returns the user's id and password hash.
func (s *Store) GetUser(ctx context.Context, username string) (int64, string, error) {
	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, hash, err
}

// CreateSession stores a login session.
func (s *Store) CreateSession(ctx context.Context, id string, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
		id, userID, timeString(expires))
	return err
}

// GetSession returns the session's user and expiry.
func (s *Store) GetSession(ctx context.Context, id string) (int64, time.Time, error) {
	var userID int64
	var exp string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE id = ?`, id).Scan(&userID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, ErrNotFound
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	return userID, mustTime(exp), nil
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteExpiredSessions removes sessions past their expiry.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowString())
	return err
}

// SessionUsername returns the username owning a login session.
func (s *Store) SessionUsername(ctx context.Context, id string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT u.username FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}
