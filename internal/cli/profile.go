package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/config"
)

func (application *app) profileCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "profile",
		Short: "Show, export and import the environment profile",
		Long: `The environment profile describes the host a stack lands on: the external
proxy or tunnel networks it may join, where bind-mounted data lives, and the
defaults applied to every stack (TZ, PUID/PGID, restart policy, log rotation,
resource limits, local domain suffix). It is the profile: block of the config;
export it to copy it to another host or to keep several (gen --profile FILE).`,
	}
	var target string
	exportCommand := &cobra.Command{
		Use:     "export",
		Short:   "Write the profile as profile.yaml",
		Long:    "Write the config's profile to a file (mode 0600), or to standard output without --to.",
		Example: "  composewright profile export --to nas.profile.yaml\n  composewright profile export > profile.yaml",
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return application.runProfileExport(target) },
	}
	exportCommand.Flags().StringVar(&target, "to", "", "file to write (default: standard output)")
	command.AddCommand(
		&cobra.Command{
			Use:     "show",
			Short:   "Print the profile in effect",
			Long:    "Print the profile: block of the effective configuration.",
			Example: "  composewright profile show",
			Args:    cobra.NoArgs,
			RunE:    func(*cobra.Command, []string) error { return application.runProfileExport("") },
		},
		exportCommand,
		&cobra.Command{
			Use:   "import FILE",
			Short: "Replace the config's profile with a profile.yaml",
			Long: `Validate a profile.yaml and store it as the profile: block of the config file.
Unknown keys and invalid values are rejected before anything is written.`,
			Example: "  composewright profile import nas.profile.yaml",
			Args:    cobra.ExactArgs(1),
			RunE:    func(_ *cobra.Command, arguments []string) error { return application.runProfileImport(arguments[0]) },
		},
	)
	return command
}

func (application *app) runProfileExport(target string) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	if application.global.json && target == "" {
		return writeJSON(application.stdout, loaded.Config.Profile)
	}
	rendered, err := config.RenderProfile(loaded.Config.Profile)
	if err != nil {
		return err
	}
	if target == "" {
		_, err = application.stdout.Write(rendered)
		return err
	}
	if err := atomicfile.Write(target, rendered, config.FileMode); err != nil {
		return err
	}
	fmt.Fprintln(application.stdout, "wrote", target)
	return nil
}

func (application *app) runProfileImport(file string) error {
	content, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	profile, err := config.DecodeProfile(content)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	rendered, err := config.RenderProfile(profile)
	if err != nil {
		return err
	}
	if err := config.SetInFile(loaded.Paths.ConfigFile, "profile", string(rendered)); err != nil {
		return err
	}
	fmt.Fprintf(application.stdout, "profile imported into %s\n", loaded.Paths.ConfigFile)
	return nil
}
