package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	// GitHubHost names GitHub repositories, whose token is
	// scm.github_token_ref's.
	GitHubHost          = "github.com"
	githubWeb           = "https://" + GitHubHost
	wwwPrefix           = "www."
	authorizationHeader = "Authorization"
	// maxAnswerBytes caps a forge's JSON answer: a release, a project or
	// its version.
	maxAnswerBytes     = 1 << 20
	latestReleaseError = "fetch the latest release of %s: %w"
	fetchError         = "fetch %s: %w"
	unpackError        = "unpack %s: %w"
	notFoundError      = "%s: repository or ref not found (private repositories need a token for %s in scm.token_refs)"
	statusError        = "%s: unexpected status %s"
	probeError         = "ask %s which forge it runs: %w"
	unknownForgeError  = "%s answers neither as Gitea nor as GitLab for %s (private repositories need a token for it in scm.token_refs)"
)

// repositoryURL is a repository on a code host other than GitHub: the
// scheme and host with any port, the owner and the repository.
var repositoryURL = regexp.MustCompile(`^(https?://([A-Za-z0-9.-]{1,253})(?::[0-9]{1,5})?)/([A-Za-z0-9._-]{1,100})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?/?$`)

// relativeNames would climb out of an API path instead of naming an owner
// or a repository.
var relativeNames = []string{".", ".."}

// codeHosts are the code hosts known by name; any other is asked which
// forge it runs.
var codeHosts = map[string]string{GitHubHost: SourceGitHub, gitlabHost: SourceGitLab, codebergHost: SourceGitea}

// Location is a repository on a code host, as its URL names it.
type Location struct {
	// Base is the scheme and host, with any port, the code host answers
	// at, such as https://codeberg.org.
	Base string
	// Host is the host name alone, as scm.allow_hosts and scm.token_refs
	// name code hosts.
	Host       string
	Owner      string
	Repository string
}

// URL is the form stored and compared everywhere.
func (location Location) URL() string {
	return location.Base + "/" + location.Owner + "/" + location.Repository
}

// String names the repository in messages, as codeberg.org/owner/name.
func (location Location) String() string {
	return location.Host + "/" + location.Owner + "/" + location.Repository
}

// ParseRepository recognises a GitHub repository as ParseGitHub does, and
// an http or https URL of a repository on any other code host; the egress
// guard decides whether that host may be reached, and over plain HTTP.
func ParseRepository(text string) (Location, bool) {
	if owner, repository, ok := ParseGitHub(text); ok {
		return Location{Base: githubWeb, Host: GitHubHost, Owner: owner, Repository: repository}, true
	}
	match := repositoryURL.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return Location{}, false
	}
	host, owner, repository := strings.ToLower(match[2]), match[3], match[4]
	// A GitHub URL ParseGitHub refuses, such as plain http, stays refused.
	onGitHub := strings.TrimPrefix(host, wwwPrefix) == GitHubHost
	climbsOut := slices.Contains(relativeNames, owner) || slices.Contains(relativeNames, repository)
	if onGitHub || climbsOut {
		return Location{}, false
	}
	return Location{Base: strings.ToLower(match[1]), Host: host, Owner: owner, Repository: repository}, true
}

// Remote downloads a repository tarball from its code host through the
// guarded SCM client, at ref, else at its latest release, else at its
// default branch, and keeps only the files extraction reads, as GitHub
// does. token is the code host's own.
func Remote(ctx context.Context, guard *netguard.Guard, token string, location Location, ref string) (*Source, error) {
	if ref != "" && !validRef(ref) {
		return nil, fmt.Errorf(invalidRef, ref)
	}
	kind, err := Detect(ctx, guard, token, location)
	if err != nil {
		return nil, err
	}
	switch kind {
	case SourceGitHub:
		return GitHub(ctx, guard, token, location.Owner, location.Repository, ref)
	case SourceGitLab:
		return gitlab(ctx, gitlabAPI(guard, token), location, ref)
	}
	return gitea(ctx, giteaAPI(guard, token), location, ref)
}

// Detect names the forge a repository's code host runs, as a source kind:
// GitHub, GitLab and Gitea for github.com, gitlab.com and codeberg.org,
// without asking. Any other host is asked once for Gitea's version, then
// once for the GitLab project, through the guard, which refuses a host off
// scm.allow_hosts before anything is sent.
func Detect(ctx context.Context, guard *netguard.Guard, token string, location Location) (string, error) {
	if kind, known := codeHosts[location.Host]; known {
		return kind, nil
	}
	isGitea, err := giteaAPI(guard, token).answers(ctx, location.Base+giteaVersionPath, giteaVersionField)
	if err != nil {
		return "", fmt.Errorf(probeError, location.Host, err)
	}
	if isGitea {
		return SourceGitea, nil
	}
	isGitLab, err := gitlabAPI(guard, token).answers(ctx, gitlabProject(location), gitlabProjectField)
	if err != nil {
		return "", fmt.Errorf(probeError, location.Host, err)
	}
	if !isGitLab {
		return "", fmt.Errorf(unknownForgeError, location.Host, location)
	}
	return SourceGitLab, nil
}

// forgeAPI asks a GitLab or Gitea host through the guarded SCM client,
// with the token in the header that forge reads.
type forgeAPI struct {
	guard       *netguard.Guard
	token       string
	tokenHeader string
	tokenPrefix string
}

// request asks for address with the token.
func (api forgeAPI) request(ctx context.Context, address string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if api.token != "" {
		request.Header.Set(api.tokenHeader, api.tokenPrefix+api.token)
	}
	return request, nil
}

// get sends the token to the host asked alone: net/http drops
// Authorization on a redirect to another host, but keeps a header such as
// PRIVATE-TOKEN.
func (api forgeAPI) get(ctx context.Context, address string) (*http.Response, error) {
	request, err := api.request(ctx, address)
	if err != nil {
		return nil, err
	}
	client := api.guard.Client(netguard.SCM)
	guarded := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Host != request.URL.Host {
			next.Header.Del(api.tokenHeader)
		}
		return guarded(next, via)
	}
	return client.Do(request)
}

// answers reports whether address answers with a JSON object holding
// field, as only the forge asked for does. A redirect, such as GitLab's to
// its sign-in page, is no such answer and is not followed.
func (api forgeAPI) answers(ctx context.Context, address, field string) (bool, error) {
	request, err := api.request(ctx, address)
	if err != nil {
		return false, err
	}
	client := api.guard.Client(netguard.SCM)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	body, err := netguard.ReadBody(response, maxAnswerBytes)
	if err != nil {
		return false, err
	}
	var answer map[string]json.RawMessage
	return json.Unmarshal(body, &answer) == nil && answer[field] != nil, nil
}

// latestRelease reads the tag of the latest release from address with
// tag, or "" when the repository has none.
func (api forgeAPI) latestRelease(ctx context.Context, address string, location Location, tag func([]byte) (string, error)) (string, error) {
	response, err := api.get(ctx, address)
	if err != nil {
		return "", fmt.Errorf(latestReleaseError, location, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if err := forgeStatus(response, location); err != nil {
		return "", err
	}
	body, err := netguard.ReadBody(response, maxAnswerBytes)
	if err != nil {
		return "", err
	}
	return tag(body)
}

// tarball downloads the tarball at address and keeps the files extraction
// reads in a private temporary directory, with the limits and path checks
// of a GitHub tarball.
func (api forgeAPI) tarball(ctx context.Context, address, kind string, location Location, ref string) (*Source, error) {
	response, err := api.get(ctx, address)
	if err != nil {
		return nil, fmt.Errorf(fetchError, location, err)
	}
	defer response.Body.Close()
	if err := forgeStatus(response, location); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", temporaryPattern)
	if err != nil {
		return nil, err
	}
	source := &Source{Kind: kind, URL: location.URL(), Ref: ref, Owner: location.Owner, Repository: location.Repository,
		Directory: directory, cleanup: func() error { return os.RemoveAll(directory) }}
	extracted, err := untar(response.Body, directory, tarballLimit)
	if err != nil {
		return nil, errors.Join(fmt.Errorf(unpackError, location, err), source.Close())
	}
	source.Commit = extracted.Commit
	return source, nil
}

func forgeStatus(response *http.Response, location Location) error {
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf(notFoundError, location, location.Host)
	}
	return fmt.Errorf(statusError, location, response.Status)
}
