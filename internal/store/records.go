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
	err = store.transaction(ctx, func(transaction *sql.Tx) error {
		existing, err := findProject(ctx, transaction, record.Project.Name)
		found := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if found && !replace {
			return nil
		}
		if found {
			if _, err := transaction.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, existing.ID); err != nil {
				return err
			}
		}
		project := record.Project
		project.ID = newID()
		_, err = transaction.ExecContext(ctx, `INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			project.ID, project.Name, project.Slug, project.SourceKind, project.SourceURL, project.SourceRef,
			project.SourceCommit, project.SourceOwner, project.Status, unix(project.CreatedAt), unix(project.UpdatedAt))
		if err != nil {
			return err
		}
		// Oldest first: versions made in the same second are listed in the
		// order they were stored, which has to match their numbers.
		oldestFirst := slices.SortedFunc(slices.Values(record.Generations), func(left, right GenerationRecord) int {
			return cmp.Compare(left.Generation.Number, right.Generation.Number)
		})
		for _, kept := range oldestFirst {
			generation := kept.Generation
			generation.ID, generation.ProjectID, generation.LogsPath = newID(), project.ID, ""
			if err := insertGeneration(ctx, transaction, generation, kept.Images); err != nil {
				return err
			}
		}
		restored = true
		return store.replaceEnvVars(ctx, transaction, project.ID, record.Variables)
	})
	return restored && err == nil, err
}
