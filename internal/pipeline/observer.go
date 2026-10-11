package pipeline

import (
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/sandbox"
)

// Steps of a run, in the order they happen.
const (
	StepFetch    = "fetch"
	StepExtract  = "extract"
	StepPlan     = "plan"
	StepImages   = "images"
	StepModel    = "model"
	StepPolicy   = "policy"
	StepRender   = "render"
	StepValidate = "validate"
	StepRepair   = "repair"

	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
	StateSkipped = "skipped"
)

// Step reports that a stage started, finished, failed or was skipped. Note
// carries the reason for a failure or a skip.
type Step struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// Observer receives progress while a run goes on. Every field may be nil;
// the CLI uses none of them and prints the result at the end.
type Observer struct {
	Step func(Step)
	// Facts receives the redacted facts and the source as soon as they are
	// extracted.
	Facts func(*extract.Facts, *fetch.Source)
	// Line receives redacted sandbox output while a stack runs.
	Line func(sandbox.Line)
}

func (observer Observer) step(name, state, note string) {
	if observer.Step != nil {
		observer.Step(Step{Name: name, State: state, Note: note})
	}
}

func (observer Observer) failed(name string, err error) error {
	observer.step(name, StateFailed, err.Error())
	return err
}

func (observer Observer) facts(facts *extract.Facts, source *fetch.Source) {
	if observer.Facts == nil {
		return
	}
	redacted, err := facts.Redacted()
	if err == nil {
		observer.Facts(redacted, source)
	}
}
