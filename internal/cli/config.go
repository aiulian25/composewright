package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"golang.org/x/term"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

var secretNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func (application *app) configCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Show, read and change config.yaml",
		Long: `Inspect and edit the config file. The same schema is used by every packaging
format; 'composewright config show' prints every key with the value in effect.

Values are validated before the file is written, comments in the file are kept,
and unknown keys are rejected so a typo never silently falls back to a default.
Secrets (API keys, tokens) never go in this file; see 'composewright config set-secret'.`,
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the effective configuration and resolved paths",
			Long: `Print the configuration after defaults, the config file and environment
overrides are applied, followed by the resolved file locations.`,
			Example: "  composewright config show\n  composewright config show --json",
			Args:    cobra.NoArgs,
			RunE:    func(*cobra.Command, []string) error { return application.runConfigShow() },
		},
		&cobra.Command{
			Use:     "get KEY",
			Short:   "Print one effective value",
			Long:    "Print the effective value of a dotted key, for example docker.validate.timeout.",
			Example: "  composewright config get llm.provider\n  composewright config get profile.storage",
			Args:    cobra.ExactArgs(1),
			RunE:    func(_ *cobra.Command, arguments []string) error { return application.runConfigGet(arguments[0]) },
		},
		&cobra.Command{
			Use:   "set KEY VALUE",
			Short: "Change one value in the config file",
			Long: `Change one dotted key in the config file. VALUE is parsed as YAML, so lists use
flow syntax. The file is created from the defaults if it does not exist.`,
			Example: `  composewright config set ui.theme light
  composewright config set llm.provider anthropic
  composewright config set registries.allow '[docker.io/library/*, ghcr.io/*]'
  composewright config set profile.storage.base_path /srv/docker`,
			Args: cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, arguments []string) error {
				return application.runConfigSet(arguments[0], arguments[1])
			},
		},
		&cobra.Command{
			Use:   "set-secret NAME",
			Short: "Store an API key or token encrypted in the database",
			Long: `Read a secret from standard input (without echo on a terminal) and store it
encrypted in the database under NAME. Reference it from the config as
keyring:NAME, for example llm.api_key_ref: keyring:llm. The value never
appears in the config file, on the command line or in logs.`,
			Example: `  composewright config set-secret llm
  printf %s "$TOKEN" | composewright config set-secret github`,
			Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, arguments []string) error {
				return application.runConfigSetSecret(command.Context(), arguments[0])
			},
		},
	)
	return command
}

func (application *app) runConfigShow() error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"config": loaded.Config, "paths": loaded.Paths, "config_file_exists": loaded.FileExists})
	}
	rendered, err := config.Render(loaded.Config)
	if err != nil {
		return err
	}
	fmt.Fprintf(application.stdout, "# config file: %s (%s)\n# data dir:    %s\n# key file:    %s\n",
		loaded.Paths.ConfigFile, describe(loaded.FileExists, "found", "not found, defaults in use"),
		loaded.Paths.DataDir, loaded.Paths.KeyFile)
	_, err = application.stdout.Write(rendered)
	return err
}

func (application *app) runConfigGet(key string) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	value, err := config.Lookup(loaded.Config, key)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]string{"key": key, "value": value})
	}
	fmt.Fprintln(application.stdout, value)
	return nil
}

// runConfigSet does not load the config first, so a value that keeps it
// from loading can be set right; the file is written only if the result
// validates.
func (application *app) runConfigSet(key, value string) error {
	file := config.FilePath(application.global.configFile)
	if err := config.SetInFile(file, key, value); err != nil {
		return err
	}
	application.logger.Info("config updated", "file", file, "setting", key)
	if !application.global.json {
		fmt.Fprintf(application.stdout, "%s set in %s\n", key, file)
	}
	return nil
}

func (application *app) runConfigSetSecret(ctx context.Context, name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("secret name %q must be lowercase letters, digits, dot, dash or underscore", name)
	}
	value, err := application.readSecret("Value for " + name + ": ")
	if err != nil {
		return err
	}
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	if err := workspace.Store.SetSecret(ctx, name, value); err != nil {
		return err
	}
	if err := workspace.Store.Audit(ctx, workspace.Actor, store.ActionSetSecret, "", map[string]any{"name": name}); err != nil {
		return err
	}
	if !application.global.json {
		fmt.Fprintf(application.stdout, "secret %s stored encrypted; reference it as %s%s\n", name, config.SecretRefKeyring, name)
	}
	return nil
}

// terminal is standard input when it is a terminal.
func (application *app) terminal() (*os.File, bool) {
	file, isFile := application.stdin.(*os.File)
	return file, isFile && term.IsTerminal(int(file.Fd()))
}

// readSecret reads one line without echo when stdin is a terminal.
func (application *app) readSecret(prompt string) (string, error) {
	if file, isTerminal := application.terminal(); isTerminal {
		fmt.Fprint(application.stderr, prompt)
		value, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(application.stderr)
		return strings.TrimSpace(string(value)), err
	}
	value, err := bufio.NewReader(application.stdin).ReadString('\n')
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.Join(errors.New("no secret on standard input"), err)
	}
	return value, nil
}
