// Package release asks GitHub Releases for composewright's latest release.
// It runs only when someone asks, from the CLI or from Settings.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	// repository is where the check looks; rename with the project.
	repository   = "aiulian25/composewright"
	latestURL    = "https://" + netguard.ReleaseHost + "/repos/" + repository + "/releases/latest"
	bodyLimit    = 1 << 20
	acceptHeader = "application/vnd.github+json"
)

// Release is a published release. A zero Release means none is published.
type Release struct {
	Tag       string    `json:"tag_name"`
	URL       string    `json:"html_url"`
	Published time.Time `json:"published_at"`
}

// Latest asks GitHub Releases once, through the guard's release purpose.
func Latest(ctx context.Context, guard *netguard.Guard) (Release, error) {
	return Fetch(ctx, guard, latestURL)
}

// Fetch reads the latest release from address through the guard.
func Fetch(ctx context.Context, guard *netguard.Guard, address string) (Release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", acceptHeader)
	response, err := guard.Client(netguard.Release).Do(request)
	if err != nil {
		return Release{}, err
	}
	defer response.Body.Close()
	body, err := netguard.ReadBody(response, bodyLimit)
	if err != nil {
		return Release{}, err
	}
	if response.StatusCode == http.StatusNotFound {
		return Release{}, nil
	}
	var latest Release
	if err := json.Unmarshal(body, &latest); err != nil || latest.Tag == "" {
		return Release{}, fmt.Errorf("unexpected answer from GitHub Releases (%s)", response.Status)
	}
	return latest, nil
}
