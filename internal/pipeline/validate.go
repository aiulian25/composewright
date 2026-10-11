package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

// ErrBlocked means the policy gate blocks the plan, so it must not run.
var ErrBlocked = errors.New("the policy gate blocks this plan; resolve its findings first")

const (
	logsDirName      = "logs"
	logFileMode      = 0o600
	logDirMode       = 0o700
	startHeader      = "== docker compose up\n"
	logsHeader       = "\n== docker compose logs\n"
	statusLinePrefix = "Validation: "
	minimumSecret    = 4
)

// Repairer proposes a corrected plan after a failed run; llm.Assistant is
// the real one. Without one, a failure is left for a human.
type Repairer interface {
	Repair(ctx context.Context, plan *ir.IR, verdict sandbox.Verdict) (*ir.IR, llm.Call, error)
}

// Approval shows the plan and returns whether it may run. Nothing runs
// before it returns true.
type Approval func(result *Result) (bool, error)

// validationLoop runs the stack, repairs and re-runs up to the configured
// number of iterations counted from first, and records every verdict and
// its logs.
func validationLoop(ctx context.Context, dependencies Dependencies, options Options, result *Result, first int) error {
	observer := dependencies.Observer
	runner := dependencies.Sandbox
	if runner == nil {
		return errors.New("validation needs the docker CLI; pass --no-validate to skip it")
	}
	if err := runner.RequireCompose(ctx); err != nil {
		return err
	}
	if removed, err := runner.Janitor(ctx); err != nil || len(removed) > 0 {
		result.Notices = append(result.Notices, janitorNotice(removed, err))
	}
	limit := max(options.MaxIterations, 1)
	for iteration := first; ; iteration++ {
		observer.step(StepValidate, StateRunning, fmt.Sprintf("run %d of at most %d", iteration, limit))
		input := sandboxInput(result.Plan, result.Files, result.Values, iteration)
		input.Live = observer.Line
		verdict, err := runner.Validate(ctx, input)
		if err != nil {
			return observer.failed(StepValidate, err)
		}
		result.Verdicts = append(result.Verdicts, verdict)
		status := statusAfter(verdict, dependencies.Repairer != nil && iteration < limit)
		if err := record(ctx, dependencies.Workspace, result.Project, result.Generation, verdict, status); err != nil {
			return observer.failed(StepValidate, err)
		}
		verdictState := StateDone
		if !verdict.Passed() {
			verdictState = StateFailed
		}
		observer.step(StepValidate, verdictState, describe(verdict))
		if status != store.StatusFailed {
			return nil
		}
		observer.step(StepRepair, StateRunning, fmt.Sprintf("asking the model to repair run %d", iteration))
		if err := repair(ctx, dependencies, options, result, verdict, iteration+1); err != nil {
			result.Notices = append(result.Notices, "repair stopped: "+err.Error())
			observer.step(StepRepair, StateFailed, err.Error())
			return dependencies.Workspace.Store.SetProjectStatus(ctx, result.Project.ID, store.StatusNeedsHuman)
		}
		observer.step(StepRepair, StateDone, "")
	}
}

// ApproveAndValidate runs a stored project's latest generation in the
// sandbox and repairs it up to the configured number of runs: what the web
// UI does once a person approves the Review page. The approval is audited
// with how it was given. A blocked plan is refused.
func ApproveAndValidate(ctx context.Context, dependencies Dependencies, name, via string) (*Result, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	report, err := regate(ctx, dependencies, stored)
	if err != nil {
		return nil, err
	}
	if report.Blocked() {
		return nil, blockedError(name, report)
	}
	result, err := stored.result()
	if err != nil {
		return nil, err
	}
	result.Report = report
	options := stored.Options
	options.MaxIterations = dependencies.Config.Docker.Validate.MaxIterations
	details := map[string]any{"generation": stored.Generation.ID, "via": via, "allow_needs": options.AllowNeeds}
	if err := dependencies.Workspace.Store.Audit(ctx, dependencies.Workspace.Actor, store.ActionApprove, stored.Project.ID, details); err != nil {
		return nil, err
	}
	return result, validationLoop(ctx, dependencies, options, result, 1)
}

// regate runs both policy gates again on a stored version with today's
// configuration: its stored report reflects the allowlist and the profile
// as they were when it was made.
func regate(ctx context.Context, dependencies Dependencies, stored *Stored) (policy.Report, error) {
	project, err := composefile.Load(ctx, composeFileName, []byte(stored.Generation.ComposeYAML), nil, false)
	if err != nil {
		return policy.Report{}, fmt.Errorf("%w: %w", ErrComposeInvalid, err)
	}
	profile, err := loadProfile(dependencies.Config.Profile, stored.Options.ProfileFile)
	if err != nil {
		return policy.Report{}, err
	}
	input := policyInput(dependencies.Config, stored.Plan, stored.source(), stored.Options)
	_, input.Images = stored.verifiedImages()
	input.Profile = profile
	return policy.Evaluate(input).Merge(policy.EvaluateYAML(ctx, project, []byte(stored.Generation.ComposeYAML), input)), nil
}

// blockedError names the first finding that blocks, so a refusal is clear
// even when the stored report, made with older settings, shows none.
func blockedError(name string, report policy.Report) error {
	for _, finding := range report.Findings {
		if finding.Severity >= policy.Need {
			return fmt.Errorf("%w: %s: %s %s", ErrBlocked, name, finding.Rule, finding.Message)
		}
	}
	return fmt.Errorf("%w: %s", ErrBlocked, name)
}

// statusAfter is passed for a PASS; failed while another iteration may
// repair it; needs_human once nothing more will be tried.
func statusAfter(verdict sandbox.Verdict, canRetry bool) string {
	switch {
	case verdict.Passed():
		return store.StatusPassed
	case canRetry:
		return store.StatusFailed
	}
	return store.StatusNeedsHuman
}

// repair asks the repairer for a new plan, keeps only the changes a repair
// may make (ir.MergeRepair), and runs it through the same gates as the
// first plan; it becomes a new generation.
func repair(ctx context.Context, dependencies Dependencies, options Options, result *Result, verdict sandbox.Verdict, iteration int) error {
	proposal, call, err := dependencies.Repairer.Repair(ctx, result.Plan, verdict)
	if err != nil {
		return err
	}
	plan, rejected := ir.MergeRepair(result.Plan, proposal)
	result.Notices = append(result.Notices, rejected...)
	if samePlan(plan, result.Plan) {
		return errors.New("the repair changed nothing a repair may change")
	}
	plan.Unfilled = rules.Gaps(plan)
	result.Plan, result.Call = plan, call
	options.Edited = false
	input := policyInput(dependencies.Config, plan, result.Source, options)
	input.Images = result.Checks
	if err := renderAndGate(ctx, dependencies, options, result, input, nil); err != nil {
		return err
	}
	if result.Report.Blocked() {
		return fmt.Errorf("the repaired plan is blocked by the policy gate")
	}
	return persist(ctx, dependencies, options, result, iteration, store.ActionGenerate)
}

// Revalidate runs a stored project's latest generation once more, as the
// `validate` command asks; it never repairs.
func Revalidate(ctx context.Context, dependencies Dependencies, name string) (*Stored, sandbox.Verdict, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, sandbox.Verdict{}, err
	}
	report, err := regate(ctx, dependencies, stored)
	if err != nil {
		return stored, sandbox.Verdict{}, err
	}
	if report.Blocked() {
		return stored, sandbox.Verdict{}, blockedError(name, report)
	}
	if err := dependencies.Sandbox.RequireCompose(ctx); err != nil {
		return stored, sandbox.Verdict{}, err
	}
	env, err := stored.EnvFile(Revealed)
	if err != nil {
		return stored, sandbox.Verdict{}, err
	}
	files := Files{Compose: []byte(stored.Generation.ComposeYAML), Env: env}
	verdict, err := dependencies.Sandbox.Validate(ctx, sandboxInput(stored.Plan, files, stored.Values, stored.Generation.Iteration))
	if err != nil {
		return stored, verdict, err
	}
	err = record(ctx, dependencies.Workspace, stored.Project, stored.Generation, verdict, statusAfter(verdict, false))
	return stored, verdict, err
}

func sandboxInput(plan *ir.IR, files Files, values map[string]string, iteration int) sandbox.Input {
	input := sandbox.Input{Project: plan.App.Name, Compose: files.Compose, Env: files.Env, Iteration: iteration,
		Published: map[string][]string{}, CompletedOK: map[string]bool{}, Probe: httpProbe(plan)}
	for _, service := range plan.Services {
		for _, port := range service.Ports {
			if port.Publish {
				input.Published[service.Name] = append(input.Published[service.Name], strconv.Itoa(port.Container)+"/"+port.Protocol)
			}
		}
		input.CompletedOK[service.Name] = service.Role == ir.RoleInit
	}
	for _, entry := range plan.EnvFile {
		if value := values[entry.Key]; entry.Secret && len(value) >= minimumSecret {
			input.Secrets = append(input.Secrets, value)
		}
	}
	return input
}

// httpProbe is the plan's probe when it names a TCP port of its service:
// an HTTP request cannot reach a UDP one, so such a probe is not run.
func httpProbe(plan *ir.IR) ir.Probe {
	service, found := plan.Service(plan.Probe.Service)
	if !found {
		return ir.Probe{}
	}
	tcp := slices.ContainsFunc(service.Ports, func(port ir.Port) bool {
		return port.Container == plan.Probe.Port && port.Protocol == ir.ProtocolTCP
	})
	if !tcp {
		return ir.Probe{}
	}
	return plan.Probe
}

// record keeps the verdict with its generation, the scrubbed logs in a 0600
// file, the project status, and an audit entry.
func record(ctx context.Context, workspace *workspace.Workspace, project store.Project, generation store.Generation, verdict sandbox.Verdict, status string) error {
	directory := filepath.Join(workspace.Paths.ProjectsDir(), project.Slug, logsDirName)
	if err := os.MkdirAll(directory, logDirMode); err != nil {
		return err
	}
	logsPath := filepath.Join(directory, fmt.Sprintf("%s-%d.log", generation.ID, verdict.Iteration))
	content := startHeader + verdict.StartOutput + logsHeader + verdict.Logs
	if err := os.WriteFile(logsPath, []byte(content), logFileMode); err != nil {
		return err
	}
	encoded, err := json.Marshal(verdict)
	if err != nil {
		return err
	}
	database := workspace.Store
	deploy := statusLine(generation.DeployMD, describe(verdict))
	if err := database.RecordValidation(ctx, generation.ID, string(encoded), logsPath, deploy); err != nil {
		return err
	}
	if err := database.SetProjectStatus(ctx, project.ID, status); err != nil {
		return err
	}
	details := map[string]any{"generation": generation.ID, "iteration": verdict.Iteration, "result": verdict.Result, "reason": verdict.Reason,
		"sandbox": verdict.Project, "limits_enforced": verdict.Sandbox.LimitsEnforced, "rootless": verdict.Sandbox.Rootless}
	return database.Audit(ctx, workspace.Actor, store.ActionValidate, project.ID, details)
}

// KeptLogs reads back the logs record kept for a generation's last run:
// compose's own output, then the services' lines. A run whose file is gone
// has none.
func KeptLogs(generation store.Generation) ([]sandbox.Line, error) {
	if generation.LogsPath == "" {
		return nil, nil
	}
	content, err := os.ReadFile(generation.LogsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	start, logs, _ := strings.Cut(strings.TrimPrefix(string(content), startHeader), logsHeader)
	lines := []sandbox.Line{}
	for _, text := range strings.Split(start, "\n") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		lines = append(lines, sandbox.Line{Source: sandbox.SourceCompose, Text: text})
	}
	return append(lines, sandbox.ParseLogs(logs)...), nil
}

// describe is the one-line validation status written into DEPLOY.md.
func describe(verdict sandbox.Verdict) string {
	text := fmt.Sprintf("%s on %s in %s (iteration %d, sandbox %s", verdict.Result, time.Now().UTC().Format(time.DateOnly),
		verdict.Duration.Round(time.Second), verdict.Iteration, verdict.Sandbox.Tier)
	if verdict.Sandbox.Rootless {
		text += ", rootless"
	}
	if !verdict.Sandbox.LimitsEnforced {
		text += ", resource limits not enforced"
	}
	text += ")"
	if !verdict.Passed() {
		text += " · " + verdict.Reason + ": " + verdict.Detail
	}
	return text
}

func statusLine(deploy, status string) string {
	lines := strings.Split(deploy, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, statusLinePrefix) {
			lines[index] = statusLinePrefix + status
		}
	}
	return strings.Join(lines, "\n")
}

func janitorNotice(removed []string, err error) string {
	if err != nil {
		return "could not check for stale sandboxes: " + err.Error()
	}
	return "removed stale sandboxes left by an earlier run: " + strings.Join(removed, ", ")
}

func samePlan(left, right *ir.IR) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
