package backup

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/secrets"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	testPassphrase = "correct horse battery staple"
	// overLimit is one byte more than an override file may hold.
	overLimit = 256<<10 + 1
)

func sampleArchive() Archive {
	llm := config.Default().LLM
	llm.Provider, llm.APIKeyRef = config.ProviderAnthropic, config.SecretRefKeyring+"llm-claude"
	return Archive{Format: formatVersion, Version: "test", Settings: Settings{LLM: llm, Theme: config.ThemeLight},
		Keys: map[string]string{"llm-claude": "test-key-value"},
		Projects: []store.ProjectRecord{{Project: store.Project{Name: "miniflux", Slug: "miniflux"},
			Generations: []store.GenerationRecord{{Generation: store.Generation{Number: 3, IRJSON: "{}"}}},
			Variables:   []store.EnvVar{{Key: "ADMIN_PASSWORD", Value: "s3cret-value", Secret: true}}}}}
}

// TestSealedBackupOpensOnlyWithItsPassphrase: no secret is readable in the
// file; it opens with its passphrase into what was sealed, and a wrong
// passphrase, a changed byte or another file is refused.
func TestSealedBackupOpensOnlyWithItsPassphrase(t *testing.T) {
	sealed, err := Seal(sampleArchive(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cret-value")) || bytes.Contains(sealed, []byte("test-key-value")) {
		t.Fatal("a secret is readable in the backup file")
	}
	opened, err := Open(sealed, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Keys["llm-claude"] != "test-key-value" || opened.Projects[0].Variables[0].Value != "s3cret-value" || opened.Settings.Theme != config.ThemeLight {
		t.Fatalf("opened %+v", opened)
	}
	if _, err := Open(sealed, "another long passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	changed := slices.Clone(sealed)
	changed[len(changed)-1] ^= 1
	if _, err := Open(changed, testPassphrase); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("changed byte: %v", err)
	}
	if _, err := Open([]byte("PK\x03\x04 not a backup"), testPassphrase); !errors.Is(err, ErrNotABackup) {
		t.Fatalf("another file: %v", err)
	}
	if _, err := Seal(sampleArchive(), "short"); err == nil {
		t.Fatal("a short passphrase sealed a backup")
	}
}

// TestArchiveProblems: nothing is restored from a backup whose project names
// could leave the projects folder, whose keys its settings do not use, whose
// .env keys are not variable names or whose versions hold no readable plan.
func TestArchiveProblems(t *testing.T) {
	if err := sampleArchive().problem(); err != nil {
		t.Fatalf("a sound archive: %v", err)
	}
	cases := map[string]func(*Archive){
		"path in a name": func(archive *Archive) {
			archive.Projects[0].Project.Name, archive.Projects[0].Project.Slug = "../etc", "../etc"
		},
		"slug differs":   func(archive *Archive) { archive.Projects[0].Project.Slug = "other" },
		"twice the same": func(archive *Archive) { archive.Projects = append(archive.Projects, archive.Projects[0]) },
		"unused key":     func(archive *Archive) { archive.Keys["github"] = "x" },
		"the passphrase": func(archive *Archive) {
			archive.Settings.LLM.APIKeyRef = config.SecretRefKeyring + PassphraseName
			archive.Keys = map[string]string{PassphraseName: "chosen by someone else"}
		},
		"bad .env key":    func(archive *Archive) { archive.Projects[0].Variables[0].Key = "A=B" },
		"unreadable plan": func(archive *Archive) { archive.Projects[0].Generations[0].Generation.IRJSON = "{" },
		"newer format":    func(archive *Archive) { archive.Format = formatVersion + 1 },
		"override path":   func(archive *Archive) { archive.Catalogue = map[string][]byte{"../escape.yaml": nil} },
		"override kind":   func(archive *Archive) { archive.Catalogue = map[string][]byte{"notes.txt": nil} },
		"override size":   func(archive *Archive) { archive.Catalogue = map[string][]byte{"large.yaml": make([]byte, overLimit)} },
	}
	for name, mutate := range cases {
		archive := sampleArchive()
		mutate(&archive)
		if archive.problem() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	key, err := secrets.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(directory, config.DatabaseName), filepath.Join(directory, config.BackupsDir),
		secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// TestCatalogueOverridesRoundTrip: a backup carries the override files of
// the catalogue.d folder it was collected from and opens with them; one
// from an install without the folder carries none, and one made before
// overrides existed still opens.
func TestCatalogueOverridesRoundTrip(t *testing.T) {
	ctx, database := context.Background(), openStore(t)
	directory := filepath.Join(t.TempDir(), config.CatalogueDir)
	if err := os.MkdirAll(directory, config.DirMode); err != nil {
		t.Fatal(err)
	}
	override := []byte("apps:\n  - images: [ghcr.io/x/bookorbit]\n    healthcheck: [\"CMD\", \"/app\", \"healthcheck\"]\n")
	if err := os.WriteFile(filepath.Join(directory, "bookorbit.yaml"), override, config.FileMode); err != nil {
		t.Fatal(err)
	}
	collected := map[string]map[string][]byte{directory: {"bookorbit.yaml": override}, filepath.Join(directory, "missing"): nil}
	for folder, want := range collected {
		archive, err := Collect(ctx, database, config.Default(), folder, "test", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := Seal(archive, testPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := Open(sealed, testPassphrase)
		if err != nil || !maps.EqualFunc(opened.Catalogue, want, bytes.Equal) {
			t.Fatalf("from %s: overrides %q, %v", folder, opened.Catalogue, err)
		}
	}
	sealed, err := Seal(sampleArchive(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(sealed, testPassphrase); err != nil || opened.Catalogue != nil {
		t.Fatalf("a backup without overrides: %q, %v", opened.Catalogue, err)
	}
}
