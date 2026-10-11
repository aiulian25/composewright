// Package eval measures the pipeline on the pinned corpus:
// per engine (rules, assisted, model), how many plans pass the policy gate
// and, with a Docker daemon, how many first validation runs pass.
package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/corpus"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	notValidated = "-"
	evalVersion  = "eval"
	fellBack     = "fell back to the rules; "
)

type Row struct {
	Engine     string        `json:"engine"`
	Repository string        `json:"repository"`
	PolicyPass bool          `json:"policy_pass"`
	Confidence string        `json:"confidence"`
	Services   int           `json:"services"`
	Variables  int           `json:"variables"`
	Unfilled   int           `json:"unfilled"`
	Validation string        `json:"validation"`
	Reason     string        `json:"reason,omitempty"`
	Iterations int           `json:"iterations"`
	Duration   time.Duration `json:"duration_ns"`
	Limits     bool          `json:"limits_enforced"`
	Model      string        `json:"model,omitempty"`
	Fallback   bool          `json:"fell_back_to_rules"`
	Error      string        `json:"error,omitempty"`
}

type Report struct {
	Rows []Row `json:"rows"`
}

// Options choose how much of the pipeline runs: validation needs a Docker
// daemon and network access to pull images; a model (LLM.Provider other than
// none) adds the assisted and model engines.
type Options struct {
	Validate bool
	Timeout  time.Duration
	LLM      config.LLM
}

// Run plans, and optionally validates, every corpus entry with every engine
// available: the rules always, the assisted and model engines with a model.
func Run(ctx context.Context, entries []corpus.Entry, options Options) (Report, error) {
	dependencies, cleanup, err := prepare(ctx, options)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()
	engines := []string{pipeline.EngineRules}
	if dependencies.Assistant != nil {
		engines = pipeline.Engines
	}
	var report Report
	for _, engine := range engines {
		for _, entry := range entries {
			report.Rows = append(report.Rows, run(ctx, dependencies, entry, engine, options))
		}
	}
	return report, nil
}

// prepare wires the eval like the CLI. Without validation the images are
// not checked, so the rules engine stays fast and needs no network; a
// model, when configured, is always attached.
func prepare(ctx context.Context, options Options) (pipeline.Dependencies, func(), error) {
	loaded := &config.Loaded{Config: config.Default()}
	if options.LLM.Provider != "" {
		loaded.Config.LLM = options.LLM
	}
	if options.Timeout > 0 {
		loaded.Config.Docker.Validate.Timeout = options.Timeout
	}
	cleanup := func() {}
	var opened *workspace.Workspace
	if options.Validate {
		root, err := os.MkdirTemp("", "composewright-eval-*")
		if err != nil {
			return pipeline.Dependencies{}, nil, err
		}
		cleanup = func() { _ = os.RemoveAll(root) }
		loaded.Paths = config.Paths{ConfigFile: filepath.Join(root, "config.yaml"), DataDir: filepath.Join(root, "data"),
			KeyFile: filepath.Join(root, "data", "keys", "master.key")}
		if opened, err = openWorkspace(ctx, loaded); err != nil {
			cleanup()
			return pipeline.Dependencies{}, nil, err
		}
		closeWorkspace := cleanup
		cleanup = func() { _ = opened.Close(); closeWorkspace() }
	}
	connection, err := pipeline.Connect(ctx, pipeline.Setup{Config: loaded.Config, Paths: loaded.Paths, Workspace: opened,
		Version: evalVersion, Engine: pipeline.EngineAssisted, Validate: options.Validate})
	if err == nil && connection.Note != "" {
		err = errors.New(connection.Note)
	}
	if err != nil {
		cleanup()
		return pipeline.Dependencies{}, nil, err
	}
	dependencies := connection.Dependencies
	dependencies.Approve = func(*pipeline.Result) (bool, error) { return true, nil }
	if !options.Validate {
		dependencies.Inspector = nil
	}
	return dependencies, cleanup, nil
}

func openWorkspace(ctx context.Context, loaded *config.Loaded) (*workspace.Workspace, error) {
	if _, err := workspace.Init(ctx, loaded); err != nil {
		return nil, err
	}
	return workspace.Open(ctx, loaded)
}

func run(ctx context.Context, dependencies pipeline.Dependencies, entry corpus.Entry, engine string, options Options) Row {
	row := Row{Engine: engine, Repository: entry.Slug, Validation: notValidated}
	pipelineOptions := pipeline.Options{Source: entry.Directory, Upstream: entry.Source.URL, AllowRegistries: entry.AllowRegistries,
		Engine: engine, RunSandbox: options.Validate, MaxIterations: 1}
	if !options.Validate {
		pipelineOptions.StopAfter = pipeline.StageIR
	}
	result, err := pipeline.Generate(ctx, dependencies, pipelineOptions)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	plan := result.Plan
	row.Model, row.Fallback = result.Call.Model, result.Engine != engine
	if row.Fallback {
		row.Error = fellBack + strings.Join(result.Notices, "; ")
	}
	row.PolicyPass, row.Confidence = !result.Report.Blocked(), plan.Confidence
	row.Services, row.Variables, row.Unfilled = len(plan.Services), len(plan.EnvFile), len(plan.Unfilled)
	if len(result.Verdicts) == 0 {
		return row
	}
	first := result.Verdicts[0]
	row.Validation, row.Reason, row.Iterations = first.Result, first.Reason, len(result.Verdicts)
	row.Duration, row.Limits = first.Duration, first.Sandbox.LimitsEnforced
	if !first.Passed() {
		row.Reason += ": " + first.Detail
	}
	return row
}

// Write prints one line per repository and a summary per engine.
func (report Report) Write(output io.Writer) error {
	const padding = 2
	writer := tabwriter.NewWriter(output, 0, 0, padding, ' ', 0)
	fmt.Fprintln(writer, "ENGINE\tREPOSITORY\tPOLICY\tCONFIDENCE\tSERVICES\tVARIABLES\tUNFILLED\tVALIDATION\tTIME\tDETAIL")
	passed, validated, valid, total, fallbacks := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, row := range report.Rows {
		total[row.Engine]++
		if row.Fallback {
			fallbacks[row.Engine]++
		}
		verdict := "blocked"
		if row.PolicyPass {
			verdict = "pass"
			passed[row.Engine]++
		}
		if row.Validation != notValidated {
			validated[row.Engine]++
		}
		if row.Validation == sandbox.ResultPass {
			valid[row.Engine]++
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s%s\n", row.Engine, row.Repository, verdict, row.Confidence,
			row.Services, row.Variables, row.Unfilled, row.Validation, row.Duration.Round(time.Second), row.Reason, row.Error)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, engine := range pipeline.Engines {
		count := total[engine]
		if count == 0 {
			continue
		}
		fmt.Fprintf(output, "%s: policy pass %d/%d (%s%%)", engine, passed[engine], count, percent(passed[engine], count))
		if validated[engine] > 0 {
			fmt.Fprintf(output, " · first-run validation PASS %d/%d (%s%%)", valid[engine], validated[engine], percent(valid[engine], validated[engine]))
		}
		if fallbacks[engine] > 0 {
			fmt.Fprintf(output, " · %d fell back to the rules", fallbacks[engine])
		}
		fmt.Fprintln(output)
	}
	return nil
}

func percent(part, whole int) string {
	const hundred = 100
	if whole == 0 {
		return "0"
	}
	return strconv.Itoa(part * hundred / whole)
}
