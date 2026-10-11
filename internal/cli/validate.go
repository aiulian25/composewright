package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

func (application *app) validateCommand() *cobra.Command {
	var timeout time.Duration
	command := &cobra.Command{
		Use:   "validate <project>",
		Short: "Run a project's latest generation in the sandbox again",
		Long: `Start the stored compose.yaml and .env in a throw-away Compose project named
cw-<project>-<id> and judge it. The run gets an injected override: restart
"no", no-new-privileges, memory, CPU and process caps, every published port
moved to an ephemeral loopback port, no external networks and no host bind
mounts. It waits with 'up --wait', reads 'ps' and 'logs', and is always removed
with 'down -v --remove-orphans'.

A run fails when a container exits, a healthcheck reports unhealthy, the wait
times out, the stack cannot start, or the logs contain a fatal line (panic:,
FATAL, permission denied, address already in use, no such file, or a repeated
connection refused). Running this command is the approval; it is audited.`,
		Example: "  composewright validate miniflux\n  composewright validate miniflux --timeout 10m",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runValidate(command.Context(), arguments[0], timeout)
		},
	}
	command.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait for the stack to become healthy (default: docker.validate.timeout)")
	return command
}

func (application *app) runValidate(ctx context.Context, name string, timeout time.Duration) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	opened, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer opened.Close()
	configuration := loaded.Config
	if timeout > 0 {
		configuration.Docker.Validate.Timeout = timeout
	}
	runner, err := sandbox.FromConfig(configuration, loaded.Paths.ProjectsDir())
	if err != nil {
		return err
	}
	project, err := opened.Store.FindProject(ctx, name)
	if err != nil {
		return err
	}
	if err := opened.Store.Audit(ctx, opened.Actor, store.ActionApprove, project.ID, map[string]any{"via": "validate command"}); err != nil {
		return err
	}
	stored, verdict, err := pipeline.Revalidate(ctx, pipeline.Dependencies{Config: configuration, Workspace: opened, Sandbox: runner}, name)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"project": stored.Project.Name, "verdict": verdict})
	}
	application.printVerdict(verdict)
	return validationError([]sandbox.Verdict{verdict})
}

// printVerdict never relies on colour: the result is always spelled out.
func (application *app) printVerdict(verdict sandbox.Verdict) {
	output := application.stdout
	result := application.colours.ok(verdict.Result)
	if !verdict.Passed() {
		result = application.colours.danger(verdict.Result) + " (" + verdict.Reason + ")"
	}
	fmt.Fprintf(output, "\nvalidation %s in %s · iteration %d · %s\n", result, verdict.Duration.Round(time.Second), verdict.Iteration, sandboxText(verdict.Sandbox))
	rows := make([][]string, 0, len(verdict.Services))
	for _, state := range verdict.Services {
		rows = append(rows, []string{"  " + sanitize(state.Service), state.State, state.Health, fmt.Sprintf("exit %d", state.ExitCode)})
	}
	_ = table(output, rows...)
	if verdict.Detail != "" {
		fmt.Fprintln(output, "  "+sanitize(verdict.Detail))
	}
	if verdict.Passed() && verdict.Reach != nil {
		fmt.Fprintln(output, "  "+sanitize(verdict.Reach.Said))
	}
	for _, line := range verdict.LogMatches {
		fmt.Fprintln(output, "  "+application.colours.muted(sanitize(line)))
	}
}

func sandboxText(environment sandbox.Environment) string {
	text := "sandbox tier " + environment.Tier
	if environment.Rootless {
		text += ", rootless"
	}
	if !environment.LimitsEnforced {
		text += ", resource limits NOT enforced by this daemon"
	}
	return text
}
