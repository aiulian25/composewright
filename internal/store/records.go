package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
)

// ProjectRecord is a project with everything a backup keeps of it: its
// generations with their images, and its .env with secrets opened.
type ProjectRecord struct {
	Project     Project            `json:"project"`
	Generations []GenerationRecord `json:"generations"`
	Variables   []EnvVar           `json:"variables"`
}

// GenerationRecord is a generation with its images.
type GenerationRecord struct {
	Generation Generation `json:"generation"`
	Images     []Image    `json:"images"`
}

// ProjectRecords reads every project for a backup.
func (store *Store) ProjectRecords(ctx context.Context) ([]ProjectRecord, error) {
	records, _, err := store.BackupRecords(ctx, nil)
	return records, err
}

// BackupRecords reads project data and referenced credentials from one read
// transaction; a generation and its current variables cannot straddle a write.
func (store *Store) BackupRecords(ctx context.Context, names []string) (records []ProjectRecord, keys map[string]string, err error) {
	err = store.transaction(ctx, func(tx *sql.Tx) error {
		view := store.view(tx)
		var failure error
		records, failure = view.projectRecords(ctx)
		if failure != nil {
			return failure
		}
		keys = map[string]string{}
		for _, name := range names {
			value, found, failure := view.Secret(ctx, name)
			if failure != nil {
				return failure
			}
			if found {
				keys[name] = value
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return records, keys, nil
}

func (store *Store) projectRecords(ctx context.Context) ([]ProjectRecord, error) {
	summaries, err := store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]ProjectRecord, 0, len(summaries))
	for _, summary := range summaries {
		record, err := store.projectRecord(ctx, summary.Project)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (store *Store) projectRecord(ctx context.Context, project Project) (ProjectRecord, error) {
	project.OrganizationRevision = 0 // Concurrency tokens are local UI state, not backup data.
	record := ProjectRecord{Project: project}
	generations, err := store.Generations(ctx, project.ID)
	if err != nil {
		return record, err
	}
	for _, generation := range generations {
		images, err := store.Images(ctx, generation.ID)
		if err != nil {
			return record, err
		}
		if generation.Files, err = store.Files(ctx, generation.ID); err != nil {
			return record, err
		}
		record.Generations = append(record.Generations, GenerationRecord{Generation: generation, Images: images})
	}
	record.Variables, err = store.EnvVars(ctx, project.ID)
	return record, err
}

// RestoreProject writes a project from a backup in one transaction, keeping
// the numbers and times of its generations. A project of the same name is
// kept unless replace is set, which deletes it first; restored says whether
// the backup's project was written. Its secrets are sealed with this
// install's key, and its generations point at no run logs, which a backup
// does not carry.
func (store *Store) RestoreProject(ctx context.Context, record ProjectRecord, replace bool) (restored bool, err error) {
	err = store.transaction(ctx, func(tx *sql.Tx) error {
		var failure error
		restored, failure = store.restoreProject(ctx, tx, record, replace)
		return failure
	})
	return restored && err == nil, err
}

func (store *Store) restoreProject(ctx context.Context, transaction *sql.Tx, record ProjectRecord, replace bool) (bool, error) {

	existing, err := findProject(ctx, transaction, record.Project.Name)
	found := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if found && !replace {
		return false, nil
	}
	if found {
		if _, err := transaction.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, existing.ID); err != nil {
			return false, err
		}
	}
	project := record.Project
	project.ID = newID()
	_, err = transaction.ExecContext(ctx, `INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		project.ID, project.Name, project.Slug, project.SourceKind, project.SourceURL, project.SourceRef,
		project.SourceCommit, project.SourceOwner, project.Status, unix(project.CreatedAt), unix(project.UpdatedAt))
	if err != nil {
		return false, err
	}
	if err := restoreOrganization(ctx, transaction, project); err != nil {
		return false, err
	}
	// Oldest first: versions made in the same second are listed in the
	// order they were stored, which has to match their numbers.
	oldestFirst := slices.SortedFunc(slices.Values(record.Generations), func(left, right GenerationRecord) int {
		return cmp.Compare(left.Generation.Number, right.Generation.Number)
	})
	for _, kept := range oldestFirst {
		generation := kept.Generation
		generation.ID, generation.ProjectID, generation.LogsPath = newID(), project.ID, ""
		if err := store.insertGeneration(ctx, transaction, generation, kept.Images); err != nil {
			return false, err
		}
	}
	err = store.replaceEnvVars(ctx, transaction, project.ID, record.Variables)
	return err == nil, err
}
