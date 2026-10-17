package llm

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	probeTimeout = 30 * time.Second
	probeSystem  = `Answer with the JSON {"ok": true}.`
	probePrompt  = "ping"
	probeSummary = "Reachable · answered as %s · %d in / %d out tokens · %.1f s"
)

var probeSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)

// ProbeResult is what one test request to a model found.
type ProbeResult struct {
	Response
	Elapsed time.Duration
}

// Summary says who answered, how many tokens it took and how long.
func (result ProbeResult) Summary() string {
	return fmt.Sprintf(probeSummary, result.Model, result.InputTokens, result.OutputTokens, result.Elapsed.Seconds())
}

// Probe sends one tiny schema-constrained request to check that a
// connection works: the address, the model name and the key. Its errors
// never contain the key.
func Probe(ctx context.Context, settings config.LLM, apiKey string, guard *netguard.Guard) (ProbeResult, error) {
	provider, err := New(settings, apiKey, guard)
	if err != nil {
		return ProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	started := time.Now()
	response, err := provider.Generate(ctx, Request{System: probeSystem, Prompt: probePrompt, Schema: probeSchema})
	if err != nil {
		return ProbeResult{}, err
	}
	response.Model = cmp.Or(response.Model, settings.Model)
	return ProbeResult{Response: response, Elapsed: time.Since(started)}, nil
}
