package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// RestoreProgress is committed with the restored database records. A restart
// can distinguish a committed batch from an operation that must be retried.
type RestoreProgress struct {
	Restored []string
	Kept     []string
}

// RestoreRecords applies all projects, credentials and its recovery checkpoint
// atomically. Expected records protect edits made since the recovery snapshot.
func (store *Store) RestoreRecords(ctx context.Context, records, expected []ProjectRecord, keys map[string]string, replace bool, checkpoint string) (progress RestoreProgress, err error) {
	err = store.transaction(ctx, func(tx *sql.Tx) error {
		view := store.view(tx)
		old := map[string]ProjectRecord{}
		for _, record := range expected {
			old[record.Project.Name] = record
		}
		for _, record := range records {
			current, failure := findProject(ctx, tx, record.Project.Name)
			if failure != nil && !errors.Is(failure, ErrNotFound) {
				return failure
			}
			if failure == nil && !replace {
				progress.Kept = append(progress.Kept, record.Project.Name)
				continue
			}
			before, wasPresent := old[record.Project.Name]
			if wasPresent != (failure == nil) {
				return errors.New("project state changed after restore preparation; cancel and prepare the restore again")
			}
			if wasPresent {
				actual, failure := view.projectRecord(ctx, current)
				if failure != nil {
					return failure
				}
				a, _ := json.Marshal(actual)
				b, _ := json.Marshal(before)
				if !bytes.Equal(a, b) {
					return fmt.Errorf("project %s changed after restore preparation; cancel and prepare again", record.Project.Name)
				}
			}
			if _, failure := store.restoreProject(ctx, tx, record, replace); failure != nil {
				return failure
			}
			progress.Restored = append(progress.Restored, record.Project.Name)
		}
		for name, value := range keys {
			if failure := store.setSecret(ctx, tx, name, value); failure != nil {
				return failure
			}
		}
		content, failure := json.Marshal(progress)
		if failure != nil {
			return failure
		}
		return store.setSecret(ctx, tx, checkpoint, string(content))
	})
	if err != nil {
		return RestoreProgress{}, err
	}
	return progress, nil
}

// PrepareRestore replaces the intent and removes a previous completed batch's
// checkpoint in one transaction, before any restored content can change.
func (store *Store) PrepareRestore(ctx context.Context, journal, checkpoint, content string) error {
	return store.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, checkpoint); err != nil {
			return err
		}
		return store.setSecret(ctx, tx, journal, content)
	})
}
