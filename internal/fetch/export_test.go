package fetch

import "testing"

// PointGitHubAt sends GitHub API requests to api and raw file requests to
// raw, and caps tarballs at limit, until the test ends.
func PointGitHubAt(t testing.TB, api, raw string, limit int64) {
	t.Helper()
	previousAPI, previousRaw, previousLimit := githubAPI, rawContentBase, tarballLimit
	githubAPI, rawContentBase, tarballLimit = api+"/repos/", raw+"/", limit
	t.Cleanup(func() { githubAPI, rawContentBase, tarballLimit = previousAPI, previousRaw, previousLimit })
}

// Limits of the trees API fallback.
const (
	MaxSparseFiles   = maxSparseFiles
	MaxRawFiles      = maxRawFiles
	RateLimitReserve = rateLimitReserve
	MaxKeptBytes     = maxKeptBytes
	MaxTreeRequests  = maxTreeRequests
)

// TreeRef is treeRef, the ref the trees API reads.
var TreeRef = treeRef
