package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/workspace"
)

func (application *app) initCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the data directory, master key and default config",
		Long: `Create everything composewright needs on this machine, and check what already exists:

  - the data directory (mode 0700), which holds the database and saved projects
  - the master key (keys/master.key, mode 0600) that encrypts every stored secret
  - a commented config.yaml with the defaults, unless one already exists

Running init again is safe: nothing is overwritten. It refuses to continue when
the data directory or the key file can be read by group or others, and prints
the chmod command that fixes it.

Back up the key file together with the data directory. The database alone is
useless without the key, and the key alone protects nothing.`,
		Example: `  composewright init
  composewright init --data-dir /srv/composewright
  COMPOSEWRIGHT_MASTER_KEY_FILE=$CREDENTIALS_DIRECTORY/kek composewright init`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error { return application.runInit(command.Context()) },
	}
}

func (application *app) runInit(ctx context.Context) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	result, err := workspace.Init(ctx, loaded)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{
			"config_file": result.Paths.ConfigFile, "config_created": result.ConfigCreated,
			"data_dir": result.Paths.DataDir, "key_file": result.Paths.KeyFile,
			"key_id": result.KeyID, "key_created": result.KeyCreated,
		})
	}
	output := application.stdout
	fmt.Fprintln(output, application.colours.ok("composewright is ready."))
	err = table(output,
		[]string{"  config", result.Paths.ConfigFile, describe(result.ConfigCreated, "created", "kept")},
		[]string{"  data", result.Paths.DataDir, ""},
		[]string{"  key", result.Paths.KeyFile, describe(result.KeyCreated, "created", "kept") + ", id " + result.KeyID},
	)
	if err != nil {
		return err
	}
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Next steps:")
	fmt.Fprintln(output, "  composewright doctor")
	fmt.Fprintln(output, "  composewright gen https://github.com/miniflux/v2")
	fmt.Fprintln(output)
	fmt.Fprintln(output, application.colours.muted("Back up the key file and the data directory together; neither is useful without the other."))
	return nil
}

func describe(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}
	return whenFalse
}
