package doctor

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
)

// TestKnownProxiesAreValidNetworks keeps the proxy table in the profile's
// own vocabulary, so a network added from it always saves.
func TestKnownProxiesAreValidNetworks(t *testing.T) {
	profile := config.Default().Profile
	for _, proxy := range knownProxies {
		profile.Networks = []config.ProfileNetwork{{Name: "found", Kind: proxy.kind, Provider: proxy.provider}}
		if err := profile.Validate(); err != nil {
			t.Fatalf("%s: %v", proxy.name, err)
		}
	}
}

// TestProxyNetworks offers the networks of running proxies and tunnels that
// the profile does not list, and nothing for other containers.
func TestProxyNetworks(t *testing.T) {
	answers := map[string]string{
		"network ls": "bridge\nhost\nnone\nnpm_default\nproxy\nshop_default\n",
		"ps": `{"Names":"npm","Image":"jc21/nginx-proxy-manager:latest","Networks":"npm_default,bridge"}` + "\n" +
			`{"Names":"traefik","Image":"traefik:v3.1","Networks":"proxy"}` + "\n" +
			`{"Names":"newt","Image":"docker.io/fosrl/newt@sha256:` + strings.Repeat("a", 64) + `","Networks":"npm_default"}` + "\n" +
			`{"Names":"shop","Image":"ghcr.io/example/shop:1","Networks":"shop_default"}`,
	}
	run := func(_ context.Context, _ string, _ io.Writer, arguments ...string) ([]byte, []byte, error) {
		for command, answer := range answers {
			if strings.HasPrefix(strings.Join(arguments, " "), command) {
				return []byte(answer), nil, nil
			}
		}
		return nil, nil, nil
	}
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	docker := sandbox.New(run, limits, time.Minute, "policy", t.TempDir())
	profile := config.Default().Profile
	profile.Networks = []config.ProfileNetwork{{Name: "proxy", Kind: "proxy", Provider: "traefik"}}
	host, err := ProxyNetworks(context.Background(), docker, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(host.Found) != 1 || host.Found[0].Network != "npm_default" || host.Found[0].Provider != "npm" || len(host.Existing) != 6 || len(host.Proxies) != 3 {
		t.Fatalf("found %+v, existing %v", host.Found, host.Existing)
	}
}

// TestSharedNetworksGoToTheirNamesake: Nginx Proxy Manager joined to the
// newt tunnel's network does not make that network NPM's; each network goes
// to the proxy or tunnel named after it.
func TestSharedNetworksGoToTheirNamesake(t *testing.T) {
	attached := []FoundNetwork{
		{Network: "newt", Kind: "proxy", Provider: "npm", Proxy: "Nginx Proxy Manager", Container: "npm"},
		{Network: "npm", Kind: "proxy", Provider: "npm", Proxy: "Nginx Proxy Manager", Container: "npm"},
		{Network: "newt", Kind: "tunnel", Provider: "pangolin", Proxy: "Pangolin (newt)", Container: "newt"},
		{Network: "edge", Kind: "tunnel", Provider: "cloudflare", Proxy: "Cloudflare Tunnel", Container: "cf"},
	}
	chosen := owners(attached)
	want := map[string]string{"newt": "pangolin", "npm": "npm", "edge": "cloudflare"}
	for network, provider := range want {
		if chosen[network].Provider != provider {
			t.Errorf("%s goes to %q, want %q", network, chosen[network].Provider, provider)
		}
	}
}
