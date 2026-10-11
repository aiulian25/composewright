package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultTemplateMatchesPlan(t *testing.T) {
	config := Default()
	if err := config.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if config.UI.Theme != ThemeDark || config.Docker.Validate.Timeout != 5*time.Minute || config.Docker.Validate.MaxIterations != 3 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if config.Listen != "127.0.0.1:8484" || config.Docker.SandboxTier != SandboxTierPolicy || config.LLM.Provider != ProviderOllama {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	want := []string{"docker.io/library/*", "ghcr.io/*", "lscr.io/linuxserver/*", "quay.io/*", "docker.io/valkey/*"}
	if !reflect.DeepEqual(config.Registries.Allow, want) {
		t.Fatalf("registries %v", config.Registries.Allow)
	}
}

type pathCase struct {
	name       string
	env        map[string]string
	flags      Flags
	files      []string
	fileData   string
	wantConfig string
	wantData   string
	wantKey    string
}

func TestPathResolutionMatrix(t *testing.T) {
	cases := []pathCase{
		{name: "xdg defaults", wantConfig: "{home}/.config/composewright/config.yaml", wantData: "{home}/.local/share/composewright", wantKey: "{home}/.local/share/composewright/keys/master.key"},
		{name: "xdg overrides", env: map[string]string{"XDG_CONFIG_HOME": "{root}/cfg", "XDG_DATA_HOME": "{root}/xdgdata"},
			wantConfig: "{root}/cfg/composewright/config.yaml", wantData: "{root}/xdgdata/composewright"},
		{name: "env beats xdg", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/env.yaml", "COMPOSEWRIGHT_DATA_DIR": "{root}/envdata", "XDG_CONFIG_HOME": "{root}/cfg", "XDG_DATA_HOME": "{root}/xdgdata"},
			wantConfig: "{root}/env.yaml", wantData: "{root}/envdata"},
		{name: "flags beat env", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/env.yaml", "COMPOSEWRIGHT_DATA_DIR": "{root}/envdata"},
			flags: Flags{ConfigFile: "{root}/flag.yaml", DataDir: "{root}/flagdata"}, wantConfig: "{root}/flag.yaml", wantData: "{root}/flagdata"},
		{name: "config file data_dir beats xdg", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/c.yaml", "XDG_DATA_HOME": "{root}/xdgdata"},
			files: []string{"{root}/c.yaml"}, fileData: "data_dir: {root}/fromfile\n", wantConfig: "{root}/c.yaml", wantData: "{root}/fromfile"},
		{name: "env beats config file data_dir", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/c.yaml", "COMPOSEWRIGHT_DATA_DIR": "{root}/envdata"},
			files: []string{"{root}/c.yaml"}, fileData: "data_dir: {root}/fromfile\n", wantConfig: "{root}/c.yaml", wantData: "{root}/envdata"},
		{name: "key file from config", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/c.yaml"}, files: []string{"{root}/c.yaml"},
			fileData: "master_key_file: {root}/creds/kek\n", wantConfig: "{root}/c.yaml", wantKey: "{root}/creds/kek"},
		{name: "key file env beats config", env: map[string]string{"COMPOSEWRIGHT_CONFIG": "{root}/c.yaml", "COMPOSEWRIGHT_MASTER_KEY_FILE": "{root}/env.kek"},
			files: []string{"{root}/c.yaml"}, fileData: "master_key_file: {root}/creds/kek\n", wantConfig: "{root}/c.yaml", wantKey: "{root}/env.kek"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			expand := strings.NewReplacer("{root}", root, "{home}", home).Replace
			for _, variable := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", EnvConfig, EnvDataDir, EnvMasterKeyFile} {
				t.Setenv(variable, "")
			}
			t.Setenv("HOME", home)
			for key, value := range testCase.env {
				t.Setenv(key, expand(value))
			}
			for _, file := range testCase.files {
				writeFile(t, expand(file), expand(testCase.fileData))
			}
			loaded, err := Load(Flags{ConfigFile: expand(testCase.flags.ConfigFile), DataDir: expand(testCase.flags.DataDir)})
			if err != nil {
				t.Fatal(err)
			}
			assertPath(t, "config", loaded.Paths.ConfigFile, expand(testCase.wantConfig))
			assertPath(t, "data", loaded.Paths.DataDir, expand(testCase.wantData))
			assertPath(t, "key", loaded.Paths.KeyFile, expand(testCase.wantKey))
		})
	}
}

func assertPath(t *testing.T, what, got, want string) {
	t.Helper()
	if want != "" && got != want {
		t.Errorf("%s path = %s, want %s", what, got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), FileMode); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	if _, err := Decode([]byte("docker:\n  sandbox_teir: policy\n")); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"tier":           func(c *Config) { c.Docker.SandboxTier = "yolo" },
		"timeout":        func(c *Config) { c.Docker.Validate.Timeout = time.Second },
		"iterations":     func(c *Config) { c.Docker.Validate.MaxIterations = 99 },
		"memory":         func(c *Config) { c.Docker.Validate.MemoryLimit = "lots" },
		"provider":       func(c *Config) { c.LLM.Provider = "magic" },
		"base url":       func(c *Config) { c.LLM.BaseURL = "ftp://x" },
		"api key ref":    func(c *Config) { c.LLM.APIKeyRef = "plaintext-key" },
		"registry":       func(c *Config) { c.Registries.Allow = []string{"https://evil/*"} },
		"private target": func(c *Config) { c.Network.AllowPrivateTargets = []string{"192.168.1.10"} },
		"update check":   func(c *Config) { c.Network.UpdateCheck = true },
		"theme":          func(c *Config) { c.UI.Theme = "neon" },
		"network kind":   func(c *Config) { c.Profile.Networks = []ProfileNetwork{{Name: "npm", Kind: "wormhole"}} },
		"one name twice": func(c *Config) {
			c.Profile.Networks = []ProfileNetwork{{Name: "a", Kind: "proxy"}, {Name: "a", Kind: "tunnel"}}
		},
		"bind needs base":  func(c *Config) { c.Profile.Storage.Style = StorageBind },
		"relative base":    func(c *Config) { c.Profile.Storage.BasePath = "docker" },
		"layout":           func(c *Config) { c.Profile.Storage.Layout = "{base}/{project}" },
		"layout traversal": func(c *Config) { c.Profile.Storage.Layout = "{base}/../{project}/{volume}" },
		"owner":            func(c *Config) { c.Profile.Storage.Owner = "root" },
		"restart":          func(c *Config) { c.Profile.Defaults.Restart = "sometimes" },
		"domain":           func(c *Config) { c.Profile.Defaults.DomainSuffix = "not a domain" },
		"network domain": func(c *Config) {
			c.Profile.Networks = []ProfileNetwork{{Name: "npm", Kind: "proxy", Domain: "https://npm.example.com"}}
		},
	}
	for name, mutate := range cases {
		config := Default()
		mutate(&config)
		if err := config.Validate(); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
}

// TestModelEndpointsOverPlainHTTP: plain HTTP reaches a model only on this
// machine or the local network, where the API key does not cross the
// internet; a public host needs https or an allow_private_targets entry.
func TestModelEndpointsOverPlainHTTP(t *testing.T) {
	cases := []struct {
		baseURL string
		targets []string
		valid   bool
	}{
		{"http://api.openai.com/v1", nil, false},
		{"http://ollama.example.com:11434", nil, false},
		{"https://api.openai.com/v1", nil, true},
		{"http://api.openai.com/v1", []string{"api.openai.com:80"}, true},
		{"http://127.0.0.1:11434", nil, true},
		{"http://192.168.1.123:11434", nil, true},
		{"http://100.101.102.103:11434", nil, true},
		{"http://[fd00::5]:11434", nil, true},
		{"http://nuc:11434", nil, true},
		{"http://nuc.lan:11434", nil, true},
		{"http://host.docker.internal:11434", nil, true},
	}
	for _, testCase := range cases {
		config := Default()
		config.LLM.BaseURL, config.Network.AllowPrivateTargets = testCase.baseURL, testCase.targets
		if err := config.Validate(); (err == nil) != testCase.valid {
			t.Errorf("%s with %v: error %v, want valid %v", testCase.baseURL, testCase.targets, err, testCase.valid)
		}
	}
	config := Default()
	config.LLM.Connections = []Connection{{Name: "openai", Provider: ProviderOpenAICompatible, BaseURL: "http://api.openai.com/v1",
		Model: "gpt", APIKeyRef: "keyring:llm-openai", MaxInputTokens: 8000}}
	err := config.Validate()
	if err == nil || !strings.Contains(err.Error(), "sends the API key unencrypted") || !strings.Contains(err.Error(), "list api.openai.com:80 in network.allow_private_targets") {
		t.Fatalf("a saved connection over plain HTTP to a public host: %v", err)
	}
}

// TestSandboxTierRunscIsRefused: runsc would read as gVisor isolation that
// nothing applies, so it is refused with a hint; proxy stays a label.
func TestSandboxTierRunscIsRefused(t *testing.T) {
	config := Default()
	config.Docker.SandboxTier = "runsc"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "runsc is not implemented yet") {
		t.Fatalf("runsc: %v", err)
	}
	config.Docker.SandboxTier = SandboxTierProxy
	if err := config.Validate(); err != nil {
		t.Fatalf("proxy: %v", err)
	}
}

func TestSetInFileKeepsCommentsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SetInFile(path, "ui.theme", "light"); err != nil {
		t.Fatal(err)
	}
	if err := SetInFile(path, "registries.allow", "[docker.io/library/*, ghcr.io/*]"); err != nil {
		t.Fatal(err)
	}
	if err := SetInFile(path, "ui.theme", "neon"); err == nil {
		t.Fatal("invalid value written")
	}
	if err := SetInFile(path, "ui.colour", "dark"); err == nil {
		t.Fatal("unknown key written")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "# dark (default) | light | system") {
		t.Fatalf("comment lost:\n%s", content)
	}
	config, err := Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if config.UI.Theme != ThemeLight || len(config.Registries.Allow) != 2 {
		t.Fatalf("values not applied: theme=%s allow=%v", config.UI.Theme, config.Registries.Allow)
	}
	value, err := Lookup(config, "docker.validate.timeout")
	if err != nil || value != "5m0s" {
		t.Fatalf("lookup = %q, %v", value, err)
	}
	if _, err := Lookup(config, "docker.nope"); err == nil {
		t.Fatal("unknown key looked up")
	}
}
