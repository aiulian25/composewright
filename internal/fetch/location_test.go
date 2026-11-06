package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/netguard"
)

const scopeCommit = "a123456789012345678901234567890123456789"

func TestGitHubTreeLocationParsing(t *testing.T) {
	for _, text := range []string{"https://github.com/acme/mono/tree/feature%2Fnew/apps/caf%C3%A9%20app", "github.com/acme/mono/blob/feature%2Fnew/apps/caf%C3%A9%20app/compose.yaml#L2"} {
		location, ok, err := ParseGitHubLocation(text)
		if err != nil || !ok || location.Owner != "acme" || location.Parts[0] != "feature/new" || location.Parts[2] != "café app" {
			t.Fatalf("parse %s: %+v %v", text, location, err)
		}
	}
	for _, suffix := range []string{"main/../secret", "main/%2e%2e/secret", "main/%252e%252e/secret", "main/apps//x", "main/apps%5Cx", "main/%00x", "main//"} {
		_, ok, err := ParseGitHubLocation("https://github.com/acme/mono/tree/" + suffix)
		if !ok || err == nil {
			t.Errorf("unsafe path accepted: %s", suffix)
		}
	}
	for _, text := range []string{"http://github.com/acme/mono/tree/main/apps/a", "https://user:dummy@github.com/acme/mono/tree/main/a", "https://github.com:443/acme/mono/tree/main/a", "https://github.com/acme/mono/tree/main/a?ref=x"} {
		if _, ok, err := ParseGitHubLocation(text); !ok || err == nil {
			t.Errorf("unsafe link accepted %s", text)
		}
	}
	for _, text := range []string{"https://github.com/acme/mono", "git@github.com:acme/mono.git", "https://github.com.evil.test/acme/mono/tree/main/a", "https://gitlab.com/acme/mono/tree/main/a"} {
		if _, ok, _ := ParseGitHubLocation(text); ok {
			t.Errorf("changed root/forge parsing: %s", text)
		}
	}
}

func TestGitHubTreeResolutionRequiresOneChoice(t *testing.T) {
	location, _, _ := ParseGitHubLocation("https://github.com/acme/mono/tree/feature/new/apps/api")
	refs := map[string]string{"heads/feature/new": scopeCommit}
	resolve := func(ref string) (string, bool, error) { sha, found := refs[ref]; return sha, found, nil }
	selected, err := resolveTree(location, "", resolve)
	if err != nil || selected.Ref != "heads/feature/new" || selected.Path != "apps/api" || selected.Commit != scopeCommit {
		t.Fatalf("slash branch: %+v %v", selected, err)
	}
	refs["heads/feature"] = scopeCommit
	if _, err := resolveTree(location, "", resolve); err == nil || !strings.Contains(err.Error(), "heads/feature (path") || !strings.Contains(err.Error(), "heads/feature/new") {
		t.Fatalf("ambiguous split not offered: %v", err)
	}
	selected, err = resolveTree(location, "heads/feature/new", resolve)
	if err != nil || selected.Path != "apps/api" {
		t.Fatal("explicit branch did not disambiguate", err)
	}
	refs["tags/feature/new"] = scopeCommit
	if _, err := resolveTree(location, "feature/new", resolve); err == nil || !strings.Contains(err.Error(), "tags/feature/new") {
		t.Fatalf("branch/tag ambiguity lost: %v", err)
	}
	if _, err := resolveTree(location, "tags/feature/new", resolve); err != nil {
		t.Fatal(err)
	}
	encoded, _, _ := ParseGitHubLocation("https://github.com/acme/mono/tree/feature%2Fnew/apps/api")
	delete(refs, "tags/feature/new")
	if _, err := resolveTree(encoded, "", resolve); err != nil {
		t.Fatal("encoded ref boundary became ambiguous", err)
	}
	fail := errors.New("rate limited")
	if _, err := resolveTree(location, "", func(string) (string, bool, error) { return "", false, fail }); !errors.Is(err, fail) {
		t.Fatal("partial API failure was guessed around")
	}
	if _, err := resolveTree(location, "", func(string) (string, bool, error) { return "not-a-sha", true, nil }); err == nil {
		t.Fatal("invalid commit accepted")
	}
}

func TestGitHubSubprojectPinsAndCleansUp(t *testing.T) {
	location, _, _ := ParseGitHubLocation("https://github.com/acme/mono/blob/feature%2Fnew/apps/api/compose.yaml")
	var requested []string
	closed := 0
	archive := func(commit string) (*Source, error) {
		requested = append(requested, commit)
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "apps/api"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "apps/api/compose.yaml"), []byte("services: {}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return &Source{Directory: root, Commit: commit, cleanup: func() error { closed++; return os.RemoveAll(root) }}, nil
	}
	source, err := githubSubproject(location, "", nil, func(ref string) (string, bool, error) { return scopeCommit, ref == "heads/feature/new", nil }, archive)
	if err != nil || source.Subdir != "apps/api" || source.SelectedFile != "apps/api/compose.yaml" || source.URL != location.URL {
		t.Fatalf("scoped source: %+v %v", source, err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	root := "apps/api"
	again, err := githubSubproject(location, scopeCommit, &root, func(string) (string, bool, error) {
		t.Fatal("regeneration resolved a mutable ref")
		return "", false, nil
	}, archive)
	if err != nil || again.Subdir != root || again.SelectedFile != "" {
		t.Fatalf("regeneration: %+v %v", again, err)
	}
	_ = again.Close()
	missing := "absent"
	if _, err := githubSubproject(location, scopeCommit, &missing, nil, archive); err == nil {
		t.Fatal("missing folder accepted")
	}
	if closed != 3 || !reflect.DeepEqual(requested, []string{scopeCommit, scopeCommit, scopeCommit}) {
		t.Fatalf("pin/cleanup mismatch: %v %d", requested, closed)
	}
	if _, err := githubSubproject(location, "main", &root, nil, archive); err == nil {
		t.Fatal("regeneration without immutable commit accepted")
	}
}

func TestPinnedArchiveCommitWithoutPAX(t *testing.T) {
	if got, err := pinnedArchiveCommit(scopeCommit, ""); err != nil || got != scopeCommit || treeRef(got, "main") != scopeCommit {
		t.Fatal("sparse fallback lost the resolved commit")
	}
	if _, err := pinnedArchiveCommit(scopeCommit, strings.Repeat("b", 40)); err == nil {
		t.Fatal("mismatched archive accepted")
	}
	if got, err := pinnedArchiveCommit("main", ""); err != nil || got != "" {
		t.Fatal("root fallback invented a commit")
	}
}

func TestGitHubSubprojectEncodedPathKeepsRepositoryEvidence(t *testing.T) {
	location, ok, err := ParseGitHubLocation("https://github.com/acme/mono/tree/main/apps/caf%C3%A9%20100%25")
	if !ok || err != nil {
		t.Fatal(err)
	}
	source, err := githubSubproject(location, "", nil, func(ref string) (string, bool, error) { return scopeCommit, ref == "heads/main", nil }, func(commit string) (*Source, error) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "apps/café 100%"), 0o700); err != nil {
			t.Fatal(err)
		}
		return &Source{Directory: root, Commit: commit}, nil
	})
	if err != nil || source.Subdir != "apps/café 100%" || source.Commit != scopeCommit {
		t.Fatalf("encoded path was altered: %+v %v", source, err)
	}
}

func TestGitHubTreeHiddenPathsAndAbbreviatedCommits(t *testing.T) {
	location, _, _ := ParseGitHubLocation("https://github.com/acme/mono/tree/main/.deployment")
	var asked []string
	selected, err := resolveTree(location, "", func(ref string) (string, bool, error) {
		asked = append(asked, ref)
		return scopeCommit, ref == "heads/main", nil
	})
	if err != nil || selected.Path != ".deployment" {
		t.Fatalf("hidden folder rejected: %+v %v", selected, err)
	}
	for _, ref := range asked {
		if strings.Contains(ref, "/.deployment") {
			t.Fatal("queried an invalid Git ref instead of the hidden directory")
		}
	}
	short := scopeCommit[:7]
	location, _, _ = ParseGitHubLocation("https://github.com/acme/mono/tree/" + short + "/apps/api")
	selected, err = resolveTree(location, "", func(ref string) (string, bool, error) { return scopeCommit, ref == short, nil })
	if err != nil || selected.Commit != scopeCommit || selected.Path != "apps/api" {
		t.Fatalf("short commit not pinned: %+v %v", selected, err)
	}
}

func TestGitHubSubprojectHonorsSCMAllowlist(t *testing.T) {
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.SCM: {"unrelated.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	location, _, _ := ParseGitHubLocation("https://github.com/acme/mono/tree/main/apps/api")
	if _, err := GitHubSubproject(context.Background(), guard, "dummy-token", location, "", nil); !errors.Is(err, netguard.ErrHostNotAllowed) {
		t.Fatalf("tree intake bypassed SCM allowlist: %v", err)
	}
}
