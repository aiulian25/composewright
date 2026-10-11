package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store/storetest"
	"github.com/aiulian25/composewright/internal/workspace"
)

// TestAuditCheck: doctor reports an intact chain with its size, and fails,
// naming the entry, once a row is edited behind composewright's back.
func TestAuditCheck(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("COMPOSEWRIGHT_MASTER_KEY_FILE", "")
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(root, "config.yaml"), DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Init(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	loaded.FileExists = true
	if check := auditCheck(ctx, loaded); check.Status != StatusOK || check.Detail != "hash chain intact, 1 entry" {
		t.Fatalf("untouched log: %+v", check)
	}
	storetest.EditAuditActor(t, loaded.Paths.Database(), 1)
	if check := auditCheck(ctx, loaded); check.Status != StatusFail || !strings.Contains(check.Detail, "entry 1 does not match the chain") {
		t.Fatalf("edited log: %+v", check)
	}
}
