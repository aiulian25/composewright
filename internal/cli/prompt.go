package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/pipeline"
)

var errCaptured = errors.New("request captured")

// capturing is a provider that keeps the request instead of sending it.
type capturing struct {
	request *llm.Request
}

func (provider *capturing) Name() string { return "capture" }

func (provider *capturing) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	provider.request = &request
	return llm.Response{}, errCaptured
}

func (application *app) debugPromptCommand() *cobra.Command {
	var draft bool
	var upstream string
	command := &cobra.Command{
		Use:   "prompt <path>",
		Short: "Print the exact request the model would get for a local checkout",
		Long: `Run the pipeline on a local directory without contacting anything, stop where
the model would be asked, and print that request as JSON: the system prompt,
the user prompt with its labelled blocks, and the answer schema. Nothing is
sent anywhere. Use it to see what a model reads, and to replay a request
against a model by hand.`,
		Example: "  composewright debug prompt ./checkout --upstream https://github.com/miniflux/v2\n  composewright debug prompt --draft ./checkout",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runDebugPrompt(command.Context(), arguments[0], upstream, draft)
		},
	}
	command.Flags().BoolVar(&draft, "draft", false, "show the whole-plan request of the model engine instead of the fill request")
	command.Flags().StringVar(&upstream, "upstream", "", "repository URL the directory was checked out from")
	return command
}

func (application *app) runDebugPrompt(ctx context.Context, path, upstream string, draft bool) error {
	loaded, err := application.loadConfig()
	if err != nil {
		return err
	}
	provider := &capturing{}
	engine := pipeline.EngineAssisted
	if draft {
		engine = pipeline.EngineModel
	}
	settings := loaded.Config.LLM
	dependencies := pipeline.Dependencies{Config: loaded.Config, Version: application.build.Version,
		Assistant: llm.NewAssistant(provider, settings.Model, settings.MaxInputTokens)}
	options := pipeline.Options{Source: path, Upstream: upstream, Engine: engine, StopAfter: pipeline.StageIR}
	if _, err := pipeline.Generate(ctx, dependencies, options); err != nil {
		return err
	}
	if provider.request == nil {
		return errors.New("the rules filled every field with enough confidence, so no model would be asked")
	}
	request := provider.request
	return writeJSON(application.stdout, map[string]any{"system": request.System, "prompt": request.Prompt, "schema": request.Schema})
}
