package extract_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/rules"
)

func scopeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestScopedExtractionPreservesEvidenceAndPrecedence(t *testing.T) {
	root := scopeFiles(t, map[string]string{
		"README.md": "# Root\n\nROOT_PRIVATE_MARKER", ".env": "SHARED=wrong-root\n",
		"apps/other/README.md":    "# Other\n\nSIBLING_PRIVATE_MARKER",
		"apps/other/compose.yaml": "services:\n  unrelated:\n    image: private/other:1\n",
		"apps/api/README.md":      "# API\n\nThe selected application.\n",
		"apps/api/compose.yaml": `services:
  app:
    image: docker.io/library/nginx:1.27-alpine
    env_file: [../../shared/common.env]
    volumes: [../../shared/web.yaml:/etc/app.yaml:ro]
`,
		"shared/common.env": "SHARED=actual\n", "shared/common.env.example": "SHARED=example\n",
		"shared/web.yaml": "services:\n  not_a_deployment:\n    image: private/config:1\n",
	})
	source := &fetch.Source{Kind: fetch.SourceGitHub, Directory: root, Subdir: "apps/api", Owner: "acme", Repository: "mono", URL: "https://github.com/acme/mono/tree/main/apps/api", Commit: strings.Repeat("a", 40)}
	facts, err := extract.ExtractSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(facts.SharedFiles, []string{"shared/common.env", "shared/web.yaml"}) || facts.SourceRoot != "apps/api" {
		t.Fatalf("scope metadata: %+v", facts)
	}
	encoded, _ := json.Marshal(facts)
	for _, excluded := range []string{"SIBLING_PRIVATE_MARKER", "ROOT_PRIVATE_MARKER", "private/other", "private/config", "wrong-root"} {
		if strings.Contains(string(encoded), excluded) {
			t.Errorf("out-of-scope deployment facts included %s", excluded)
		}
	}
	if len(facts.ComposeFiles) != 1 || facts.ComposeFiles[0].Path != "apps/api/compose.yaml" || facts.Summary.Location.File != "apps/api/README.md" {
		t.Fatalf("evidence changed: %+v", facts.ComposeFiles)
	}
	if len(facts.Readmes) != 1 || facts.Readmes[0] != "apps/api/README.md" {
		t.Fatalf("model documents escaped scope: %v", facts.Readmes)
	}
	plan := rules.Build(context.Background(), rules.Input{Facts: facts, Source: source})
	entry, found := plan.EnvEntry("SHARED")
	if !found || entry.Value != "actual" {
		t.Fatalf("shared actual/sample precedence: %+v", entry)
	}
	if len(plan.ConfigFiles) != 1 || plan.ConfigFiles[0].Source != "shared/web.yaml" {
		t.Fatalf("shared bundled input lost: %+v", plan.ConfigFiles)
	}
	if !strings.Contains(strings.Join(facts.Problems, " "), "Shared deployment inputs included: shared/common.env, shared/web.yaml") {
		t.Fatal("included shared inputs not disclosed")
	}
}

func TestScopedExtractionHonorsAliasesAndRejectsExpansions(t *testing.T) {
	root := scopeFiles(t, map[string]string{"apps/api/compose.yaml": `x-common: &common
  env_file: ../../shared/common.env
services:
  app:
    <<: *common
    image: docker.io/library/nginx:1.27-alpine
`, "shared/common.env": "SHARED=value\n"})
	source := &fetch.Source{Directory: root, Subdir: "apps/api"}
	facts, err := extract.ExtractSource(context.Background(), source)
	if err != nil || !reflect.DeepEqual(facts.SharedFiles, []string{"shared/common.env"}) {
		t.Fatalf("merged env_file was not scoped: %+v %v", facts, err)
	}
	cases := map[string]string{
		"escape":    "env_file: ../../../outside.env",
		"dynamic":   "env_file: ${FILE}",
		"directory": "volumes: [../../shared:/etc/app:ro]",
		"missing":   "env_file: ../../absent.env",
		"extends":   "extends: {file: ../../shared/compose.yaml, service: app}",
	}
	for name, property := range cases {
		t.Run(name, func(t *testing.T) {
			content := "services:\n  app:\n    image: docker.io/library/nginx:1.27-alpine\n    " + property + "\n"
			if err := os.WriteFile(filepath.Join(root, "apps/api/compose.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := extract.ExtractSource(context.Background(), source); err == nil {
				t.Fatal("unsafe/unknown scope accepted")
			}
		})
	}
}

func TestScopedExtractionRefusesSharedSymlinks(t *testing.T) {
	root := scopeFiles(t, map[string]string{"apps/api/compose.yaml": "services:\n  app:\n    image: docker.io/library/nginx:1.27-alpine\n    env_file: ../../shared/config.env\n", "shared/keep.yaml": "x: y"})
	outside := scopeFiles(t, map[string]string{"secret.env": "PRIVATE=outside\n"})
	if err := os.Symlink(filepath.Join(outside, "secret.env"), filepath.Join(root, "shared/config.env")); err != nil {
		t.Fatal(err)
	}
	if _, err := extract.ExtractSource(context.Background(), &fetch.Source{Directory: root, Subdir: "apps/api"}); err == nil {
		t.Fatal("shared symlink accepted")
	}
}

func TestScopedExtractionIncludesExamplesAndExplicitOverrides(t *testing.T) {
	root := scopeFiles(t, map[string]string{
		"apps/api/compose.yaml": `x-base: &base
  env_file: ../../shared/unused.env
services:
  app:
    <<: *base
    image: docker.io/library/nginx:1.27-alpine
    env_file: ../../shared/common.env
    volumes: [../../shared/default.conf:/etc/default.conf:ro]
`,
		"shared/common.env.example": "SHARED=example\n", "shared/unused.env": "SHARED=unused\n",
		"shared/default.conf.example": "server {}\n",
	})
	facts, err := extract.ExtractSource(context.Background(), &fetch.Source{Directory: root, Subdir: "apps/api"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(facts.SharedFiles, []string{"shared/common.env.example", "shared/default.conf.example"}) {
		t.Fatalf("examples or explicit-over-merged precedence wrong: %v", facts.SharedFiles)
	}
	if !reflect.DeepEqual(facts.ComposeFiles[0].Services[0].EnvFiles, []string{"../../shared/common.env"}) {
		t.Fatal("inherited env_file overrode explicit value")
	}
	if err := os.WriteFile(filepath.Join(root, "shared/common.env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	facts, err = extract.ExtractSource(context.Background(), &fetch.Source{Directory: root, Subdir: "apps/api"})
	if err != nil || !reflect.DeepEqual(facts.SharedFiles, []string{"shared/common.env", "shared/default.conf.example"}) {
		t.Fatalf("empty actual file lost precedence: %v %v", facts, err)
	}
}
