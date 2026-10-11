package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
