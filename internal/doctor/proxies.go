package doctor

import (
	"context"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/sandbox"
)

// knownProxy is a reverse proxy or tunnel image, by repository path on any
// registry, with the kind and provider its network has in profile.networks.
type knownProxy struct {
	repository string
	kind       string
	provider   string
	name       string
}

var knownProxies = []knownProxy{
	{"jc21/nginx-proxy-manager", "proxy", "npm", "Nginx Proxy Manager"},
	{"library/traefik", "proxy", "traefik", "Traefik"},
	{"library/caddy", "proxy", "caddy", "Caddy"},
	{"lucaslorentz/caddy-docker-proxy", "proxy", "caddy", "Caddy"},
	{"linuxserver/swag", "proxy", "", "SWAG"},
	{"library/haproxy", "proxy", "", "HAProxy"},
	{"nginxproxy/nginx-proxy", "proxy", "", "nginx-proxy"},
	{"jwilder/nginx-proxy", "proxy", "", "nginx-proxy"},
	{"fosrl/newt", "tunnel", "pangolin", "Pangolin (newt)"},
	{"cloudflare/cloudflared", "tunnel", "cloudflare", "Cloudflare Tunnel"},
	{"tailscale/tailscale", "vpn", "tailscale", "Tailscale"},
}

// builtinNetworks are Docker's own networks, which a stack cannot join as
// an external network.
var builtinNetworks = []string{"bridge", "host", "none"}

const (
	// composeNetworkSuffix ends the network Compose makes for a project, as
	// in npm_default for a project named npm.
	composeNetworkSuffix = "_default"
	// shortestNameMatch keeps names such as "a" from matching everything.
	shortestNameMatch = 3
)

// FoundNetwork is a network a running proxy or tunnel on the Docker host is
// attached to: apps that join it can be reached through that proxy.
type FoundNetwork struct {
	Network   string
	Kind      string
	Provider  string
	Proxy     string
	Container string
}

// HostNetworks is what the Docker host has for the profile: the networks
// that exist, the proxies and tunnels running, their networks the profile
// does not list yet, and for every network of theirs the one apps on it are
// reached through.
type HostNetworks struct {
	Existing []string
	Proxies  []string
	Found    []FoundNetwork
	Owners   map[string]FoundNetwork
}

// ProxyNetworks asks the Docker host for its networks and for running
// proxies and tunnels.
func ProxyNetworks(ctx context.Context, docker *sandbox.Sandbox, profile config.Profile) (HostNetworks, error) {
	existing, err := docker.Networks(ctx)
	if err != nil {
		return HostNetworks{}, err
	}
	containers, err := docker.Containers(ctx)
	if err != nil {
		return HostNetworks{}, err
	}
	host := HostNetworks{Existing: existing}
	var attached []FoundNetwork
	for _, container := range containers {
		proxy, known := recognise(container.Image)
		if !known {
			continue
		}
		host.Proxies = append(host.Proxies, proxy.name)
		for _, network := range container.Networks {
			if slices.Contains(builtinNetworks, network) {
				continue
			}
			attached = append(attached, FoundNetwork{Network: network, Kind: proxy.kind, Provider: proxy.provider, Proxy: proxy.name, Container: container.Name})
		}
	}
	host.Owners = owners(attached)
	for _, candidate := range attached {
		_, listed := profile.Network(candidate.Network)
		offered := slices.ContainsFunc(host.Found, func(found FoundNetwork) bool { return found.Network == candidate.Network })
		if listed || offered {
			continue
		}
		host.Found = append(host.Found, host.Owners[candidate.Network])
	}
	return host, nil
}

// owners picks, for each network, the proxy or tunnel apps on it are reached
// through when several share it: the one named after the network, as newt on
// newt or npm on npm_default, else the first one attached.
func owners(attached []FoundNetwork) map[string]FoundNetwork {
	chosen := map[string]FoundNetwork{}
	for _, candidate := range attached {
		_, taken := chosen[candidate.Network]
		if taken || !namedAfter(candidate) {
			continue
		}
		chosen[candidate.Network] = candidate
	}
	for _, candidate := range attached {
		if _, taken := chosen[candidate.Network]; !taken {
			chosen[candidate.Network] = candidate
		}
	}
	return chosen
}

// namedAfter reports whether a network and its proxy's container share a
// name, ignoring case and Compose's _default suffix.
func namedAfter(candidate FoundNetwork) bool {
	network := strings.ToLower(strings.TrimSuffix(candidate.Network, composeNetworkSuffix))
	container := strings.ToLower(candidate.Container)
	if len(network) < shortestNameMatch || len(container) < shortestNameMatch {
		return false
	}
	return strings.Contains(network, container) || strings.Contains(container, network)
}

// recognise matches an image, with any registry, tag or digest, against
// the known proxies and tunnels.
func recognise(reference string) (knownProxy, bool) {
	image, err := ir.ParseImage(reference)
	if err != nil {
		return knownProxy{}, false
	}
	index := slices.IndexFunc(knownProxies, func(proxy knownProxy) bool { return proxy.repository == image.Repository })
	if index < 0 {
		return knownProxy{}, false
	}
	return knownProxies[index], true
}
