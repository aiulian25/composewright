package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aiulian25/composewright/internal/pipeline"
)

const (
	// proposalField carries the hash of the proposal shown, so Apply
	// stores only that one.
	proposalField = "proposal"
	repairTitle   = "Repair of "
	repairCLI     = "composewright validate %s --repair"
	noModelHint   = " Set one up in Settings › Models."
	// proposalLifetime is how long a shown proposal can still be applied.
	proposalLifetime = 30 * time.Minute
	// proposalKeySeparator keeps a person's name apart from a hash.
	proposalKeySeparator = "\x00"
)

var (
	// errProposalGone means the proposal posted is not one kept for this
	// person: it was applied already, expired, or never shown.
	errProposalGone = errors.New("this proposal is no longer kept; ask the model again")
	// errModelUnavailable is a configured model that could not be set up.
	errModelUnavailable = errors.New("the model could not be set up")
	// repairRefusals are the reasons shown on the repair page; any other
	// error is the server's.
	repairRefusals = []error{pipeline.ErrNoRepairer, pipeline.ErrNothingToRepair, pipeline.ErrNoRepair, pipeline.ErrEditedVersion,
		pipeline.ErrProposalStale, pipeline.ErrBlocked, errProposalGone, errModelUnavailable}
)

// repairView is a model's proposed repair of a failed run: compose.yaml
// and .env with its changed lines marked M, and what the gate says.
type repairView struct {
	Project string
	Found   bool
	Hash    string
	Model   string
	Version int
	// Edited is a version a person wrote, which a repair cannot keep.
	Edited     bool
	Blocked    bool
	PolicyText string
	PolicyTone string
	Findings   []findingView
	Rejected   []string
	Notes      []string
	Compose    []badgedLine
	Env        []badgedLine
	Removed    []string
}

// shownProposal is a proposal as one person was shown it.
type shownProposal struct {
	proposal *pipeline.Proposal
	shown    time.Time
}

// shownProposals keeps the proposals shown, by person and hash, so Apply
// stores exactly what was shown without asking the model again.
type shownProposals struct {
	mutex sync.Mutex
	kept  map[string]shownProposal
}

func proposalKey(username, hash string) string {
	return username + proposalKeySeparator + hash
}

// keep remembers a shown proposal and forgets those that expired.
func (proposals *shownProposals) keep(username string, proposal *pipeline.Proposal, now time.Time) {
	proposals.mutex.Lock()
	defer proposals.mutex.Unlock()
	if proposals.kept == nil {
		proposals.kept = map[string]shownProposal{}
	}
	for key, shown := range proposals.kept {
		if now.Sub(shown.shown) > proposalLifetime {
			delete(proposals.kept, key)
		}
	}
	proposals.kept[proposalKey(username, proposal.Hash)] = shownProposal{proposal: proposal, shown: now}
}

// take hands out a kept proposal of the project once: a second Apply of
// it finds nothing.
func (proposals *shownProposals) take(username, name, hash string, now time.Time) (*pipeline.Proposal, bool) {
	proposals.mutex.Lock()
	defer proposals.mutex.Unlock()
	key := proposalKey(username, hash)
	shown, found := proposals.kept[key]
	delete(proposals.kept, key)
	usable := found && now.Sub(shown.shown) <= proposalLifetime && shown.proposal.Current.Project.Name == name
	return shown.proposal, usable
}

// proposeRepair asks the model how to repair the latest failed run and
// shows the proposal; nothing is stored.
func (server *Server) proposeRepair(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx, name, username := request.Context(), request.PathValue("name"), current.user.Username
	dependencies, err := server.repairDependencies(ctx, username)
	if err != nil {
		return server.renderRepair(writer, request, current, name, nil, err)
	}
	proposal, err := pipeline.ProposeRepair(ctx, dependencies, name, webMask)
	if err != nil {
		return server.renderRepair(writer, request, current, name, nil, err)
	}
	server.proposals.keep(username, proposal, server.options.Now())
	return server.renderRepair(writer, request, current, name, proposal, nil)
}

// applyRepair stores the proposal shown as a new version and starts its
// test run; the model is not asked again.
func (server *Server) applyRepair(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx, name, username := request.Context(), request.PathValue("name"), current.user.Username
	if err := server.busy(name); err != nil {
		return err
	}
	proposal, found := server.proposals.take(username, name, request.PostFormValue(proposalField), server.options.Now())
	if !found {
		return server.renderRepair(writer, request, current, name, nil, errProposalGone)
	}
	dependencies := pipeline.Dependencies{Config: server.config(), Workspace: server.as(username)}
	if err := proposal.Apply(ctx, dependencies); err != nil {
		return server.renderRepair(writer, request, current, name, proposalUnlessStale(proposal, err), err)
	}
	return server.startValidation(writer, request, current, name)
}

// proposalUnlessStale is the proposal to show again with a refusal; one
// for an older version is not.
func proposalUnlessStale(proposal *pipeline.Proposal, err error) *pipeline.Proposal {
	if errors.Is(err, pipeline.ErrProposalStale) {
		return nil
	}
	return proposal
}

// repairDependencies connect the configured model as the repairer, whatever
// engine made the project; a model that cannot be set up says why.
func (server *Server) repairDependencies(ctx context.Context, username string) (pipeline.Dependencies, error) {
	connection, err := server.connect(ctx, username, pipeline.EngineAssisted, false)
	if err != nil {
		return connection.Dependencies, err
	}
	if connection.Dependencies.Repairer == nil && connection.Note != "" {
		return connection.Dependencies, fmt.Errorf("%w: %s", errModelUnavailable, connection.Note)
	}
	return connection.Dependencies, nil
}

// renderRepair shows a proposal, a refusal, or both: a proposal that
// cannot be applied. An error that is no refusal is the server's.
func (server *Server) renderRepair(writer http.ResponseWriter, request *http.Request, current *visit, name string, proposal *pipeline.Proposal,
	problem error,
) error {
	refused := slices.ContainsFunc(repairRefusals, func(refusal error) bool { return errors.Is(problem, refusal) })
	if problem != nil && !refused {
		return problem
	}
	view := repairView{Project: name}
	if proposal != nil {
		var err error
		if view, err = proposalView(proposal); err != nil {
			return err
		}
	}
	page := server.page(request, current, repairTitle+name).flow(stepValidate).of(name)
	page.Data, page.CLI = view, fmt.Sprintf(repairCLI, name)
	if problem == nil {
		return server.render(writer, http.StatusOK, "repair", page)
	}
	page.Error = capitalised(problem.Error()) + "."
	if errors.Is(problem, pipeline.ErrNoRepairer) {
		page.Error += noModelHint
	}
	return server.render(writer, http.StatusConflict, "repair", page)
}

func proposalView(proposal *pipeline.Proposal) (repairView, error) {
	stored := proposal.Current
	currentEnv, err := stored.EnvFile(webMask)
	if err != nil {
		return repairView{}, err
	}
	compose, env := string(proposal.Compose), string(proposal.Env)
	addedCompose, removedCompose := pipeline.ChangedLines(stored.Generation.ComposeYAML, compose)
	addedEnv, removedEnv := pipeline.ChangedLines(string(currentEnv), env)
	removed := make([]string, 0, len(removedCompose)+len(removedEnv))
	for _, line := range append(removedCompose, removedEnv...) {
		removed = append(removed, line.Text)
	}
	view := repairView{Project: stored.Project.Name, Found: true, Hash: proposal.Hash, Model: proposal.Call.Model,
		Version: stored.Generation.Number + 1, Edited: stored.Options.Edited, Blocked: proposal.Report.Blocked(),
		PolicyText: policyText(proposal.Report), PolicyTone: tonesFor("", proposal.Report).Policy, Findings: findingViews(proposal.Report, compose),
		Rejected: proposal.Rejected, Notes: proposal.Notes, Removed: removed,
		Compose: modelLines(composeLines(proposal.Plan, compose, "", nil), addedCompose),
		Env:     modelLines(plainLines(env, languageEnv), addedEnv)}
	return view, nil
}

// modelLines badges with M only the lines this repair added: a line the
// model wrote in an earlier version loses its M.
func modelLines(lines []badgedLine, added []pipeline.ChangedLine) []badgedLine {
	for index := range lines {
		if lines[index].Badge == badgeModel {
			lines[index].Badge, lines[index].Tone = "", ""
		}
	}
	for _, line := range added {
		if line.Index < len(lines) {
			lines[line.Index].Badge, lines[line.Index].Tone, lines[line.Index].Marked = badgeModel, badgeTones[badgeModel], true
		}
	}
	return lines
}
