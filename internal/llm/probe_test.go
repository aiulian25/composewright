package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
)

const probeModel = "qwen2.5-coder:7b"

// probeGuard allows the connection under test as PolicyFromConfig does for
// a configuration whose llm block is that connection.
func probeGuard(t *testing.T, settings config.LLM) *netguard.Guard {
	t.Helper()
	configuration := config.Default()
	configuration.LLM = settings
	policy, err := netguard.PolicyFromConfig(configuration, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	guard, err := netguard.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

// TestProbe: a working connection reports the model that answered, its
// tokens and the time, after one schema-constrained request; a refused one
// returns the provider's error without the key.
func TestProbe(t *testing.T) {
	answer := `{"model":"` + probeModel + `","choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":23,"completion_tokens":4}}`
	recorder, server := start(t, reply(http.StatusOK, answer))
	settings := config.LLM{Provider: config.ProviderOpenAICompatible, BaseURL: server.URL + "/v1", Model: probeModel, MaxInputTokens: 8000}
	result, err := Probe(context.Background(), settings, testKey, probeGuard(t, settings))
	if err != nil {
		t.Fatal(err)
	}
	summary := result.Summary()
	if !strings.HasPrefix(summary, "Reachable · answered as "+probeModel+" · 23 in / 4 out tokens · ") || !strings.HasSuffix(summary, " s") {
		t.Fatalf("summary: %s", summary)
	}
	request := recorder.requests[0]
	required, _ := field(request.body, "response_format", "json_schema", "schema", "required").([]any)
	if len(recorder.requests) != 1 || len(required) != 1 || required[0] != "ok" {
		t.Fatalf("not one request with the ok schema: %+v", recorder.requests)
	}
	_, refusing := start(t, reply(http.StatusNotFound, `{"error":"model `+probeModel+` not found for key `+testKey+`"}`))
	settings.BaseURL = refusing.URL + "/v1"
	_, err = Probe(context.Background(), settings, testKey, probeGuard(t, settings))
	if err == nil || !strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), testKey) {
		t.Fatalf("refused probe: %v", err)
	}
}
