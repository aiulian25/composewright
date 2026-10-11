// Package cli wires the cobra command tree to the domain packages. It parses
// flags, calls into the domain and prints results; it holds no domain logic.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/workspace"
)

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

type globalOptions struct {
	configFile string
	dataDir    string
	logLevel   string
	json       bool
	noColor    bool
	yes        bool
}

type app struct {
	build   BuildInfo
	global  globalOptions
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	logger  *slog.Logger
	colours palette
}

// codedError carries a specific process exit code through cobra.
type codedError struct {
	code int
	err  error
}

func (failure codedError) Error() string { return failure.err.Error() }
func (failure codedError) Unwrap() error { return failure.err }

func Execute(build BuildInfo) int {
	restrictUmask()
	application := &app{build: build, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	return application.run(os.Args[1:])
}

func (application *app) run(arguments []string) int {
	root := application.rootCommand()
	root.SetArgs(arguments)
	root.SetIn(application.stdin)
	root.SetOut(application.stdout)
	root.SetErr(application.stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	fmt.Fprintln(application.stderr, application.colours.danger("error:"), redact.Text(err.Error()))
	var failure codedError
	if errors.As(err, &failure) {
		return failure.code
	}
	return exitFailure
}

func (application *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "composewright",
		Short: "Turn a repository into a validated compose.yaml and a documented .env",
		Long: `composewright reads a repository (a GitHub URL or a local directory), works out how
the application is meant to be deployed, writes a compose.yaml and a documented
.env with every secret generated, and proves the stack boots by running it in an
isolated, resource-capped Compose project before you deploy it yourself.

Deterministic rules fill the plan first; a language model (Ollama, Anthropic or
any OpenAI-compatible endpoint) is only asked about what the rules could not
determine, and never writes YAML. Nothing runs without your approval, secrets
are stored encrypted, and nothing is contacted except the hosts you configure.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return application.setup() },
		// A Version turns on cobra's --version flag, declared below.
		Version: application.versionLine(),
	}
	root.SetVersionTemplate(versionTemplate)
	root.Flags().Bool("version", false, "print the version, as the version command does")
	flags := root.PersistentFlags()
	flags.StringVar(&application.global.configFile, "config", "", "config file (default: $COMPOSEWRIGHT_CONFIG, $XDG_CONFIG_HOME/composewright)")
	flags.StringVar(&application.global.dataDir, "data-dir", "", "data directory (default: $COMPOSEWRIGHT_DATA_DIR, config data_dir, $XDG_DATA_HOME/composewright)")
	flags.StringVar(&application.global.logLevel, "log-level", logLevelWarn, "diagnostics written to stderr: debug, info, warn or error")
	flags.BoolVar(&application.global.json, "json", false, "print machine-readable JSON on stdout")
	flags.BoolVar(&application.global.noColor, "no-color", false, "disable colour (NO_COLOR is honoured too)")
	flags.BoolVar(&application.global.yes, "yes", false, "answer yes to confirmations; every use is written to the audit log")
	root.AddCommand(
		application.initCommand(),
		application.genCommand(),
		application.validateCommand(),
		application.configCommand(),
		application.listCommand(),
		application.showCommand(),
		application.exportCommand(),
		application.rmCommand(),
		application.profileCommand(),
		application.keysCommand(),
		application.doctorCommand(),
		application.serveCommand(),
		application.passwordCommand(),
		application.versionCommand(),
		application.debugCommand(),
	)
	return root
}

func (application *app) setup() error {
	level, err := parseLevel(application.global.logLevel)
	if err != nil {
		return err
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(application.stderr, options)
	if application.global.json {
		handler = slog.NewJSONHandler(application.stderr, options)
	}
	application.logger = slog.New(redact.NewHandler(handler))
	application.colours = newPalette(application.global.noColor || application.global.json, application.stdout)
	return nil
}

func parseLevel(value string) (slog.Level, error) {
	levels := map[string]slog.Level{
		logLevelDebug: slog.LevelDebug, logLevelInfo: slog.LevelInfo,
		logLevelWarn: slog.LevelWarn, logLevelError: slog.LevelError,
	}
	level, found := levels[strings.ToLower(value)]
	if !found {
		return 0, fmt.Errorf("--log-level %q must be debug, info, warn or error", value)
	}
	return level, nil
}

func (application *app) loadConfig() (*config.Loaded, error) {
	return config.Load(config.Flags{ConfigFile: application.global.configFile, DataDir: application.global.dataDir})
}

// guard builds the egress guard for this invocation from the config.
func (application *app) guard(configuration config.Config, extraRegistries []string) (*netguard.Guard, error) {
	policy, err := netguard.PolicyFromConfig(configuration, extraRegistries, application.build.Version)
	if err != nil {
		return nil, err
	}
	return netguard.New(policy)
}

func (application *app) openWorkspace(ctx context.Context) (*workspace.Workspace, error) {
	loaded, err := application.loadConfig()
	if err != nil {
		return nil, err
	}
	return workspace.Open(ctx, loaded)
}

// confirm asks a yes/no question on stderr. --yes answers it; the caller
// records that in the audit log.
func (application *app) confirm(question string) (bool, error) {
	if application.global.yes {
		return true, nil
	}
	fmt.Fprintf(application.stderr, "%s [y/N] ", question)
	answer, err := bufio.NewReader(application.stdin).ReadString('\n')
	if err != nil && answer == "" {
		return false, errors.New("no answer on stdin; pass --yes to confirm non-interactively")
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
