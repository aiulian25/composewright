//go:build eval

package eval

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/corpus"
)

const reportFile = "../../eval-report.json"

// TestCorpus is `make eval`: it prints the per-engine table and writes
// eval-report.json at the repository root. EVAL_VALIDATE=1 also runs every
// plan in the sandbox (needs Docker and network access). EVAL_PROVIDER adds
// the assisted and model engines; EVAL_BASE_URL, EVAL_MODEL and
// EVAL_API_KEY_ENV (the variable holding the key) complete it.
func TestCorpus(t *testing.T) {
	entries, err := corpus.Load("../../testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if only := os.Getenv("EVAL_ONLY"); only != "" {
		entries = slices.DeleteFunc(entries, func(entry corpus.Entry) bool { return !slices.Contains(strings.Split(only, ","), entry.Slug) })
	}
	timeout, _ := time.ParseDuration(os.Getenv("EVAL_TIMEOUT"))
	report, err := Run(context.Background(), entries, Options{Validate: os.Getenv("EVAL_VALIDATE") != "", Timeout: timeout, LLM: modelSettings()})
	if err != nil {
		t.Fatal(err)
	}
	if err := report.Write(os.Stdout); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportFile, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// providerDefaults are the base URL, model and key variable of each provider
// when EVAL_* does not name them.
var providerDefaults = map[string][3]string{
	config.ProviderAnthropic:        {"https://api.anthropic.com", "claude-opus-5-5", "ANTHROPIC_API_KEY"},
	config.ProviderOpenAICompatible: {"https://api.openai.com/v1", "", "OPENAI_API_KEY"},
	config.ProviderOllama:           {"http://127.0.0.1:11434", "qwen2.5-coder:14b", ""},
}

func modelSettings() config.LLM {
	settings := config.Default().LLM
	settings.Provider = config.ProviderNone
	provider := os.Getenv("EVAL_PROVIDER")
	defaults, known := providerDefaults[provider]
	if !known {
		return settings
	}
	settings.Provider = provider
	settings.BaseURL = cmp.Or(os.Getenv("EVAL_BASE_URL"), defaults[0])
	settings.Model = cmp.Or(os.Getenv("EVAL_MODEL"), defaults[1])
	settings.APIKeyRef = config.SecretRefEnv + cmp.Or(os.Getenv("EVAL_API_KEY_ENV"), defaults[2])
	return settings
}
