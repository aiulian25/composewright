package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	failedLog        = "miniflux-1  | 2026-10-10T08:15:02.123456789Z level=error msg=\"listen tcp: address already in use\"\n"
	testMask         = "<hidden>"
	repairModel      = "repairer"
	repairPromptHash = "hash"
)

// commandRepairer proposes command for miniflux, besides fakeRepairer's
// extra healthcheck retry.
func commandRepairer(command ...string) *fakeRepairer {
	return &fakeRepairer{mutate: func(plan *ir.IR) {
		service, _ := plan.Service("miniflux")
		service.Command = command
	}}
}

// failedProject generates miniflux and runs it once without a repairer,
// so the run fails and is left for a person.
func failedProject(t *testing.T) Dependencies {
	t.Helper()
	dependencies := testDependencies(t, &scriptedDocker{states: []string{unhealthy}, logs: failedLog})
	generated(t, dependencies)
	if _, err := ApproveAndValidate(context.Background(), dependencies, "miniflux", "test"); err != nil {
		t.Fatal(err)
	}
	if got := status(t, dependencies); got != store.StatusNeedsHuman {
		t.Fatalf("status %s after the failed run", got)
	}
	return dependencies
}

// TestProposeAndApplyRepair: a proposal is rendered from the kept verdict
// and logs, audited and not stored; Apply stores it as the next version,
// and a proposal for a version that is no longer the latest is refused.
func TestProposeAndApplyRepair(t *testing.T) {
	ctx := context.Background()
	dependencies := failedProject(t)
	if _, err := ProposeRepair(ctx, dependencies, "miniflux", testMask); !errors.Is(err, ErrNoRepairer) {
		t.Fatalf("without a model: %v", err)
	}
	repairer := commandRepairer("miniflux", "-listen", ":8081")
	dependencies.Repairer = repairer
	before, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ProposeRepair(ctx, dependencies, "miniflux", testMask)
	if err != nil {
		t.Fatal(err)
	}
	if repairer.verdict.Reason != sandbox.ReasonUnhealthy || !strings.Contains(repairer.verdict.LogTails["miniflux"], "address already in use") {
		t.Fatalf("the repair was not asked about the kept run: %+v", repairer.verdict)
	}
	added, _ := ChangedLines(before.Generation.ComposeYAML, string(proposal.Compose))
	if !slices.ContainsFunc(added, func(line ChangedLine) bool { return strings.Contains(line.Text, "-listen") }) {
		t.Fatalf("the new command is not among the changed lines: %+v", added)
	}
	if len(before.MaskedKeys()) == 0 {
		t.Fatal("miniflux has no secret to mask")
	}
	for _, key := range before.MaskedKeys() {
		if strings.Contains(string(proposal.Env), before.Values[key]) {
			t.Fatalf("the proposed .env shows the secret %s", key)
		}
	}
	if after, _ := Load(ctx, dependencies.Workspace, "miniflux"); after.Generation.ID != before.Generation.ID {
		t.Fatal("a proposal was stored")
	}
	if proposals := repairEntries(t, dependencies, before.Project.ID, outcomeProposed); proposals != 1 {
		t.Fatalf("%d proposals audited with their prompt hash, want 1", proposals)
	}
	again, err := ProposeRepair(ctx, dependencies, "miniflux", testMask)
	if err != nil {
		t.Fatal(err)
	}
	if err := proposal.Apply(ctx, dependencies); err != nil {
		t.Fatal(err)
	}
	if err := again.Apply(ctx, dependencies); !errors.Is(err, ErrProposalStale) {
		t.Fatalf("a second apply for the same version: %v", err)
	}
	after, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation.Number != before.Generation.Number+1 || after.Generation.Iteration != before.Generation.Iteration+1 ||
		!strings.Contains(after.Generation.ComposeYAML, "-listen") {
		t.Fatalf("applied version %d iteration %d:\n%s", after.Generation.Number, after.Generation.Iteration, after.Generation.ComposeYAML)
	}
	if _, err := ProposeRepair(ctx, dependencies, "miniflux", testMask); !errors.Is(err, ErrNothingToRepair) {
		t.Fatalf("an untested version: %v", err)
	}
}

// repairEntries counts the project's propose_repair audit entries with
// outcome, the fake model and its prompt hash.
func repairEntries(t *testing.T, dependencies Dependencies, projectID, outcome string) int {
	t.Helper()
	entries, err := dependencies.Workspace.Store.AuditPage(context.Background(), store.AuditFilter{ProjectID: projectID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Action == store.ActionProposeRepair && entry.Details["outcome"] == outcome &&
			entry.Details["prompt_hash"] == repairPromptHash && entry.Details["model"] == repairModel {
			count++
		}
	}
	return count
}

// TestAnAnswerThatIsNoRepair: an answer that changes only what a repair
// may not change is audited and says what was left out.
func TestAnAnswerThatIsNoRepair(t *testing.T) {
	dependencies := failedProject(t)
	dependencies.Repairer = &fakeRepairer{mutate: func(plan *ir.IR) {
		plan.Services[0].Healthcheck.Retries--
		plan.Services[0].Image.Repository = "evil/miner"
	}}
	_, err := ProposeRepair(context.Background(), dependencies, "miniflux", testMask)
	if !errors.Is(err, ErrNoRepair) || !strings.Contains(err.Error(), "left out: ") {
		t.Fatalf("an image swap: %v", err)
	}
	stored, _ := Load(context.Background(), dependencies.Workspace, "miniflux")
	if unused := repairEntries(t, dependencies, stored.Project.ID, outcomeNoRepair); unused != 1 {
		t.Fatalf("%d answers that are no repair audited, want 1", unused)
	}
}

// TestChangedLines: a changed line is found by its place, also when the
// same text is elsewhere in the file.
func TestChangedLines(t *testing.T) {
	before := "a:\n  retries: 3\nb:\n  retries: 10\n"
	after := "a:\n  retries: 10\n  # why\nb:\n  retries: 10\n"
	added, removed := ChangedLines(before, after)
	if !slices.Equal(added, []ChangedLine{{Index: 1, Text: "retries: 10"}}) || !slices.Equal(removed, []ChangedLine{{Index: 1, Text: "retries: 3"}}) {
		t.Fatalf("added %+v, removed %+v", added, removed)
	}
}

// TestRepairOfAnEditedVersion: an edited version that failed shows a
// proposal, which cannot be applied.
func TestRepairOfAnEditedVersion(t *testing.T) {
	ctx := context.Background()
	dependencies := failedProject(t)
	stored, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EditCompose(ctx, dependencies, "miniflux", []byte(stored.Generation.ComposeYAML+"# mine\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Revalidate(ctx, dependencies, "miniflux"); err != nil {
		t.Fatal(err)
	}
	dependencies.Repairer = commandRepairer("miniflux", "-debug")
	proposal, err := ProposeRepair(ctx, dependencies, "miniflux", testMask)
	if err != nil {
		t.Fatal(err)
	}
	if err := proposal.Apply(ctx, dependencies); !errors.Is(err, ErrEditedVersion) {
		t.Fatalf("an apply to an edited version: %v", err)
	}
}

// repairModelProvider answers a repair with one note and records the
// prompts it was sent.
type repairModelProvider struct {
	prompts []string
}

func (provider *repairModelProvider) Name() string { return "scripted" }

func (provider *repairModelProvider) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	provider.prompts = append(provider.prompts, request.System+request.Prompt)
	return llm.Response{Text: `{"services":[],"env_file":[],"post_install_notes":["Give the database a minute."],"confidence":"low"}`}, nil
}

// TestRepairPromptKeepsSecrets: the real assistant's repair prompt carries
// no stored secret, and the same failed run gives the same proposal hash.
func TestRepairPromptKeepsSecrets(t *testing.T) {
	ctx := context.Background()
	dependencies := failedProject(t)
	provider := &repairModelProvider{}
	dependencies.Repairer = llm.NewAssistant(provider, "configured", 60000)
	first, err := ProposeRepair(ctx, dependencies, "miniflux", testMask)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProposeRepair(ctx, dependencies, "miniflux", testMask)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.Call.PromptHash == "" {
		t.Fatalf("the same run proposed %s, then %s", first.Hash, second.Hash)
	}
	if len(first.Notes) != 1 || !strings.Contains(provider.prompts[0], "address already in use") {
		t.Fatalf("notes %q; the prompt lacks the logs", first.Notes)
	}
	stored := first.Current
	for _, entry := range stored.Plan.EnvFile {
		if value := stored.Values[entry.Key]; entry.Secret && len(value) >= minimumSecret && strings.Contains(provider.prompts[0], value) {
			t.Fatalf("the prompt carries the secret %s", entry.Key)
		}
	}
}
