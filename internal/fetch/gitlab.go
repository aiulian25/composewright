package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	gitlabHost         = "gitlab.com"
	gitlabProjectsPath = "/api/v4/projects/"
	gitlabReleasesPath = "/releases?per_page=1"
	gitlabArchivePath  = "/repository/archive" + tarballExtension
	gitlabRefQuery     = "?sha="
	gitlabTokenHeader  = "PRIVATE-TOKEN"
	// gitlabProjectField is in every project GitLab describes.
	gitlabProjectField = "path_with_namespace"
	readReleasesError  = "read the releases: %w"
)

func gitlabAPI(guard *netguard.Guard, token string) forgeAPI {
	return forgeAPI{guard: guard, token: token, tokenHeader: gitlabTokenHeader}
}

// gitlabProject is a project's API address: GitLab names a project by its
// URL-encoded path.
func gitlabProject(location Location) string {
	return location.Base + gitlabProjectsPath + url.PathEscape(location.Owner+"/"+location.Repository)
}

// gitlab fetches a GitLab project's tarball. Without a ref GitLab serves
// the default branch.
func gitlab(ctx context.Context, api forgeAPI, location Location, ref string) (*Source, error) {
	project := gitlabProject(location)
	if ref == "" {
		release, err := api.latestRelease(ctx, project+gitlabReleasesPath, location, newestTag)
		if err != nil {
			return nil, err
		}
		ref = release
	}
	address := project + gitlabArchivePath
	if ref != "" {
		address += gitlabRefQuery + url.QueryEscape(ref)
	}
	return api.tarball(ctx, address, SourceGitLab, location, ref)
}

// newestTag reads the tag of the first release GitLab lists, the latest.
func newestTag(body []byte) (string, error) {
	var releases []json.RawMessage
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf(readReleasesError, err)
	}
	if len(releases) == 0 {
		return "", nil
	}
	return releaseTag(releases[0])
}
