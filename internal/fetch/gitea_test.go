package fetch_test

import (
	"fmt"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
)

const tarballSuffix = ".tar.gz"

func TestGiteaFetchesTheLatestRelease(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitea, fakeRelease)
	source, err := forge.fetchWith("", "")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(sourceFormat, fetch.SourceGitea, forge.location().URL(), fakeRelease, fakeCommit, fakeOwner, fakeRepository)
	if got := described(source); got != want {
		t.Fatalf("source %s, want %s", got, want)
	}
	if !forge.askedFor(giteaArchivePath+fakeRelease+tarballSuffix) || len(extractedFiles(t, source)) == 0 {
		t.Fatalf("the release tarball was not read: %v", forge.requests())
	}
}

// TestGiteaWithoutReleasesReadsTheDefaultBranch: the source names no ref,
// as a GitHub repository without releases does.
func TestGiteaWithoutReleasesReadsTheDefaultBranch(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitea, "")
	source, err := forge.fetchWith("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !forge.askedFor(giteaArchivePath+"HEAD"+tarballSuffix) || source.Ref != "" || source.Commit != fakeCommit {
		t.Fatalf("source %s after asking for %v", described(source), forge.requests())
	}
}

// TestGiteaSendsItsToken on every request, and asks for a ref it is given
// without looking for a release.
func TestGiteaSendsItsToken(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitea, fakeRelease)
	if _, err := forge.fetchWith(fakeToken, fakeRef); err != nil {
		t.Fatal(err)
	}
	if forge.askedFor(giteaRepository+"/releases/latest") || !forge.askedFor(giteaArchivePath+fakeRef+tarballSuffix) {
		t.Fatalf("asked for %v", forge.requests())
	}
	for uri, token := range forge.tokensUnder("/") {
		if token != giteaTokenPrefix+fakeToken {
			t.Errorf("%s was sent %q", uri, token)
		}
	}
}
