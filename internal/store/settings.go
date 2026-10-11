package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aiulian25/composewright/internal/secrets"
)

// SetSecret stores a sealed setting such as an API key.
func (store *Store) SetSecret(ctx context.Context, name, value string) error {
	sealed, err := store.sealer.Seal([]byte(value), secrets.SettingsAAD(name))
	if err != nil {
		return err
	}
	_, err = store.database.ExecContext(ctx, `INSERT INTO settings (key, value_plain, value_ct, nonce, key_id) VALUES (?, NULL, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_plain=NULL, value_ct=excluded.value_ct, nonce=excluded.nonce, key_id=excluded.key_id`,
		name, sealed.Ciphertext, sealed.Nonce, sealed.KeyID)
	return err
}

// Secret returns a sealed setting; found is false when it was never set.
func (store *Store) Secret(ctx context.Context, name string) (value string, found bool, err error) {
	var sealed secrets.Sealed
	var keyID sql.NullString
	err = store.database.QueryRowContext(ctx, `SELECT value_ct, nonce, key_id FROM settings WHERE key=? AND value_ct IS NOT NULL`, name).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sealed.KeyID = keyID.String
	plaintext, err := store.sealer.Open(sealed, secrets.SettingsAAD(name))
	return string(plaintext), err == nil, err
}

// HasSecret reports whether a sealed setting is stored, without opening it.
func (store *Store) HasSecret(ctx context.Context, name string) (bool, error) {
	var stored int
	err := store.database.QueryRowContext(ctx, `SELECT 1 FROM settings WHERE key=? AND value_ct IS NOT NULL`, name).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (store *Store) DeleteSecret(ctx context.Context, name string) (bool, error) {
	result, err := store.database.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, name)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}
