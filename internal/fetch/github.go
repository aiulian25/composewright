package fetch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aiulian25/composewright/internal/netguard"
)

var (
	githubHTTPS = regexp.MustCompile(`^(?:https://)?(?:www\.)?github\.com/([A-Za-z0-9-]{1,39})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?/?$`)
	githubSSH   = regexp.MustCompile(`^git@github\.com:([A-Za-z0-9-]{1,39})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?$`)
	remoteURL   = regexp.MustCompile(`^\s*url\s*=\s*(\S+)\s*$`)
)

// ParseGitHub recognises https and ssh GitHub repository URLs.
func ParseGitHub(text string) (owner, repository string, ok bool) {
	text = strings.TrimSpace(text)
	for _, pattern := range []*regexp.Regexp{githubHTTPS, githubSSH} {
		if match := pattern.FindStringSubmatch(text); match != nil && match[2] != "." && match[2] != ".." {
			return match[1], match[2], true
		}
	}
	return "", "", false
}

// CanonicalGitHubURL is the form stored and compared everywhere.
func CanonicalGitHubURL(owner, repository string) string {
	return "https://github.com/" + owner + "/" + repository
}

// originRemote reads the origin URL from a checkout's .git/config.
func originRemote(directory string) string {
	content, err := os.ReadFile(filepath.Join(directory, ".git", "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if match := remoteURL.FindStringSubmatch(line); inOrigin && match != nil {
			return match[1]
		}
	}
	return ""
}

const (
	githubAPI          = "https://api.github.com/repos/"
	githubAccept       = "application/vnd.github+json"
	githubVersion      = "2022-11-28"
	githubVersionField = "X-GitHub-Api-Version"
	rateLimitRemaining = "X-RateLimit-Remaining"
	temporaryPattern   = "composewright-fetch-*"
	tarballPath        = "/tarball"
	latestReleasePath  = "/releases/latest"
	maxReleaseBytes    = 1 << 20
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// GitHub downloads a repository tarball through the guarded SCM client
// (api.github.com redirects once to codeload.github.com) and keeps only the
// files extraction reads in a private temporary directory. git is never run.
// Without a ref it reads the latest release, whose compose file and README
// describe the image people pull; the default branch can be ahead of it.
func GitHub(ctx context.Context, guard *netguard.Guard, token, owner, repository, ref string) (*Source, error) {
	if ref != "" && !validRef(ref) {
		return nil, fmt.Errorf("ref %q is not a branch, tag or commit", ref)
	}
	if ref == "" {
		release, err := latestRelease(ctx, guard, token, owner, repository)
		if err != nil {
			return nil, err
		}
		ref = release
	}
	address := repositoryAPI(owner, repository) + tarballPath
	if ref != "" {
		address += "/" + ref
	}
	response, err := githubGet(ctx, guard, token, address)
	if err != nil {
		return nil, fmt.Errorf("fetch %s/%s: %w", owner, repository, err)
	}
	defer response.Body.Close()
	if err := githubStatus(response, owner, repository); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", temporaryPattern)
	if err != nil {
		return nil, err
	}
	source := &Source{Kind: SourceGitHub, URL: CanonicalGitHubURL(owner, repository), Ref: ref, Owner: owner, Repository: repository,
		Directory: directory, cleanup: func() error { return os.RemoveAll(directory) }}
	extracted, err := Untar(response.Body, directory)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("unpack %s/%s: %w", owner, repository, err), source.Close())
	}
	source.Commit = extracted.Commit
	return source, nil
}

func validRef(ref string) bool {
	return refPattern.MatchString(ref) && !strings.Contains(ref, "..")
}

func repositoryAPI(owner, repository string) string {
	return githubAPI + url.PathEscape(owner) + "/" + url.PathEscape(repository)
}

// githubGet asks the GitHub API through the guarded SCM client.
func githubGet(ctx context.Context, guard *netguard.Guard, token, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", githubAccept)
	request.Header.Set(githubVersionField, githubVersion)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return guard.Client(netguard.SCM).Do(request)
}

// latestRelease is the tag of the repository's latest release, or "" when it
// has none.
func latestRelease(ctx context.Context, guard *netguard.Guard, token, owner, repository string) (string, error) {
	response, err := githubGet(ctx, guard, token, repositoryAPI(owner, repository)+latestReleasePath)
	if err != nil {
		return "", fmt.Errorf("fetch the latest release of %s/%s: %w", owner, repository, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if err := githubStatus(response, owner, repository); err != nil {
		return "", err
	}
	body, err := netguard.ReadBody(response, maxReleaseBytes)
	if err != nil {
		return "", err
	}
	return releaseTag(body)
}

// releaseTag reads a release's tag; one that is no usable ref leaves the
// default branch to be read instead.
func releaseTag(body []byte) (string, error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("read the latest release: %w", err)
	}
	if !validRef(release.TagName) {
		return "", nil
	}
	return release.TagName, nil
}

func githubStatus(response *http.Response, owner, repository string) error {
	switch {
	case response.StatusCode == http.StatusOK:
		return nil
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("github.com/%s/%s: repository or ref not found (private repositories need scm.github_token_ref)", owner, repository)
	case response.Header.Get(rateLimitRemaining) == "0":
		return errors.New("GitHub API rate limit reached (60 requests per hour without a token); set scm.github_token_ref")
	}
	return fmt.Errorf("github.com/%s/%s: unexpected status %s", owner, repository, response.Status)
}
