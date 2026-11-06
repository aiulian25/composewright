package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/aiulian25/composewright/internal/secrets"
)

type Generation struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	// Number is the version a person sees, v1 first; pruning never reuses it.
	Number         int       `json:"number"`
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
	// Files are the files the generation bundles beside compose.yaml, by
	// path, kept sealed. Reading generations leaves them out; Files reads
	// them.
	Files map[string][]byte `json:"files,omitempty"`
	// Base, when set, is the version a change was made from:
	// RecordGeneration refuses the change with ErrStale once another
	// version is the latest. It is never stored.
	Base string `json:"-"`
}

// ErrStale is a change made from a version that is no longer the latest,
// as another tab or the command line saved one since.
var ErrStale = errors.New("this project has a newer version than the one you changed")

type Image struct {
	Service    string    `json:"service"`
	Reference  string    `json:"reference"`
	Digest     string    `json:"digest"`
	Registry   string    `json:"registry"`
	OCISource  string    `json:"oci_source"`
	VerifiedAt time.Time `json:"verified_at"`
	// Labels are the image's OCI labels and Created when it was built, as
	// the registry said when this version was made.
	Labels  map[string]string `json:"labels,omitempty"`
	Created time.Time         `json:"created"`
}

const generationColumns = `id, project_id, iteration, engine, provider, model, prompt_hash, ir_json, compose_yaml, deploy_md,
	options_json, policy_verdict_json, validation_verdict_json, logs_path, facts_json, created_at, number`

// keptUntested is how many generations that never ran in the sandbox a
// project keeps; tested ones keep their verdict and logs for good.
const keptUntested = 20

// AddGeneration stores a generation and its images.
func (store *Store) AddGeneration(ctx context.Context, generation Generation, images []Image) (Generation, error) {
	var added Generation
	err := store.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		added, err = store.addGeneration(ctx, transaction, generation, images)
		return err
	})
	return added, err
}

func (store *Store) addGeneration(ctx context.Context, transaction *sql.Tx, generation Generation, images []Image) (Generation, error) {
	generation.ID, generation.CreatedAt = newID(), time.Now()
	err := transaction.QueryRowContext(ctx, `SELECT COALESCE(MAX(number), 0) + 1 FROM generations WHERE project_id=?`, generation.ProjectID).
		Scan(&generation.Number)
	if err != nil {
		return Generation{}, err
	}
	return generation, store.insertGeneration(ctx, transaction, generation, images)
}

// insertGeneration writes a generation, its images and its files as they
// are, the files sealed.
func (store *Store) insertGeneration(ctx context.Context, transaction *sql.Tx, generation Generation, images []Image) error {
	_, err := transaction.ExecContext(ctx, `INSERT INTO generations (`+generationColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		generation.ID, generation.ProjectID, generation.Iteration, generation.Engine, generation.Provider, generation.Model,
		generation.PromptHash, generation.IRJSON, generation.ComposeYAML, generation.DeployMD, generation.OptionsJSON,
		generation.PolicyJSON, generation.ValidationJSON, generation.LogsPath, generation.FactsJSON, unix(generation.CreatedAt), generation.Number)
	if err != nil {
		return err
	}
	for _, image := range images {
		labels, err := labelsJSON(image.Labels)
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `INSERT INTO images (id, generation_id, service, reference, digest, registry, oci_source, verified_at,
			labels_json, created) VALUES (?,?,?,?,?,?,?,?,?,?)`, newID(), generation.ID, image.Service, image.Reference, image.Digest, image.Registry,
			image.OCISource, unix(image.VerifiedAt), labels, unix(image.Created))
		if err != nil {
			return err
		}
	}
	return store.insertFiles(ctx, transaction, generation.ID, generation.Files)
}

func (store *Store) insertFiles(ctx context.Context, transaction *sql.Tx, generationID string, files map[string][]byte) error {
	for _, path := range slices.Sorted(maps.Keys(files)) {
		sealed, err := store.sealer.Load().Seal(files[path], secrets.FileAAD(generationID, path))
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `INSERT INTO generation_files (id, generation_id, path, content_ct, nonce, key_id) VALUES (?,?,?,?,?,?)`,
			newID(), generationID, path, sealed.Ciphertext, sealed.Nonce, sealed.KeyID)
		if err != nil {
			return err
		}
	}
	return nil
}

// Files reads the files a generation bundles, by path.
func (store *Store) Files(ctx context.Context, generationID string) (map[string][]byte, error) {
	rows, err := store.reader().QueryContext(ctx, `SELECT path, content_ct, nonce, key_id FROM generation_files WHERE generation_id=?`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := map[string][]byte{}
	for rows.Next() {
		var path string
		var sealed secrets.Sealed
		if err := rows.Scan(&path, &sealed.Ciphertext, &sealed.Nonce, &sealed.KeyID); err != nil {
			return nil, err
		}
		if files[path], err = store.sealer.Load().Open(sealed, secrets.FileAAD(generationID, path)); err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	return files, rows.Err()
}

// RecordGeneration stores a generation with everything that belongs to it
// in one transaction: its project, the generation and its images, the
// project's .env, and the audit entries events builds once the project and
// the generation have their ids. It also drops the project's untested
// generations beyond the newest keptUntested. A failure in any part leaves
// none of them.
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
		if err := latestIs(ctx, transaction, saved.ID, generation.Base); err != nil {
			return err
		}
		if added, err = store.addGeneration(ctx, transaction, generation, images); err != nil {
			return err
		}
		if err := pruneGenerations(ctx, transaction, saved.ID); err != nil {
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

// latestIs refuses with ErrStale when base is set and the project's latest
// version is another.
func latestIs(ctx context.Context, transaction *sql.Tx, projectID, base string) error {
	if base == "" {
		return nil
	}
	var latest string
	err := transaction.QueryRowContext(ctx, `SELECT id FROM generations WHERE project_id=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID).Scan(&latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if latest != base {
		return ErrStale
	}
	return nil
}

// pruneGenerations deletes a project's untested generations beyond the
// newest keptUntested, and their images with them. A generation gets its
// logs only with its verdict, so no log file is left behind.
func pruneGenerations(ctx context.Context, transaction *sql.Tx, projectID string) error {
	_, err := transaction.ExecContext(ctx, `DELETE FROM generations WHERE project_id=? AND validation_verdict_json='' AND id NOT IN
		(SELECT id FROM generations WHERE project_id=? AND validation_verdict_json='' ORDER BY created_at DESC, rowid DESC LIMIT ?)`,
		projectID, projectID, keptUntested)
	return err
}

// RemoveDrafts deletes a project's versions numbered below before that were
// never tested, with their images: once a later version passes, a draft it
// replaced is only clutter. It returns how many it removed.
func (store *Store) RemoveDrafts(ctx context.Context, projectID string, before int) (int64, error) {
	removed, err := store.database.ExecContext(ctx, `DELETE FROM generations WHERE project_id=? AND validation_verdict_json='' AND number < ?`,
		projectID, before)
	if err != nil {
		return 0, err
	}
	return removed.RowsAffected()
}

// RecordValidation stores a verdict, where its logs are, and the DEPLOY.md
// carrying the validation status line.
func (store *Store) RecordValidation(ctx context.Context, generationID, verdictJSON, logsPath, deploy string) error {
	_, err := store.database.ExecContext(ctx, `UPDATE generations SET validation_verdict_json=?, logs_path=?, deploy_md=? WHERE id=?`,
		verdictJSON, logsPath, deploy, generationID)
	return err
}

func (store *Store) LatestGeneration(ctx context.Context, projectID string) (Generation, error) {
	row := store.reader().QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE project_id=?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, fmt.Errorf("generation for project %s: %w", projectID, ErrNotFound)
	}
	return generation, err
}

// Generations lists a project's generations, newest first.
func (store *Store) Generations(ctx context.Context, projectID string) ([]Generation, error) {
	rows, err := store.reader().QueryContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE project_id=?
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
	rows, err := store.reader().QueryContext(ctx, `SELECT service, reference, digest, registry, oci_source, verified_at, labels_json, created
		FROM images WHERE generation_id=? ORDER BY service`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []Image
	for rows.Next() {
		var image Image
		var verified, created int64
		var labels string
		if err := rows.Scan(&image.Service, &image.Reference, &image.Digest, &image.Registry, &image.OCISource, &verified, &labels, &created); err != nil {
			return nil, err
		}
		if image.Labels, err = labelsFrom(labels); err != nil {
			return nil, fmt.Errorf("labels of image %s: %w", image.Reference, err)
		}
		image.VerifiedAt, image.Created = fromUnix(verified), fromUnix(created)
		images = append(images, image)
	}
	return images, rows.Err()
}

// labelsJSON is how labels are stored: "" when there are none.
func labelsJSON(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(labels)
	return string(encoded), err
}

// labelsFrom reads labels as labelsJSON stores them.
func labelsFrom(encoded string) (map[string]string, error) {
	if encoded == "" {
		return nil, nil
	}
	var labels map[string]string
	err := json.Unmarshal([]byte(encoded), &labels)
	return labels, err
}

func scanGeneration(row rowScanner) (Generation, error) {
	var generation Generation
	var created int64
	err := row.Scan(&generation.ID, &generation.ProjectID, &generation.Iteration, &generation.Engine, &generation.Provider,
		&generation.Model, &generation.PromptHash, &generation.IRJSON, &generation.ComposeYAML, &generation.DeployMD,
		&generation.OptionsJSON, &generation.PolicyJSON, &generation.ValidationJSON, &generation.LogsPath, &generation.FactsJSON, &created,
		&generation.Number)
	generation.CreatedAt = fromUnix(created)
	return generation, err
}
