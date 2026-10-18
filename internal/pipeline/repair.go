package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

var (
	// ErrNoRepairer means no model is connected to ask for a repair.
	ErrNoRepairer = errors.New("no model is connected, so none can propose a repair")
	// ErrNothingToRepair means the latest version has no failed run.
	ErrNothingToRepair = errors.New("the latest version has no failed test run to repair")
	// ErrNoRepair means the model was asked but its answer is no repair.
	ErrNoRepair = errors.New("the model proposed no repair")
	// ErrEditedVersion refuses to apply a repair to compose.yaml a person
	// edited.
	ErrEditedVersion = errors.New("this version is your edited compose.yaml: a repair renders compose.yaml from the plan and would drop your edits, so make the change in compose.yaml yourself")
	// ErrProposalStale means a newer version was stored after the repair
	// was proposed, so it is not applied.
	ErrProposalStale = errors.New("the project has a newer version than the one this repair was proposed for; ask the model again")
)

const (
	// hashSeparator keeps the parts of a proposal's hash apart.
	hashSeparator = "\x00"
	leftOutText   = "; left out: "
	rejectedJoin  = "; "
	// The outcome a propose_repair audit entry records.
	outcomeProposed = "proposed"
	outcomeNoRepair = "no repair"
)

// Proposal is a model's repair of a failed run, rendered and gated but not
// stored: Apply stores it as a new version.
type Proposal struct {
	// Current is the version the repair starts from.
	Current *Stored
	Plan    *ir.IR
	Compose []byte
	// Env is the proposed .env with secrets masked as asked.
	Env      []byte
	Report   policy.Report
	Rejected []string
	// Notes are the deployment notes the model added.
	Notes []string
	Call  llm.Call
	// Hash names what was proposed for which version and prompt.
	Hash   string
	result *Result
}

// ProposeRepair asks the repairer to repair the latest version's failed
// run from its kept verdict and logs, and renders and gates the answer
// without storing it. Every answer is audited with its prompt hash, also
// one that is no repair.
func ProposeRepair(ctx context.Context, dependencies Dependencies, name, mask string) (*Proposal, error) {
	if dependencies.Repairer == nil {
		return nil, ErrNoRepairer
	}
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	verdict, tested := StoredVerdict(stored.Generation)
	if !tested || verdict.Passed() {
		return nil, ErrNothingToRepair
	}
	start, logs, err := keptRun(stored.Generation)
	if err != nil {
		return nil, err
	}
	verdict.StartOutput, verdict.LogTails = start, sandbox.Tails(logs)
	result, err := stored.result()
	if err != nil {
		return nil, err
	}
	call, rejected, repairErr := repaired(ctx, dependencies, stored.Options, result, verdict)
	outcome := outcomeProposed
	if repairErr != nil {
		outcome = outcomeNoRepair
	}
	details := map[string]any{"generation": stored.Generation.ID, "model": call.Model, "prompt_hash": call.PromptHash, "outcome": outcome}
	if err := dependencies.Workspace.Store.Audit(ctx, dependencies.Workspace.Actor, store.ActionProposeRepair, stored.Project.ID, details); err != nil {
		return nil, err
	}
	if repairErr != nil {
		return nil, withRejected(repairErr, rejected)
	}
	return newProposal(stored, result, rejected, mask)
}

// withRejected adds to a repair's error what the model was not allowed to
// change.
func withRejected(err error, rejected []string) error {
	if len(rejected) == 0 {
		return err
	}
	return fmt.Errorf("%w%s%s", err, leftOutText, strings.Join(rejected, rejectedJoin))
}

func newProposal(stored *Stored, result *Result, rejected []string, mask string) (*Proposal, error) {
	proposed := &Stored{Plan: result.Plan, Values: result.Values}
	env, err := proposed.EnvFile(mask)
	if err != nil {
		return nil, err
	}
	hidden, err := proposed.EnvFile(redact.Mask)
	if err != nil {
		return nil, err
	}
	notes := slices.DeleteFunc(slices.Clone(result.Plan.PostInstallNotes), func(note string) bool {
		return slices.Contains(stored.Plan.PostInstallNotes, note)
	})
	sum := sha256.Sum256([]byte(strings.Join([]string{stored.Generation.ID, result.Call.PromptHash, string(result.Files.Compose), string(hidden)}, hashSeparator)))
	return &Proposal{Current: stored, Plan: result.Plan, Compose: result.Files.Compose, Env: env, Report: result.Report, Rejected: rejected, Notes: notes,
		Call: result.Call, Hash: hex.EncodeToString(sum[:]), result: result}, nil
}

// Apply stores the proposal as the next version, made by a repair, unless
// it is refused or a newer version was stored since it was proposed.
func (proposal *Proposal) Apply(ctx context.Context, dependencies Dependencies) error {
	if err := proposal.Refusal(); err != nil {
		return err
	}
	current := proposal.Current
	latest, err := Load(ctx, dependencies.Workspace, current.Project.Name)
	if err != nil {
		return err
	}
	if latest.Generation.ID != current.Generation.ID {
		return ErrProposalStale
	}
	return persist(ctx, dependencies, current.Options, proposal.result, current.Generation.Iteration+1, store.ActionGenerate)
}

// Refusal is why the proposal cannot be applied: it repairs an edited
// version, or the policy gate blocks it.
func (proposal *Proposal) Refusal() error {
	if proposal.Current.Options.Edited {
		return ErrEditedVersion
	}
	if proposal.Report.Blocked() {
		return blockedError(proposal.Current.Project.Name, proposal.Report)
	}
	return nil
}

// StoredVerdict reads the sandbox verdict kept with a version, if it was
// tested.
func StoredVerdict(generation store.Generation) (sandbox.Verdict, bool) {
	var verdict sandbox.Verdict
	if generation.ValidationJSON == "" || json.Unmarshal([]byte(generation.ValidationJSON), &verdict) != nil {
		return verdict, false
	}
	return verdict, verdict.Result != ""
}

// ChangedLine is a line a repair adds or removes: its index among the
// file's lines and its text without indentation.
type ChangedLine struct {
	Index int
	Text  string
}

// ChangedLines are the lines an ordered line diff from before to after
// adds and removes, so a line repeated elsewhere in the file still counts;
// blank and comment lines are left out.
func ChangedLines(before, after string) (added, removed []ChangedLine) {
	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	common := commonTails(beforeLines, afterLines)
	beforeIndex, afterIndex := 0, 0
	for beforeIndex < len(beforeLines) || afterIndex < len(afterLines) {
		bothLeft := beforeIndex < len(beforeLines) && afterIndex < len(afterLines)
		switch {
		case bothLeft && beforeLines[beforeIndex] == afterLines[afterIndex]:
			beforeIndex, afterIndex = beforeIndex+1, afterIndex+1
		case afterIndex < len(afterLines) && (beforeIndex == len(beforeLines) || common[beforeIndex][afterIndex+1] >= common[beforeIndex+1][afterIndex]):
			added = appendChanged(added, afterIndex, afterLines[afterIndex])
			afterIndex++
		default:
			removed = appendChanged(removed, beforeIndex, beforeLines[beforeIndex])
			beforeIndex++
		}
	}
	return added, removed
}

// commonTails[b][a] is the length of the longest common subsequence of
// before's lines from b and after's from a.
// ponytail: quadratic table, fine for files of a few hundred lines; a
// Myers diff if files grow to thousands.
func commonTails(before, after []string) [][]int {
	common := make([][]int, len(before)+1)
	for index := range common {
		common[index] = make([]int, len(after)+1)
	}
	for beforeIndex := len(before) - 1; beforeIndex >= 0; beforeIndex-- {
		for afterIndex := len(after) - 1; afterIndex >= 0; afterIndex-- {
			common[beforeIndex][afterIndex] = max(common[beforeIndex+1][afterIndex], common[beforeIndex][afterIndex+1])
			if before[beforeIndex] == after[afterIndex] {
				common[beforeIndex][afterIndex] = common[beforeIndex+1][afterIndex+1] + 1
			}
		}
	}
	return common
}

func appendChanged(lines []ChangedLine, index int, text string) []ChangedLine {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, commentStart) {
		return lines
	}
	return append(lines, ChangedLine{Index: index, Text: trimmed})
}
