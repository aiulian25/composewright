package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/workspace"
)

type genFlags struct {
	options      pipeline.Options
	noLLM        bool
	noValidate   bool
	noLabelCheck bool
	offline      bool
	timeout      time.Duration
}

func (application *app) genCommand() *cobra.Command {
	var flags genFlags
	command := &cobra.Command{
		Use:   "gen <url|path>",
		Short: "Generate a validated compose.yaml and .env for a repository",
		Long: `Run the pipeline for a repository (a local checkout; GitHub URLs are fetched as
a tarball): extract deployment facts with their file and line, build a plan
with deterministic rules, gate it with the policy, render compose.yaml, .env
and DEPLOY.md with the environment profile, re-check the rendered file, and
store the result with secrets encrypted.

--engine picks who writes the plan, by default llm.engine ("rules" unless you
change it): "rules" uses deterministic rules only; "assisted" lets the
configured model (llm.provider) fill only the fields the rules left empty or
uncertain, never images, needs or policy results; "model" asks the model for
the whole plan as a baseline. The model never sees secrets and its answer
passes the same gates. When the model is unreachable or llm.provider is none,
the rules' plan is used and a note says so. --no-llm is --engine rules.

Nothing that needs a privilege is granted unless you pass --allow-need for it;
images outside the registry allowlist need --allow-registry. A blocked plan is
still stored so you can inspect it, and gen exits with status 2.

--stop-after facts prints what extraction found (the Analyse step);
--stop-after ir prints the plan without rendering or storing it.`,
		Example: `  composewright gen https://github.com/miniflux/v2
  composewright gen ./my-checkout --upstream https://github.com/myorg/myapp
  composewright gen ./homeassistant --allow-need host_network
  composewright gen ./app --expose-via npm_default --storage bind
  composewright gen --stop-after facts ./my-checkout`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			flags.options.Source = arguments[0]
			return application.runGen(command.Context(), flags)
		},
	}
	set := command.Flags()
	set.StringVar(&flags.options.Name, "name", "", "project name (default: the app service, else the repository name)")
	set.StringVar(&flags.options.Ref, "ref", "", "branch, tag or commit to fetch (default: the latest release, else the default branch)")
	set.StringVar(&flags.options.Owner, "owner", "", "repository owner whose image namespaces are trusted (default: the GitHub owner)")
	set.StringVar(&flags.options.Upstream, "upstream", "", "repository URL a local directory was checked out from (default: its origin remote)")
	set.StringVar(&flags.options.ExposeVia, "expose-via", "", "how the app's ports are published: local, lan or none (default: none when it joins a network, else local)")
	set.StringSliceVar(&flags.options.Networks, "network", nil, "join this network from profile.networks (repeatable; default: the profile's default networks)")
	set.StringVar(&flags.options.Storage, "storage", "", "volumes or bind (bind mounts under profile.storage.base_path; default: profile.storage.style)")
	set.StringVar(&flags.options.ProfileFile, "profile", "", "use this exported profile.yaml instead of the config's profile")
	set.StringSliceVar(&flags.options.AllowNeeds, "allow-need", nil, "grant a requested capability: "+strings.Join(ir.Capabilities, ", ")+" (repeatable, audited)")
	set.StringSliceVar(&flags.options.AllowRegistries, "allow-registry", nil, "trust images matching this pattern, e.g. docker.io/vaultwarden/* (repeatable)")
	set.BoolVar(&flags.offline, "offline", false, "composewright contacts nothing: local directories only, images are not verified (no digest pinning); a sandbox run still lets Docker pull images, add --no-validate to prevent that")
	set.BoolVar(&flags.noLabelCheck, "no-label-check", false, "verify image digests with HEAD only; skip reading image configs (labels, healthchecks), which Docker Hub counts as a pull")
	set.StringVar(&flags.options.Engine, "engine", "", "who writes the plan: "+strings.Join(pipeline.Engines, ", ")+" (default: llm.engine)")
	set.BoolVar(&flags.noLLM, "no-llm", false, "use the deterministic rules only, never a language model (same as --engine rules)")
	set.BoolVar(&flags.noValidate, "no-validate", false, "store the result without running it in the sandbox")
	set.IntVar(&flags.options.MaxIterations, "max-iterations", 0, "validation runs including repairs (default: docker.validate.max_iterations)")
	set.DurationVar(&flags.timeout, "timeout", 0, "how long to wait for the stack to become healthy (default: docker.validate.timeout)")
	set.StringVar(&flags.options.StopAfter, "stop-after", "", "stop after a stage and print it: "+strings.Join(pipeline.Stages, ", "))
	return command
}

func (application *app) runGen(ctx context.Context, flags genFlags) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	stops := flags.options.StopAfter == pipeline.StageFacts || flags.options.StopAfter == pipeline.StageIR
	var opened *workspace.Workspace
	if !stops {
		if opened, err = workspace.Open(ctx, loaded); err != nil {
			return err
		}
		defer opened.Close()
	}
	configuration := loaded.Config
	if flags.timeout > 0 {
		configuration.Docker.Validate.Timeout = flags.timeout
	}
	if flags.options.Engine == "" {
		flags.options.Engine = loaded.Config.LLM.Engine
	}
	if flags.noLLM {
		flags.options.Engine = pipeline.EngineRules
	}
	flags.options.RunSandbox = !flags.noValidate && !stops
	connection, err := pipeline.Connect(ctx, pipeline.Setup{Config: configuration, Paths: loaded.Paths, Workspace: opened,
		Version: application.build.Version, Engine: flags.options.Engine, ExtraRegistries: flags.options.AllowRegistries,
		Offline: flags.offline, NoLabelCheck: flags.noLabelCheck, Validate: flags.options.RunSandbox})
	if err != nil {
		return err
	}
	if connection.Note != "" {
		application.note(connection.Note)
	}
	flags.options.Engine = connection.Engine
	flags.options.ApprovedByFlag = application.global.yes
	if flags.options.MaxIterations == 0 {
		flags.options.MaxIterations = configuration.Docker.Validate.MaxIterations
	}
	return application.generate(ctx, connection.Dependencies, flags)
}

func (application *app) note(text string) {
	fmt.Fprintln(application.stderr, application.colours.muted("note"), sanitize(text))
}

func (application *app) generate(ctx context.Context, dependencies pipeline.Dependencies, flags genFlags) error {
	stops := flags.options.StopAfter == pipeline.StageFacts || flags.options.StopAfter == pipeline.StageIR
	shown := false
	dependencies.Approve = func(result *pipeline.Result) (bool, error) {
		shown = true
		if err := application.showPlan(result); err != nil {
			return false, err
		}
		return application.confirm(fmt.Sprintf("\nRun %s in an isolated sandbox (pulls %d images, removed afterwards)?", result.Plan.App.Name, len(result.Plan.Services)))
	}
	result, err := pipeline.Generate(ctx, dependencies, flags.options)
	if err != nil {
		return err
	}
	if flags.options.StopAfter == pipeline.StageFacts {
		return application.showFacts(result)
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"source": result.Source, "plan": result.Plan, "policy": result.Report,
			"engine": result.Engine, "model_call": result.Call, "project": result.Project, "generation": result.Generation.ID,
			"verdicts": result.Verdicts, "notices": result.Notices})
	}
	if !shown {
		if err := application.showPlan(result); err != nil {
			return err
		}
	}
	if stops {
		return blockedError(result.Report)
	}
	for _, notice := range result.Notices {
		fmt.Fprintln(application.stdout, application.colours.muted("note"), sanitize(notice))
	}
	for _, verdict := range result.Verdicts {
		application.printVerdict(verdict)
	}
	fmt.Fprintf(application.stdout, "\nstored as project %s (generation %s)\n", result.Project.Name, short(result.Generation.ID))
	if !result.Report.Blocked() {
		fmt.Fprintf(application.stdout, "next: composewright show %s · composewright export %s --to ./%s\n", result.Project.Name, result.Project.Name, result.Project.Name)
	}
	return errors.Join(blockedError(result.Report), validationError(result.Verdicts))
}

func (application *app) showPlan(result *pipeline.Result) error {
	if application.global.json {
		return nil
	}
	if err := application.printPlan(application.stdout, result.Plan); err != nil {
		return err
	}
	fmt.Fprintln(application.stdout, application.colours.muted(engineText(result)))
	return application.printPolicy(application.stdout, result.Report)
}

func engineText(result *pipeline.Result) string {
	text := "engine: " + result.Engine
	if result.Call.Model != "" {
		text += " (" + sanitize(result.Call.Provider+" "+result.Call.Model) + ")"
	}
	return text
}

func validationError(verdicts []sandbox.Verdict) error {
	if len(verdicts) == 0 || verdicts[len(verdicts)-1].Passed() {
		return nil
	}
	last := verdicts[len(verdicts)-1]
	return codedError{code: exitInvalid, err: fmt.Errorf("validation failed (%s): %s", last.Reason, last.Detail)}
}

func (application *app) showFacts(result *pipeline.Result) error {
	facts, err := result.Facts.Redacted()
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"source": result.Source, "facts": facts})
	}
	return application.printFacts(application.stdout, facts)
}

func (application *app) printPolicy(output io.Writer, report policy.Report) error {
	fmt.Fprintf(output, "\npolicy: %d deny · %d need · %d warn · %d filled from defaults\n", report.Count(policy.Deny),
		report.Count(policy.Need), report.Count(policy.Warn), report.Count(policy.Fix))
	return application.printFindings(output, report)
}

func blockedError(report policy.Report) error {
	if !report.Blocked() {
		return nil
	}
	return codedError{code: exitBlocked, err: fmt.Errorf("the plan is blocked by %d deny and %d need findings", report.Count(policy.Deny), report.Count(policy.Need))}
}

func short(identifier string) string {
	const length = 12
	return identifier[:min(len(identifier), length)]
}

// printFacts is the CLI form of the Analyse screen: every row names the file
// and line it came from.
func (application *app) printFacts(output io.Writer, facts *extract.Facts) error {
	fmt.Fprintf(output, "%d files read · %d compose files · %d Dockerfiles · %d env samples · %d variables\n\n",
		len(facts.Files), len(facts.ComposeFiles), len(facts.Dockerfiles), len(facts.EnvSamples), len(facts.Variables))
	rows := [][]string{{"KIND", "VALUE", "SOURCE"}}
	for _, image := range facts.Images {
		rows = append(rows, []string{"image", sanitize(image.Reference), image.Location.String()})
	}
	for _, file := range facts.ComposeFiles {
		for _, service := range file.Services {
			rows = append(rows, composeRows(service)...)
		}
	}
	for _, dockerfile := range facts.Dockerfiles {
		rows = append(rows, dockerfileRows(dockerfile)...)
	}
	for _, variable := range facts.Variables {
		kind := "env"
		if redact.IsSecretName(variable.Name) {
			kind = "env " + application.colours.warn("secret")
		}
		rows = append(rows, []string{kind, sanitize(variable.Name), variableSources(variable)})
	}
	for _, note := range facts.Notes {
		rows = append(rows, []string{"note", sanitize(truncate(note.Text)), note.Location.String()})
	}
	for _, note := range facts.Suspicious {
		rows = append(rows, []string{application.colours.danger("suspicious"), sanitize(truncate(note.Text)), note.Location.String()})
	}
	for _, problem := range facts.Problems {
		rows = append(rows, []string{application.colours.warn("problem"), sanitize(problem), ""})
	}
	return table(output, rows...)
}

func composeRows(service extract.ComposeService) [][]string {
	var rows [][]string
	for _, port := range service.Ports {
		rows = append(rows, []string{"port", strconv.Itoa(port.Target) + "/" + port.Protocol + " (" + service.Name + ")", port.Location.String()})
	}
	for _, volume := range service.Volumes {
		rows = append(rows, []string{"volume", volume.Target + " (" + service.Name + ", " + volume.Type + ")", volume.Location.String()})
	}
	if service.Healthcheck != nil && len(service.Healthcheck.Test) > 0 {
		rows = append(rows, []string{"healthcheck", sanitize(strings.Join(service.Healthcheck.Test, " ")) + " (" + service.Name + ")", service.Healthcheck.Location.String()})
	}
	return rows
}

func dockerfileRows(dockerfile extract.Dockerfile) [][]string {
	var rows [][]string
	for _, port := range dockerfile.Exposed {
		rows = append(rows, []string{"port", strconv.Itoa(port.Port) + "/" + port.Protocol, port.Location.String()})
	}
	for _, volume := range dockerfile.Volumes {
		rows = append(rows, []string{"volume", sanitize(volume.Path), volume.Location.String()})
	}
	if dockerfile.User != "" {
		rows = append(rows, []string{"user", sanitize(dockerfile.User), dockerfile.UserAt.String()})
	}
	if dockerfile.Healthcheck != nil && len(dockerfile.Healthcheck.Test) > 0 {
		rows = append(rows, []string{"healthcheck", sanitize(strings.Join(dockerfile.Healthcheck.Test, " ")), dockerfile.Healthcheck.Location.String()})
	}
	return rows
}

func variableSources(variable extract.Variable) string {
	const shown = 2
	locations := make([]string, 0, shown)
	for index, mention := range variable.Mentions {
		if index == shown {
			locations = append(locations, fmt.Sprintf("+%d more", len(variable.Mentions)-shown))
			break
		}
		locations = append(locations, mention.Location.String())
	}
	return strings.Join(locations, " · ")
}

func truncate(text string) string {
	const limit = 100
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
