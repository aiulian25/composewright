package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	ActionInit         = "init"
	ActionGenerate     = "generate"
	ActionApprove      = "approve"
	ActionValidate     = "validate"
	ActionReveal       = "reveal"
	ActionExport       = "export"
	ActionDelete       = "delete"
	ActionRotate       = "rotate"
	ActionPurgeKeys    = "purge_keys"
	ActionSetSecret    = "set_secret"
	ActionDeleteSecret = "delete_secret"
	ActionGrantNeed    = "grant_need"
	ActionRevise       = "revise"
	ActionEdit         = "edit"
	ActionSetup        = "setup"
	ActionLogin        = "login"
	ActionPassword     = "password"
	ActionSettings     = "settings"
	genesisAuditHash   = ""
)

type AuditEntry struct {
	Sequence  int64          `json:"seq"`
	Time      time.Time      `json:"ts"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	ProjectID string         `json:"project_id,omitempty"`
	Details   map[string]any `json:"details"`
	Hash      string         `json:"hash"`
}

// Audit appends an entry whose hash covers the previous entry's hash, so
// editing or deleting any row breaks every hash after it.
// Event is one audit entry to write: what was done, and its details.
type Event struct {
	Action  string
	Details map[string]any
}

func (store *Store) Audit(ctx context.Context, actor, action, projectID string, details map[string]any) error {
	return store.transaction(ctx, func(transaction *sql.Tx) error {
		return appendAudit(ctx, transaction, actor, action, projectID, details)
	})
}

// appendAudit chains an entry onto the newest one.
func appendAudit(ctx context.Context, transaction *sql.Tx, actor, action, projectID string, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	var previous string
	var sequence int64
	err = transaction.QueryRowContext(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&sequence, &previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	sequence++
	now := time.Now().UTC().Unix()
	hash := auditHash(sequence, now, actor, action, projectID, string(encoded), previous)
	_, err = transaction.ExecContext(ctx, `INSERT INTO audit_log (seq, ts, actor, action, project_id, details_json, prev_hash, hash)
		VALUES (?,?,?,?,?,?,?,?)`, sequence, now, actor, action, projectID, string(encoded), previous, hash)
	return err
}

// VerifyAudit recomputes the chain and returns the number of entries.
func (store *Store) VerifyAudit(ctx context.Context) (int, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT seq, ts, actor, action, project_id, details_json, prev_hash, hash
		FROM audit_log ORDER BY seq`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	previous, count := genesisAuditHash, 0
	for rows.Next() {
		var sequence, timestamp int64
		var actor, action, projectID, details, storedPrevious, stored string
		if err := rows.Scan(&sequence, &timestamp, &actor, &action, &projectID, &details, &storedPrevious, &stored); err != nil {
			return count, err
		}
		count++
		expected := auditHash(sequence, timestamp, actor, action, projectID, details, previous)
		if storedPrevious != previous || stored != expected || sequence != int64(count) {
			return count, fmt.Errorf("audit log entry %d does not match the chain: it was edited, reordered or a row was removed", sequence)
		}
		previous = stored
	}
	return count, rows.Err()
}

func (store *Store) AuditEntries(ctx context.Context, projectID string, limit int) ([]AuditEntry, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT seq, ts, actor, action, project_id, details_json, hash FROM audit_log
		WHERE ?='' OR project_id=? ORDER BY seq DESC LIMIT ?`, projectID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		var timestamp int64
		var details string
		if err := rows.Scan(&entry.Sequence, &timestamp, &entry.Actor, &entry.Action, &entry.ProjectID, &details, &entry.Hash); err != nil {
			return nil, err
		}
		entry.Time = fromUnix(timestamp)
		if err := json.Unmarshal([]byte(details), &entry.Details); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func auditHash(sequence, timestamp int64, actor, action, projectID, details, previous string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", sequence, timestamp, actor, action, projectID, details, previous)))
	return hex.EncodeToString(digest[:])
}
