package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	// releaseRepository is where `version --check` looks; rename with the project.
	releaseRepository = "aiulian25/composewright"
	releaseURL        = "https://" + netguard.ReleaseHost + "/repos/" + releaseRepository + "/releases/latest"
	releaseBodyLimit  = 1 << 20
	// versionTemplate prints the root command's Version, which holds versionLine.
	versionTemplate = "{{.Version}}\n"
)

func (application *app) versionCommand() *cobra.Command {
	var check bool
	command := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long: `Print the version, commit and build date of this binary.

composewright never checks for updates on its own. --check asks GitHub Releases
for the latest version once, through the egress guard; it is the only request
composewright ever makes about itself.`,
		Example: "  composewright version\n  composewright version --check",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return application.runVersion(command.Context(), check)
		},
	}
	command.Flags().BoolVar(&check, "check", false, "ask GitHub Releases for the latest version (one request)")
	return command
}

func (application *app) runVersion(ctx context.Context, check bool) error {
	latest := ""
	if check {
		var err error
		if latest, err = application.latestRelease(ctx); err != nil {
			return err
		}
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]string{
			"version": application.build.Version, "commit": application.build.Commit, "date": application.build.Date,
			"go": runtime.Version(), "latest": latest,
		})
	}
	fmt.Fprintln(application.stdout, application.versionLine())
	if check {
		fmt.Fprintf(application.stdout, "latest release: %s\n", latest)
	}
	return nil
}

// versionLine is what both `version` and `--version` print.
func (application *app) versionLine() string {
	return fmt.Sprintf("composewright %s (commit %s, built %s, %s %s/%s)", application.build.Version, application.build.Commit,
		application.build.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func (application *app) latestRelease(ctx context.Context) (string, error) {
	loaded, err := application.loadConfig()
	if err != nil {
		return "", err
	}
	guard, err := application.guard(loaded.Config, nil)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := guard.Client(netguard.Release).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := netguard.ReadBody(response, releaseBodyLimit)
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusNotFound {
		return "none published yet", nil
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil || release.TagName == "" {
		return "", fmt.Errorf("unexpected answer from GitHub Releases (%s)", response.Status)
	}
	return release.TagName, nil
}
