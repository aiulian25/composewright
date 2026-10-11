package config

import (
	_ "embed"
	"time"
)

//go:embed default-config.yaml
var DefaultTemplate []byte

type Config struct {
	Listen        string     `yaml:"listen"`
	DataDir       string     `yaml:"data_dir"`
	MasterKeyFile string     `yaml:"master_key_file"`
	Docker        Docker     `yaml:"docker"`
	LLM           LLM        `yaml:"llm"`
	Registries    Registries `yaml:"registries"`
	SCM           SCM        `yaml:"scm"`
	Network       Network    `yaml:"network"`
	UI            UI         `yaml:"ui"`
	Profile       Profile    `yaml:"profile"`
	Backup        Backup     `yaml:"backup"`
}

// Backup is how often composewright backs itself up on its own, and how
// many of those automatic backups it keeps.
type Backup struct {
	Schedule  string `yaml:"schedule"`
	EveryDays int    `yaml:"every_days"`
	Keep      int    `yaml:"keep"`
}

type Docker struct {
	Host        string   `yaml:"host"`
	CLIPath     string   `yaml:"cli_path"`
	SandboxTier string   `yaml:"sandbox_tier"`
	Validate    Validate `yaml:"validate"`
}

type Validate struct {
	Timeout       time.Duration `yaml:"timeout"`
	MemoryLimit   string        `yaml:"memory_limit"`
	CPUs          float64       `yaml:"cpus"`
	PIDsLimit     int           `yaml:"pids_limit"`
	MaxIterations int           `yaml:"max_iterations"`
}

type LLM struct {
	Engine         string `yaml:"engine"`
	Provider       string `yaml:"provider"`
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	APIKeyRef      string `yaml:"api_key_ref"`
	MaxInputTokens int    `yaml:"max_input_tokens"`
	// Connections are saved model connections to switch between: other
	// services, or other accounts on one. The fields above are the one in
	// use; using a connection copies it there.
	Connections []Connection `yaml:"connections"`
}

// Connection is a saved model connection, with its own key.
type Connection struct {
	Name           string `yaml:"name"`
	Provider       string `yaml:"provider"`
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	APIKeyRef      string `yaml:"api_key_ref"`
	MaxInputTokens int    `yaml:"max_input_tokens"`
}

// InUse reports whether the connection is the one the model fields hold.
func (llm LLM) InUse(connection Connection) bool {
	return llm.Provider == connection.Provider && llm.BaseURL == connection.BaseURL && llm.Model == connection.Model &&
		llm.APIKeyRef == connection.APIKeyRef
}

// Use makes a connection the one in use.
func (llm LLM) Use(connection Connection) LLM {
	llm.Provider, llm.BaseURL, llm.Model = connection.Provider, connection.BaseURL, connection.Model
	llm.APIKeyRef, llm.MaxInputTokens = connection.APIKeyRef, connection.MaxInputTokens
	return llm
}

// Current is the connection in use, as a connection.
func (llm LLM) Current(name string) Connection {
	return Connection{Name: name, Provider: llm.Provider, BaseURL: llm.BaseURL, Model: llm.Model, APIKeyRef: llm.APIKeyRef,
		MaxInputTokens: llm.MaxInputTokens}
}

type Registries struct {
	Allow []string `yaml:"allow"`
}

type SCM struct {
	AllowHosts     []string `yaml:"allow_hosts"`
	GitHubTokenRef string   `yaml:"github_token_ref"`
}

type Network struct {
	AllowPrivateTargets []string `yaml:"allow_private_targets"`
	UpdateCheck         bool     `yaml:"update_check"`
}

type UI struct {
	AuthRequired bool          `yaml:"auth_required"`
	SessionTTL   time.Duration `yaml:"session_ttl"`
	Theme        string        `yaml:"theme"`
}

type Profile struct {
	Networks       []ProfileNetwork `yaml:"networks"`
	Storage        Storage          `yaml:"storage"`
	Defaults       Defaults         `yaml:"defaults"`
	RegistryMirror string           `yaml:"registry_mirror"`
}

// ProfileNetwork is a proxy, tunnel or VPN network projects can join.
// Domain is the one it serves apps under: a project behind it is reached at
// <project>.<domain>, which its URL settings are set to.
type ProfileNetwork struct {
	Name     string `yaml:"name"`
	Kind     string `yaml:"kind"`
	Provider string `yaml:"provider"`
	Default  bool   `yaml:"default"`
	Domain   string `yaml:"domain,omitempty"`
}

type Storage struct {
	Style    string `yaml:"style"`
	BasePath string `yaml:"base_path"`
	Layout   string `yaml:"layout"`
	Owner    string `yaml:"owner"`
	SaveDir  string `yaml:"save_dir"`
}

type Defaults struct {
	TZ           string  `yaml:"tz"`
	PUID         int     `yaml:"puid"`
	PGID         int     `yaml:"pgid"`
	Restart      string  `yaml:"restart"`
	DomainSuffix string  `yaml:"domain_suffix"`
	LogMaxSize   string  `yaml:"log_max_size"`
	LogMaxFile   int     `yaml:"log_max_file"`
	MemoryLimit  string  `yaml:"memory_limit"`
	CPUs         float64 `yaml:"cpus"`
	PIDsLimit    int     `yaml:"pids_limit"`
}

func (profile Profile) Network(name string) (ProfileNetwork, bool) {
	for _, network := range profile.Networks {
		if network.Name == name {
			return network, true
		}
	}
	return ProfileNetwork{}, false
}

// DefaultNetworks are the networks a new project joins unless someone
// chooses otherwise.
func (profile Profile) DefaultNetworks() []string {
	var names []string
	for _, network := range profile.Networks {
		if network.Default {
			names = append(names, network.Name)
		}
	}
	return names
}
