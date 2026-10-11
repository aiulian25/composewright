package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/aiulian25/composewright/internal/secrets"
)

// RecordKey notes a key in the keys table the first time it is seen.
func (store *Store) RecordKey(ctx context.Context, keyID string) error {
	_, err := store.database.ExecContext(ctx, `INSERT INTO keys (key_id, created_at) VALUES (?, ?) ON CONFLICT(key_id) DO NOTHING`,
		keyID, unix(time.Now()))
	return err
}

// KeyUsage counts sealed values per key id across env vars and settings.
func (store *Store) KeyUsage(ctx context.Context) (map[string]int, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT key_id, COUNT(*) FROM (
		SELECT key_id FROM env_vars WHERE is_secret = 1 UNION ALL SELECT key_id FROM settings WHERE value_ct IS NOT NULL
	) GROUP BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]int{}
	for rows.Next() {
		var keyID string
		var count int
		if err := rows.Scan(&keyID, &count); err != nil {
			return nil, err
		}
		usage[keyID] = count
	}
	return usage, rows.Err()
}

// ResealAll moves every sealed value to target in one transaction and marks
// the previous current key as retired. Each value is re-opened with target
// before commit, so a failed rotation changes nothing.
func (store *Store) ResealAll(ctx context.Context, target secrets.Key) (int, error) {
	resealed := 0
	err := store.transaction(ctx, func(transaction *sql.Tx) error {
		count, err := store.resealEnvVars(ctx, transaction, target)
		if err != nil {
			return err
		}
		settings, err := store.resealSettings(ctx, transaction, target)
		if err != nil {
			return err
		}
		resealed = count + settings
		now := unix(time.Now())
		if _, err := transaction.ExecContext(ctx, `UPDATE keys SET retired_at=? WHERE retired_at IS NULL AND key_id<>?`, now, target.ID); err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `INSERT INTO keys (key_id, created_at) VALUES (?, ?) ON CONFLICT(key_id) DO NOTHING`, target.ID, now)
		return err
	})
	return resealed, err
}

type sealedRow struct {
	id     string
	aad    []byte
	sealed secrets.Sealed
}

func (store *Store) resealEnvVars(ctx context.Context, transaction *sql.Tx, target secrets.Key) (int, error) {
	rows, err := collect(ctx, transaction, `SELECT id, project_id, key, value_ct, nonce, key_id FROM env_vars WHERE is_secret = 1`,
		func(projectID, key string) []byte { return secrets.ProjectAAD(projectID, key) })
	if err != nil {
		return 0, err
	}
	return len(rows), store.rewrite(ctx, transaction, rows, target, `UPDATE env_vars SET value_ct=?, nonce=?, key_id=? WHERE id=?`)
}

func (store *Store) resealSettings(ctx context.Context, transaction *sql.Tx, target secrets.Key) (int, error) {
	rows, err := collect(ctx, transaction, `SELECT key, '', key, value_ct, nonce, key_id FROM settings WHERE value_ct IS NOT NULL`,
		func(_, key string) []byte { return secrets.SettingsAAD(key) })
	if err != nil {
		return 0, err
	}
	return len(rows), store.rewrite(ctx, transaction, rows, target, `UPDATE settings SET value_ct=?, nonce=?, key_id=? WHERE key=?`)
}

func collect(ctx context.Context, transaction *sql.Tx, query string, aad func(owner, key string) []byte) ([]sealedRow, error) {
	result, err := transaction.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var rows []sealedRow
	for result.Next() {
		var row sealedRow
		var owner, key string
		if err := result.Scan(&row.id, &owner, &key, &row.sealed.Ciphertext, &row.sealed.Nonce, &row.sealed.KeyID); err != nil {
			return nil, err
		}
		row.aad = aad(owner, key)
		rows = append(rows, row)
	}
	return rows, result.Err()
}

func (store *Store) rewrite(ctx context.Context, transaction *sql.Tx, rows []sealedRow, target secrets.Key, update string) error {
	check := secrets.NewSealer(target)
	for _, row := range rows {
		moved, err := store.sealer.Reseal(row.sealed, row.aad, target)
		if err != nil {
			return err
		}
		if _, err := check.Open(moved, row.aad); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, update, moved.Ciphertext, moved.Nonce, moved.KeyID, row.id); err != nil {
			return err
		}
	}
	return nil
}
