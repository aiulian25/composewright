package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
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
			if dependencies.Sandbox != nil {
				t.Fatal("a sandbox was built for a run that does not validate")
			}
		})
	}
}
