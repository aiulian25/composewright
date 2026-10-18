package fetch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	fakeRelease = "v1.2.0"
	// neitherForge answers nothing a forge would.
	neitherForge       = "neither"
	giteaVersionPath   = "/api/v1/version"
	giteaRepository    = "/api/v1/repos/" + fakeOwner + "/" + fakeRepository
	giteaArchivePath   = giteaRepository + "/archive/"
	giteaVersionAnswer = `{"version":"1.22.0"}`
	giteaTokenHeader   = "Authorization"
	giteaTokenPrefix   = "token "
	gitlabProject      = "/api/v4/projects/" + fakeOwner + "%2F" + fakeRepository
	gitlabReleases     = gitlabProject + "/releases?per_page=1"
	gitlabArchive      = gitlabProject + "/repository/archive.tar.gz"
	gitlabTokenHeader  = "PRIVATE-TOKEN"
	gitlabAnswer       = `{"id":7,"path_with_namespace":"` + fakeOwner + `/` + fakeRepository + `"}`
	// movedArchive is where a fake GitLab redirects its tarball, on the
	// other name of the same server.
	movedArchive = "/moved/archive.tar.gz"
	// signInPage is where GitLab sends a request it has no route for, here
	// on a single sign-on host off the allowlist.
	signInPage = "https://sso.example.com/users/sign_in"
	// sourceFormat describes a source by what a fetch fills in.
	sourceFormat = "%s %s ref=%q commit=%s %s/%s"
	hostileRef   = "../../etc"
)

// fakeForge answers as a Gitea or GitLab host, or as neither, for
// fakeOwner/fakeRepository: its latest release, when it has one, and its
// tarball at any ref. It records each request with the token it carried.
type fakeForge struct {
	t       *testing.T
	server  *httptest.Server
	release string
	tarball []byte
	// moved sends a GitLab tarball request to movedArchive on localhost.
	moved  bool
	mutex  sync.Mutex
	asked  []string
	tokens map[string]string
}

func newFakeForge(t *testing.T, kind, release string) *fakeForge {
	t.Helper()
	forge := &fakeForge{t: t, release: release, tarball: tarball(t), tokens: map[string]string{}}
	handlers := map[string]http.HandlerFunc{fetch.SourceGitea: forge.serveGitea, fetch.SourceGitLab: forge.serveGitLab, neitherForge: http.NotFound}
	forge.server = httptest.NewServer(forge.recording(handlers[kind]))
	t.Cleanup(forge.server.Close)
	return forge
}

func (forge *fakeForge) recording(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		uri := request.URL.RequestURI()
		forge.mutex.Lock()
		forge.asked = append(forge.asked, uri)
		forge.tokens[uri] = request.Header.Get(giteaTokenHeader) + request.Header.Get(gitlabTokenHeader)
		forge.mutex.Unlock()
		next(writer, request)
	}
}

func (forge *fakeForge) serveGitea(writer http.ResponseWriter, request *http.Request) {
	uri := request.URL.RequestURI()
	switch {
	case uri == giteaVersionPath:
		_, _ = writer.Write([]byte(giteaVersionAnswer))
	case uri == giteaRepository+"/releases/latest" && forge.release != "":
		_ = json.NewEncoder(writer).Encode(map[string]string{"tag_name": forge.release})
	case strings.HasPrefix(uri, giteaArchivePath):
		_, _ = writer.Write(forge.tarball)
	default:
		http.NotFound(writer, request)
	}
}

func (forge *fakeForge) serveGitLab(writer http.ResponseWriter, request *http.Request) {
	uri := request.URL.RequestURI()
	switch {
	case uri == giteaVersionPath:
		http.Redirect(writer, request, signInPage, http.StatusFound)
	case uri == gitlabProject:
		_, _ = writer.Write([]byte(gitlabAnswer))
	case uri == gitlabReleases:
		_ = json.NewEncoder(writer).Encode(forge.gitlabReleases())
	case strings.HasPrefix(uri, gitlabArchive) && forge.moved:
		address, _ := url.Parse(forge.server.URL)
		http.Redirect(writer, request, "http://"+rawHost+":"+address.Port()+movedArchive, http.StatusFound)
	case strings.HasPrefix(uri, gitlabArchive), uri == movedArchive:
		_, _ = writer.Write(forge.tarball)
	default:
		http.NotFound(writer, request)
	}
}

func (forge *fakeForge) gitlabReleases() []map[string]string {
	if forge.release == "" {
		return []map[string]string{}
	}
	return []map[string]string{{"tag_name": forge.release}}
}

// requests are the URIs asked for so far, in order.
func (forge *fakeForge) requests() []string {
	forge.mutex.Lock()
	defer forge.mutex.Unlock()
	return slices.Clone(forge.asked)
}

func (forge *fakeForge) askedFor(uri string) bool {
	return slices.Contains(forge.requests(), uri)
}

// tokensUnder are the tokens of the requests whose URI starts with prefix.
func (forge *fakeForge) tokensUnder(prefix string) map[string]string {
	forge.mutex.Lock()
	defer forge.mutex.Unlock()
	tokens := map[string]string{}
	for uri, token := range forge.tokens {
		if strings.HasPrefix(uri, prefix) {
			tokens[uri] = token
		}
	}
	return tokens
}

func (forge *fakeForge) location() fetch.Location {
	forge.t.Helper()
	location, ok := fetch.ParseRepository(forge.server.URL + "/" + fakeOwner + "/" + fakeRepository)
	if !ok {
		forge.t.Fatalf("%s is not read as a repository URL", forge.server.URL)
	}
	return location
}

// fetchWith fetches the fake's repository at ref, sending token.
func (forge *fakeForge) fetchWith(token, ref string) (*fetch.Source, error) {
	forge.t.Helper()
	source, err := fetch.Remote(context.Background(), forgeGuard(forge.t, forge), token, forge.location(), ref)
	if source != nil {
		forge.t.Cleanup(func() { _ = source.Close() })
	}
	return source, err
}

// forgeGuard lets SCM requests reach the fakes over plain HTTP, by address
// and as localhost.
func forgeGuard(t *testing.T, forges ...*fakeForge) *netguard.Guard {
	t.Helper()
	policy := netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.SCM: {apiHost, rawHost}}}
	for _, forge := range forges {
		address, _ := url.Parse(forge.server.URL)
		policy.PrivateTargets = append(policy.PrivateTargets, address.Host, rawHost+":"+address.Port())
	}
	guard, err := netguard.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func described(source *fetch.Source) string {
	return fmt.Sprintf(sourceFormat, source.Kind, source.URL, source.Ref, source.Commit, source.Owner, source.Repository)
}

func TestParseRepository(t *testing.T) {
	valid := map[string]fetch.Location{
		"https://gitlab.com/gitlab-org/gitlab-runner":   {Base: "https://gitlab.com", Host: "gitlab.com", Owner: "gitlab-org", Repository: "gitlab-runner"},
		"https://Codeberg.org/forgejo/forgejo.git/":     {Base: "https://codeberg.org", Host: "codeberg.org", Owner: "forgejo", Repository: "forgejo"},
		"http://192.168.1.10:3000/me/my_app.v2":         {Base: "http://192.168.1.10:3000", Host: "192.168.1.10", Owner: "me", Repository: "my_app.v2"},
		"git@github.com:sissbruecker/linkding.git":      {Base: "https://github.com", Host: "github.com", Owner: "sissbruecker", Repository: "linkding"},
		"https://www.github.com/louislam/uptime-kuma/":  {Base: "https://github.com", Host: "github.com", Owner: "louislam", Repository: "uptime-kuma"},
		"https://github.com.evil.example/miniflux/v2":   {Base: "https://github.com.evil.example", Host: "github.com.evil.example", Owner: "miniflux", Repository: "v2"},
		"  https://git.example.com/group/project.git  ": {Base: "https://git.example.com", Host: "git.example.com", Owner: "group", Repository: "project"},
	}
	for input, want := range valid {
		if got, ok := fetch.ParseRepository(input); !ok || got != want {
			t.Errorf("ParseRepository(%q) = %+v %v, want %+v", input, got, ok, want)
		}
	}
	for _, input := range []string{"http://github.com/a/b", "http://www.github.com/a/b", "https://github.com:443/a/b", "https://gitlab.com/group/sub/project",
		"https://codeberg.org/a/b/src/branch/main", "https://codeberg.org/../b", "https://codeberg.org/a/..", "https://codeberg.org/a/.",
		"ftp://codeberg.org/a/b", "codeberg.org/a/b", "git@codeberg.org:a/b.git", "https://codeberg.org/a", "file:///etc/passwd"} {
		if location, ok := fetch.ParseRepository(input); ok {
			t.Errorf("ParseRepository(%q) accepted as %+v", input, location)
		}
	}
}

// TestDetectPicksTheForge: GitHub, GitLab and Codeberg are known by name,
// so the guard, which refuses them all, is never asked; any other host is
// asked once whether it is Gitea, then once whether it is GitLab.
func TestDetectPicksTheForge(t *testing.T) {
	gitea, gitlab, neither := newFakeForge(t, fetch.SourceGitea, ""), newFakeForge(t, fetch.SourceGitLab, ""), newFakeForge(t, neitherForge, "")
	guard := forgeGuard(t, gitea, gitlab, neither)
	cases := []struct {
		name     string
		location fetch.Location
		want     string
	}{
		{"github.com", fetch.Location{Base: "https://github.com", Host: "github.com", Owner: "miniflux", Repository: "v2"}, fetch.SourceGitHub},
		{"gitlab.com", fetch.Location{Base: "https://gitlab.com", Host: "gitlab.com", Owner: "gitlab-org", Repository: "gitlab-runner"}, fetch.SourceGitLab},
		{"codeberg.org", fetch.Location{Base: "https://codeberg.org", Host: "codeberg.org", Owner: "forgejo", Repository: "forgejo"}, fetch.SourceGitea},
		{"a Gitea host", gitea.location(), fetch.SourceGitea},
		{"a GitLab host", gitlab.location(), fetch.SourceGitLab},
	}
	for _, testCase := range cases {
		if got, err := fetch.Detect(context.Background(), guard, "", testCase.location); err != nil || got != testCase.want {
			t.Errorf("%s: got %q, %v; want %q", testCase.name, got, err, testCase.want)
		}
	}
	giteaAsked, gitlabAsked := gitea.requests(), gitlab.requests()
	if !slices.Equal(giteaAsked, []string{giteaVersionPath}) || !slices.Equal(gitlabAsked, []string{giteaVersionPath, gitlabProject}) {
		t.Fatalf("asked the Gitea host %v and the GitLab host %v, want one probe each", giteaAsked, gitlabAsked)
	}
	if _, err := fetch.Detect(context.Background(), guard, "", neither.location()); err == nil || !strings.Contains(err.Error(), "neither as Gitea nor as GitLab") {
		t.Fatalf("a host that is no forge: %v", err)
	}
}

// TestRemoteRefusesAHostOffTheAllowlist: the guard refuses a code host
// scm.allow_hosts lacks before anything is sent, and the error names it.
func TestRemoteRefusesAHostOffTheAllowlist(t *testing.T) {
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.SCM: {"github.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"https://codeberg.org/forgejo/forgejo", "https://gitlab.com/gitlab-org/gitlab-runner", "https://git.example.com/owner/app"} {
		location, _ := fetch.ParseRepository(address)
		_, err := fetch.Remote(context.Background(), guard, fakeToken, location, "")
		if !errors.Is(err, netguard.ErrHostNotAllowed) || !strings.Contains(err.Error(), location.Host+" (scm)") {
			t.Errorf("%s: got %v, want the guard to refuse %s", address, err, location.Host)
		}
	}
}

func TestRemoteRejectsHostileRefs(t *testing.T) {
	forge := newFakeForge(t, fetch.SourceGitea, fakeRelease)
	if _, err := forge.fetchWith("", hostileRef); err == nil || !strings.Contains(err.Error(), "ref") || len(forge.requests()) != 0 {
		t.Fatalf("ref %q: %v after asking for %v", hostileRef, err, forge.requests())
	}
}
