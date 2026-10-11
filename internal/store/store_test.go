package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/secrets"
)

func openTestStore(t *testing.T) (*Store, string, secrets.Key) {
	t.Helper()
	key, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "composewright.db")
	store, err := Open(context.Background(), path, filepath.Join(directory, "backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path, key
}

func saveProject(t *testing.T, store *Store, name string) Project {
	t.Helper()
	project, err := store.SaveProject(context.Background(), Project{Name: name, Slug: name, SourceKind: "github", Status: StatusGenerated})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestDatabaseFileIsPrivate(t *testing.T) {
	_, path, _ := openTestStore(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("database mode %04o", info.Mode().Perm())
	}
}

func TestSecretsAreNeverStoredInPlaintext(t *testing.T) {
	ctx := context.Background()
	store, path, _ := openTestStore(t)
	project := saveProject(t, store, "miniflux")
	const secretValue = "correct-horse-battery-staple-123"
	variables := []EnvVar{
		{Key: "BASE_URL", Value: "http://localhost:8080", Required: true, Source: "readme"},
		{Key: "POSTGRES_PASSWORD", Value: secretValue, Secret: true, Required: true, Source: "sidecar_rule"},
	}
	if err := store.ReplaceEnvVars(ctx, project.ID, variables); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecret(ctx, "llm", "sk-live-should-not-leak"); err != nil {
		t.Fatal(err)
	}
	got, err := store.EnvVars(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Value != secretValue || got[0].Value != "http://localhost:8080" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if value, found, err := store.Secret(ctx, "llm"); err != nil || !found || value != "sk-live-should-not-leak" {
		t.Fatalf("setting round trip: %q %v %v", value, found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, path + "-wal"} {
		content, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{secretValue, "sk-live-should-not-leak"} {
			if bytes.Contains(content, []byte(leaked)) {
				t.Fatalf("%s contains a secret in plaintext", filepath.Base(file))
			}
		}
	}
}

// TestHasSecretOpensNothing: HasSecret tells a stored key from a missing one
// without decrypting, so it answers even for a key that would not open.
func TestHasSecretOpensNothing(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	if err := store.SetSecret(ctx, "llm-claude", "sk-test"); err != nil {
		t.Fatal(err)
	}
	otherKey, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	store.sealer = secrets.NewSealer(otherKey)
	if _, _, err := store.Secret(ctx, "llm-claude"); err == nil {
		t.Fatal("a key sealed under another master key opened")
	}
	for name, want := range map[string]bool{"llm-claude": true, "llm-missing": false} {
		if stored, err := store.HasSecret(ctx, name); err != nil || stored != want {
			t.Fatalf("HasSecret(%q) = %v, %v; want %v", name, stored, err, want)
		}
	}
}

func TestSwappedCiphertextIsRejected(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	first, second := saveProject(t, store, "first"), saveProject(t, store, "second")
	for _, project := range []Project{first, second} {
		if err := store.ReplaceEnvVars(ctx, project.ID, []EnvVar{{Key: "ADMIN_PASSWORD", Value: project.Name + "-secret", Secret: true}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.database.ExecContext(ctx, `UPDATE env_vars SET value_ct=(SELECT value_ct FROM env_vars WHERE project_id=?),
		nonce=(SELECT nonce FROM env_vars WHERE project_id=?) WHERE project_id=?`, second.ID, second.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnvVars(ctx, first.ID); err == nil {
		t.Fatal("a value moved from another project decrypted")
	}
	if values, err := store.EnvVars(ctx, second.ID); err != nil || values[0].Value != "second-secret" {
		t.Fatalf("untouched project damaged: %+v %v", values, err)
	}
}

func TestDeleteProjectWipesSecrets(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	project := saveProject(t, store, "doomed")
	if err := store.ReplaceEnvVars(ctx, project.ID, []EnvVar{{Key: "TOKEN", Value: "x123456", Secret: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddGeneration(ctx, Generation{ProjectID: project.ID, Engine: "rules", IRJSON: "{}"}, []Image{{Service: "app", Reference: "docker.io/library/nginx:1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.database.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM env_vars) + (SELECT COUNT(*) FROM generations) + (SELECT COUNT(*) FROM images)`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d dependent rows survived the delete", remaining)
	}
	if _, err := store.FindProject(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted project still found: %v", err)
	}
}

// TestRecordGenerationIsAllOrNothing: a generation is stored with its
// project, images, .env and audit entries, or not at all. Two variables with
// one key break the .env after the project and generation rows are written,
// and none of the five tables keeps a row.
func TestRecordGenerationIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	project := Project{Name: "app", Slug: "app", SourceKind: "github", Status: StatusGenerated}
	generation := Generation{Engine: "rules", IRJSON: "{}"}
	images := []Image{{Service: "app", Reference: "docker.io/library/nginx:1"}}
	events := func(_ Project, generation Generation) []Event {
		return []Event{{Action: ActionGenerate, Details: map[string]any{"generation": generation.ID}},
			{Action: ActionGrantNeed, Details: map[string]any{"capability": "low_port", "generation": generation.ID}}}
	}
	twice := []EnvVar{{Key: "TOKEN", Value: "first", Secret: true}, {Key: "TOKEN", Value: "second", Secret: true}}
	if _, _, err := store.RecordGeneration(ctx, project, generation, images, twice, "cli:test", events); err == nil {
		t.Fatal("a .env with one key twice was stored")
	}
	var rows int
	if err := store.database.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM projects) + (SELECT COUNT(*) FROM generations)
		+ (SELECT COUNT(*) FROM images) + (SELECT COUNT(*) FROM env_vars) + (SELECT COUNT(*) FROM audit_log)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d rows of a failed generation remain", rows)
	}
	saved, added, err := store.RecordGeneration(ctx, project, generation, images, twice[:1], "cli:test", events)
	if err != nil {
		t.Fatal(err)
	}
	if added.ProjectID != saved.ID {
		t.Fatalf("generation %+v is not tied to project %s", added, saved.ID)
	}
	if count, err := store.VerifyAudit(ctx); err != nil || count != 2 {
		t.Fatalf("%d audit entries (%v), want the generation and the grant", count, err)
	}
	if values, err := store.EnvVars(ctx, saved.ID); err != nil || len(values) != 1 || values[0].Value != "first" {
		t.Fatalf("stored .env: %+v %v", values, err)
	}
}

// TestPruneGenerationsKeepsTestedOnes: a project keeps its newest 20
// untested generations and every tested one, however old, and numbering
// goes on from the newest.
func TestPruneGenerationsKeepsTestedOnes(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	project := Project{Name: "app", Slug: "app", SourceKind: "github", Status: StatusGenerated}
	noEvents := func(Project, Generation) []Event { return nil }
	images := []Image{{Service: "app", Reference: "docker.io/library/nginx:1"}}
	var first, latest Generation
	for number := 1; number <= 25; number++ {
		_, added, err := store.RecordGeneration(ctx, project, Generation{Engine: "rules", IRJSON: "{}"}, images, nil, "cli:test", noEvents)
		if err != nil {
			t.Fatal(err)
		}
		if number == 2 || number == 5 {
			if err := store.RecordValidation(ctx, added.ID, `{"result":"pass"}`, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if number == 1 {
			first = added
		}
		latest = added
	}
	generations, err := store.Generations(ctx, latest.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var untested, tested []int
	for _, generation := range generations {
		if generation.ValidationJSON == "" {
			untested = append(untested, generation.Number)
			continue
		}
		tested = append(tested, generation.Number)
	}
	if len(untested) != keptUntested || untested[len(untested)-1] != 6 || !slices.Equal(tested, []int{5, 2}) || latest.Number != 25 {
		t.Fatalf("kept untested %v and tested %v; the newest is number %d", untested, tested, latest.Number)
	}
	if kept, err := store.Images(ctx, first.ID); err != nil || len(kept) != 0 {
		t.Fatalf("a pruned generation's images stayed: %v %v", kept, err)
	}
}

// TestMigrationNumbersExistingGenerations: a database from before version
// numbers numbers each project's generations in the order they were made,
// equal times in the order they were stored, and goes on from there.
func TestMigrationNumbersExistingGenerations(t *testing.T) {
	const lastUnnumbered = 2
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "composewright.db")
	if err := createPrivate(path); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open(driverName, dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	older := &Store{database: database}
	if _, err := database.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	pending, err := pendingMigrations(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range pending {
		if migration.version > lastUnnumbered {
			continue
		}
		if err := older.apply(ctx, migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = database.ExecContext(ctx, `INSERT INTO projects (id, name, slug, source_kind, status, created_at, updated_at) VALUES
		('p1', 'one', 'one', 'local', 'generated', 1, 1), ('p2', 'two', 'two', 'local', 'generated', 1, 1);
		INSERT INTO generations (id, project_id, iteration, engine, ir_json, created_at) VALUES
		('third', 'p1', 1, 'rules', '{}', 200), ('first', 'p1', 1, 'rules', '{}', 100), ('second', 'p1', 1, 'rules', '{}', 100),
		('only', 'p2', 1, 'rules', '{}', 150)`)
	if err := errors.Join(err, database.Close()); err != nil {
		t.Fatal(err)
	}
	key, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, path, filepath.Join(directory, "backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	want := map[string]int{"first": 1, "second": 2, "third": 3, "only": 1}
	for _, projectID := range []string{"p1", "p2"} {
		generations, err := store.Generations(ctx, projectID)
		if err != nil {
			t.Fatal(err)
		}
		for _, generation := range generations {
			if generation.Number != want[generation.ID] {
				t.Errorf("%s is number %d, want %d", generation.ID, generation.Number, want[generation.ID])
			}
		}
	}
	if added, err := store.AddGeneration(ctx, Generation{ProjectID: "p1", Engine: "rules", IRJSON: "{}"}, nil); err != nil || added.Number != 4 {
		t.Fatalf("the next generation after the migration is number %d: %v", added.Number, err)
	}
}

// TestRestoreProjectKeepsVersions: a project read for a backup restores into
// another store, with its own key, as it was: version numbers and times, the
// tested verdict without its log, the images and the secrets, which open with
// the new key. A project already there is kept unless replacing it was asked
// for.
func TestRestoreProjectKeepsVersions(t *testing.T) {
	ctx := context.Background()
	source, _, _ := openTestStore(t)
	project := Project{Name: "miniflux", Slug: "miniflux", SourceKind: "github", Status: StatusGenerated}
	noEvents := func(Project, Generation) []Event { return nil }
	images := []Image{{Service: "miniflux", Reference: "docker.io/miniflux/miniflux:2"}}
	variables := []EnvVar{{Key: "ADMIN_PASSWORD", Value: "s3cret-value", Secret: true}, {Key: "PORT", Value: "8080"}}
	var tested Generation
	for range 2 {
		_, added, err := source.RecordGeneration(ctx, project, Generation{Engine: "rules", IRJSON: "{}"}, images, variables, "cli:test", noEvents)
		if err != nil {
			t.Fatal(err)
		}
		tested = added
	}
	if err := source.RecordValidation(ctx, tested.ID, `{"result":"pass"}`, "/data/projects/miniflux/logs/run.log", ""); err != nil {
		t.Fatal(err)
	}
	records, err := source.ProjectRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("records %d: %v", len(records), err)
	}
	target, _, _ := openTestStore(t)
	if restored, err := target.RestoreProject(ctx, records[0], false); err != nil || !restored {
		t.Fatalf("restore: %v %v", restored, err)
	}
	stored, err := target.FindProject(ctx, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	generations, err := target.Generations(ctx, stored.ID)
	if err != nil || len(generations) != 2 {
		t.Fatalf("generations %d: %v", len(generations), err)
	}
	newest := generations[0]
	kept := records[0].Generations[0].Generation
	if newest.Number != 2 || newest.ValidationJSON == "" || newest.LogsPath != "" || !newest.CreatedAt.Equal(kept.CreatedAt) {
		t.Fatalf("newest version restored as %+v", newest)
	}
	if restoredImages, err := target.Images(ctx, newest.ID); err != nil || len(restoredImages) != 1 {
		t.Fatalf("images %v: %v", restoredImages, err)
	}
	if values, err := target.EnvVars(ctx, stored.ID); err != nil || values[0].Value != "s3cret-value" {
		t.Fatalf("secrets under the new key: %+v %v", values, err)
	}
	if restored, err := target.RestoreProject(ctx, records[0], false); err != nil || restored {
		t.Fatalf("a project already there was replaced without asking: %v %v", restored, err)
	}
	if restored, err := target.RestoreProject(ctx, records[0], true); err != nil || !restored {
		t.Fatalf("replace: %v %v", restored, err)
	}
	if replaced, err := target.FindProject(ctx, "miniflux"); err != nil || replaced.ID == stored.ID {
		t.Fatalf("the project was not replaced: %v", err)
	}
}

func TestSaveProjectKeepsOneProjectPerName(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openTestStore(t)
	first := saveProject(t, store, "miniflux")
	second, err := store.SaveProject(ctx, Project{Name: "miniflux", Slug: "ignored", SourceKind: "local", Status: StatusBlocked})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Slug != "miniflux" || second.Status != StatusBlocked {
		t.Fatalf("regeneration created a new project or lost the slug: %+v", second)
	}
	for range 2 {
		if _, err := store.AddGeneration(ctx, Generation{ProjectID: first.ID, Engine: "rules", IRJSON: "{}"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	summaries, err := store.ListProjects(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].Generations != 2 {
		t.Fatalf("list: %+v %v", summaries, err)
	}
}

func TestAuditChainDetectsTampering(t *testing.T) {
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"edited details":  `UPDATE audit_log SET details_json='{"key_id":"forged"}' WHERE seq=2`,
		"removed row":     `DELETE FROM audit_log WHERE seq=2`,
		"rewritten actor": `UPDATE audit_log SET actor='cli:mallory' WHERE seq=1`,
	} {
		t.Run(name, func(t *testing.T) {
			store, _, _ := openTestStore(t)
			for _, action := range []string{ActionInit, ActionReveal, ActionExport} {
				if err := store.Audit(ctx, "cli:test", action, "", map[string]any{"key_id": "k"}); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := store.VerifyAudit(ctx); err != nil || count != 3 {
				t.Fatalf("clean chain: %d %v", count, err)
			}
			if _, err := store.database.ExecContext(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if _, err := store.VerifyAudit(ctx); err == nil {
				t.Fatal("tampering not detected")
			}
		})
	}
}

func TestResealAllMovesEveryValue(t *testing.T) {
	ctx := context.Background()
	store, _, oldKey := openTestStore(t)
	project := saveProject(t, store, "app")
	if err := store.RecordKey(ctx, oldKey.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceEnvVars(ctx, project.ID, []EnvVar{{Key: "A_PASSWORD", Value: "a-secret", Secret: true}, {Key: "PLAIN", Value: "p"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecret(ctx, "llm", "sk-1"); err != nil {
		t.Fatal(err)
	}
	newKey, _ := secrets.NewKey()
	count, err := store.ResealAll(ctx, newKey)
	if err != nil || count != 2 {
		t.Fatalf("resealed %d, %v", count, err)
	}
	usage, err := store.KeyUsage(ctx)
	if err != nil || usage[newKey.ID] != 2 || usage[oldKey.ID] != 0 {
		t.Fatalf("usage after rotation: %v %v", usage, err)
	}
	store.sealer = secrets.NewSealer(newKey)
	values, err := store.EnvVars(ctx, project.ID)
	if err != nil || values[0].Value != "a-secret" {
		t.Fatalf("value unreadable with only the new key: %+v %v", values, err)
	}
	var retired int
	if err := store.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM keys WHERE key_id=? AND retired_at IS NOT NULL`, oldKey.ID).Scan(&retired); err != nil || retired != 1 {
		t.Fatalf("old key not marked retired: %d %v", retired, err)
	}
}

func TestBackupWritesAPrivateCopy(t *testing.T) {
	store, path, _ := openTestStore(t)
	backups := filepath.Join(filepath.Dir(path), "backups")
	if err := store.backup(context.Background(), backups, 1); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(backups, "composewright-v1-*.db"))
	if len(files) != 1 {
		t.Fatalf("backups: %v", files)
	}
	info, _ := os.Stat(files[0])
	if info.Mode().Perm() != fileMode || !strings.HasSuffix(files[0], ".db") {
		t.Fatalf("backup mode %04o", info.Mode().Perm())
	}
}
