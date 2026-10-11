package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

func (application *app) debugCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "debug",
		Short:  "Inspect intermediate artefacts (for contributors)",
		Long:   "Commands that expose intermediate steps of the pipeline. They are not part of the stable CLI.",
		Hidden: true,
	}
	var owner string
	policyCommand := &cobra.Command{
		Use:   "policy PLAN.json...",
		Short: "Run the policy gate on plan files",
		Long: `Decode each plan file strictly and run the policy gate with the registry
allowlist and profile from the config. Exits with status 2 if any plan is blocked.`,
		Example: "  composewright debug policy testdata/adversarial/*.json",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, files []string) error {
			return application.runDebugPolicy(files, owner)
		},
	}
	policyCommand.Flags().StringVar(&owner, "owner", "", "repository owner whose registry namespaces are trusted")
	var output string
	untarCommand := &cobra.Command{
		Use:   "untar --out DIR < archive.tar.gz",
		Short: "Extract a repository tarball from stdin the way gen does",
		Long: `Read a gzip-compressed repository tarball from standard input and keep only the
files fact extraction reads, with the same limits and path checks as gen.
scripts/corpus-refresh.sh uses it to refresh testdata/corpus.`,
		Example: "  curl -fsSL https://codeload.github.com/miniflux/v2/tar.gz/main | composewright debug untar --out /tmp/miniflux",
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return application.runDebugUntar(output) },
	}
	untarCommand.Flags().StringVar(&output, "out", "", "empty directory to extract into")
	command.AddCommand(policyCommand, untarCommand, application.debugPromptCommand())
	return command
}

func (application *app) runDebugUntar(output string) error {
	if output == "" {
		return fmt.Errorf("--out is required")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		return err
	}
	result, err := fetch.Untar(application.stdin, output)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, result)
	}
	fmt.Fprintf(application.stdout, "%s: kept %d files, skipped %d\n", result.RootName, len(result.Kept), result.Skipped)
	return nil
}

func (application *app) runDebugPolicy(files []string, owner string) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	blocked := 0
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		name := filepath.Base(file)
		plan, err := ir.Decode(content)
		if err != nil {
			blocked++
			fmt.Fprintf(application.stdout, "%s: %s %s\n", name, application.colours.danger("rejected"), sanitize(err.Error()))
			continue
		}
		report := policy.Evaluate(policy.Input{
			Plan: plan, Profile: loaded.Config.Profile, AllowRegistries: loaded.Config.Registries.Allow, RepositoryOwner: owner,
		})
		verdict := application.colours.ok("passes")
		if report.Blocked() {
			blocked++
			verdict = application.colours.danger("blocked")
		}
		fmt.Fprintf(application.stdout, "%s: %s\n", name, verdict)
		if err := application.printFindings(application.stdout, report); err != nil {
			return err
		}
	}
	if blocked > 0 {
		return codedError{code: exitBlocked, err: fmt.Errorf("%d of %d plans blocked", blocked, len(files))}
	}
	return nil
}
