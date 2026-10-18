package pipeline

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	forgeCommit        = "4f2d9c1e8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e"
	forgeRepository    = "/acme/notes"
	forgeArchives      = "/api/v1/repos/acme/notes/archive/"
	forgeVersion       = "/api/v1/version"
	forgeVersionAnswer = `{"version":"1.22.0"}`
	tarballSuffix      = ".tar.gz"
	defaultBranch      = "HEAD"
	notesProject       = "notes"
	notesCompose       = "services:\n  notes:\n    image: docker.io/library/nginx:1.27\n    ports:\n      - \"8080:80\"\n"
)

// giteaHost is a Gitea host serving acme/notes, which has no release, as
// one tarball of forgeCommit at any ref. It records the refs asked for.
type giteaHost struct {
	server *httptest.Server
	guard  *netguard.Guard
	mutex  sync.Mutex
	refs   []string
}

func newGiteaHost(t *testing.T) *giteaHost {
	t.Helper()
	tarball := notesTarball(t)
	host := &giteaHost{}
	host.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ref, archived := strings.CutPrefix(request.URL.Path, forgeArchives)
		switch {
		case request.URL.Path == forgeVersion:
			_, _ = writer.Write([]byte(forgeVersionAnswer))
		case archived:
			host.mutex.Lock()
			host.refs = append(host.refs, strings.TrimSuffix(ref, tarballSuffix))
			host.mutex.Unlock()
			_, _ = writer.Write(tarball)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(host.server.Close)
	address := strings.TrimPrefix(host.server.URL, "http://")
	hostname, _, _ := strings.Cut(address, ":")
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.SCM: {hostname}}, PrivateTargets: []string{address}})
	if err != nil {
		t.Fatal(err)
	}
	host.guard = guard
	return host
}

func (host *giteaHost) askedRefs() []string {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	return slices.Clone(host.refs)
}

// notesTarball packs a compose file as Gitea does: in one folder, with
// the commit in the pax global header.
func notesTarball(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	err := archive.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": forgeCommit}})
	if err == nil {
		err = archive.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "notes/compose.yaml", Mode: 0o644, Size: int64(len(notesCompose))})
	}
	if err == nil {
		_, err = archive.Write([]byte(notesCompose))
	}
	if err := errors.Join(err, archive.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestOpenSourceReadsEachKindOfSource: a repository URL goes to the forge
// its host runs, through the guard, which here refuses every host but the
// fake Gitea one, so each refusal shows the API that would have been
// asked; any other URL, and a run without the network, is refused, and a
// folder is read as it is.
func TestOpenSourceReadsEachKindOfSource(t *testing.T) {
	host := newGiteaHost(t)
	cases := []struct {
		name      string
		source    string
		offline   bool
		wantKind  string
		wantError string
		refused   bool
	}{
		{name: "a folder", source: minifluxFiles, wantKind: fetch.SourceLocal},
		{name: "a Gitea host", source: host.server.URL + forgeRepository, wantKind: fetch.SourceGitea},
		{name: "codeberg.org runs Gitea", source: "https://codeberg.org/acme/notes", wantError: "codeberg.org/api/v1/repos/acme/notes/releases/latest", refused: true},
		{name: "gitlab.com runs GitLab", source: "https://gitlab.com/acme/notes", wantError: "gitlab.com/api/v4/projects/acme%2Fnotes/releases", refused: true},
		{name: "github.com runs GitHub", source: "https://github.com/acme/notes", wantError: "api.github.com/repos/acme/notes/releases/latest", refused: true},
		{name: "another host is asked", source: "https://git.example.com/acme/notes", wantError: "git.example.com/api/v1/version", refused: true},
		{name: "a GitLab subgroup", source: "https://gitlab.com/acme/tools/notes", wantError: "only https://<host>/<owner>/<repository> URLs"},
		{name: "offline", source: "https://codeberg.org/acme/notes", offline: true, wantError: "fetching from codeberg.org needs network access"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dependencies := Dependencies{Config: config.Default(), Guard: host.guard}
			if testCase.offline {
				dependencies.Guard = nil
			}
			source, err := openSource(context.Background(), dependencies, Options{Source: testCase.source})
			if source != nil {
				t.Cleanup(func() { _ = source.Close() })
			}
			if testCase.wantError == "" && (err != nil || source.Kind != testCase.wantKind) {
				t.Fatalf("got %+v, %v; want a %s source", source, err, testCase.wantKind)
			}
			if testCase.wantError != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantError)) {
				t.Fatalf("got %v, want an error with %q", err, testCase.wantError)
			}
			if refused := errors.Is(err, netguard.ErrHostNotAllowed); refused != testCase.refused {
				t.Fatalf("refused by the guard %v, want %v: %v", refused, testCase.refused, err)
			}
		})
	}
}

// TestRegenerationFetchesTheStoredCommit: a project from a Gitea host is
// generated again from the commit its latest version was read at, not from
// the default branch, which may have moved on since.
func TestRegenerationFetchesTheStoredCommit(t *testing.T) {
	ctx := context.Background()
	host := newGiteaHost(t)
	dependencies := testDependencies(t, &scriptedDocker{})
	dependencies.Guard = host.guard
	if _, err := Generate(ctx, dependencies, Options{Source: host.server.URL + forgeRepository, Name: notesProject}); err != nil {
		t.Fatal(err)
	}
	stored := load(t, dependencies, notesProject)
	if project := stored.Project; project.SourceKind != fetch.SourceGitea || project.SourceCommit != forgeCommit || project.SourceURL != host.server.URL+forgeRepository {
		t.Fatalf("stored source %s %s at %s", project.SourceKind, project.SourceURL, project.SourceCommit)
	}
	options, err := stored.Regeneration(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(ctx, dependencies, options); err != nil {
		t.Fatal(err)
	}
	if refs := host.askedRefs(); !slices.Equal(refs, []string{defaultBranch, forgeCommit}) {
		t.Fatalf("tarballs asked for %v, want the default branch, then the stored commit", refs)
	}
}

// TestRegenerationPinsCodeHostsToTheStoredCommit: a repository from any
// code host is read again at its commit; a local archive, which has one
// too, is read as it is.
func TestRegenerationPinsCodeHostsToTheStoredCommit(t *testing.T) {
	const keptRef = "main"
	for kind, pinned := range map[string]bool{fetch.SourceGitHub: true, fetch.SourceGitLab: true, fetch.SourceGitea: true, fetch.SourceLocal: false} {
		stored := &Stored{Project: store.Project{Name: notesProject, SourceKind: kind, SourceCommit: forgeCommit}, Options: Options{Ref: keptRef}}
		options, err := stored.Regeneration(nil, "")
		if err != nil || (options.Ref == forgeCommit) != pinned {
			t.Errorf("%s: ref %q, %v; pinned to the commit: %v", kind, options.Ref, err, pinned)
		}
	}
}
