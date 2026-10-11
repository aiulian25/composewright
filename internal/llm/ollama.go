package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Ollama's native chat API with the schema as format, temperature 0, and the
// schema repeated in the prompt as Ollama's documentation advises. The
// answer is streamed: a small model on a CPU can take longer than any
// sensible wait for the first byte, but it sends tokens all along.
const (
	ollamaPath         = "/api/chat"
	ollamaOutputTokens = 8192
	ollamaDoneLength   = "length"
	ollamaSchemaIntro  = "\n\nAnswer with JSON that matches this schema:\n"
	streamLineBytes    = 64 << 10
	// JSON and code tokenise worse than prose: three characters per token
	// keeps the context window large enough for the whole prompt.
	charactersPerContextToken = 3
)

var errStreamEnded = errors.New("the model's stream ended before it was done")

type ollama struct {
	endpoint
	url   string
	model string
}

type ollamaRequest struct {
	Model    string        `json:"model"`
	Messages []message     `json:"messages"`
	Stream   bool          `json:"stream"`
	Format   any           `json:"format"`
	Options  ollamaOptions `json:"options"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx"`
}

type ollamaChunk struct {
	Model   string `json:"model"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func (provider *ollama) Name() string { return "ollama" }

func (provider *ollama) Generate(ctx context.Context, request Request) (Response, error) {
	prompt := request.Prompt + ollamaSchemaIntro + string(request.Schema)
	window := (len(request.System)+len(prompt)+len(request.Schema))/charactersPerContextToken + ollamaOutputTokens
	body := ollamaRequest{Model: provider.model, Stream: true, Format: request.Schema, Options: ollamaOptions{NumCtx: window},
		Messages: []message{{Role: roleSystem, Content: request.System}, {Role: roleUser, Content: prompt}}}
	response, err := provider.open(ctx, provider.url, nil, body)
	if err != nil {
		return Response{}, err
	}
	defer response.Body.Close()
	return readStream(response.Body)
}

// readStream joins the streamed lines; the last one says why it stopped
// and how many tokens went in and out.
func readStream(body io.Reader) (Response, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, maxResponseBytes))
	scanner.Buffer(make([]byte, 0, streamLineBytes), maxResponseBytes)
	var text strings.Builder
	for scanner.Scan() {
		var chunk ollamaChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return Response{}, fmt.Errorf("the model's stream is unreadable: %w", err)
		}
		if chunk.Error != "" {
			return Response{}, fmt.Errorf("the model stopped: %s", chunk.Error)
		}
		text.WriteString(chunk.Message.Content)
		if !chunk.Done {
			continue
		}
		if chunk.DoneReason == ollamaDoneLength {
			return Response{}, ErrTruncated
		}
		return Response{Text: text.String(), Model: chunk.Model, InputTokens: chunk.PromptEvalCount, OutputTokens: chunk.EvalCount}, nil
	}
	if err := scanner.Err(); err != nil {
		return Response{}, err
	}
	return Response{}, errStreamEnded
}
