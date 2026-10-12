package llm

import (
	"context"
	"sort"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	// maxLogTail bounds the container output a repair prompt carries.
	maxLogTail      = 8 << 10
	startOutputName = "compose up"
)

// Call records which model answered and a hash of what it was asked.
type Call struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	PromptHash   string `json:"prompt_hash"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// Assistant asks a provider for a draft, gap fills or a repair. What it
// returns is schema-checked but still untrusted: the pipeline merges it with
// precedence and gates it like any other plan.
type Assistant struct {
	provider       Provider
	model          string
	maxInputTokens int
}

func NewAssistant(provider Provider, model string, maxInputTokens int) *Assistant {
	return &Assistant{provider: provider, model: model, maxInputTokens: maxInputTokens}
}

// Draft asks for a whole plan from the facts and the documentation alone.
func (assistant *Assistant) Draft(ctx context.Context, facts *extract.Facts, documents []Document) (*ir.IR, Call, error) {
	redacted, err := facts.Redacted()
	if err != nil {
		return nil, Call{}, err
	}
	return assistant.plan(ctx, draftTask, []block{{"FACTS", redacted}}, documents)
}

// Fill asks only for the listed fields of a rule-built plan, in the small
// fill schema, and returns them laid over the plan as a proposal for
// ir.Merge. A short answer is what keeps a local model quick.
func (assistant *Assistant) Fill(ctx context.Context, plan *ir.IR, fields []string, facts *extract.Facts, documents []Document) (*ir.IR, Call, error) {
	redacted, err := facts.Redacted()
	if err != nil {
		return nil, Call{}, err
	}
	schema, err := ir.FillSchemaJSON()
	if err != nil {
		return nil, Call{}, err
	}
	blocks := []block{{"FILL", fields}, {"RULE_PLAN", fillPlan(plan)}, {"FACTS", fillFactsOf(redacted, fields)}}
	text, call, err := assistant.ask(ctx, fillTask, blocks, documents, schema)
	if err != nil {
		return nil, call, err
	}
	answer, err := ir.DecodeFill([]byte(text))
	if err != nil {
		return nil, call, err
	}
	return answer.Proposal(plan), call, nil
}

// Repair asks, in the small repair schema, what must change after a failed
// sandbox run, and returns it laid over the plan as a proposal for
// ir.MergeRepair.
func (assistant *Assistant) Repair(ctx context.Context, plan *ir.IR, verdict sandbox.Verdict) (*ir.IR, Call, error) {
	summary := map[string]any{"result": verdict.Result, "reason": verdict.Reason, "detail": verdict.Detail,
		"services": verdict.Services, "log_matches": verdict.LogMatches}
	blocks := []block{{"VERDICT", summary}, {"LOGS", repairLogs(verdict)}, {"CURRENT_PLAN", secretFree(plan)}}
	schema, err := ir.RepairSchemaJSON()
	if err != nil {
		return nil, Call{}, err
	}
	text, call, err := assistant.ask(ctx, repairTask, blocks, nil, schema)
	if err != nil {
		return nil, call, err
	}
	answer, err := ir.DecodeRepair([]byte(text))
	if err != nil {
		return nil, call, err
	}
	return answer.Proposal(plan), call, nil
}

// plan asks for a whole plan in the plan schema.
func (assistant *Assistant) plan(ctx context.Context, task string, blocks []block, documents []Document) (*ir.IR, Call, error) {
	text, call, err := assistant.ask(ctx, task, blocks, documents, ir.SchemaJSON())
	if err != nil {
		return nil, call, err
	}
	decoded, err := ir.DecodeModelOutput([]byte(text))
	return decoded, call, err
}

// ask sends one schema-constrained request and returns the answer's text.
func (assistant *Assistant) ask(ctx context.Context, task string, blocks []block, documents []Document, schema []byte) (string, Call, error) {
	user, err := prompt(task, blocks, documents, len(schema), assistant.maxInputTokens)
	if err != nil {
		return "", Call{}, err
	}
	call := Call{Provider: assistant.provider.Name(), Model: assistant.model, PromptHash: promptHash(systemPrompt, user)}
	response, err := assistant.provider.Generate(ctx, Request{System: systemPrompt, Prompt: user, Schema: schema})
	if response.Model != "" {
		call.Model = response.Model
	}
	call.InputTokens, call.OutputTokens = response.InputTokens, response.OutputTokens
	return strings.TrimSpace(response.Text), call, err
}

// repairLogs shares the log budget equally between the output of `compose
// up` and each service's tail, keeping the end of each.
func repairLogs(verdict sandbox.Verdict) map[string]string {
	sources := map[string]string{}
	for service, text := range verdict.LogTails {
		sources[service] = text
	}
	if verdict.StartOutput != "" {
		sources[startOutputName] = verdict.StartOutput
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	logs := map[string]string{}
	if len(names) == 0 {
		return logs
	}
	share := maxLogTail / len(names)
	for _, name := range names {
		text := redact.Text(sources[name])
		logs[name] = text[max(len(text)-share, 0):]
	}
	return logs
}
