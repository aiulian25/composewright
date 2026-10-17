package fetch_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	fakeOwner      = "owner"
	fakeRepository = "app"
	fakeRef        = "main"
	fakeCommit     = "c52bdef6e9811f7c20cebc034a6c0ec6799dc6df"
	apiHost        = "127.0.0.1"
	rawHost        = "localhost"
	fullTarball    = fetch.MaxArchiveBytes
	tinyTarball    = 1
	// screenshotBytes of random data make the tarball span several reads,
	// so a tiny cap is hit while it is unpacked.
	screenshotBytes = 64 << 10
	tarballFolder   = "owner-app-c52bdef/"
	recursiveQuery  = "?recursive=1"
	rateLimitHeader = "X-RateLimit-Remaining"
	rawMediaType    = "application/vnd.github.raw"
	treesPath       = "/git/trees/"
	contentsPath    = "/contents/"
	regularFileMode = "100644"
	treeMode        = "040000"
	symlinkMode     = "120000"
	blobType        = "blob"
	treeType        = "tree"
	// overBudget files more than a cap allows.
	overBudget = 5
	// dockerTreeSHA names the docker folder's subtree.
	dockerTreeSHA = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
	// nodeModulesTreeSHA is a valid id, so only the skipped-folder check
	// keeps node_modules from being listed.
	nodeModulesTreeSHA = "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"
	// hostileTreeID would reach the contents API if pasted into a URL.
	hostileTreeID      = "../../contents/x?"
	symlinkTargetBytes = 11
	// keptDocuments and sourceFiles count the documents and config files,
	// and the source files, of repositoryFiles that the trees API fetches.
	keptDocuments = 5
	sourceFiles   = 2
	// plentyOfRequests leaves the contents API cap, not the rate limit, as
	// the budget.
	plentyOfRequests = "5000"
	skippedNote      = "skipped source files past the download limit: %d"
	tokenSetting     = "scm.github_token_ref"
	rawContentHost   = "raw.githubusercontent.com"
	requestsLeft     = "requests than the"
	sourcePrefix     = "src/file-"
	sourceContent    = "package src"
	// serviceFilePattern names config files that fill a download budget.
	serviceFilePattern = "deploy/service-%d.yml"
	serviceFileContent = "a: 1"
	docsPagePrefix     = "docs/page-"
	// spacedFile needs escaping in a download URL, or the # cuts it short.
	spacedFile = "deploy/my stack#1.yml"
	// executableFile is listed with executableMode, which the tarball keeps.
	executableFile = ".env.example"
	executableMode = "100755"
	fakeToken      = "fake-token"
	// treeIDPattern turns a number into a valid tree id.
	treeIDPattern = "%040x"
	nestedFolder  = "nested"
	refusedFolder = "the folder docker lists too many entries"
	defaultBranch = "HEAD"
)

// truncatedTree is a listing GitHub cut short.
var truncatedTree = map[string]any{"tree": []any{}, "truncated": true}

// repositoryFiles holds kept files, files Classify skips and a code file
// without environment reads, so both fetch paths have something to drop.
var repositoryFiles = map[string]string{
	"README.md":                "# app\n\nRun it with docker compose up.\n",
	"docker-compose.yml":       "services:\n  app:\n    image: ghcr.io/owner/app:1.0\n    ports:\n      - \"8080:8080\"\n",
	"docker/Dockerfile":        "FROM alpine\nEXPOSE 8080\n",
	executableFile:             "APP_PORT=8080\n",
	spacedFile:                 serviceFileContent,
	"main.go":                  `package main; func main() { _ = os.Getenv("APP_PORT") }`,
	"util.go":                  "package main",
	"logo.png":                 "png",
	"node_modules/x/README.md": "noise",
}

// fakeGitHub answers the tarball, trees, contents and raw endpoints for
// repositoryFiles, and records the paths asked for.
type fakeGitHub struct {
	t         *testing.T
	server    *httptest.Server
	tarball   []byte
	trees     map[string]any
	files     map[string]string
	remaining string
	token     string
	mutex     sync.Mutex
	asked     []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{t: t, tarball: tarball(t), trees: map[string]any{fakeCommit + recursiveQuery: treeOf(repositoryFiles)}, files: repositoryFiles}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeGitHub) serve(writer http.ResponseWriter, request *http.Request) {
	fake.mutex.Lock()
	fake.asked = append(fake.asked, request.URL.RequestURI())
	fake.mutex.Unlock()
	if fake.remaining != "" {
		writer.Header().Set(rateLimitHeader, fake.remaining)
	}
	repositoryPrefix := "/repos/" + fakeOwner + "/" + fakeRepository
	rawPrefix := "/" + fakeOwner + "/" + fakeRepository + "/" + fakeCommit + "/"
	path := request.URL.Path
	switch {
	case path == repositoryPrefix+"/tarball/"+fakeRef:
		_, _ = writer.Write(fake.tarball)
	case strings.HasPrefix(path, repositoryPrefix+treesPath):
		key := strings.TrimPrefix(request.URL.RequestURI(), repositoryPrefix+treesPath)
		listing, found := fake.trees[key]
		if !found {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(listing)
	case strings.HasPrefix(path, repositoryPrefix+contentsPath):
		if request.URL.Query().Get("ref") != fakeCommit || request.Header.Get("Accept") != rawMediaType {
			http.Error(writer, "wrong ref or media type", http.StatusBadRequest)
			return
		}
		fake.file(writer, request, strings.TrimPrefix(path, repositoryPrefix+contentsPath))
	case strings.HasPrefix(path, rawPrefix):
		fake.file(writer, request, strings.TrimPrefix(path, rawPrefix))
	default:
		http.NotFound(writer, request)
	}
}

func (fake *fakeGitHub) file(writer http.ResponseWriter, request *http.Request, name string) {
	content, found := fake.files[name]
	if !found {
		http.NotFound(writer, request)
		return
	}
	_, _ = writer.Write([]byte(content))
}

func (fake *fakeGitHub) askedFor(fragment string) bool {
	return fake.askedCount(fragment) > 0
}

func (fake *fakeGitHub) askedCount(fragment string) int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	count := 0
	for _, asked := range fake.asked {
		if strings.Contains(asked, fragment) {
			count++
		}
	}
	return count
}

// fetchThrough fetches the fake repository with a tarball cap of limit,
// allowing hosts for SCM.
func (fake *fakeGitHub) fetchThrough(limit int64, hosts ...string) (*fetch.Source, error) {
	fake.t.Helper()
	address, _ := url.Parse(fake.server.URL)
	fetch.PointGitHubAt(fake.t, fake.server.URL, "http://"+rawHost+":"+address.Port(), limit)
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.SCM: hosts},
		PrivateTargets: []string{address.Host, rawHost + ":" + address.Port()}})
	if err != nil {
		fake.t.Fatal(err)
	}
	source, err := fetch.GitHub(context.Background(), guard, fake.token, fakeOwner, fakeRepository, fakeRef)
	if source != nil {
		fake.t.Cleanup(func() { _ = source.Close() })
	}
	return source, err
}

func treeOf(files map[string]string) map[string]any {
	entries := []map[string]any{{"path": "docker", "mode": treeMode, "type": treeType, "sha": dockerTreeSHA},
		{"path": "config.yml", "mode": symlinkMode, "type": blobType, "size": symlinkTargetBytes}}
	for name, content := range files {
		mode := regularFileMode
		if name == executableFile {
			mode = executableMode
		}
		entries = append(entries, map[string]any{"path": name, "mode": mode, "type": blobType, "size": len(content)})
	}
	return map[string]any{"tree": entries, "truncated": false}
}

func tarball(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	headers := []*tar.Header{{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": fakeCommit}}}
	contents := map[string][]byte{"screenshots/demo.png": make([]byte, screenshotBytes)}
	_, _ = rand.Read(contents["screenshots/demo.png"])
	for name, content := range repositoryFiles {
		contents[name] = []byte(content)
	}
	for name, content := range contents {
		headers = append(headers, &tar.Header{Name: tarballFolder + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
	}
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(contents[strings.TrimPrefix(header.Name, tarballFolder)]); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(writer.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func extractedFiles(t *testing.T, source *fetch.Source) []string {
	t.Helper()
	facts, err := extract.Extract(context.Background(), source.Directory)
	if err != nil {
		t.Fatal(err)
	}
	return facts.Files
}

// TestTreesAPIFetchesWhatTheTarballWould checks that a repository past the
// tarball cap yields the same facts through the trees API, with files from
// the raw host and a note naming the fallback.
func TestTreesAPIFetchesWhatTheTarballWould(t *testing.T) {
	fake := newFakeGitHub(t)
	full, err := fake.fetchThrough(fullTarball, apiHost, rawHost)
	if err != nil {
		t.Fatal(err)
	}
	if full.Note != "" || fake.askedFor(treesPath) {
		t.Fatalf("a tarball within the cap went through the trees API: %q, %v", full.Note, fake.asked)
	}
	sparse, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if err != nil {
		t.Fatal(err)
	}
	want, got := extractedFiles(t, full), extractedFiles(t, sparse)
	if len(want) == 0 || !slices.Equal(want, got) {
		t.Fatalf("trees API kept %v, the tarball %v", got, want)
	}
	if wantNote := fmt.Sprintf("fetched %d relevant files through the trees API", len(got)); !strings.Contains(sparse.Note, wantNote) {
		t.Fatalf("note %q does not say %q", sparse.Note, wantNote)
	}
	if sparse.Commit != fakeCommit {
		t.Fatalf("commit %q", sparse.Commit)
	}
	if fake.askedFor(contentsPath) || fake.askedFor("logo.png") || fake.askedFor("node_modules") || fake.askedFor("config.yml") {
		t.Fatalf("asked for files it should not have: %v", fake.asked)
	}
}

// TestTreesAPIUsesTheContentsAPIWithoutTheRawHost covers installs whose
// scm.allow_hosts predates the raw host.
func TestTreesAPIUsesTheContentsAPIWithoutTheRawHost(t *testing.T) {
	fake := newFakeGitHub(t)
	source, err := fake.fetchThrough(tinyTarball, apiHost)
	if err != nil {
		t.Fatal(err)
	}
	if !fake.askedFor(contentsPath+"README.md") || len(extractedFiles(t, source)) == 0 {
		t.Fatalf("the contents API was not used: %v", fake.asked)
	}
}

func TestTreesAPIStopsBeforeTheRateLimitRunsOut(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.remaining = strconv.Itoa(fetch.RateLimitReserve)
	_, err := fake.fetchThrough(tinyTarball, apiHost)
	if !errors.Is(err, fetch.ErrTooLarge) || !strings.Contains(err.Error(), tokenSetting) || !strings.Contains(err.Error(), rawContentHost) ||
		!strings.Contains(err.Error(), requestsLeft) {
		t.Fatalf("got %v, want a rate limit error naming the requests left, the token and the raw host", err)
	}
	if fake.askedFor(contentsPath) {
		t.Fatal("files were fetched although the rate limit could not cover them")
	}
}

// TestTreesAPINamesTheRateLimitWithAToken checks that a token run that
// runs out of requests is told so, without the hint to set a token.
func TestTreesAPINamesTheRateLimitWithAToken(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.token = fakeToken
	fake.remaining = strconv.Itoa(fetch.RateLimitReserve)
	_, err := fake.fetchThrough(tinyTarball, apiHost)
	if !errors.Is(err, fetch.ErrTooLarge) || strings.Contains(err.Error(), tokenSetting) || !strings.Contains(err.Error(), requestsLeft) {
		t.Fatalf("got %v, want a rate limit error without the token hint", err)
	}
}

// TestTreesAPIBlamesTheFileCapWithoutARateLimitHeader leaves the contents
// API cap as the budget, which a token would not raise.
func TestTreesAPIBlamesTheFileCapWithoutARateLimitHeader(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.serveFiles(numbered(fetch.MaxSparseFiles, serviceFilePattern, serviceFileContent))
	_, err := fake.fetchThrough(tinyTarball, apiHost)
	if !errors.Is(err, fetch.ErrTooLarge) || !strings.Contains(err.Error(), rawContentHost) ||
		strings.Contains(err.Error(), tokenSetting) || strings.Contains(err.Error(), requestsLeft) {
		t.Fatalf("got %v, want the file cap error naming only the raw host", err)
	}
}

// serveFiles lists and serves files on top of repositoryFiles.
func (fake *fakeGitHub) serveFiles(extra map[string]string) map[string]any {
	files := maps.Clone(repositoryFiles)
	maps.Copy(files, extra)
	fake.files = files
	listing := treeOf(files)
	fake.trees[fakeCommit+recursiveQuery] = listing
	return listing
}

func numbered(count int, pattern, content string) map[string]string {
	files := map[string]string{}
	for index := range count {
		files[fmt.Sprintf(pattern, index)] = content
	}
	return files
}

// TestTreesAPIFailsWhenDocumentsPassTheBudget lists more config files than
// the raw host budget, all of which would have to be fetched.
func TestTreesAPIFailsWhenDocumentsPassTheBudget(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.serveFiles(numbered(fetch.MaxRawFiles, serviceFilePattern, serviceFileContent))
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if !errors.Is(err, fetch.ErrTooLarge) || !strings.Contains(err.Error(), fmt.Sprintf("more than %d relevant files", fetch.MaxRawFiles)) {
		t.Fatalf("got %v, want ErrTooLarge naming the file cap", err)
	}
	if fake.askedFor("deploy/") {
		t.Fatal("files were fetched although they could not all be")
	}
}

// fetchSkipping fetches extra source files on top of repositoryFiles and
// checks that skipped of them were left out of a successful fetch.
func (fake *fakeGitHub) fetchSkipping(extra, skipped int, hosts ...string) {
	fake.t.Helper()
	fake.serveFiles(numbered(extra, sourcePrefix+"%d.go", sourceContent))
	source, err := fake.fetchThrough(tinyTarball, hosts...)
	if err != nil {
		fake.t.Fatal(err)
	}
	if want := fmt.Sprintf(skippedNote, skipped); !strings.Contains(source.Note, want) {
		fake.t.Fatalf("note %q does not say %q", source.Note, want)
	}
	if !fake.askedFor(executableFile) {
		fake.t.Fatal("a config file was skipped")
	}
}

func TestTreesAPISkipsSourceFilesPastTheRawBudget(t *testing.T) {
	newFakeGitHub(t).fetchSkipping(fetch.MaxRawFiles, sourceFiles+keptDocuments, apiHost, rawHost)
}

func TestTreesAPISkipsSourceFilesPastTheContentsAPICap(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.remaining = plentyOfRequests
	fake.fetchSkipping(fetch.MaxSparseFiles, sourceFiles+keptDocuments, apiHost)
}

// TestTreesAPIBudgetFollowsTheRateLimit leaves requests for the documents
// and one source file beyond the reserve.
func TestTreesAPIBudgetFollowsTheRateLimit(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.remaining = strconv.Itoa(fetch.RateLimitReserve + keptDocuments + 1)
	fake.fetchSkipping(0, sourceFiles-1, apiHost)
}

// TestTreesAPIChecksTheTotalSizeBeforeDownloading lists documents that fit
// the file cap but not the total size limit.
func TestTreesAPIChecksTheTotalSizeBeforeDownloading(t *testing.T) {
	fake := newFakeGitHub(t)
	listing := fake.serveFiles(nil)
	for index := range fetch.MaxKeptBytes/fetch.Limit(fetch.Document) + 1 {
		listing["tree"] = append(listing["tree"].([]map[string]any),
			map[string]any{"path": fmt.Sprintf(docsPagePrefix+"%d.md", index), "mode": regularFileMode, "type": blobType, "size": fetch.Limit(fetch.Document)})
	}
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if !errors.Is(err, fetch.ErrTooLarge) || fake.askedFor(docsPagePrefix) {
		t.Fatalf("got %v after asking for %v, want ErrTooLarge before any download", err, fake.asked)
	}
}

// TestTreesAPISkipsSourceFilesPastTheSizeLeft lists documents that leave
// room for only a few large source files within the total size limit.
func TestTreesAPISkipsSourceFilesPastTheSizeLeft(t *testing.T) {
	fake := newFakeGitHub(t)
	readme := repositoryFiles["README.md"]
	pages := numbered(int(fetch.MaxKeptBytes/fetch.Limit(fetch.Document))-1, docsPagePrefix+"%d.md", readme)
	sources := numbered(int(fetch.Limit(fetch.Document)/fetch.Limit(fetch.Code))+overBudget, sourcePrefix+"%d.go", repositoryFiles["main.go"])
	fake.files = map[string]string{"README.md": readme}
	maps.Copy(fake.files, pages)
	maps.Copy(fake.files, sources)
	entries := append(listed(map[string]string{"README.md": readme}, int64(len(readme))),
		append(listed(pages, fetch.Limit(fetch.Document)), listed(sources, fetch.Limit(fetch.Code))...)...)
	fake.trees[fakeCommit+recursiveQuery] = map[string]any{"tree": entries}
	source, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if err != nil {
		t.Fatal(err)
	}
	fitting := int((fetch.Limit(fetch.Document) - int64(len(readme))) / fetch.Limit(fetch.Code))
	if want := fmt.Sprintf(skippedNote, len(sources)-fitting); !strings.Contains(source.Note, want) {
		t.Fatalf("note %q does not say %q", source.Note, want)
	}
	if asked := fake.askedCount(sourcePrefix); asked != fitting {
		t.Fatalf("asked for %d source files, want the %d that fit", asked, fitting)
	}
}

// listed lists files as regular blobs of size bytes each.
func listed(files map[string]string, size int64) []map[string]any {
	var entries []map[string]any
	for name := range files {
		entries = append(entries, map[string]any{"path": name, "mode": regularFileMode, "type": blobType, "size": size})
	}
	return entries
}

// TestTreesAPINamesAMissingFile lists a file GitHub then cannot serve.
func TestTreesAPINamesAMissingFile(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.serveFiles(nil)
	delete(fake.files, "README.md")
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if err == nil || !strings.Contains(err.Error(), "README.md is not in github.com/"+fakeOwner+"/"+fakeRepository+" at "+fakeCommit) {
		t.Fatalf("got %v, want an error naming the missing file", err)
	}
}

func TestTreesAPIRejectsAnInvalidSubtreeID(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.trees = map[string]any{
		fakeCommit + recursiveQuery: truncatedTree,
		fakeCommit: map[string]any{"tree": []map[string]any{
			{"path": "docker", "mode": treeMode, "type": treeType, "sha": hostileTreeID},
		}},
	}
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if err == nil || !strings.Contains(err.Error(), "invalid id") || fake.askedFor(contentsPath) {
		t.Fatalf("got %v after asking for %v, want the subtree refused", err, fake.asked)
	}
}

// TestTreesAPISkipsFilesPastTheirSizeLimit fills the contents API cap with
// small files, so counting or fetching the oversized ones would fail.
func TestTreesAPISkipsFilesPastTheirSizeLimit(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.remaining = plentyOfRequests
	listing := fake.serveFiles(numbered(fetch.MaxSparseFiles-keptDocuments-sourceFiles, serviceFilePattern, serviceFileContent))
	for index := range overBudget {
		listing["tree"] = append(listing["tree"].([]map[string]any),
			map[string]any{"path": fmt.Sprintf("huge-%d.yml", index), "mode": regularFileMode, "type": blobType, "size": fetch.Limit(fetch.Config) + 1})
	}
	if _, err := fake.fetchThrough(tinyTarball, apiHost); err != nil {
		t.Fatal(err)
	}
	if fake.askedFor("huge-") {
		t.Fatal("a file past its size limit was downloaded")
	}
}

// TestTreesAPIListsATruncatedTreeBySubtree pages a tree GitHub cut short
// and leaves skipped folders unlisted.
func TestTreesAPIListsATruncatedTreeBySubtree(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.trees = map[string]any{
		fakeCommit + recursiveQuery: truncatedTree,
		fakeCommit: map[string]any{"tree": []map[string]any{
			{"path": "README.md", "mode": regularFileMode, "type": blobType, "size": len(repositoryFiles["README.md"])},
			{"path": "docker", "mode": treeMode, "type": treeType, "sha": dockerTreeSHA},
			{"path": "node_modules", "mode": treeMode, "type": treeType, "sha": nodeModulesTreeSHA},
		}},
		dockerTreeSHA + recursiveQuery: map[string]any{"tree": []map[string]any{
			{"path": "Dockerfile", "mode": regularFileMode, "type": blobType, "size": len(repositoryFiles["docker/Dockerfile"])},
		}},
		nodeModulesTreeSHA + recursiveQuery: map[string]any{"tree": []map[string]any{
			{"path": "x/README.md", "mode": regularFileMode, "type": blobType, "size": len(repositoryFiles["node_modules/x/README.md"])},
		}},
	}
	source, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if err != nil {
		t.Fatal(err)
	}
	if got := extractedFiles(t, source); !slices.Equal(got, []string{"README.md", "docker/Dockerfile"}) {
		t.Fatalf("kept %v", got)
	}
	if fake.askedFor(treesPath + nodeModulesTreeSHA) {
		t.Fatal("a skipped folder was listed")
	}
}

// TestTreesAPIStopsAfterTheListingCap nests truncated trees deeper than the
// listing cap allows.
func TestTreesAPIStopsAfterTheListingCap(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.trees = map[string]any{}
	parent := fakeCommit
	for level := range fetch.MaxTreeRequests {
		child := fmt.Sprintf(treeIDPattern, level)
		fake.trees[parent+recursiveQuery] = truncatedTree
		fake.trees[parent] = map[string]any{"tree": []map[string]any{{"path": nestedFolder, "mode": treeMode, "type": treeType, "sha": child}}}
		parent = child
	}
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if asked := fake.askedCount(treesPath); !errors.Is(err, fetch.ErrTooLarge) || asked > fetch.MaxTreeRequests {
		t.Fatalf("got %v after %d listings, want ErrTooLarge within %d", err, asked, fetch.MaxTreeRequests)
	}
}

// TestTreesAPIRefusesATruncatedFolder lists a folder whose recursive and
// top listings GitHub both cut short.
func TestTreesAPIRefusesATruncatedFolder(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.trees = map[string]any{
		fakeCommit + recursiveQuery: truncatedTree,
		fakeCommit: map[string]any{"tree": []map[string]any{
			{"path": "README.md", "mode": regularFileMode, "type": blobType, "size": len(repositoryFiles["README.md"])},
			{"path": "docker", "mode": treeMode, "type": treeType, "sha": dockerTreeSHA},
		}},
		dockerTreeSHA + recursiveQuery: truncatedTree,
		dockerTreeSHA:                  truncatedTree,
	}
	_, err := fake.fetchThrough(tinyTarball, apiHost, rawHost)
	if !errors.Is(err, fetch.ErrTooLarge) || !strings.Contains(err.Error(), refusedFolder) || fake.askedFor("README.md") {
		t.Fatalf("got %v after asking for %v, want the folder refused before any download", err, fake.asked)
	}
}

func TestTreeRefPrefersTheCommitThenTheRef(t *testing.T) {
	checks := []struct{ commit, ref, want string }{
		{fakeCommit, fakeRef, fakeCommit},
		{"", fakeRef, fakeRef},
		{"", "", defaultBranch},
	}
	for _, check := range checks {
		if got := fetch.TreeRef(check.commit, check.ref); got != check.want {
			t.Errorf("TreeRef(%q, %q) = %q, want %q", check.commit, check.ref, got, check.want)
		}
	}
}
