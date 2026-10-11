package config

import "time"

const (
	AppName        = "composewright"
	ConfigFileName = "config.yaml"
	DatabaseName   = "composewright.db"
	KeysDirName    = "keys"
	KeyFileName    = "master.key"
	ProjectsDir    = "projects"
	BackupsDir     = "backups"

	EnvConfig        = "COMPOSEWRIGHT_CONFIG"
	EnvDataDir       = "COMPOSEWRIGHT_DATA_DIR"
	EnvMasterKeyFile = "COMPOSEWRIGHT_MASTER_KEY_FILE"

	DirMode  = 0o700
	FileMode = 0o600

	SandboxTierPolicy   = "policy"
	SandboxTierRootless = "rootless"
	SandboxTierProxy    = "proxy"
	// sandboxTierRunsc is refused with a hint: it reads as gVisor isolation,
	// but no runtime applies it yet.
	sandboxTierRunsc = "runsc"

	ProviderAnthropic        = "anthropic"
	ProviderOpenAICompatible = "openai_compatible"
	ProviderOllama           = "ollama"
	ProviderNone             = "none"

	schemeHTTP       = "http"
	schemeHTTPS      = "https"
	defaultHTTPPort  = "80"
	defaultHTTPSPort = "443"

	// The engines decide who writes a plan: the rules alone, the rules with
	// a model filling what they leave open, or a model drafting it all.
	EngineRules    = "rules"
	EngineAssisted = "assisted"
	EngineModel    = "model"

	ThemeDark   = "dark"
	ThemeLight  = "light"
	ThemeSystem = "system"

	StorageVolumes = "volumes"
	StorageBind    = "bind"

	SecretRefKeyring = "keyring:"
	SecretRefEnv     = "env:"

	LayoutBase    = "{base}"
	LayoutProject = "{project}"
	LayoutVolume  = "{volume}"

	MaxIterationsLimit = 10
	minimumTimeout     = 10 * time.Second
)

// Preset is a ready set of connection values for a model service: where it
// lives, a model to start with and how much a prompt may hold.
type Preset struct {
	ID             string
	Provider       string
	BaseURL        string
	Model          string
	MaxInputTokens int
}

const (
	cloudInputTokens = 100000
	localInputTokens = 8000
	freeInputTokens  = 32000
)

// Presets are the model services Settings offers, paid and free, and any
// other service that speaks the OpenAI API. OpenRouter's model is the
// person's pick from its list.
var Presets = []Preset{
	{ID: "anthropic", Provider: ProviderAnthropic, BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5", MaxInputTokens: cloudInputTokens},
	{ID: "openai", Provider: ProviderOpenAICompatible, BaseURL: "https://api.openai.com/v1", Model: "gpt-5-mini", MaxInputTokens: cloudInputTokens},
	{ID: "ollama", Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: "qwen2.5-coder:7b", MaxInputTokens: localInputTokens},
	{ID: "gemini", Provider: ProviderOpenAICompatible, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Model: "gemini-2.5-flash", MaxInputTokens: cloudInputTokens},
	{ID: "openrouter", Provider: ProviderOpenAICompatible, BaseURL: "https://openrouter.ai/api/v1", MaxInputTokens: freeInputTokens},
	{ID: "custom", Provider: ProviderOpenAICompatible, MaxInputTokens: freeInputTokens},
}

var (
	sandboxTiers   = []string{SandboxTierPolicy, SandboxTierRootless, SandboxTierProxy}
	providers      = []string{ProviderAnthropic, ProviderOpenAICompatible, ProviderOllama, ProviderNone}
	modelProviders = []string{ProviderAnthropic, ProviderOpenAICompatible, ProviderOllama}
	engines        = []string{EngineRules, EngineAssisted, EngineModel}
	themes         = []string{ThemeDark, ThemeLight, ThemeSystem}
	storageStyles  = []string{StorageVolumes, StorageBind}
	networkKinds   = []string{"proxy", "tunnel", "vpn", "other"}
	networkVendors = []string{"", "npm", "traefik", "caddy", "pangolin", "cloudflare", "tailscale"}
	restartModes   = []string{"no", "always", "on-failure", "unless-stopped"}
)
