package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/secrets"
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

// interruptAfterReseal stores a secret and leaves the workspace as a rotation
// that stopped right after ResealAll committed: master.key.next exists and
// already seals every value. It returns the old and the next key's ids.
func interruptAfterReseal(t *testing.T, loaded *config.Loaded) (string, string) {
	t.Helper()
	ctx := context.Background()
	workspace, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if err := workspace.Store.SetSecret(ctx, "llm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	next, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteKeyFile(loaded.Paths.KeyFile+nextKeySuffix, next); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Store.ResealAll(ctx, next); err != nil {
		t.Fatal(err)
	}
	return workspace.Key.ID, next.ID
}

func secretReadable(t *testing.T, workspace *Workspace) {
	t.Helper()
	if value, _, err := workspace.Store.Secret(context.Background(), "llm"); err != nil || value != "sk-test" {
		t.Fatalf("sealed setting lost: %q %v", value, err)
	}
}

// TestRotationResumesAfterInterruption: rotating again after an interrupted
// rotation reuses the key it left behind instead of failing on the file,
// and a retry that fails keeps that key, which may seal every value.
func TestRotationResumesAfterInterruption(t *testing.T) {
	ctx := context.Background()
	loaded := initialise(t)
	_, nextID := interruptAfterReseal(t, loaded)
	workspace, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	secretReadable(t, workspace)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := workspace.RotateKey(canceled); err == nil {
		t.Fatal("a rotation without a database transaction succeeded")
	}
	if _, err := os.Stat(loaded.Paths.KeyFile + nextKeySuffix); err != nil {
		t.Fatalf("a failed retry removed the key every value is sealed with: %v", err)
	}
	result, err := workspace.RotateKey(ctx)
	if err != nil {
		t.Fatalf("the interrupted rotation cannot finish: %v", err)
	}
	if result.NewKeyID != nextID {
		t.Fatalf("new key %s, want the interrupted rotation's %s", result.NewKeyID, nextID)
	}
	secretReadable(t, workspace)
	if status, err := workspace.KeyStatus(ctx); err != nil || status.Pending {
		t.Fatalf("rotation still pending: %+v %v", status, err)
	}
}

// TestOpenFinishesAnInterruptedKeySwap: a rotation that stopped between its
// two renames leaves no master.key, only master.key.next; opening the
// workspace, or initialising it as serve --init does on every container
// start, finishes the swap instead of calling it uninitialised.
func TestOpenFinishesAnInterruptedKeySwap(t *testing.T) {
	entries := map[string]func(context.Context, *config.Loaded) error{
		"open": func(ctx context.Context, loaded *config.Loaded) error {
			workspace, err := Open(ctx, loaded)
			if err == nil {
				workspace.Close()
			}
			return err
		},
		"init": func(ctx context.Context, loaded *config.Loaded) error {
			_, err := Init(ctx, loaded)
			return err
		},
	}
	for name, enter := range entries {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			loaded := initialise(t)
			oldID, nextID := interruptAfterReseal(t, loaded)
			retiredDir := filepath.Join(filepath.Dir(loaded.Paths.KeyFile), retiredDirName)
			if err := os.MkdirAll(retiredDir, secrets.KeyDirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(loaded.Paths.KeyFile, filepath.Join(retiredDir, oldID+keyExtension)); err != nil {
				t.Fatal(err)
			}
			if err := enter(ctx, loaded); err != nil {
				t.Fatalf("an interrupted swap left the workspace unusable: %v", err)
			}
			workspace, err := Open(ctx, loaded)
			if err != nil {
				t.Fatal(err)
			}
			defer workspace.Close()
			if workspace.Key.ID != nextID {
				t.Fatalf("current key %s, want the rotation's %s", workspace.Key.ID, nextID)
			}
			secretReadable(t, workspace)
		})
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
