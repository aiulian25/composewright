package pipeline

import (
	"context"
	"os"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/rules"
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
)

// Engines are the ways a plan is made: rules alone, rules with the model
// filling what they left open, or the model alone.
var Engines = []string{EngineRules, EngineAssisted, EngineModel}

// draft builds the first plan. The model engine asks the model for the whole
// plan; a failure leaves the rules' plan and says so.
func draft(ctx context.Context, dependencies Dependencies, options Options, result *Result) {
	rulePlan := rules.Build(ctx, rules.Input{Facts: result.Facts, Source: result.Source, Name: options.Name})
	result.Plan, result.Engine = rulePlan, EngineRules
	if options.Engine != EngineModel || dependencies.Assistant == nil {
		return
	}
	observer := dependencies.Observer
	observer.step(StepModel, StateRunning, "")
	proposal, call, err := dependencies.Assistant.Draft(ctx, result.Facts, documents(result.Source.Directory, result.Facts.Readmes))
	if err != nil {
		result.Notices = append(result.Notices, modelFailed+err.Error())
		observer.step(StepModel, StateSkipped, modelFailed+err.Error())
		return
	}
	drafted := ir.FromModel(proposal)
	drafted.App.Name, drafted.App.UpstreamURL = rulePlan.App.Name, rulePlan.App.UpstreamURL
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
	proposal, call, err := dependencies.Assistant.Fill(ctx, result.Plan, fields, result.Facts, documents(result.Source.Directory, result.Facts.Readmes))
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
