package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// OpenAI-compatible Chat Completions with strict structured outputs; works
// for OpenAI, OpenRouter, LM Studio, vLLM and the llama.cpp server. base_url
// includes the API version, e.g. https://api.openai.com/v1.
const (
	openAIPath           = "/chat/completions"
	openAIFinishLength   = "length"
	openAIFinishFiltered = "content_filter"
	openAIAuthorization  = "Authorization"
	openAIBearerPrefix   = "Bearer "
)

type openAICompatible struct {
	endpoint
	url   string
	model string
}

type openAIRequest struct {
	Model          string       `json:"model"`
	Messages       []message    `json:"messages"`
	ResponseFormat openAIFormat `json:"response_format"`
}

type openAIFormat struct {
	Type       string           `json:"type"`
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type openAIReply struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (provider *openAICompatible) Name() string { return "openai_compatible" }

func (provider *openAICompatible) Generate(ctx context.Context, request Request) (Response, error) {
	body := openAIRequest{Model: provider.model,
		Messages:       []message{{Role: roleSystem, Content: request.System}, {Role: roleUser, Content: request.Prompt}},
		ResponseFormat: openAIFormat{Type: formatJSONSchema, JSONSchema: openAIJSONSchema{Name: schemaName, Strict: true, Schema: request.Schema}}}
	headers := map[string]string{}
	if provider.apiKey != "" {
		headers[openAIAuthorization] = openAIBearerPrefix + provider.apiKey
	}
	var reply openAIReply
	if err := provider.post(ctx, provider.url, headers, body, &reply); err != nil {
		return Response{}, err
	}
	if len(reply.Choices) == 0 {
		return Response{}, errors.New("the model returned no choices")
	}
	choice := reply.Choices[0]
	refused := choice.Message.Refusal != "" || choice.FinishReason == openAIFinishFiltered
	switch {
	case refused:
		return Response{}, ErrRefused
	case choice.FinishReason == openAIFinishLength:
		return Response{}, ErrTruncated
	}
	return Response{Text: choice.Message.Content, Model: reply.Model, InputTokens: reply.Usage.PromptTokens, OutputTokens: reply.Usage.CompletionTokens}, nil
}
