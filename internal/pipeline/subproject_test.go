package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/store"
)

func TestSubprojectRegenerationKeepsCommitRootAndCompose(t *testing.T) {
	const commit = "a123456789012345678901234567890123456789"
	stored := &Stored{Project: store.Project{Name: "api", SourceKind: fetch.SourceGitHub, SourceCommit: commit}, Options: Options{Source: "https://github.com/acme/mono/blob/feature%2Fnew/apps/api/compose.yaml", SourceRoot: "apps/api", Ref: "heads/feature/new", ComposeFile: "apps/api/compose.yaml"}}
	options, err := stored.Regeneration(nil, "")
	if err != nil || options.SourceRoot != "apps/api" || options.Ref != commit || options.Source != stored.Options.Source || options.ComposeFile != "apps/api/compose.yaml" || !options.Regenerating {
		t.Fatalf("regeneration lost scoped identity: %+v %v", options, err)
	}
}

func TestSubprojectSourceUsesRemoteGuardAndRejectsEscapes(t *testing.T) {
	for _, address := range []string{"https://github.com/acme/mono/tree/main/apps/api", "github.com/acme/mono/blob/main/apps/api/compose.yaml"} {
		if _, err := openSource(context.Background(), Dependencies{}, Options{Source: address}); err == nil || !strings.Contains(err.Error(), "needs network access through netguard") {
			t.Fatalf("tree treated as local/unguarded: %v", err)
		}
	}
	if _, err := openSource(context.Background(), Dependencies{}, Options{Source: "https://github.com/acme/mono/tree/main/%2e%2e/secret"}); err == nil || !strings.Contains(err.Error(), "invalid GitHub tree/blob") {
		t.Fatal("escape not refused before fetching", err)
	}
}

func TestSubprojectBareLinkIsNotMadeIntoALocalPath(t *testing.T) {
	for _, address := range []string{"github.com/acme/mono/tree/main/apps/api", "https://github.com/acme/mono/blob/main/apps/api/compose.yaml"} {
		options := Options{Source: address}
		if err := absolutePaths(&options); err != nil || options.Source != address {
			t.Fatalf("tree link rewritten as local path: %q %v", options.Source, err)
		}
	}
}
