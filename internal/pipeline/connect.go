package pipeline

import (
	"context"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/workspace"
)

const keyUnreadable = "the API key could not be read, so the rules work alone: "

// Setup describes how a run may reach the outside world.
type Setup struct {
	Config          config.Config
	Paths           config.Paths
	Workspace       *workspace.Workspace
	Version         string
	Engine          string
	ExtraRegistries []string
	// Offline stops every connection composewright makes: no repository
	// fetch, no image check and no model.
	Offline      bool
	NoLabelCheck bool
	// Validate builds the sandbox, which needs the docker CLI.
	Validate bool
}

// Connection is a run's dependencies and the engine it really uses; Note
// says why that engine differs from the one asked for.
type Connection struct {
	Dependencies Dependencies
	Engine       string
	Note         string
}

// Connect builds the dependencies of a run from the configuration: the
// egress guard and image inspector unless offline, the model unless the
// rules work alone, and the sandbox when the run validates. The CLI and the
// web UI share it so both reach exactly the same hosts.
func Connect(ctx context.Context, setup Setup) (Connection, error) {
	dependencies := Dependencies{Config: setup.Config, Workspace: setup.Workspace, Version: setup.Version}
	policy, err := netguard.PolicyFromConfig(setup.Config, setup.ExtraRegistries, setup.Version)
	if err != nil {
		return Connection{}, err
	}
	guard, err := netguard.New(policy)
	if err != nil {
		return Connection{}, err
	}
	if !setup.Offline {
		dependencies.Guard = guard
		dependencies.Inspector = images.NewInspector(guard, imageCache(setup.Workspace), !setup.NoLabelCheck)
	}
	if dependencies.GitHubToken, err = resolveSecret(ctx, setup.Workspace, setup.Config.SCM.GitHubTokenRef); err != nil {
		return Connection{}, err
	}
	connection := Connection{Dependencies: dependencies}
	connection.Engine, connection.Note = connectModel(ctx, &connection.Dependencies, setup, guard)
	if !setup.Validate {
		return connection, nil
	}
	connection.Dependencies.Sandbox, err = sandbox.FromConfig(setup.Config, setup.Paths.ProjectsDir())
	return connection, err
}

// connectModel attaches the configured model as assistant and repairer
// unless the rules work alone: offline, engine rules, or llm.provider none.
// A model that cannot be set up leaves the rules alone, and says why.
func connectModel(ctx context.Context, dependencies *Dependencies, setup Setup, guard *netguard.Guard) (engine, note string) {
	settings := setup.Config.LLM
	rulesOnly := setup.Offline || setup.Engine == EngineRules || setup.Engine == EngineManual || settings.Provider == config.ProviderNone
	if rulesOnly {
		return EngineRules, ""
	}
	key, err := resolveSecret(ctx, setup.Workspace, settings.APIKeyRef)
	if err != nil {
		return EngineRules, keyUnreadable + err.Error()
	}
	provider, err := llm.New(settings, key, guard)
	if err != nil {
		return EngineRules, err.Error() + "; the rules work alone"
	}
	assistant := llm.NewAssistant(provider, settings.Model, settings.MaxInputTokens)
	dependencies.Assistant, dependencies.Repairer = assistant, assistant
	return setup.Engine, ""
}

// resolveSecret reads keyring: references from the workspace and env:
// references from the environment, which works before init.
func resolveSecret(ctx context.Context, opened *workspace.Workspace, reference string) (string, error) {
	if opened == nil {
		return workspace.ResolveEnvironmentSecret(reference), nil
	}
	return opened.ResolveSecret(ctx, reference)
}

func imageCache(opened *workspace.Workspace) images.Cache {
	if opened == nil {
		return nil
	}
	return opened.Store
}
