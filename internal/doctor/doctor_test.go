package doctor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store/storetest"
	"github.com/aiulian25/composewright/internal/workspace"
)

// loadedIn is a configuration whose files all go under a fresh temporary
// directory, where nothing exists yet.
func loadedIn(t *testing.T) *config.Loaded {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("COMPOSEWRIGHT_MASTER_KEY_FILE", "")
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(root, "config.yaml"), DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// initialised runs composewright init for loaded.
func initialised(t *testing.T, loaded *config.Loaded) {
	t.Helper()
	if _, err := workspace.Init(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	loaded.FileExists = true
}

// TestWorkspaceCheck: doctor fails while there is no data directory, saying
// to run init, and passes once init made it.
func TestWorkspaceCheck(t *testing.T) {
	ctx := context.Background()
	loaded := loadedIn(t)
	if check := workspaceCheck(ctx, loaded); check.Status != StatusFail || !strings.Contains(check.Fix, "composewright init") {
		t.Fatalf("before init: %+v", check)
	}
	initialised(t, loaded)
	if check := workspaceCheck(ctx, loaded); check.Status != StatusOK || !strings.Contains(check.Detail, "is private, master key") {
		t.Fatalf("after init: %+v", check)
	}
}

// TestModelCheck: no model is fine, Anthropic without a key warns and says
// how to add one, and a key in the environment is enough. The model itself
// is never contacted.
func TestModelCheck(t *testing.T) {
	const keyVariable = "COMPOSEWRIGHT_TEST_ANTHROPIC_KEY"
	ctx := context.Background()
	loaded := loadedIn(t)
	loaded.Config.LLM.Provider = config.ProviderNone
	if check := modelCheck(ctx, loaded); check.Status != StatusOK || !strings.Contains(check.Detail, "llm.provider is none") {
		t.Fatalf("no model: %+v", check)
	}
	loaded.Config.LLM.Provider, loaded.Config.LLM.APIKeyRef = config.ProviderAnthropic, config.SecretRefEnv+keyVariable
	t.Setenv(keyVariable, "")
	if check := modelCheck(ctx, loaded); check.Status != StatusWarn || !strings.Contains(check.Fix, "config set-secret llm") {
		t.Fatalf("Anthropic without a key: %+v", check)
	}
	t.Setenv(keyVariable, "test-key-value")
	if check := modelCheck(ctx, loaded); check.Status != StatusOK || !strings.Contains(check.Detail, "API key stored") {
		t.Fatalf("Anthropic with a key: %+v", check)
	}
}

// TestContactCheck: asked to, doctor sends the model in use one request
// through the egress guard and reports who answered, or the error.
func TestContactCheck(t *testing.T) {
	ctx := context.Background()
	loaded := loadedIn(t)
	initialised(t, loaded)
	loaded.Config.LLM.Provider = config.ProviderNone
	if check := contactCheck(ctx, loaded, nil); check.Status != StatusOK || check.Name != contactedName {
		t.Fatalf("no model: %+v", check)
	}
	answer := `{"model":"qwen2.5-coder:7b","message":{"content":"{\"ok\":true}"},"done":true,"prompt_eval_count":23,"eval_count":4}`
	ollama := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/chat" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, answer+"\n")
	}))
	t.Cleanup(ollama.Close)
	loaded.Config.LLM.Provider, loaded.Config.LLM.Model, loaded.Config.LLM.APIKeyRef = config.ProviderOllama, "qwen2.5-coder:7b", ""
	for base, want := range map[string]string{ollama.URL: "Reachable · answered as qwen2.5-coder:7b · 23 in / 4 out tokens", ollama.URL + "/wrong": "404"} {
		loaded.Config.LLM.BaseURL = base
		policy, err := netguard.PolicyFromConfig(loaded.Config, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		guard, err := netguard.New(policy)
		if err != nil {
			t.Fatal(err)
		}
		check := contactCheck(ctx, loaded, guard)
		failed := check.Status == StatusFail && check.Fix != ""
		if !strings.Contains(check.Detail, want) || (want == "404") != failed {
			t.Fatalf("%s: %+v", base, check)
		}
	}
}

// TestAuditCheck: doctor reports an intact chain with its size, and fails,
// naming the entry, once a row is edited behind composewright's back.
func TestAuditCheck(t *testing.T) {
	ctx := context.Background()
	loaded := loadedIn(t)
	initialised(t, loaded)
	if check := auditCheck(ctx, loaded); check.Status != StatusOK || check.Detail != "hash chain intact, 1 entry" {
		t.Fatalf("untouched log: %+v", check)
	}
	storetest.EditAuditActor(t, loaded.Paths.Database(), 1)
	if check := auditCheck(ctx, loaded); check.Status != StatusFail || !strings.Contains(check.Detail, "entry 1 does not match the chain") {
		t.Fatalf("edited log: %+v", check)
	}
}

// TestTierCheck: the proxy tier is only a label, so doctor warns when Docker
// is not reached over TCP, where a socket proxy listens; otherwise the
// daemon's own containment decides.
func TestTierCheck(t *testing.T) {
	cases := []struct {
		name        string
		environment sandbox.Environment
		dockerHost  string
		status      string
		detail      string
	}{
		{"proxy on the default socket", sandbox.Environment{Tier: config.SandboxTierProxy}, "", StatusWarn, "reached at the default socket, not through a socket proxy"},
		{"proxy on a unix socket", sandbox.Environment{Tier: config.SandboxTierProxy}, "unix:///var/run/docker.sock", StatusWarn, "not through a socket proxy"},
		{"proxy over tcp, rootless", sandbox.Environment{Tier: config.SandboxTierProxy, Rootless: true}, "tcp://socket-proxy:2375", StatusOK, "rootless daemon"},
		{"policy on a rootful daemon", sandbox.Environment{Tier: config.SandboxTierPolicy}, "", StatusWarn, "rootful daemon"},
		{"runsc with gVisor on a rootless daemon", sandbox.Environment{Tier: config.SandboxTierRunsc, Rootless: true, Runtimes: []string{"runc", "runsc"}}, "", StatusOK, "gVisor"},
		{"runsc with gVisor on a rootful daemon", sandbox.Environment{Tier: config.SandboxTierRunsc, Runtimes: []string{"runc", "runsc"}}, "", StatusWarn, "daemon is rootful"},
		{"runsc without gVisor", sandbox.Environment{Tier: config.SandboxTierRunsc, Runtimes: []string{"runc"}}, "", StatusFail, "no runsc runtime"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			check := tierCheck(testCase.environment, testCase.dockerHost)
			if check.Status != testCase.status || !strings.Contains(check.Detail, testCase.detail) {
				t.Fatalf("%+v, want %s with %q", check, testCase.status, testCase.detail)
			}
		})
	}
}
