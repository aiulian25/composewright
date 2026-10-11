package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is the web UI account. v1 has a single administrator.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is a signed-in browser. Only the SHA-256 of the cookie value is
// stored, so a copy of the database cannot be used to sign in.
type Session struct {
	IDHash    string
	UserID    string
	CSRF      string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
}

func (store *Store) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	now := time.Now()
	user := User{ID: newID(), Username: username, PasswordHash: passwordHash, CreatedAt: now, UpdatedAt: now}
	_, err := store.database.ExecContext(ctx, `INSERT INTO users (id, username, pw_hash, created_at, updated_at) VALUES (?,?,?,?,?)`,
		user.ID, user.Username, user.PasswordHash, unix(now), unix(now))
	return user, err
}

func (store *Store) HasUsers(ctx context.Context) (bool, error) {
	var count int
	err := store.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count > 0, err
}

func (store *Store) UserByName(ctx context.Context, username string) (User, error) {
	return store.user(ctx, `SELECT id, username, pw_hash, created_at, updated_at FROM users WHERE username=?`, username)
}

func (store *Store) UserByID(ctx context.Context, id string) (User, error) {
	return store.user(ctx, `SELECT id, username, pw_hash, created_at, updated_at FROM users WHERE id=?`, id)
}

// Administrator is the account v1 has: the first one created.
func (store *Store) Administrator(ctx context.Context) (User, error) {
	return store.user(ctx, `SELECT id, username, pw_hash, created_at, updated_at FROM users ORDER BY created_at, id LIMIT 1`)
}

func (store *Store) user(ctx context.Context, query string, arguments ...any) (User, error) {
	var user User
	var created, updated int64
	err := store.database.QueryRowContext(ctx, query, arguments...).Scan(&user.ID, &user.Username, &user.PasswordHash, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("user: %w", ErrNotFound)
	}
	user.CreatedAt, user.UpdatedAt = fromUnix(created), fromUnix(updated)
	return user, err
}

// SetPassword replaces the hash and signs the user out everywhere except the
// session given, which is the one that made the change.
func (store *Store) SetPassword(ctx context.Context, userID, passwordHash, keepSession string) error {
	return store.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `UPDATE users SET pw_hash=?, updated_at=? WHERE id=?`, passwordHash, unix(time.Now()), userID); err != nil {
			return err
		}
		_, err := transaction.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id_hash<>?`, userID, keepSession)
		return err
	})
}

func (store *Store) CreateSession(ctx context.Context, session Session) error {
	_, err := store.database.ExecContext(ctx, `INSERT INTO sessions (id_hash, user_id, csrf, created_at, last_seen, expires_at) VALUES (?,?,?,?,?,?)`,
		session.IDHash, session.UserID, session.CSRF, unix(session.CreatedAt), unix(session.LastSeen), unix(session.ExpiresAt))
	return err
}

func (store *Store) SessionByHash(ctx context.Context, idHash string) (Session, error) {
	var session Session
	var created, seen, expires int64
	err := store.database.QueryRowContext(ctx, `SELECT id_hash, user_id, csrf, created_at, last_seen, expires_at FROM sessions WHERE id_hash=?`, idHash).
		Scan(&session.IDHash, &session.UserID, &session.CSRF, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session: %w", ErrNotFound)
	}
	session.CreatedAt, session.LastSeen, session.ExpiresAt = fromUnix(created), fromUnix(seen), fromUnix(expires)
	return session, err
}

func (store *Store) TouchSession(ctx context.Context, idHash string, now time.Time) error {
	_, err := store.database.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE id_hash=?`, unix(now), idHash)
	return err
}

func (store *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := store.database.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash=?`, idHash)
	return err
}

// DeleteStaleSessions removes sessions past their absolute expiry or idle
// longer than idle.
func (store *Store) DeleteStaleSessions(ctx context.Context, now time.Time, idle time.Duration) error {
	_, err := store.database.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=? OR last_seen<=?`, unix(now), unix(now.Add(-idle)))
	return err
}
