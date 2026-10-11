package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

func initialise(t *testing.T) *config.Loaded {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("COMPOSEWRIGHT_MASTER_KEY_FILE", "")
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(root, "config.yaml"), DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	loaded.FileExists = true
	return loaded
}

func TestRotationKeepsEverySecretReadable(t *testing.T) {
	ctx := context.Background()
	loaded := initialise(t)
	workspace, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.Store.SaveProject(ctx, store.Project{Name: "app", Slug: "app", SourceKind: "local", Status: store.StatusGenerated})
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Store.ReplaceEnvVars(ctx, project.ID, []store.EnvVar{{Key: "DB_PASSWORD", Value: "keep-me", Secret: true}}); err != nil {
		t.Fatal(err)
	}
	oldID := workspace.Key.ID
	result, err := workspace.RotateKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	workspace.Close()
	if result.OldKeyID != oldID || result.Resealed != 1 {
		t.Fatalf("rotation result %+v", result)
	}
	if _, err := os.Stat(result.Retired); err != nil {
		t.Fatalf("old key not retired: %v", err)
	}

	reopened, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Key.ID != result.NewKeyID {
		t.Fatalf("current key %s, want %s", reopened.Key.ID, result.NewKeyID)
	}
	values, err := reopened.Store.EnvVars(ctx, project.ID)
	if err != nil || values[0].Value != "keep-me" {
		t.Fatalf("secret lost in rotation: %+v %v", values, err)
	}
	purged, err := reopened.PurgeRetiredKeys(ctx)
	if err != nil || len(purged) != 1 || purged[0] != oldID {
		t.Fatalf("purge: %v %v", purged, err)
	}
	if values, err := reopened.Store.EnvVars(ctx, project.ID); err != nil || values[0].Value != "keep-me" {
		t.Fatalf("secret lost after purge: %v", err)
	}
	if count, err := reopened.Store.VerifyAudit(ctx); err != nil || count != 3 {
		t.Fatalf("audit after init, rotate, purge: %d %v", count, err)
	}
}

func TestPurgeRefusesWhileARetiredKeyIsInUse(t *testing.T) {
	ctx := context.Background()
	loaded := initialise(t)
	workspace, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if err := workspace.Store.SetSecret(ctx, "llm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	retiredDir := filepath.Join(loaded.Paths.KeysDir(), retiredDirName)
	if err := os.MkdirAll(retiredDir, 0o700); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(loaded.Paths.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retiredDir, workspace.Key.ID+keyExtension), current, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.PurgeRetiredKeys(ctx); err == nil {
		t.Fatal("purge deleted a key that still protects a value")
	}
}

func TestOpenRefusesLooseDataDirectory(t *testing.T) {
	loaded := initialise(t)
	if err := os.Chmod(loaded.Paths.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), loaded); err == nil {
		t.Fatal("a world-readable data directory was accepted")
	}
}
