package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	testKey       = "sk-test-0123456789abcdef"
	minifluxFiles = "../../testdata/corpus/miniflux/files"
	minifluxPlan  = "../../testdata/corpus/miniflux/expected.ir.json"
)

type captured struct {
	path    string
	headers http.Header
	body    map[string]any
}

// replay serves recorded reply envelopes in order, with the plan put where
// the fixture says __PLAN__, and records every request.
type replay struct {
	mutex    sync.Mutex
	requests []captured
	replies  []func() (int, string)
}

func (server *replay) handle(writer http.ResponseWriter, request *http.Request) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	data, _ := io.ReadAll(request.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	server.requests = append(server.requests, captured{path: request.URL.Path, headers: request.Header.Clone(), body: body})
	status, reply := server.replies[min(len(server.requests), len(server.replies))-1]()
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, reply)
}

func start(t *testing.T, replies ...func() (int, string)) (*replay, *httptest.Server) {
	t.Helper()
	recorder := &replay{replies: replies}
	server := httptest.NewServer(http.HandlerFunc(recorder.handle))
	t.Cleanup(server.Close)
	return recorder, server
}

func fixture(t *testing.T, name, plan string) func() (int, string) {
	t.Helper()
	envelope, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(plan)
	text := strings.Replace(string(envelope), `"__PLAN__"`, string(quoted), 1)
	return func() (int, string) { return http.StatusOK, text }
}

func reply(status int, text string) func() (int, string) {
	return func() (int, string) { return status, text }
}

func planText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(minifluxPlan)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func facts(t *testing.T) *extract.Facts {
	t.Helper()
	extracted, err := extract.Extract(context.Background(), minifluxFiles)
	if err != nil {
		t.Fatal(err)
	}
	return extracted
}

// guardFor allows exactly the test server, as PolicyFromConfig allows a
// configured model host.
func guardFor(t *testing.T, server *httptest.Server) *netguard.Guard {
	t.Helper()
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.LLM: {"127.0.0.1"}},
		PrivateTargets: []string{strings.TrimPrefix(server.URL, "http://")}})
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func assistantFor(t *testing.T, provider, url string, guard *netguard.Guard) *Assistant {
	t.Helper()
	settings := config.LLM{Provider: provider, BaseURL: url, Model: "configured-model", MaxInputTokens: 60000}
	built, err := New(settings, testKey, guard)
	if err != nil {
		t.Fatal(err)
	}
	return NewAssistant(built, settings.Model, settings.MaxInputTokens)
}

func field(body map[string]any, path ...string) any {
	var value any = body
	for _, key := range path {
		object, _ := value.(map[string]any)
		value = object[key]
	}
	return value
}

func TestAnthropicRequestShape(t *testing.T) {
	recorder, server := start(t, fixture(t, "anthropic.json", planText(t)))
	plan, call, err := assistantFor(t, config.ProviderAnthropic, server.URL, guardFor(t, server)).Draft(context.Background(), facts(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := recorder.requests[0]
	checks := map[string]bool{
		"path":             request.path == "/v1/messages",
		"api key header":   request.headers.Get("x-api-key") == testKey,
		"version header":   request.headers.Get("anthropic-version") == anthropicVersion,
		"fallback beta":    request.headers.Get("anthropic-beta") == anthropicFallbackBeta,
		"fallbacks":        request.body["fallbacks"] == anthropicFallbacks,
		"model":            request.body["model"] == "configured-model",
		"max tokens":       request.body["max_tokens"] == float64(anthropicMaxTokens),
		"schema format":    field(request.body, "output_config", "format", "type") == formatJSONSchema,
		"schema attached":  field(request.body, "output_config", "format", "schema", "$defs") != nil,
		"system prompt":    strings.Contains(request.body["system"].(string), "UNTRUSTED"),
		"no thinking set":  request.body["thinking"] == nil,
		"plan decoded":     plan.App.Name == "miniflux",
		"answering model":  call.Model == "claude-opus-5-5",
		"prompt hash":      len(call.PromptHash) == 64,
		"token usage":      call.InputTokens == 18234 && call.OutputTokens == 2950,
		"provider in call": call.Provider == config.ProviderAnthropic,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
}

func TestAnthropicStopReasons(t *testing.T) {
	truncated := `{"model":"claude-opus-5-5","stop_reason":"max_tokens","content":[{"type":"text","text":"{\"ir_version\""}]}`
	recorder, server := start(t, reply(http.StatusOK, truncated), fixture(t, "anthropic.json", planText(t)))
	if _, _, err := assistantFor(t, config.ProviderAnthropic, server.URL, guardFor(t, server)).Draft(context.Background(), facts(t), nil); err != nil {
		t.Fatalf("the retry with a larger budget failed: %v", err)
	}
	if len(recorder.requests) != 2 || recorder.requests[1].body["max_tokens"] != float64(anthropicRetryMaxTokens) {
		t.Fatalf("no retry with %d tokens: %d requests", anthropicRetryMaxTokens, len(recorder.requests))
	}
	_, refusing := start(t, reply(http.StatusOK, `{"model":"claude-opus-5-5","stop_reason":"refusal","content":[]}`))
	if _, _, err := assistantFor(t, config.ProviderAnthropic, refusing.URL, guardFor(t, refusing)).Draft(context.Background(), facts(t), nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("refusal: %v", err)
	}
	_, rejecting := start(t, reply(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key `+testKey+`"}}`))
	_, _, err := assistantFor(t, config.ProviderAnthropic, rejecting.URL, guardFor(t, rejecting)).Draft(context.Background(), facts(t), nil)
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error leaks the key or hides the status: %v", err)
	}
	if _, err := New(config.LLM{Provider: config.ProviderAnthropic}, "", nil); err == nil {
		t.Fatal("anthropic without a key was accepted")
	}
}

func TestOpenAICompatibleRequestShape(t *testing.T) {
	recorder, server := start(t, fixture(t, "openai.json", planText(t)))
	plan, call, err := assistantFor(t, config.ProviderOpenAICompatible, server.URL+"/v1", guardFor(t, server)).Draft(context.Background(), facts(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := recorder.requests[0]
	messages, _ := request.body["messages"].([]any)
	checks := map[string]bool{
		"path":          request.path == "/v1/chat/completions",
		"bearer":        request.headers.Get("Authorization") == "Bearer "+testKey,
		"strict schema": field(request.body, "response_format", "json_schema", "strict") == true,
		"schema type":   field(request.body, "response_format", "type") == formatJSONSchema,
		"two messages":  len(messages) == 2,
		"plan decoded":  plan.App.Name == "miniflux",
		"usage":         call.InputTokens == 17120,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	for text, want := range map[string]error{
		`{"choices":[{"finish_reason":"stop","message":{"content":"","refusal":"I can't help with that."}}]}`: ErrRefused,
		`{"choices":[{"finish_reason":"length","message":{"content":"{"}}]}`:                                  ErrTruncated,
	} {
		_, failing := start(t, reply(http.StatusOK, text))
		if _, _, err := assistantFor(t, config.ProviderOpenAICompatible, failing.URL, guardFor(t, failing)).Draft(context.Background(), facts(t), nil); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", text, err, want)
		}
	}
}

func TestOllamaRequestShape(t *testing.T) {
	recorder, server := start(t, fixture(t, "ollama.ndjson", planText(t)))
	if _, _, err := assistantFor(t, config.ProviderOllama, server.URL, guardFor(t, server)).Draft(context.Background(), facts(t), nil); err != nil {
		t.Fatal(err)
	}
	request := recorder.requests[0]
	messages, _ := request.body["messages"].([]any)
	user, _ := messages[1].(map[string]any)
	checks := map[string]bool{
		"path":             request.path == "/api/chat",
		"streamed":         request.body["stream"] == true,
		"temperature 0":    field(request.body, "options", "temperature") == float64(0),
		"context window":   field(request.body, "options", "num_ctx").(float64) > float64(ollamaOutputTokens),
		"format is schema": field(request.body, "format", "$defs") != nil,
		"schema in prompt": strings.Contains(user["content"].(string), strings.TrimSpace(ollamaSchemaIntro)),
		"no auth header":   request.headers.Get("Authorization") == "",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	for text, want := range map[string]string{
		`{"model":"qwen2.5-coder:14b","message":{"content":"{"},"done":true,"done_reason":"length"}`:            ErrTruncated.Error(),
		`{"message":{"content":"{"},"done":false}` + "\n" + `{"error":"model runner has unexpectedly stopped"}`: "unexpectedly stopped",
		`{"message":{"content":"{"},"done":false}`:                                                              errStreamEnded.Error(),
	} {
		_, failing := start(t, reply(http.StatusOK, text))
		_, _, err := assistantFor(t, config.ProviderOllama, failing.URL, guardFor(t, failing)).Draft(context.Background(), facts(t), nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("stream %q: got %v, want %q", text, err, want)
		}
	}
}

func TestRequestsGoThroughNetguard(t *testing.T) {
	_, server := start(t, fixture(t, "ollama.ndjson", planText(t)))
	if _, _, err := assistantFor(t, config.ProviderOllama, server.URL, guardFor(t, server)).Draft(context.Background(), facts(t), nil); err != nil {
		t.Fatalf("allowed model host refused: %v", err)
	}
	other, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.LLM: {"api.anthropic.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := assistantFor(t, config.ProviderOllama, server.URL, other).Draft(context.Background(), facts(t), nil); err == nil {
		t.Fatal("a host outside the llm allowlist was contacted")
	}
}

// recording is a provider that keeps the request and answers with a fixed
// text, or with an empty fill answer when it is asked in the fill schema.
type recording struct {
	request Request
	answer  string
}

const (
	emptyFill   = `{"summary":"","services":[],"env_file":[],"post_install_notes":[],"suspicious_instructions":[],"confidence":"low"}`
	emptyRepair = `{"services":[],"env_file":[],"post_install_notes":[],"confidence":"low"}`
)

func (provider *recording) Name() string { return "recording" }

func (provider *recording) Generate(_ context.Context, request Request) (Response, error) {
	provider.request = request
	fill, _ := ir.FillSchemaJSON()
	repair, _ := ir.RepairSchemaJSON()
	switch string(request.Schema) {
	case string(fill):
		return Response{Text: emptyFill}, nil
	case string(repair):
		return Response{Text: emptyRepair}, nil
	}
	return Response{Text: provider.answer}, nil
}

func TestPromptsCarryNoSecrets(t *testing.T) {
	repository := t.TempDir()
	files := map[string]string{
		"README.md":   "# Demo\n\nSet `API_TOKEN=tok_live_abcdef123456` and log in with admin:CorrectHorse99.\n\n## Install with Docker\n\n```sh\ndocker run -e SECRET_KEY=sk_live_zyxwvu987654 -p 8000:8000 demo/app:1.0\n```\n",
		".env.sample": "SECRET_KEY=sk_live_zyxwvu987654\nDATABASE_URL=postgres://demo:DbPassw0rd!@db/demo\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extracted, err := extract.Extract(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ir.IR{IRVersion: ir.Version, App: ir.App{Name: "demo"},
		Services: []ir.Service{{Name: "app", Role: ir.RoleApp, Env: []ir.EnvRef{{Key: "SESSION_SECRET", Literal: "Sess10nSecretValue", Secret: true}}}},
		EnvFile: []ir.EnvFileEntry{{Key: "SECRET_KEY", Value: "sk_live_zyxwvu987654", Secret: true},
			{Key: "DATABASE_URL", Value: "postgres://demo:DbPassw0rd!@db/demo"}, {Key: "DB_ALIAS", Value: "${SECRET_KEY}"}}}
	provider := &recording{answer: planText(t)}
	assistant := NewAssistant(provider, "model", 60000)
	documents := []Document{{Path: "README.md", Text: files["README.md"]}}
	if _, _, err := assistant.Fill(context.Background(), plan, []string{"app.summary"}, extracted, documents); err != nil {
		t.Fatal(err)
	}
	sent := provider.request.System + provider.request.Prompt
	for _, secret := range []string{"tok_live_abcdef123456", "sk_live_zyxwvu987654", "DbPassw0rd", "Sess10nSecretValue"} {
		if strings.Contains(sent, secret) {
			t.Errorf("the prompt contains %q", secret)
		}
	}
	for _, want := range []string{`${SECRET_KEY}`, "FILL:\n", "RULE_PLAN:\n", "FACTS:\n", "REPO_CONTENT:\n"} {
		if !strings.Contains(sent, want) {
			t.Errorf("%q missing from the prompt", want)
		}
	}
	verdict := sandbox.Verdict{Result: sandbox.ResultFail, Reason: sandbox.ReasonCrash,
		LogTails: map[string]string{"app": strings.Repeat("noise\n", 4000) + "app-1 | password=Hunter2Hunter2 rejected\n"}}
	if _, _, err := assistant.Repair(context.Background(), plan, verdict); err != nil {
		t.Fatal(err)
	}
	repairPrompt := provider.request.Prompt
	if strings.Contains(repairPrompt, "Hunter2Hunter2") || strings.Contains(repairPrompt, "Sess10nSecretValue") || len(repairPrompt) > maxLogTail+len(planText(t))*2 {
		t.Errorf("the repair prompt leaks a secret or carries too much log (%d bytes)", len(repairPrompt))
	}
}

// TestBudgetCountsTheAnswerSchema: the README gets what the system prompt
// and the schema actually sent leave, and a prompt whose facts and plan
// alone exceed the budget is refused instead of sent.
func TestBudgetCountsTheAnswerSchema(t *testing.T) {
	const schemaRoom, smallSchema = 2000, 500
	documents := []Document{{Path: "README.md", Text: "# App\n" + strings.Repeat("intro ", 100)}}
	tokens := (len(systemPrompt) + schemaRoom) / charactersPerToken
	small, err := prompt("TASK", nil, documents, smallSchema, tokens)
	if err != nil || !strings.Contains(small, "intro intro") {
		t.Fatalf("a small schema should leave room for the README: %v\n%s", err, small)
	}
	if _, err := prompt("TASK", nil, documents, schemaRoom, tokens); err == nil || !strings.Contains(err.Error(), "llm.max_input_tokens") {
		t.Fatalf("an over-budget prompt was not refused: %v", err)
	}
}

func TestSectionsKeepDeploymentFirst(t *testing.T) {
	readme := "# App\nShort intro.\n\n## Features\n" + strings.Repeat("feature text ", 400) +
		"\n## Installation with Docker\n```sh\n# not a heading\ndocker run app\n```\n\n## Contributing\n" + strings.Repeat("contrib ", 400)
	content := selectSections([]Document{{Path: "README.md", Text: readme}}, 400)
	if len(content.Files) != 1 || content.OmittedSections != 2 {
		t.Fatalf("selection: %+v", content)
	}
	sections := content.Files[0].Sections
	if len(sections) != 2 || !strings.HasPrefix(sections[0], "# App") || !strings.Contains(sections[1], "# not a heading") {
		t.Fatalf("sections: %q", sections)
	}
}

func TestHostileOutputIsRejected(t *testing.T) {
	privileged := strings.Replace(planText(t), `"role": "app",`, `"role": "app", "privileged": true,`, 1)
	for name, answer := range map[string]string{
		"privileged key": privileged,
		"yaml":           "services:\n  app:\n    image: evil/miner\n",
		"missing field":  `{"ir_version":"1"}`,
		"prose":          "Here is your plan: {}",
	} {
		provider := &recording{answer: answer}
		if _, _, err := NewAssistant(provider, "model", 60000).Draft(context.Background(), facts(t), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
