package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/doctor"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/sandbox"
)

func (application *app) doctorCommand() *cobra.Command {
	var setupRootless bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Check Docker, the sandbox tier, permissions and the egress allowlist",
		Long: `Check everything composewright depends on and say how to fix what is wrong:
the data directory and master key permissions, the docker CLI and compose
plugin (2.24.4 or later), whether the daemon is rootless, whether it enforces
memory, CPU and process limits, that the profile's external networks exist,
and which hosts the egress guard allows.

doctor never changes anything. --setup-rootless prints the steps to run a
dedicated rootless Docker daemon on the host, the recommended sandbox, and
the composewright container on it.`,
		Example: "  composewright doctor\n  composewright doctor --json\n  composewright doctor --setup-rootless",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return application.runDoctor(command.Context(), setupRootless)
		},
	}
	command.Flags().BoolVar(&setupRootless, "setup-rootless", false, "print the rootless Docker setup steps")
	return command
}

func (application *app) runDoctor(ctx context.Context, setupRootless bool) error {
	if setupRootless {
		fmt.Fprintln(application.stdout, doctor.RootlessRecipe())
		return nil
	}
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	runner, dockerErr := sandbox.FromConfig(loaded.Config, loaded.Paths.ProjectsDir())
	checks := doctor.Run(ctx, doctor.Inputs{Loaded: loaded, Sandbox: runner, Docker: dockerErr})
	policy, err := netguard.PolicyFromConfig(loaded.Config, nil, application.build.Version)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"checks": checks, "egress": policy.Hosts, "private_targets": policy.PrivateTargets})
	}
	rows := make([][]string, 0, len(checks))
	failed := 0
	for _, check := range checks {
		rows = append(rows, []string{"  " + application.statusLabel(check.Status), check.Name, sanitize(check.Detail)})
		if check.Fix != "" {
			rows = append(rows, []string{"", "", application.colours.muted("fix: " + sanitize(check.Fix))})
		}
		if check.Status == doctor.StatusFail {
			failed++
		}
	}
	if err := table(application.stdout, rows...); err != nil {
		return err
	}
	fmt.Fprintln(application.stdout, "\nOutbound connections are allowed only to:")
	for _, purpose := range []netguard.Purpose{netguard.SCM, netguard.Registry, netguard.LLM, netguard.Release} {
		fmt.Fprintf(application.stdout, "  %-9s %v\n", purpose, policy.Hosts[purpose])
	}
	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}

func (application *app) statusLabel(status string) string {
	switch status {
	case doctor.StatusOK:
		return application.colours.ok(status)
	case doctor.StatusWarn:
		return application.colours.warn(status)
	}
	return application.colours.danger(status)
}
