// Package llm asks a language model for a deployment plan. The model returns
// the plan as IR JSON constrained by the plan schema; it never writes YAML,
// never sees a secret and has no tools. Every request goes through the
// netguard client for the llm purpose.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/redact"
)

const (
	maxResponseBytes = 8 << 20
	maxErrorBody     = 64 << 10
	maxErrorText     = 300
	contentTypeJSON  = "application/json"
	roleSystem       = "system"
	roleUser         = "user"
	schemaName       = "composewright_plan"
)

var (
	// ErrRefused means the provider declined to answer.
	ErrRefused = errors.New("the model declined the request")
	// ErrTruncated means the answer did not fit the output budget.
	ErrTruncated = errors.New("the model ran out of output tokens")
)

// Request is one schema-constrained completion.
type Request struct {
	System string
	Prompt string
	Schema json.RawMessage
}

type Response struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
}

// Provider is one model API. Anthropic, OpenAI-compatible servers and Ollama
// implement it over plain HTTP, without a vendor SDK.
type Provider interface {
	Generate(ctx context.Context, request Request) (Response, error)
	Name() string
}

// New builds the configured provider; every request it makes goes through
// the guard's llm client.
func New(settings config.LLM, apiKey string, guard *netguard.Guard) (Provider, error) {
	base := strings.TrimRight(settings.BaseURL, "/")
	endpoint := endpoint{guard: guard, apiKey: apiKey}
	switch settings.Provider {
	case config.ProviderAnthropic:
		if apiKey == "" {
			return nil, errors.New("the anthropic provider needs an API key: run `composewright config set-secret llm` or point llm.api_key_ref at env:ANTHROPIC_API_KEY")
		}
		return &anthropic{endpoint: endpoint, url: base + anthropicPath, model: settings.Model}, nil
	case config.ProviderOpenAICompatible:
		return &openAICompatible{endpoint: endpoint, url: base + openAIPath, model: settings.Model}, nil
	case config.ProviderOllama:
		return &ollama{endpoint: endpoint, url: base + ollamaPath, model: settings.Model}, nil
	}
	return nil, fmt.Errorf("llm.provider %q does not call a model", settings.Provider)
}

// endpoint posts JSON and keeps the API key out of every error it returns.
type endpoint struct {
	guard  *netguard.Guard
	apiKey string
}

func (endpoint endpoint) post(ctx context.Context, url string, headers map[string]string, body, reply any) error {
	response, err := endpoint.open(ctx, url, headers, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := netguard.ReadBody(response, maxResponseBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, reply); err != nil {
		return fmt.Errorf("%s returned an unreadable answer: %w", response.Request.URL.Host, err)
	}
	return nil
}

// open sends a JSON request and returns the response once the provider has
// accepted it; the caller reads and closes the body.
func (endpoint endpoint) open(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentTypeJSON)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := endpoint.guard.Client(netguard.LLM).Do(request)
	if err != nil {
		return nil, errors.New(endpoint.scrub(err.Error()))
	}
	if response.StatusCode == http.StatusOK {
		return response, nil
	}
	defer response.Body.Close()
	data, err := netguard.ReadBody(response, maxErrorBody)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s answered %s: %s", request.URL.Host, response.Status, endpoint.scrub(string(data)))
}

func (endpoint endpoint) scrub(text string) string {
	text = redact.Text(redact.Values(text, []string{endpoint.apiKey}))
	runes := []rune(strings.TrimSpace(text))
	return string(runes[:min(len(runes), maxErrorText)])
}
