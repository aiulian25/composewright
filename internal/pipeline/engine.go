package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	EngineRules    = config.EngineRules
	EngineAssisted = config.EngineAssisted
	EngineModel    = config.EngineModel
	// EngineManual marks a version a person wrote by hand. No model repairs
	// it: a repair renders compose.yaml from the plan and would drop the edits.
	EngineManual = "manual"
	// yamlGateRule forbids keys such as privileged in compose.yaml itself.
	yamlGateRule = "P13"

	maxDocumentBytes = 256 << 10
	modelFailed      = "the model could not help, so this plan is the rules' alone: "
	nothingToAsk     = "the rules filled every field with enough confidence, so the model was not asked"
	rulesOnly        = "rules only: no model is used for this plan"
	keptChoiceNotice = "an optional service switched on before stays off: "
	modelLeftOut     = "the model's plan leaves out an optional service you switched on, so it does not run: "
)

// Engines are the ways a plan is made: rules alone, rules with the model
// filling what they left open, or the model alone.
var Engines = []string{EngineRules, EngineAssisted, EngineModel}

// draft builds the first plan from options.ComposeFile, when the facts have
// it, with the optional services in options.EnabledOptional switched on,
// which it spells as the plan does, and records in options.ComposeFile the
// file the rules built from. Named none, a regeneration keeps the compose
// file and the optional services the project chose; any the
// repository, or the compose file chosen, no longer offers stay off, with a
// notice. The model engine asks the model for the whole plan; a failure
// leaves the rules' plan and says so.
func draft(ctx context.Context, dependencies Dependencies, options *Options, result *Result) error {
	input := rules.Input{Facts: result.Facts, Source: result.Source, Name: options.Name, EnabledOptional: options.EnabledOptional,
		ComposeFile: options.ComposeFile}
	rulePlan := rules.Build(ctx, input)
	kept := options.EnabledOptional == nil
	previous, err := previousOptions(ctx, dependencies.Workspace, rulePlan.App.Name, result.Source.URL, options.Uploaded)
	if err != nil {
		return err
	}
	carryPrevious(&input, previous, kept)
	composeCarried := input.ComposeFile != options.ComposeFile
	if composeCarried {
		// The file carried over may name its app service otherwise; the
		// project keeps the name it was found by.
		input.Name = cmp.Or(input.Name, rulePlan.App.Name)
	}
	if composeCarried || !slices.Equal(input.EnabledOptional, options.EnabledOptional) {
		rulePlan = rules.Build(ctx, input)
	}
	// The file the rules built from is kept, so a regeneration stays on it
	// and never looks again for one they could not find.
	options.ComposeFile = rules.ChosenCompose(input)
	variantNote, variantMissing := rules.MissingVariantNote(rulePlan, input.ComposeFile)
	if variantMissing {
		result.Notices = append(result.Notices, variantNote)
	}
	enabled, err := rules.Enabled(rulePlan.OptionalServices, input.EnabledOptional)
	if err != nil && !kept && !options.Regenerating {
		return err
	}
	if err != nil {
		result.Notices = append(result.Notices, keptChoiceNotice+err.Error())
	}
	options.EnabledOptional = enabled
	result.Plan, result.Engine = rulePlan, EngineRules
	if options.Engine != EngineModel || dependencies.Assistant == nil {
		return nil
	}
	modelDraft(ctx, dependencies, enabled, options.ComposeFile, result)
	if variantMissing && result.Plan != rulePlan {
		result.Plan.PostInstallNotes = append(result.Plan.PostInstallNotes, variantNote)
	}
	return nil
}

// carryPrevious fills in from the options the project was last generated
// with what a person left unnamed: the compose file, and the optional
// services when kept.
func carryPrevious(input *rules.Input, previous Options, kept bool) {
	input.ComposeFile = cmp.Or(input.ComposeFile, previous.ComposeFile)
	if kept {
		input.EnabledOptional = previous.EnabledOptional
	}
}

// previousOptions are the options the project name was last generated
// with, when it comes from the same source; an upload always does.
func previousOptions(ctx context.Context, workspace *workspace.Workspace, name, sourceURL string, uploaded bool) (Options, error) {
	var options Options
	if workspace == nil {
		return options, nil
	}
	project, generation, found, err := latestGeneration(ctx, workspace.Store, name)
	if err != nil || !found || (!uploaded && project.SourceURL != sourceURL) {
		return options, err
	}
	// Only the options are read, and a decode error is ignored: a
	// regeneration is how a version that no longer loads is mended, so
	// nothing of it may stop one.
	_ = json.Unmarshal([]byte(generation.OptionsJSON), &options)
	return options, nil
}

// latestGeneration is the latest version of the project name, if it has one.
func latestGeneration(ctx context.Context, database *store.Store, name string) (store.Project, store.Generation, bool, error) {
	project, err := database.FindProject(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return project, store.Generation{}, false, nil
	}
	if err != nil {
		return project, store.Generation{}, false, err
	}
	generation, err := database.LatestGeneration(ctx, project.ID)
	if errors.Is(err, store.ErrNotFound) {
		return project, generation, false, nil
	}
	return project, generation, err == nil, err
}

// modelDraft asks the model for the whole plan from the facts of what
// Compose starts, so it never sees an optional service that is off, nor
// another compose file than the one the rules built from. A depends_on it copied
// onto one that is off is dropped, as the rules drop it, and a service
// switched on that it left out gets a notice.
func modelDraft(ctx context.Context, dependencies Dependencies, enabled []string, composeFile string, result *Result) {
	rulePlan := result.Plan
	observer := dependencies.Observer
	observer.step(StepModel, StateRunning, "")
	facts := result.Facts.Started(enabled).FromCompose(composeFile)
	proposal, call, err := dependencies.Assistant.Draft(ctx, facts, documents(result.Source.Directory, result.Facts.Readmes))
	if err != nil {
		result.Notices = append(result.Notices, modelFailed+err.Error())
		observer.step(StepModel, StateSkipped, modelFailed+err.Error())
		return
	}
	drafted := ir.FromModel(proposal)
	drafted.App.Name, drafted.App.UpstreamURL = rulePlan.App.Name, rulePlan.App.UpstreamURL
	// The rules list what the upstream documents; a model never does.
	drafted.OptionalEnv, drafted.OptionalServices = rulePlan.OptionalEnv, rulePlan.OptionalServices
	rules.DropOffDependencies(drafted)
	for _, name := range enabled {
		if _, planned := drafted.Service(name); !planned {
			result.Notices = append(result.Notices, modelLeftOut+name)
		}
	}
	drafted.Unfilled = rules.Gaps(drafted)
	result.Plan, result.Engine, result.Call = drafted, EngineModel, call
	observer.step(StepModel, StateDone, "")
}

// fill asks the model only for the fields the rules left open or filled
// below the threshold, and merges the answer with precedence: images, needs
// and confident rule values never change.
func fill(ctx context.Context, dependencies Dependencies, options Options, result *Result) {
	observer := dependencies.Observer
	if options.Engine != EngineAssisted || dependencies.Assistant == nil {
		if result.Engine == EngineRules {
			observer.step(StepModel, StateSkipped, rulesOnly)
		}
		return
	}
	result.Engine = EngineAssisted
	fields := ir.Fillable(result.Plan, ir.ModelThreshold)
	if len(fields) == 0 {
		result.Notices = append(result.Notices, nothingToAsk)
		observer.step(StepModel, StateSkipped, nothingToAsk)
		return
	}
	observer.step(StepModel, StateRunning, "")
	facts := result.Facts.Started(options.EnabledOptional).FromCompose(options.ComposeFile)
	proposal, call, err := dependencies.Assistant.Fill(ctx, result.Plan, fields, facts, documents(result.Source.Directory, result.Facts.Readmes))
	if err != nil {
		result.Engine = EngineRules
		result.Notices = append(result.Notices, modelFailed+err.Error())
		observer.step(StepModel, StateSkipped, modelFailed+err.Error())
		return
	}
	merged, rejected := ir.Merge(result.Plan, proposal, ir.ModelThreshold)
	merged.Unfilled = rules.Gaps(merged)
	result.Plan, result.Call = merged, call
	result.Notices = append(result.Notices, rejected...)
	observer.step(StepModel, StateDone, "")
}

// documents reads the READMEs extraction found, inside the source directory
// only.
func documents(directory string, paths []string) []llm.Document {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil
	}
	defer root.Close()
	var found []llm.Document
	for _, path := range paths {
		content, err := root.ReadFile(path)
		if err != nil {
			continue
		}
		found = append(found, llm.Document{Path: path, Text: string(content[:min(len(content), maxDocumentBytes)])})
	}
	return found
}
