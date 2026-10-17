package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	treesPath      = "/git/trees/"
	recursiveQuery = "?recursive=1"
	contentsPath   = "/contents/"
	refQuery       = "?ref="
	githubRawFile  = "application/vnd.github.raw"
	rawContentHost = "raw.githubusercontent.com"
	headRef        = "HEAD"
	treeType       = "tree"
	blobType       = "blob"
	regularMode    = "100644"
	executableMode = "100755"
	// sparseRoot stands in for the top-level folder of a tarball, so tree
	// paths pass the same name checks as archive entries.
	sparseRoot      = "repository"
	sparseNote      = "repository too large for a tarball; fetched %d relevant files through the trees API"
	skippedCodeNote = "; skipped source files past the download limit: %d"
	missingFile     = "%s is not in github.com/%s/%s at %s"
	tooManyFiles    = "%w: more than %d relevant files"
	tooManyAPIFiles = "%w: more than %d relevant files for the GitHub API; %s"
	tooFewRequests  = "%w: %d relevant files need more GitHub API requests than the %d left (%d kept for the rest of the run); %s"
	tooManyBytes    = "%w: relevant files over %d MB"
	raiseLimitHint  = "set scm.github_token_ref or "
	rawHostHint     = "add %s to scm.allow_hosts"
	listFilesError  = "list the files of %s/%s: %w"
	readListError   = "read the file list of %s/%s: %w"
	fetchFileError  = "fetch %s: %w"
	invalidTreeID   = "tree %q has an invalid id"
	tooManyEntries  = "%w: %s lists too many entries"
	folderName      = "the folder %s"
	topFolder       = "the top folder"
	tooManyListings = "%w: more than %d tree listings"
	megabyte        = 1 << 20
	// maxRawFiles caps the files downloaded from the raw host, which has no
	// API rate limit.
	maxRawFiles = 2000
	// maxSparseFiles caps the files downloaded through the contents API,
	// one request each.
	maxSparseFiles = 400
	// rateLimitReserve leaves API requests for the rest of the run, such as
	// image and release lookups.
	rateLimitReserve = 10
	// maxTreeRequests caps the subtree listings a truncated tree needs.
	maxTreeRequests = 100
	maxTreeBytes    = 16 << 20
)

// rawContentBase is a variable so tests can point it at a fake GitHub.
var rawContentBase = "https://" + rawContentHost + "/"

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type treeListing struct {
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// sparseFetch reads one commit of a repository file by file.
type sparseFetch struct {
	ctx        context.Context
	guard      *netguard.Guard
	token      string
	owner      string
	repository string
	ref        string
	// remaining is the API rate limit left after the last tree listing,
	// or -1 when GitHub did not say.
	remaining    int
	treeRequests int
}

// treeRef is what the trees API and the file downloads read: the commit the
// tarball named, else the ref asked for, else the default branch.
func treeRef(commit, ref string) string {
	if commit != "" {
		return commit
	}
	if ref != "" {
		return ref
	}
	return headRef
}

// sparse empties destination and fills it with the files Classify keeps,
// listed by the Git trees API and downloaded one at a time, under the same
// per-file and total size limits as a tarball and within a download budget.
// Files come from raw.githubusercontent.com when it is allowed, since the
// contents API spends one request of the hourly API limit per file. It
// returns the note the Fetch step shows.
func sparse(ctx context.Context, guard *netguard.Guard, token, owner, repository, ref, destination string) (string, error) {
	root, err := emptied(destination)
	if err != nil {
		return "", err
	}
	defer root.Close()
	fetcher := &sparseFetch{ctx: ctx, guard: guard, token: token, owner: owner, repository: repository, ref: ref, remaining: -1}
	entries, err := fetcher.listTree(ref, "")
	if err != nil {
		return "", err
	}
	raw := guard.Allows(netguard.SCM, hostOf(rawContentBase))
	budget := fetcher.budget(raw)
	documents, code := relevantFiles(entries)
	if len(documents) > budget {
		return "", fetcher.overBudget(raw, len(documents), budget)
	}
	documentBytes := totalSize(documents)
	if documentBytes > maxKeptBytes {
		return "", fmt.Errorf(tooManyBytes, ErrTooLarge, maxKeptBytes/megabyte)
	}
	affordable := affordableCode(code, budget-len(documents), maxKeptBytes-documentBytes)
	wanted := append(documents, affordable...)
	unpacking := &extraction{root: root}
	for _, entry := range wanted {
		open := func() (io.ReadCloser, error) { return fetcher.download(entry.Path, raw) }
		if err := unpacking.add(sparseRoot+"/"+entry.Path, false, true, entry.Size, open); err != nil {
			return "", err
		}
	}
	note := fmt.Sprintf(sparseNote, len(unpacking.result.Kept))
	if skipped := len(code) - len(affordable); skipped > 0 {
		note += fmt.Sprintf(skippedCodeNote, skipped)
	}
	return note, nil
}

// emptied opens destination and removes what the tarball left in it. The
// folder itself stays, so its private name is never free for another user
// to take.
func emptied(destination string) (*os.Root, error) {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return nil, errors.Join(err, root.Close())
		}
	}
	return root, nil
}

// budget is how many files may be downloaded: a fixed cap from the raw host,
// else what the API rate limit leaves, within the contents API cap.
func (fetcher *sparseFetch) budget(raw bool) int {
	if raw {
		return maxRawFiles
	}
	if fetcher.remaining < 0 {
		return maxSparseFiles
	}
	return max(0, min(maxSparseFiles, fetcher.remaining-rateLimitReserve))
}

func (fetcher *sparseFetch) overBudget(raw bool, files, budget int) error {
	if raw {
		return fmt.Errorf(tooManyFiles, ErrTooLarge, budget)
	}
	hint := fmt.Sprintf(rawHostHint, rawContentHost)
	if !fetcher.rateLimited() {
		return fmt.Errorf(tooManyAPIFiles, ErrTooLarge, budget, hint)
	}
	if fetcher.token == "" {
		hint = raiseLimitHint + hint
	}
	return fmt.Errorf(tooFewRequests, ErrTooLarge, files, fetcher.remaining, rateLimitReserve, hint)
}

// rateLimited is true when the API rate limit set the budget below the
// contents API cap.
func (fetcher *sparseFetch) rateLimited() bool {
	return fetcher.remaining >= 0 && fetcher.remaining-rateLimitReserve < maxSparseFiles
}

// relevantFiles splits the regular files Classify keeps, each within its
// size limit, into documents and config files, which are always fetched,
// and source files, which are fetched while the budget lasts since most
// are dropped after download for not reading the environment.
func relevantFiles(entries []treeEntry) (documents, code []treeEntry) {
	for _, entry := range entries {
		kind := Classify(entry.Path)
		if !regularBlob(entry) || kind == Skip || entry.Size > Limit(kind) {
			continue
		}
		if kind == Code {
			code = append(code, entry)
			continue
		}
		documents = append(documents, entry)
	}
	return documents, code
}

// affordableCode takes source files in tree order while count files and
// bytes are left.
func affordableCode(code []treeEntry, count int, bytes int64) []treeEntry {
	var affordable []treeEntry
	for _, entry := range code {
		if len(affordable) >= count || entry.Size > bytes {
			break
		}
		bytes -= entry.Size
		affordable = append(affordable, entry)
	}
	return affordable
}

func totalSize(entries []treeEntry) int64 {
	var total int64
	for _, entry := range entries {
		total += entry.Size
	}
	return total
}

func regularBlob(entry treeEntry) bool {
	return entry.Type == blobType && (entry.Mode == regularMode || entry.Mode == executableMode)
}

// listTree lists every file under the tree sha, naming each from prefix. A
// recursive listing GitHub truncated is listed again one subtree at a time.
func (fetcher *sparseFetch) listTree(sha, prefix string) ([]treeEntry, error) {
	listing, err := fetcher.tree(sha + recursiveQuery)
	if err != nil {
		return nil, err
	}
	if !listing.Truncated {
		return prefixed(listing.Tree, prefix), nil
	}
	top, err := fetcher.tree(sha)
	if err != nil {
		return nil, err
	}
	if top.Truncated {
		return nil, fmt.Errorf(listFilesError, fetcher.owner, fetcher.repository, fmt.Errorf(tooManyEntries, ErrTooLarge, folderOf(prefix)))
	}
	var entries []treeEntry
	for _, entry := range top.Tree {
		skipped := slices.Contains(skippedDirs, strings.ToLower(entry.Path))
		if entry.Type != treeType || skipped {
			entries = append(entries, prefixed([]treeEntry{entry}, prefix)...)
			continue
		}
		if !commitPattern.MatchString(entry.SHA) {
			return nil, fmt.Errorf(listFilesError, fetcher.owner, fetcher.repository, fmt.Errorf(invalidTreeID, prefix+entry.Path))
		}
		nested, err := fetcher.listTree(entry.SHA, prefix+entry.Path+"/")
		if err != nil {
			return nil, err
		}
		entries = append(entries, nested...)
	}
	return entries, nil
}

func folderOf(prefix string) string {
	if prefix == "" {
		return topFolder
	}
	return fmt.Sprintf(folderName, strings.TrimSuffix(prefix, "/"))
}

func prefixed(entries []treeEntry, prefix string) []treeEntry {
	for index := range entries {
		entries[index].Path = prefix + entries[index].Path
	}
	return entries
}

// tree asks for one tree listing and notes the rate limit left.
func (fetcher *sparseFetch) tree(shaAndQuery string) (treeListing, error) {
	if fetcher.treeRequests >= maxTreeRequests {
		return treeListing{}, fmt.Errorf(tooManyListings, ErrTooLarge, maxTreeRequests)
	}
	fetcher.treeRequests++
	response, err := githubGet(fetcher.ctx, fetcher.guard, fetcher.token, repositoryAPI(fetcher.owner, fetcher.repository)+treesPath+shaAndQuery)
	if err != nil {
		return treeListing{}, fmt.Errorf(listFilesError, fetcher.owner, fetcher.repository, err)
	}
	defer response.Body.Close()
	if err := githubStatus(response, fetcher.owner, fetcher.repository); err != nil {
		return treeListing{}, err
	}
	if remaining, err := strconv.Atoi(response.Header.Get(rateLimitRemaining)); err == nil {
		fetcher.remaining = remaining
	}
	body, err := netguard.ReadBody(response, maxTreeBytes)
	if err != nil {
		return treeListing{}, err
	}
	var listing treeListing
	if err := json.Unmarshal(body, &listing); err != nil {
		return treeListing{}, fmt.Errorf(readListError, fetcher.owner, fetcher.repository, err)
	}
	return listing, nil
}

// download opens one file's content, from the raw host or the contents API.
func (fetcher *sparseFetch) download(name string, raw bool) (io.ReadCloser, error) {
	escaped := escapePath(name)
	address := repositoryAPI(fetcher.owner, fetcher.repository) + contentsPath + escaped + refQuery + url.QueryEscape(fetcher.ref)
	if raw {
		address = rawContentBase + url.PathEscape(fetcher.owner) + "/" + url.PathEscape(fetcher.repository) + "/" + fetcher.ref + "/" + escaped
	}
	response, err := githubGetAs(fetcher.ctx, fetcher.guard, fetcher.token, address, githubRawFile)
	if err != nil {
		return nil, fmt.Errorf(fetchFileError, name, err)
	}
	if response.StatusCode == http.StatusNotFound {
		_ = response.Body.Close()
		return nil, fmt.Errorf(missingFile, name, fetcher.owner, fetcher.repository, fetcher.ref)
	}
	if err := githubStatus(response, fetcher.owner, fetcher.repository); err != nil {
		_ = response.Body.Close()
		return nil, fmt.Errorf(fetchFileError, name, err)
	}
	return response.Body, nil
}

func escapePath(name string) string {
	segments := strings.Split(name, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func hostOf(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
