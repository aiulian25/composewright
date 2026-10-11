package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Generation struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	Iteration      int       `json:"iteration"`
	Engine         string    `json:"engine"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	PromptHash     string    `json:"prompt_hash"`
	IRJSON         string    `json:"ir_json"`
	ComposeYAML    string    `json:"compose_yaml"`
	DeployMD       string    `json:"deploy_md"`
	OptionsJSON    string    `json:"options_json"`
	PolicyJSON     string    `json:"policy_verdict_json"`
	ValidationJSON string    `json:"validation_verdict_json"`
	LogsPath       string    `json:"logs_path"`
	FactsJSON      string    `json:"facts_json"`
	CreatedAt      time.Time `json:"created_at"`
}

type Image struct {
	Service    string    `json:"service"`
	Reference  string    `json:"reference"`
	Digest     string    `json:"digest"`
	Registry   string    `json:"registry"`
	OCISource  string    `json:"oci_source"`
	VerifiedAt time.Time `json:"verified_at"`
}

const generationColumns = `id, project_id, iteration, engine, provider, model, prompt_hash, ir_json, compose_yaml, deploy_md,
	options_json, policy_verdict_json, validation_verdict_json, logs_path, facts_json, created_at`

// AddGeneration stores a generation and its images.
func (store *Store) AddGeneration(ctx context.Context, generation Generation, images []Image) (Generation, error) {
	var added Generation
	err := store.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		added, err = addGeneration(ctx, transaction, generation, images)
		return err
	})
	return added, err
}

func addGeneration(ctx context.Context, transaction *sql.Tx, generation Generation, images []Image) (Generation, error) {
	generation.ID, generation.CreatedAt = newID(), time.Now()
	_, err := transaction.ExecContext(ctx, `INSERT INTO generations (`+generationColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		generation.ID, generation.ProjectID, generation.Iteration, generation.Engine, generation.Provider, generation.Model,
		generation.PromptHash, generation.IRJSON, generation.ComposeYAML, generation.DeployMD, generation.OptionsJSON,
		generation.PolicyJSON, generation.ValidationJSON, generation.LogsPath, generation.FactsJSON, unix(generation.CreatedAt))
	if err != nil {
		return Generation{}, err
	}
	for _, image := range images {
		_, err := transaction.ExecContext(ctx, `INSERT INTO images (id, generation_id, service, reference, digest, registry, oci_source, verified_at)
			VALUES (?,?,?,?,?,?,?,?)`, newID(), generation.ID, image.Service, image.Reference, image.Digest, image.Registry,
			image.OCISource, unix(image.VerifiedAt))
		if err != nil {
			return Generation{}, err
		}
	}
	return generation, nil
}

// RecordGeneration stores a generation with everything that belongs to it
// in one transaction: its project, the generation and its images, the
// project's .env, and the audit entries events builds once the project and
// the generation have their ids. A failure in any part leaves none of them.
func (store *Store) RecordGeneration(ctx context.Context, project Project, generation Generation, images []Image, variables []EnvVar,
	actor string, events func(Project, Generation) []Event) (Project, Generation, error) {
	var saved Project
	var added Generation
	err := store.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		if saved, err = upsertProject(ctx, transaction, project); err != nil {
			return err
		}
		generation.ProjectID = saved.ID
		if added, err = addGeneration(ctx, transaction, generation, images); err != nil {
			return err
		}
		if err := store.replaceEnvVars(ctx, transaction, saved.ID, variables); err != nil {
			return err
		}
		for _, event := range events(saved, added) {
			if err := appendAudit(ctx, transaction, actor, event.Action, saved.ID, event.Details); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Project{}, Generation{}, err
	}
	return saved, added, nil
}

// RecordValidation stores a verdict, where its logs are, and the DEPLOY.md
// carrying the validation status line.
func (store *Store) RecordValidation(ctx context.Context, generationID, verdictJSON, logsPath, deploy string) error {
	_, err := store.database.ExecContext(ctx, `UPDATE generations SET validation_verdict_json=?, logs_path=?, deploy_md=? WHERE id=?`,
		verdictJSON, logsPath, deploy, generationID)
	return err
}

func (store *Store) LatestGeneration(ctx context.Context, projectID string) (Generation, error) {
	row := store.database.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE project_id=?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, fmt.Errorf("generation for project %s: %w", projectID, ErrNotFound)
	}
	return generation, err
}

// Generations lists a project's generations, newest first.
func (store *Store) Generations(ctx context.Context, projectID string) ([]Generation, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE project_id=?
		ORDER BY created_at DESC, rowid DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var generations []Generation
	for rows.Next() {
		generation, err := scanGeneration(rows)
		if err != nil {
			return nil, err
		}
		generations = append(generations, generation)
	}
	return generations, rows.Err()
}

func (store *Store) Images(ctx context.Context, generationID string) ([]Image, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT service, reference, digest, registry, oci_source, verified_at
		FROM images WHERE generation_id=? ORDER BY service`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []Image
	for rows.Next() {
		var image Image
		var verified int64
		if err := rows.Scan(&image.Service, &image.Reference, &image.Digest, &image.Registry, &image.OCISource, &verified); err != nil {
			return nil, err
		}
		image.VerifiedAt = fromUnix(verified)
		images = append(images, image)
	}
	return images, rows.Err()
}

func scanGeneration(row rowScanner) (Generation, error) {
	var generation Generation
	var created int64
	err := row.Scan(&generation.ID, &generation.ProjectID, &generation.Iteration, &generation.Engine, &generation.Provider,
		&generation.Model, &generation.PromptHash, &generation.IRJSON, &generation.ComposeYAML, &generation.DeployMD,
		&generation.OptionsJSON, &generation.PolicyJSON, &generation.ValidationJSON, &generation.LogsPath, &generation.FactsJSON, &created)
	generation.CreatedAt = fromUnix(created)
	return generation, err
}
