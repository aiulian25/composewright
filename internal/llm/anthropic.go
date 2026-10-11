package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Anthropic Messages API with structured outputs: output_config.format holds
// the plan schema, and a declined request is retried server-side on the
// model Anthropic recommends (fallbacks "default").
const (
	anthropicPath           = "/v1/messages"
	anthropicVersion        = "2023-06-01"
	anthropicFallbackBeta   = "server-side-fallback-2026-07-01"
	anthropicFallbacks      = "default"
	anthropicMaxTokens      = 32000
	anthropicRetryMaxTokens = 64000
	anthropicStopRefusal    = "refusal"
	anthropicStopMaxTokens  = "max_tokens"
	anthropicTextBlock      = "text"
	formatJSONSchema        = "json_schema"
)

type anthropic struct {
	endpoint
	url   string
	model string
}

type anthropicRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system"`
	Messages     []message       `json:"messages"`
	OutputConfig anthropicOutput `json:"output_config"`
	Fallbacks    string          `json:"fallbacks"`
}

type anthropicOutput struct {
	Format anthropicFormat `json:"format"`
}

type anthropicFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicReply struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (provider *anthropic) Name() string { return "anthropic" }

// Generate retries once with a larger output budget when the answer was cut
// off; thinking shares that budget.
func (provider *anthropic) Generate(ctx context.Context, request Request) (Response, error) {
	response, err := provider.call(ctx, request, anthropicMaxTokens)
	if errors.Is(err, ErrTruncated) {
		return provider.call(ctx, request, anthropicRetryMaxTokens)
	}
	return response, err
}

func (provider *anthropic) call(ctx context.Context, request Request, maxTokens int) (Response, error) {
	body := anthropicRequest{Model: provider.model, MaxTokens: maxTokens, System: request.System,
		Messages:     []message{{Role: roleUser, Content: request.Prompt}},
		OutputConfig: anthropicOutput{Format: anthropicFormat{Type: formatJSONSchema, Schema: request.Schema}},
		Fallbacks:    anthropicFallbacks}
	headers := map[string]string{"x-api-key": provider.apiKey, "anthropic-version": anthropicVersion, "anthropic-beta": anthropicFallbackBeta}
	var reply anthropicReply
	if err := provider.post(ctx, provider.url, headers, body, &reply); err != nil {
		return Response{}, err
	}
	switch reply.StopReason {
	case anthropicStopRefusal:
		return Response{}, ErrRefused
	case anthropicStopMaxTokens:
		return Response{}, ErrTruncated
	}
	var text strings.Builder
	for _, block := range reply.Content {
		if block.Type == anthropicTextBlock {
			text.WriteString(block.Text)
		}
	}
	return Response{Text: text.String(), Model: reply.Model, InputTokens: reply.Usage.InputTokens, OutputTokens: reply.Usage.OutputTokens}, nil
}
