package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	StatusGenerated  = "generated"
	StatusBlocked    = "blocked"
	StatusPassed     = "passed"
	StatusFailed     = "failed"
	StatusNeedsHuman = "needs_human"
)

type Project struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	SourceKind   string    `json:"source_kind"`
	SourceURL    string    `json:"source_url"`
	SourceRef    string    `json:"source_ref"`
	SourceCommit string    `json:"source_commit"`
	SourceOwner  string    `json:"source_owner"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProjectSummary is a project with the facts `list` shows about its latest
// generation.
type ProjectSummary struct {
	Project
	Generations int    `json:"generations"`
	Engine      string `json:"engine"`
	Images      string `json:"images"`
}

const (
	projectColumns        = `id, name, slug, source_kind, source_url, source_ref, source_commit, source_owner, status, created_at, updated_at`
	projectColumnsAliased = `p.id, p.name, p.slug, p.source_kind, p.source_url, p.source_ref, p.source_commit, p.source_owner, p.status, p.created_at, p.updated_at`
)

// SaveProject creates the project, or updates the source and status of the
// project with the same name so regenerating keeps history in one place.
func (store *Store) SaveProject(ctx context.Context, project Project) (Project, error) {
	var saved Project
	err := store.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		saved, err = upsertProject(ctx, transaction, project)
		return err
	})
	return saved, err
}

func upsertProject(ctx context.Context, transaction *sql.Tx, project Project) (Project, error) {
	existing, err := findProject(ctx, transaction, project.Name)
	now := time.Now()
	if errors.Is(err, ErrNotFound) {
		project.ID, project.CreatedAt, project.UpdatedAt = newID(), now, now
		_, err = transaction.ExecContext(ctx, `INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			project.ID, project.Name, project.Slug, project.SourceKind, project.SourceURL, project.SourceRef,
			project.SourceCommit, project.SourceOwner, project.Status, unix(now), unix(now))
		return project, err
	}
	if err != nil {
		return Project{}, err
	}
	project.ID, project.Slug, project.CreatedAt, project.UpdatedAt = existing.ID, existing.Slug, existing.CreatedAt, now
	_, err = transaction.ExecContext(ctx, `UPDATE projects SET source_kind=?, source_url=?, source_ref=?, source_commit=?,
		source_owner=?, status=?, updated_at=? WHERE id=?`,
		project.SourceKind, project.SourceURL, project.SourceRef, project.SourceCommit, project.SourceOwner, project.Status, unix(now), project.ID)
	return project, err
}

func (store *Store) SetProjectStatus(ctx context.Context, projectID, status string) error {
	_, err := store.database.ExecContext(ctx, `UPDATE projects SET status=?, updated_at=? WHERE id=?`, status, unix(time.Now()), projectID)
	return err
}

// FindProject looks a project up by name, slug or full id.
func (store *Store) FindProject(ctx context.Context, reference string) (Project, error) {
	return findProject(ctx, store.database, reference)
}

// rowQuerier is the database, or a transaction a lookup takes part in.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, arguments ...any) *sql.Row
}

func findProject(ctx context.Context, querier rowQuerier, reference string) (Project, error) {
	row := querier.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE name=? OR slug=? OR id=?`,
		reference, reference, reference)
	project, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project %q: %w", reference, ErrNotFound)
	}
	return project, err
}

func (store *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT `+projectColumnsAliased+`,
		(SELECT COUNT(*) FROM generations g WHERE g.project_id = p.id),
		COALESCE((SELECT engine FROM generations g WHERE g.project_id = p.id ORDER BY created_at DESC, rowid DESC LIMIT 1), ''),
		COALESCE((SELECT GROUP_CONCAT(i.reference, ' ') FROM images i WHERE i.generation_id =
			(SELECT id FROM generations g WHERE g.project_id = p.id ORDER BY created_at DESC, rowid DESC LIMIT 1)), '')
		FROM projects p ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []ProjectSummary
	for rows.Next() {
		var summary ProjectSummary
		var created, updated int64
		project := &summary.Project
		if err := rows.Scan(&project.ID, &project.Name, &project.Slug, &project.SourceKind, &project.SourceURL, &project.SourceRef,
			&project.SourceCommit, &project.SourceOwner, &project.Status, &created, &updated,
			&summary.Generations, &summary.Engine, &summary.Images); err != nil {
			return nil, err
		}
		project.CreatedAt, project.UpdatedAt = fromUnix(created), fromUnix(updated)
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

// DeleteProject removes the project; generations, images and every sealed
// secret go with it through ON DELETE CASCADE.
func (store *Store) DeleteProject(ctx context.Context, projectID string) error {
	result, err := store.database.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("project %s: %w", projectID, ErrNotFound)
	}
	return nil
}

type rowScanner interface {
	Scan(destinations ...any) error
}

func scanProject(row rowScanner) (Project, error) {
	var project Project
	var created, updated int64
	err := row.Scan(&project.ID, &project.Name, &project.Slug, &project.SourceKind, &project.SourceURL, &project.SourceRef,
		&project.SourceCommit, &project.SourceOwner, &project.Status, &created, &updated)
	project.CreatedAt, project.UpdatedAt = fromUnix(created), fromUnix(updated)
	return project, err
}
