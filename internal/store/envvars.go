package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aiulian25/composewright/internal/secrets"
)

// EnvVar is one .env entry with its plaintext value; the store seals the
// value of secret entries before writing and opens it when reading.
type EnvVar struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Secret      bool   `json:"secret"`
	Required    bool   `json:"required"`
	Source      string `json:"source"`
	SourceRef   string `json:"source_ref"`
	Description string `json:"description"`
}

// ReplaceEnvVars stores the complete .env of a project in one transaction.
func (store *Store) ReplaceEnvVars(ctx context.Context, projectID string, variables []EnvVar) error {
	return store.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `DELETE FROM env_vars WHERE project_id=?`, projectID); err != nil {
			return err
		}
		for _, variable := range variables {
			if err := store.insertEnvVar(ctx, transaction, projectID, variable); err != nil {
				return err
			}
		}
		return nil
	})
}

func (store *Store) insertEnvVar(ctx context.Context, transaction *sql.Tx, projectID string, variable EnvVar) error {
	var plain sql.NullString
	var sealed secrets.Sealed
	if variable.Secret {
		var err error
		sealed, err = store.sealer.Seal([]byte(variable.Value), secrets.ProjectAAD(projectID, variable.Key))
		if err != nil {
			return err
		}
	} else {
		plain = sql.NullString{String: variable.Value, Valid: true}
	}
	_, err := transaction.ExecContext(ctx, `INSERT INTO env_vars (id, project_id, key, is_secret, value_plain, value_ct, nonce, key_id,
		source, source_ref, description, required) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), projectID, variable.Key, variable.Secret, plain, nullBytes(sealed.Ciphertext), nullBytes(sealed.Nonce),
		nullString(sealed.KeyID), variable.Source, variable.SourceRef, variable.Description, variable.Required)
	return err
}

// EnvVars returns the stored .env entries with secrets decrypted. A row that
// fails authentication (edited, swapped between projects or variables) is an
// error, never an empty value.
func (store *Store) EnvVars(ctx context.Context, projectID string) ([]EnvVar, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT key, is_secret, value_plain, value_ct, nonce, key_id, source, source_ref,
		description, required FROM env_vars WHERE project_id=? ORDER BY rowid`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var variables []EnvVar
	for rows.Next() {
		variable, err := store.scanEnvVar(rows, projectID)
		if err != nil {
			return nil, err
		}
		variables = append(variables, variable)
	}
	return variables, rows.Err()
}

func (store *Store) scanEnvVar(rows *sql.Rows, projectID string) (EnvVar, error) {
	var variable EnvVar
	var plain, keyID sql.NullString
	var sealed secrets.Sealed
	err := rows.Scan(&variable.Key, &variable.Secret, &plain, &sealed.Ciphertext, &sealed.Nonce, &keyID,
		&variable.Source, &variable.SourceRef, &variable.Description, &variable.Required)
	if err != nil || !variable.Secret {
		variable.Value = plain.String
		return variable, err
	}
	sealed.KeyID = keyID.String
	value, err := store.sealer.Open(sealed, secrets.ProjectAAD(projectID, variable.Key))
	if err != nil {
		return EnvVar{}, errors.Join(errors.New("cannot decrypt "+variable.Key), err)
	}
	variable.Value = string(value)
	return variable, nil
}

func nullBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return value
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
