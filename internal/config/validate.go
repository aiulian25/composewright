package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/go-units"
)

var (
	networkNamePattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	connectionPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	registryPattern      = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._/-]*)?\*?$`)
	hostnamePattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	ownerPattern         = regexp.MustCompile(`^[0-9]+:[0-9]+$`)
	secretRefNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	// localSuffixes are names only a local resolver answers.
	localSuffixes = []string{".local", ".lan", ".home.arpa", ".internal"}
	// sharedAddressSpace is 100.64.0.0/10, carrier-grade NAT and the
	// addresses Tailscale hands out: not reachable from the internet.
	sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")
)

func (config Config) Validate() error {
	return errors.Join(
		sandboxTierProblem(config.Docker.SandboxTier),
		config.Docker.Validate.validate(),
		config.LLM.validate(config.Network.AllowPrivateTargets),
		validateRegistries(config.Registries.Allow),
		validateHosts("scm.allow_hosts", config.SCM.AllowHosts),
		validateSecretRef("scm.github_token_ref", config.SCM.GitHubTokenRef),
		validatePrivateTargets(config.Network.AllowPrivateTargets),
		updateCheckDisabled(config.Network.UpdateCheck),
		oneOf("ui.theme", config.UI.Theme, themes),
		config.Profile.Validate(),
	)
}

// sandboxTierProblem accepts only the tiers composewright can stand behind;
// runsc gets a hint, since nothing would run under gVisor.
func sandboxTierProblem(tier string) error {
	err := oneOf("docker.sandbox_tier", tier, sandboxTiers)
	if err == nil || tier != sandboxTierRunsc {
		return err
	}
	return fmt.Errorf("%w (runsc is not implemented yet)", err)
}

func (validate Validate) validate() error {
	var problems []error
	if validate.Timeout < minimumTimeout {
		problems = append(problems, fmt.Errorf("docker.validate.timeout must be at least %s", minimumTimeout))
	}
	problems = append(problems, validateSize("docker.validate.memory_limit", validate.MemoryLimit))
	if validate.CPUs <= 0 {
		problems = append(problems, errors.New("docker.validate.cpus must be greater than 0"))
	}
	if validate.PIDsLimit <= 0 {
		problems = append(problems, errors.New("docker.validate.pids_limit must be greater than 0"))
	}
	if validate.MaxIterations < 1 || validate.MaxIterations > MaxIterationsLimit {
		problems = append(problems, fmt.Errorf("docker.validate.max_iterations must be between 1 and %d", MaxIterationsLimit))
	}
	return errors.Join(problems...)
}

func (llm LLM) validate(privateTargets []string) error {
	problems := []error{
		oneOf("llm.engine", llm.Engine, engines),
		oneOf("llm.provider", llm.Provider, providers),
		validateSecretRef("llm.api_key_ref", llm.APIKeyRef),
	}
	seen := map[string]bool{}
	for index, connection := range llm.Connections {
		field := fmt.Sprintf("llm.connections[%d]", index)
		if !connectionPattern.MatchString(connection.Name) || seen[connection.Name] {
			problems = append(problems, fmt.Errorf("%s.name %q must be unique: lowercase letters, digits and -", field, connection.Name))
		}
		seen[connection.Name] = true
		problems = append(problems, oneOf(field+".provider", connection.Provider, modelProviders),
			validateSecretRef(field+".api_key_ref", connection.APIKeyRef), endpointProblem(field, connection.BaseURL, connection.MaxInputTokens, privateTargets))
	}
	if llm.Provider == ProviderNone {
		return errors.Join(problems...)
	}
	return errors.Join(append(problems, endpointProblem("llm", llm.BaseURL, llm.MaxInputTokens, privateTargets))...)
}

// endpointProblem checks where a model lives and how much it may be sent.
func endpointProblem(field, baseURL string, maxInputTokens int, privateTargets []string) error {
	problems := []error{baseURLProblem(field, baseURL, privateTargets)}
	if maxInputTokens <= 0 {
		problems = append(problems, fmt.Errorf("%s.max_input_tokens must be greater than 0", field))
	}
	return errors.Join(problems...)
}

// baseURLProblem refuses anything but an http(s) URL, and plain HTTP to a
// host that is not local, which would send the API key unencrypted.
func baseURLProblem(field, baseURL string, privateTargets []string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != schemeHTTPS && parsed.Scheme != schemeHTTP) || parsed.Host == "" {
		return fmt.Errorf("%s.base_url %q must be an http(s) URL", field, baseURL)
	}
	target := HostPort(parsed)
	listed := slices.ContainsFunc(privateTargets, func(entry string) bool { return strings.EqualFold(entry, target) })
	if parsed.Scheme == schemeHTTPS || LocalEndpoint(parsed) || listed {
		return nil
	}
	return fmt.Errorf("%s.base_url %q sends the API key unencrypted to a public host; use https, or list %s in network.allow_private_targets",
		field, baseURL, target)
}

// LocalEndpoint reports whether a URL points at this machine or the local
// network, where plain HTTP does not cross the internet: a loopback,
// private, link-local or shared (Tailscale) address, a name without a dot
// such as localhost or nuc, or a name under a suffix only local resolvers
// answer.
func LocalEndpoint(parsed *url.URL) bool {
	host := strings.ToLower(parsed.Hostname())
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || sharedAddressSpace.Contains(address)
	}
	if host == "" {
		return false
	}
	return !strings.Contains(host, ".") || slices.ContainsFunc(localSuffixes, func(suffix string) bool { return strings.HasSuffix(host, suffix) })
}

// HostPort is the host:port a URL connects to, with the scheme's default
// port: the form network.allow_private_targets lists.
func HostPort(target *url.URL) string {
	port := target.Port()
	if port == "" && target.Scheme == schemeHTTP {
		port = defaultHTTPPort
	}
	if port == "" {
		port = defaultHTTPSPort
	}
	return strings.ToLower(net.JoinHostPort(target.Hostname(), port))
}

func (profile Profile) Validate() error {
	var problems []error
	seen := map[string]bool{}
	for index, network := range profile.Networks {
		field := fmt.Sprintf("profile.networks[%d]", index)
		if !networkNamePattern.MatchString(network.Name) {
			problems = append(problems, fmt.Errorf("%s.name %q is not a valid Docker network name", field, network.Name))
		}
		if seen[network.Name] {
			problems = append(problems, fmt.Errorf("%s.name %q is listed twice", field, network.Name))
		}
		seen[network.Name] = true
		problems = append(problems, oneOf(field+".kind", network.Kind, networkKinds))
		problems = append(problems, oneOf(field+".provider", network.Provider, networkProviders))
		if network.Domain != "" && !hostnamePattern.MatchString(network.Domain) {
			problems = append(problems, fmt.Errorf("%s.domain %q is not a domain such as apps.example.com", field, network.Domain))
		}
	}
	problems = append(problems, profile.Storage.validate(), profile.Defaults.validate())
	return errors.Join(problems...)
}

func (storage Storage) validate() error {
	problems := []error{oneOf("profile.storage.style", storage.Style, storageStyles)}
	if storage.Style == StorageBind && storage.BasePath == "" {
		problems = append(problems, errors.New("profile.storage.base_path is required when style is bind"))
	}
	if storage.BasePath != "" && !filepath.IsAbs(storage.BasePath) {
		problems = append(problems, fmt.Errorf("profile.storage.base_path %q must be absolute", storage.BasePath))
	}
	if !strings.Contains(storage.Layout, LayoutProject) || !strings.Contains(storage.Layout, LayoutVolume) {
		problems = append(problems, fmt.Errorf("profile.storage.layout must contain %s and %s", LayoutProject, LayoutVolume))
	}
	if strings.Contains(storage.Layout, "..") || strings.Contains(storage.SaveDir, "..") {
		problems = append(problems, errors.New("profile.storage.layout and save_dir must not contain parent directory references (..)"))
	}
	if storage.Owner != "" && !ownerPattern.MatchString(storage.Owner) {
		problems = append(problems, fmt.Errorf("profile.storage.owner %q must be numeric uid:gid", storage.Owner))
	}
	return errors.Join(problems...)
}

func (defaults Defaults) validate() error {
	problems := []error{
		oneOf("profile.defaults.restart", defaults.Restart, restartModes),
		validateSize("profile.defaults.log_max_size", defaults.LogMaxSize),
		validateSize("profile.defaults.memory_limit", defaults.MemoryLimit),
	}
	if defaults.LogMaxFile < 1 {
		problems = append(problems, errors.New("profile.defaults.log_max_file must be at least 1"))
	}
	if defaults.CPUs <= 0 || defaults.PIDsLimit <= 0 {
		problems = append(problems, errors.New("profile.defaults.cpus and pids_limit must be greater than 0"))
	}
	if defaults.PUID < 0 || defaults.PGID < 0 {
		problems = append(problems, errors.New("profile.defaults.puid and pgid must not be negative"))
	}
	if defaults.DomainSuffix != "" && !hostnamePattern.MatchString(defaults.DomainSuffix) {
		problems = append(problems, fmt.Errorf("profile.defaults.domain_suffix %q is not a hostname", defaults.DomainSuffix))
	}
	return errors.Join(problems...)
}

func validateRegistries(patterns []string) error {
	var problems []error
	for _, pattern := range patterns {
		if !registryPattern.MatchString(pattern) || strings.Count(pattern, "*") > 1 {
			problems = append(problems, fmt.Errorf("registries.allow entry %q must look like host/path or host/path/*", pattern))
		}
	}
	return errors.Join(problems...)
}

func validateHosts(field string, hosts []string) error {
	var problems []error
	for _, host := range hosts {
		if !hostnamePattern.MatchString(host) {
			problems = append(problems, fmt.Errorf("%s entry %q is not a hostname", field, host))
		}
	}
	return errors.Join(problems...)
}

func validatePrivateTargets(targets []string) error {
	var problems []error
	for _, target := range targets {
		host, port, err := net.SplitHostPort(target)
		_, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || portErr != nil {
			problems = append(problems, fmt.Errorf("network.allow_private_targets entry %q must be host:port", target))
		}
	}
	return errors.Join(problems...)
}

func validateSecretRef(field, reference string) error {
	if reference == "" {
		return nil
	}
	name, keyring := strings.CutPrefix(reference, SecretRefKeyring)
	if !keyring {
		name, _ = strings.CutPrefix(reference, SecretRefEnv)
	}
	if name == reference || !secretRefNamePattern.MatchString(name) {
		return fmt.Errorf("%s %q must be %s<name> or %s<VARIABLE>", field, reference, SecretRefKeyring, SecretRefEnv)
	}
	return nil
}

func validateSize(field, value string) error {
	if _, err := units.RAMInBytes(value); err != nil {
		return fmt.Errorf("%s %q is not a size such as 512m or 2g", field, value)
	}
	return nil
}

func updateCheckDisabled(enabled bool) error {
	if enabled {
		return errors.New("network.update_check must be false: automatic update checks are not supported, run `composewright version --check` instead")
	}
	return nil
}

func oneOf(field, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("%s %q must be one of %s", field, value, strings.Join(quoted(allowed), ", "))
}

func quoted(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.Quote(value))
	}
	return result
}
