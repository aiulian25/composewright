package fetch

import (
	"cmp"
	"context"
	"net/url"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	codebergHost          = "codeberg.org"
	giteaRepositoriesPath = "/api/v1/repos/"
	giteaVersionPath      = "/api/v1/version"
	giteaVersionField     = "version"
	giteaArchivePath      = "/archive/"
	giteaTokenPrefix      = "token "
	// giteaDefaultBranch names the default branch in a tarball address.
	// ponytail: Gitea resolves HEAD from 1.22 and Forgejo from 7; read
	// default_branch from the repository first if older hosts matter.
	giteaDefaultBranch = "HEAD"
)

func giteaAPI(guard *netguard.Guard, token string) forgeAPI {
	return forgeAPI{guard: guard, token: token, tokenHeader: authorizationHeader, tokenPrefix: giteaTokenPrefix}
}

// gitea fetches a Gitea or Forgejo repository's tarball.
func gitea(ctx context.Context, api forgeAPI, location Location, ref string) (*Source, error) {
	repository := location.Base + giteaRepositoriesPath + url.PathEscape(location.Owner) + "/" + url.PathEscape(location.Repository)
	if ref == "" {
		release, err := api.latestRelease(ctx, repository+latestReleasePath, location, releaseTag)
		if err != nil {
			return nil, err
		}
		ref = release
	}
	address := repository + giteaArchivePath + escapePath(cmp.Or(ref, giteaDefaultBranch)) + tarballExtension
	return api.tarball(ctx, address, SourceGitea, location, ref)
}
