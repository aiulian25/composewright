package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/fetch"
)

func TestConnectChoosesTheEngine(t *testing.T) {
	ollama := config.Default()
	none := config.Default()
	none.LLM.Provider = config.ProviderNone
	keyless := config.Default()
	keyless.LLM.Provider, keyless.LLM.BaseURL, keyless.LLM.APIKeyRef = config.ProviderAnthropic, "https://api.anthropic.com", "env:COMPOSEWRIGHT_UNSET_KEY"
	cases := []struct {
		name          string
		setup         Setup
		wantEngine    string
		wantModel     bool
		wantNote      string
		wantInspector bool
	}{
		{"assisted", Setup{Config: ollama, Engine: EngineAssisted}, EngineAssisted, true, "", true},
		{"model", Setup{Config: ollama, Engine: EngineModel}, EngineModel, true, "", true},
		{"rules asked for", Setup{Config: ollama, Engine: EngineRules}, EngineRules, false, "", true},
		{"offline", Setup{Config: ollama, Engine: EngineAssisted, Offline: true}, EngineRules, false, "", false},
		{"provider none", Setup{Config: none, Engine: EngineModel}, EngineRules, false, "", true},
		{"missing key", Setup{Config: keyless, Engine: EngineAssisted}, EngineRules, false, "needs an API key", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection, err := Connect(context.Background(), testCase.setup)
			if err != nil {
				t.Fatal(err)
			}
			dependencies := connection.Dependencies
			attached := dependencies.Assistant != nil && dependencies.Repairer != nil
			if connection.Engine != testCase.wantEngine || attached != testCase.wantModel || (dependencies.Inspector != nil) != testCase.wantInspector {
				t.Fatalf("engine %s, model %v, inspector %v", connection.Engine, attached, dependencies.Inspector != nil)
			}
			if !strings.Contains(connection.Note, testCase.wantNote) || (testCase.wantNote == "") != (connection.Note == "") {
				t.Fatalf("note %q, want %q", connection.Note, testCase.wantNote)
			}
		})
	}
}

func TestConnectBuildsTheDockerClientForTheHostPortCheck(t *testing.T) {
	withDocker := config.Default()
	withDocker.Docker.CLIPath = filepath.Join(t.TempDir(), "docker")
	cases := []struct {
		name        string
		setup       Setup
		wantSandbox bool
	}{
		{"without validation", Setup{Config: withDocker, Engine: EngineRules}, true},
		{"offline without validation", Setup{Config: withDocker, Engine: EngineRules, Offline: true}, false},
		{"offline validation", Setup{Config: withDocker, Engine: EngineRules, Offline: true, Validate: true}, true},
		{"no docker CLI", Setup{Config: config.Default(), Engine: EngineRules}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			connection, err := Connect(context.Background(), testCase.setup)
			if err != nil {
				t.Fatal(err)
			}
			if built := connection.Dependencies.Sandbox != nil; built != testCase.wantSandbox {
				t.Fatalf("sandbox built %v, want %v", built, testCase.wantSandbox)
			}
		})
	}
}

// TestConnectReadsEachCodeHostsToken: a code host's token comes from
// scm.token_refs, and GitHub's from scm.github_token_ref alone.
func TestConnectReadsEachCodeHostsToken(t *testing.T) {
	const (
		gitlabVariable = "COMPOSEWRIGHT_TEST_GITLAB_TOKEN"
		githubVariable = "COMPOSEWRIGHT_TEST_GITHUB_TOKEN"
		gitlabToken    = "gitlab-test-token"
		githubToken    = "github-test-token"
		gitlabHost     = "gitlab.com"
	)
	t.Setenv(gitlabVariable, gitlabToken)
	t.Setenv(githubVariable, githubToken)
	configuration := config.Default()
	configuration.SCM.GitHubTokenRef = config.SecretRefEnv + githubVariable
	configuration.SCM.TokenRefs = map[string]string{gitlabHost: config.SecretRefEnv + gitlabVariable, fetch.GitHubHost: config.SecretRefEnv + gitlabVariable}
	connection, err := Connect(context.Background(), Setup{Config: configuration, Engine: EngineRules})
	if err != nil {
		t.Fatal(err)
	}
	tokens := connection.Dependencies.Tokens
	if tokens[gitlabHost] != gitlabToken || tokens[fetch.GitHubHost] != githubToken {
		t.Fatalf("GitLab's token read %v, GitHub's %v", tokens[gitlabHost] == gitlabToken, tokens[fetch.GitHubHost] == githubToken)
	}
}
