package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

func (application *app) validateCommand() *cobra.Command {
	var timeout time.Duration
	var ignoreLogs []string
	var repair bool
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
connection refused). --ignore-log keeps a regular expression (or literal text,
when it is not a valid one) as normal for this project: a line whose text,
without its container name and timestamp, matches it never fails its runs
again. Running this command is the approval; it is audited.

--repair asks the configured model, whatever engine made the project, how to
repair the latest version's failed run. It sends the verdict and the logs,
without any secret, and prints the lines the proposal changes in compose.yaml
and .env and what the policy gate says; nothing is stored. Answer yes (or
pass --yes) to store it as a new version and run the test again. An edited
compose.yaml is never repaired: a repair renders it from the plan.`,
		Example: "  composewright validate miniflux\n  composewright validate miniflux --timeout 10m\n" +
			"  composewright validate miniflux --ignore-log 'FATAL: optional plugin missing'\n  composewright validate miniflux --repair",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if repair {
				return application.runRepair(command.Context(), arguments[0], timeout)
			}
			return application.runValidate(command.Context(), arguments[0], timeout, ignoreLogs)
		},
	}
	command.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait for the stack to become healthy (default: docker.validate.timeout)")
	command.Flags().StringArrayVar(&ignoreLogs, "ignore-log", nil, "a log line pattern that is normal for this app and never fails its runs (repeatable)")
	command.Flags().BoolVar(&repair, "repair", false, "ask the model for a repair of the last failed run, show it, and apply it when you agree")
	command.MarkFlagsMutuallyExclusive("repair", "ignore-log")
	return command
}

func (application *app) runValidate(ctx context.Context, name string, timeout time.Duration, ignoreLogs []string) error {
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
	dependencies := pipeline.Dependencies{Config: configuration, Workspace: opened, Sandbox: runner}
	for _, pattern := range ignoreLogs {
		if err := pipeline.AddBenignPattern(ctx, dependencies, name, pattern); err != nil {
			return err
		}
	}
	stored, verdict, err := pipeline.Revalidate(ctx, dependencies, name)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"project": stored.Project.Name, "verdict": verdict})
	}
	application.printVerdict(verdict)
	return validationError([]sandbox.Verdict{verdict})
}

// runRepair shows the model's repair of the latest failed run and, once
// confirmed, stores it as a new version and runs it.
func (application *app) runRepair(ctx context.Context, name string, timeout time.Duration) error {
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
	connection, err := pipeline.Connect(ctx, pipeline.Setup{Config: configuration, Paths: loaded.Paths, Workspace: opened,
		Version: application.build.Version, Engine: pipeline.EngineAssisted, Validate: true})
	if err != nil {
		return err
	}
	if connection.Note != "" {
		application.note(connection.Note)
	}
	dependencies := connection.Dependencies
	proposal, err := pipeline.ProposeRepair(ctx, dependencies, name, cliMask)
	if errors.Is(err, pipeline.ErrNoRepairer) {
		return fmt.Errorf("%w%s", err, noModelHint)
	}
	if err != nil {
		return err
	}
	if err := application.printProposal(proposal); err != nil {
		return err
	}
	if err := proposal.Refusal(); err != nil {
		return err
	}
	apply, err := application.confirm(fmt.Sprintf(repairQuestion, proposal.Current.Generation.Number+1))
	if err != nil || !apply {
		return err
	}
	if err := proposal.Apply(ctx, dependencies); err != nil {
		return err
	}
	details := map[string]any{"via": "validate --repair", "approved_by_yes_flag": application.global.yes}
	if err := opened.Store.Audit(ctx, opened.Actor, store.ActionApprove, proposal.Current.Project.ID, details); err != nil {
		return err
	}
	stored, verdict, err := pipeline.Revalidate(ctx, dependencies, name)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"project": stored.Project.Name, "verdict": verdict})
	}
	application.printVerdict(verdict)
	return validationError([]sandbox.Verdict{verdict})
}

// printProposal lists the lines a repair adds (+) and removes (-) in each
// file, what it left out and the policy gate's answer. With --json it is
// one document, and the verdict of an applied repair is the next.
func (application *app) printProposal(proposal *pipeline.Proposal) error {
	current := proposal.Current
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"project": current.Project.Name, "generation": current.Generation.ID,
			"model": proposal.Call.Model, "prompt_hash": proposal.Call.PromptHash, "compose": string(proposal.Compose), "env": string(proposal.Env),
			"policy": proposal.Report, "rejected": proposal.Rejected, "notes": proposal.Notes})
	}
	output := application.stdout
	env, err := current.EnvFile(cliMask)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, proposalHeading, sanitize(current.Project.Name), sanitize(proposal.Call.Model))
	for _, file := range []struct{ name, before, after string }{
		{export.ComposeName, current.Generation.ComposeYAML, string(proposal.Compose)}, {export.EnvName, string(env), string(proposal.Env)},
	} {
		added, removed := pipeline.ChangedLines(file.before, file.after)
		fmt.Fprintln(output, file.name)
		for _, line := range added {
			fmt.Fprintln(output, application.colours.ok(addedMark+sanitize(line.Text)))
		}
		for _, line := range removed {
			fmt.Fprintln(output, application.colours.danger(removedMark+sanitize(line.Text)))
		}
	}
	for _, rejected := range proposal.Rejected {
		fmt.Fprintln(output, leftOutPrefix+sanitize(rejected))
	}
	for _, note := range proposal.Notes {
		fmt.Fprintln(output, notePrefix+sanitize(note))
	}
	gate := application.colours.ok(gateClear)
	if proposal.Report.Blocked() {
		gate = application.colours.danger(gateBlocked)
	}
	fmt.Fprintln(output, gate)
	return nil
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
		rows = append(rows, []string{"  " + sanitize(state.Service), state.State, state.Health, fmt.Sprintf("exit %d", state.ExitCode), state.Peak()})
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
