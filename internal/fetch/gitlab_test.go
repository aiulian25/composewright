package fetch_test

import (
	"fmt"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
)

const gitlabRefQuery = "?sha="

func TestGitLabFetchesTheLatestRelease(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitLab, fakeRelease)
	source, err := forge.fetchWith("", "")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(sourceFormat, fetch.SourceGitLab, forge.location().URL(), fakeRelease, fakeCommit, fakeOwner, fakeRepository)
	if got := described(source); got != want {
		t.Fatalf("source %s, want %s", got, want)
	}
	if !forge.askedFor(gitlabArchive+gitlabRefQuery+fakeRelease) || len(extractedFiles(t, source)) == 0 {
		t.Fatalf("the release tarball was not read: %v", forge.requests())
	}
}

// TestGitLabWithoutReleasesReadsTheDefaultBranch: GitLab serves the
// default branch to a tarball request that names no ref.
func TestGitLabWithoutReleasesReadsTheDefaultBranch(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitLab, "")
	source, err := forge.fetchWith("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !forge.askedFor(gitlabArchive) || source.Ref != "" || source.Commit != fakeCommit {
		t.Fatalf("source %s after asking for %v", described(source), forge.requests())
	}
}

// TestGitLabSendsItsTokenToItsHostAlone: every GitLab API request carries
// PRIVATE-TOKEN, which net/http would otherwise keep on a redirect to
// another host.
func TestGitLabSendsItsTokenToItsHostAlone(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitLab, fakeRelease)
	forge.moved = true
	if _, err := forge.fetchWith(fakeToken, ""); err != nil {
		t.Fatal(err)
	}
	sent := forge.tokensUnder(gitlabProject)
	if len(sent) != 3 {
		t.Fatalf("GitLab was asked %v", forge.requests())
	}
	for uri, token := range sent {
		if token != fakeToken {
			t.Errorf("%s was sent %q", uri, token)
		}
	}
	if moved, asked := forge.tokensUnder(movedArchive)[movedArchive]; !asked || moved != "" {
		t.Fatalf("the moved tarball was asked for %v with token %q", asked, moved)
	}
}
