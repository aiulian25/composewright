package netguard

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
)

// ReleaseHost serves `version --check`, the only request made for updates.
const ReleaseHost = "api.github.com"

// registryHosts maps a registry to every host a manifest or config request
// touches: the API host, the token realm, and the blob storage it redirects
// to. Registries not listed are reached at their own name only.
var registryHosts = map[string][]string{
	"docker.io": {"index.docker.io", "registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com", "production.cloudfront.docker.com"},
	"ghcr.io":   {"ghcr.io", "pkg-containers.githubusercontent.com"},
	"lscr.io":   {"lscr.io", "ghcr.io", "pkg-containers.githubusercontent.com"},
	"quay.io":   {"quay.io", "cdn01.quay.io", "cdn02.quay.io", "cdn03.quay.io"},
}

// PolicyFromConfig builds the egress allowlist from the configuration: the
// LLM base URL host, the SCM hosts, the hosts behind every allowlisted
// registry, and GitHub for the manual update check.
func PolicyFromConfig(configuration config.Config, extraRegistries []string, version string) (Policy, error) {
	policy := Policy{Hosts: map[Purpose][]string{}, PrivateTargets: lowered(configuration.Network.AllowPrivateTargets), Version: version}
	policy.Hosts[SCM] = lowered(configuration.SCM.AllowHosts)
	policy.Hosts[Release] = []string{ReleaseHost}
	for _, pattern := range append(append([]string{}, configuration.Registries.Allow...), extraRegistries...) {
		policy.Hosts[Registry] = appendUnique(policy.Hosts[Registry], RegistryHosts(pattern)...)
	}
	if configuration.LLM.Provider == config.ProviderNone {
		return policy, nil
	}
	base, err := url.Parse(configuration.LLM.BaseURL)
	if err != nil {
		return Policy{}, fmt.Errorf("llm.base_url: %w", err)
	}
	policy.Hosts[LLM] = []string{strings.ToLower(base.Hostname())}
	policy.PrivateTargets = appendUnique(policy.PrivateTargets, hostPort(base))
	return policy, nil
}

// RegistryHosts lists the hosts behind an allowlist pattern such as
// "docker.io/library/*".
func RegistryHosts(pattern string) []string {
	registry, _, _ := strings.Cut(strings.ToLower(pattern), "/")
	if hosts, known := registryHosts[registry]; known {
		return hosts
	}
	host, _, err := net.SplitHostPort(registry)
	if err != nil {
		host = registry
	}
	return []string{host}
}

func lowered(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.ToLower(value)
	}
	return result
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		if !contains(values, addition) {
			values = append(values, addition)
		}
	}
	return values
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
