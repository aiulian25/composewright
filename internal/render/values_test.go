package render

import (
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
)

// TestNetworkDomains: a URL setting takes the first joined network that has
// a domain, each network routes under its own domain, and the domain suffix
// is the fallback.
func TestNetworkDomains(t *testing.T) {
	plan := &ir.IR{App: ir.App{Name: "shop"}, Services: []ir.Service{{Name: "shop", Role: ir.RoleApp,
		Image: ir.Image{Registry: "docker.io", Repository: "example/shop", Tag: "1.0"},
		Ports: []ir.Port{{Container: 8080, Protocol: ir.ProtocolTCP, Publish: true}}}},
		EnvFile: []ir.EnvFileEntry{{Key: "BASE_URL", Value: "http://localhost:8080", Provenance: ir.Provenance{Source: ir.SourceDefault}}}}
	plan.Normalize()
	profile := config.Default().Profile
	profile.Defaults.DomainSuffix = "home.arpa"
	profile.Networks = []config.ProfileNetwork{{Name: "vpn", Kind: "vpn", Provider: "tailscale"},
		{Name: "npm_default", Kind: "proxy", Provider: "npm", Domain: "npm.example.com"},
		{Name: "newt", Kind: "tunnel", Provider: "pangolin", Domain: "newt.example.com"}}
	cases := map[string][]string{
		"https://shop.npm.example.com":  {"newt", "npm_default"},
		"https://shop.newt.example.com": {"vpn", "newt"},
		"https://shop.home.arpa":        {"vpn"},
		"http://localhost:8080":         nil,
	}
	for want, networks := range cases {
		options, err := Options{Profile: profile, Networks: networks}.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		values, err := Values(plan, options, nil)
		if err != nil {
			t.Fatal(err)
		}
		if values["BASE_URL"] != want {
			t.Errorf("joining %v: BASE_URL = %q, want %q", networks, values["BASE_URL"], want)
		}
	}
	options, err := Options{Profile: profile, Networks: []string{"newt", "npm_default"}}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	compose, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "newt (tunnel): route shop.newt.example.com to http://shop:8080") {
		t.Errorf("the tunnel does not route under its own domain:\n%s", compose)
	}
	deploy := string(Deploy(plan, options, "not validated yet"))
	if !strings.Contains(deploy, "add a proxy host shop.npm.example.com") || !strings.Contains(deploy, "resource for shop.newt.example.com") {
		t.Errorf("DEPLOY.md does not name each network's domain:\n%s", deploy)
	}
}

func TestValuesKeepStoredSecretsOnlyWhereThePlanHasNone(t *testing.T) {
	plan := &ir.IR{App: ir.App{Name: "shop"}, EnvFile: []ir.EnvFileEntry{
		{Key: "DB_PASSWORD", Secret: true, Generate: ir.GeneratePassword24},
		{Key: "SMTP_PASSWORD", Secret: true},
		{Key: "DATABASE_URL", Secret: true, Value: "postgres://shop:${DB_PASSWORD}@db/shop"},
		{Key: "MODE", Value: "production"},
	}}
	stored := map[string]string{"DB_PASSWORD": "kept-generated", "SMTP_PASSWORD": "typed-in", "DATABASE_URL": "postgres://stale", "MODE": "stale"}
	options, err := Options{Profile: config.Default().Profile}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	values, err := Values(plan, options, stored)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"DB_PASSWORD": "kept-generated", "SMTP_PASSWORD": "typed-in",
		"DATABASE_URL": "postgres://shop:${DB_PASSWORD}@db/shop", "MODE": "production"}
	for key, value := range want {
		if values[key] != value {
			t.Errorf("%s = %q, want %q", key, values[key], value)
		}
	}
	fresh, _ := Values(plan, options, map[string]string{})
	if fresh["SMTP_PASSWORD"] != "" || len(fresh["DB_PASSWORD"]) != 24 {
		t.Errorf("without stored values: %v", fresh)
	}
}
