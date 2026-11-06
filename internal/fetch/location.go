package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiulian25/composewright/internal/netguard"
)

// GitHubLocation keeps URL segment boundaries: an encoded slash in a ref
// must not become an invented shorter branch/path split.
type GitHubLocation struct {
	Owner      string
	Repository string
	Kind       string
	Parts      []string
	URL        string
}

// ParseGitHubLocation recognises tree/blob links without changing root URL
// parsing. recognised remains true on invalid GitHub tree/blob links so callers
// return the safe error rather than trying another source kind.
func ParseGitHubLocation(input string) (location GitHubLocation, recognised bool, err error) {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "github.com/") || strings.HasPrefix(input, "www.github.com/") {
		input = "https://" + input
	}
	u, parseErr := url.Parse(input)
	if parseErr != nil {
		return location, false, nil //nolint:nilerr // This classifier defers malformed, unrecognized inputs to the existing source parser.
	}
	if strings.ToLower(u.Hostname()) != "github.com" && strings.ToLower(u.Hostname()) != "www.github.com" {
		return location, false, nil
	}
	raw := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(raw) < 3 || (raw[2] != "tree" && raw[2] != "blob") {
		return location, false, nil
	}
	invalid := errors.New("invalid GitHub tree/blob link: use HTTPS and a repository-relative path without dot segments, empty segments, backslashes or double encoding")
	if len(input) > 8192 || strings.Contains(u.EscapedPath(), "//") || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || len(raw) < 4 || strings.HasSuffix(u.Path, "//") {
		return location, true, invalid
	}
	owner, repo, ok := ParseGitHub("https://github.com/" + raw[0] + "/" + raw[1])
	if !ok {
		return location, true, invalid
	}
	parts := make([]string, 0, len(raw)-3)
	escaped := make([]string, 0, len(raw)-3)
	for _, segment := range raw[3:] {
		decoded, decodeErr := url.PathUnescape(segment)
		if decodeErr != nil || !SafeRepositoryPath(decoded, false) {
			return location, true, invalid
		}
		parts = append(parts, decoded)
		escaped = append(escaped, url.PathEscape(decoded))
	}
	return GitHubLocation{Owner: owner, Repository: repo, Kind: raw[2], Parts: parts, URL: CanonicalGitHubURL(owner, repo) + "/" + raw[2] + "/" + strings.Join(escaped, "/")}, true, nil
}

// SafeRepositoryPath accepts only canonical, relative slash paths. A single
// URL decoding pass is done before this check; no later layer decodes again.
func SafeRepositoryPath(name string, allowEmpty bool) bool {
	if name == "" {
		return allowEmpty
	}
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	for index := 0; index+2 < len(name); index++ {
		if name[index] == '%' && strings.ContainsRune("0123456789abcdefABCDEF", rune(name[index+1])) && strings.ContainsRune("0123456789abcdefABCDEF", rune(name[index+2])) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return filepath.IsLocal(name)
}

type treeSelection struct{ Ref, Commit, Path string }
type refResolver func(string) (string, bool, error)

// resolveTree never guesses the longest branch. Branch/tag namespaces and
// every possible ref/path boundary must produce exactly one candidate.
func resolveTree(location GitHubLocation, explicit string, resolve refResolver) (treeSelection, error) {
	if explicit != "" && !validRef(explicit) {
		return treeSelection{}, fmt.Errorf(invalidRef, explicit)
	}
	if len(location.Parts) > 20 && explicit == "" {
		return treeSelection{}, errors.New("tree link has too many possible ref/path splits; enter its branch or tag in Reference (--ref)")
	}
	var choices []treeSelection
	for split := 1; split <= len(location.Parts); split++ {
		candidate := strings.Join(location.Parts[:split], "/")
		remainder := strings.Join(location.Parts[split:], "/")
		if !validLocationRef(candidate) || !SafeRepositoryPath(remainder, true) || (location.Kind == "blob" && remainder == "") {
			continue
		}
		refs := []string{"heads/" + candidate, "tags/" + candidate}
		if commitPattern.MatchString(candidate) {
			refs = []string{candidate}
		} else if shortCommitRef(candidate) {
			refs = append(refs, candidate)
		}
		if explicit != "" {
			bare := explicit
			if strings.HasPrefix(bare, "heads/") {
				bare = strings.TrimPrefix(bare, "heads/")
			} else {
				bare = strings.TrimPrefix(bare, "tags/")
			}
			if candidate != bare {
				continue
			}
			if strings.HasPrefix(explicit, "heads/") || strings.HasPrefix(explicit, "tags/") || commitPattern.MatchString(explicit) {
				refs = []string{explicit}
			}
		}
		for _, ref := range refs {
			commit, found, err := resolve(ref)
			if err != nil {
				return treeSelection{}, err
			}
			if found {
				if ref == candidate && shortCommitRef(candidate) && !strings.HasPrefix(commit, strings.ToLower(candidate)) {
					continue
				}
				if !commitPattern.MatchString(commit) {
					return treeSelection{}, errors.New("GitHub returned an invalid commit identity")
				}
				choices = append(choices, treeSelection{Ref: ref, Commit: commit, Path: remainder})
			}
		}
	}
	if len(choices) == 1 {
		return choices[0], nil
	}
	if len(choices) == 0 {
		return treeSelection{}, errors.New("tree/blob ref was not found; enter a matching branch, tag or full commit in the link and Reference (--ref)")
	}
	var labels []string
	for _, choice := range choices {
		labels = append(labels, fmt.Sprintf("%s (path %q)", choice.Ref, choice.Path))
	}
	return treeSelection{}, fmt.Errorf("ambiguous tree/blob link; choose one in Reference (--ref): %s", strings.Join(labels, "; "))
}

// GitHubSubproject resolves the human URL, then fetches only an immutable
// commit through the existing guarded archive/sparse path. pinnedRoot is used
// only on regeneration, when the original URL's split is already recorded.
func GitHubSubproject(ctx context.Context, guard *netguard.Guard, token string, location GitHubLocation, ref string, pinnedRoot *string) (*Source, error) {
	resolve := func(ref string) (string, bool, error) {
		response, err := githubGetAs(ctx, guard, token, repositoryAPI(location.Owner, location.Repository)+"/commits/"+url.PathEscape(ref), "application/vnd.github.sha")
		if err != nil {
			return "", false, err
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		if err := githubStatus(response, location.Owner, location.Repository); err != nil {
			return "", false, err
		}
		body, err := netguard.ReadBody(response, 128)
		return strings.TrimSpace(string(body)), true, err
	}
	return githubSubproject(location, ref, pinnedRoot, resolve, func(commit string) (*Source, error) {
		return GitHub(ctx, guard, token, location.Owner, location.Repository, commit)
	})
}

func githubSubproject(location GitHubLocation, ref string, pinnedRoot *string, resolve refResolver, archive func(string) (*Source, error)) (*Source, error) {
	selection := treeSelection{}
	if pinnedRoot != nil {
		if !commitPattern.MatchString(ref) || !SafeRepositoryPath(*pinnedRoot, true) {
			return nil, errors.New("regenerating a tree source requires its saved commit and safe source root")
		}
		selection = treeSelection{Ref: ref, Commit: ref, Path: *pinnedRoot}
	} else {
		var err error
		selection, err = resolveTree(location, ref, resolve)
		if err != nil {
			return nil, err
		}
	}
	source, err := archive(selection.Commit)
	if err != nil {
		return nil, err
	}
	if source.Commit != selection.Commit {
		return nil, errors.Join(errors.New("fetched source lost its resolved commit identity"), source.Close())
	}
	if err := selectSubproject(source, location, selection, pinnedRoot != nil); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

func selectSubproject(source *Source, location GitHubLocation, selection treeSelection, pinned bool) error {
	root, err := os.OpenRoot(source.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	folder := selection.Path
	if location.Kind == "blob" && !pinned {
		info, err := root.Stat(selection.Path)
		if err != nil || !info.Mode().IsRegular() || Classify(selection.Path) == Skip {
			return errors.New("blob link must name a retained deployment file; use its directory's tree link instead")
		}
		source.SelectedFile = selection.Path
		folder = path.Dir(selection.Path)
		if folder == "." {
			folder = ""
		}
	}
	target := folder
	if target == "" {
		target = "."
	}
	info, err := root.Stat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("selected subproject %q is not a retained repository directory", folder)
	}
	source.URL, source.Ref, source.Subdir = location.URL, selection.Ref, folder
	note := "selected subproject " + path.Join(".", folder)
	if source.Note == "" {
		source.Note = note
	} else {
		source.Note += "; " + note
	}
	return nil
}

func validLocationRef(ref string) bool {
	if !validRef(ref) || strings.HasSuffix(ref, ".") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func shortCommitRef(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, character := range strings.ToLower(ref) {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
