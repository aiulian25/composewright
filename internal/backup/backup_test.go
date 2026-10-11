package backup

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

const testPassphrase = "correct horse battery staple"

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
	}
	for name, mutate := range cases {
		archive := sampleArchive()
		mutate(&archive)
		if archive.problem() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
