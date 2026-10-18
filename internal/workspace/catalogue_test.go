package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/rules"
)

// TestOpenReadsCatalogueOverrides: a data directory without catalogue.d has
// no overrides; with one, Open merges its .yaml files and names each it
// leaves out, one that does not parse and one too large to read, while
// other files there are not read.
func TestOpenReadsCatalogueOverrides(t *testing.T) {
	ctx := context.Background()
	loaded := initialise(t)
	workspace, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if catalogue := workspace.Catalogue; len(catalogue.Overrides) != 0 || len(catalogue.Problems) != 0 {
		t.Fatalf("overrides without a catalogue.d: %+v %q", catalogue.Overrides, catalogue.Problems)
	}
	workspace.Close()
	directory := loaded.Paths.CatalogueDir()
	if err := os.MkdirAll(directory, config.DirMode); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"bookorbit.yaml": []byte("apps:\n  - images: [ghcr.io/x/bookorbit]\n    healthcheck: [\"CMD\", \"/app\", \"healthcheck\"]\n"),
		"broken.yaml":    []byte("apps: ["),
		"huge.yaml":      bytes.Repeat([]byte("#"), maxCatalogueBytes+1),
		"notes.txt":      []byte("apps: ["),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), content, config.FileMode); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	catalogue := reopened.Catalogue
	if !slices.Equal(catalogue.Overrides, []rules.Override{{Name: "bookorbit.yaml", Apps: 1}}) {
		t.Fatalf("overrides %+v", catalogue.Overrides)
	}
	problems := catalogue.Problems
	if len(problems) != 2 || !strings.HasPrefix(problems[0], "broken.yaml: yaml:") || problems[1] != "huge.yaml: larger than 256 KiB" {
		t.Fatalf("problems %q", problems)
	}
}

// TestWriteCatalogueFiles: override files are written readable only by
// composewright, in a folder made for them only when there are some, and a
// name that is not a .yaml file right in the folder is refused.
func TestWriteCatalogueFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), config.CatalogueDir)
	if err := WriteCatalogueFiles(directory, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("a folder was made for no files: %v", err)
	}
	content := []byte("apps: []\n")
	if err := WriteCatalogueFiles(directory, map[string][]byte{"bookorbit.yaml": content}); err != nil {
		t.Fatal(err)
	}
	folder, folderErr := os.Stat(directory)
	file, fileErr := os.Stat(filepath.Join(directory, "bookorbit.yaml"))
	if folderErr != nil || fileErr != nil || folder.Mode().Perm() != config.DirMode || file.Mode().Perm() != config.FileMode {
		t.Fatalf("written as %v and %v: %v %v", folder, file, folderErr, fileErr)
	}
	for _, name := range []string{"../escape.yaml", "nested/inner.yaml", "notes.txt", ""} {
		if err := WriteCatalogueFiles(directory, map[string][]byte{name: content}); err == nil {
			t.Errorf("%q was written", name)
		}
	}
}
