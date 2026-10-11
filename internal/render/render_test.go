package render

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/corpus"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

const goldenDir = "../../testdata/golden"

type variant struct {
	name      string
	exposeVia string
	networks  []string
	storage   string
	profile   func(*config.Profile)
}

var variants = []variant{
	{name: "default"},
	{name: "bind", storage: config.StorageBind, profile: func(profile *config.Profile) { profile.Storage.BasePath = "/srv/docker" }},
	{name: "npm", exposeVia: "npm_default", profile: func(profile *config.Profile) {
		profile.Networks = []config.ProfileNetwork{{Name: "npm_default", Kind: "proxy", Provider: "npm"}}
		profile.Defaults.DomainSuffix = "home.arpa"
	}},
	{name: "traefik", exposeVia: "traefik", profile: func(profile *config.Profile) {
		profile.Networks = []config.ProfileNetwork{{Name: "traefik", Kind: "proxy", Provider: "traefik"}}
		profile.Defaults.DomainSuffix = "home.arpa"
		profile.Defaults.TZ = "Europe/London"
	}},
	{name: "networks", exposeVia: ExposeLocal, networks: []string{"cloudflared", "newt", "npm_default"}, profile: func(profile *config.Profile) {
		profile.Networks = []config.ProfileNetwork{{Name: "npm_default", Kind: "proxy", Provider: "npm"}, {Name: "newt", Kind: "tunnel", Provider: "pangolin"},
			{Name: "cloudflared", Kind: "tunnel", Provider: "cloudflare"}}
		profile.Defaults.DomainSuffix = "home.arpa"
	}},
}

// TestCorpusRendering renders every corpus plan that passes the policy gate in
// every variant, compares with golden files, re-parses the YAML with
// compose-go and runs the YAML gate on it (P9a, P13).
func TestCorpusRendering(t *testing.T) {
	entries, err := corpus.Load("../../testdata/corpus")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Passes() {
			continue
		}
		plan := loadPlan(t, filepath.Join(filepath.Dir(entry.Directory), "expected.ir.json"))
		for _, variant := range variants {
			t.Run(entry.Slug+"/"+variant.name, func(t *testing.T) {
				options := testOptions(t, variant)
				compose, err := Compose(plan, options)
				if err != nil {
					t.Fatal(err)
				}
				project, err := composefile.Load(context.Background(), "compose.yaml", compose, nil, false)
				if err != nil {
					t.Fatalf("compose-go rejects the rendered file: %v\n%s", err, compose)
				}
				report := policy.EvaluateYAML(context.Background(), project, compose, policy.Input{Plan: plan, Profile: options.Profile,
					AllowRegistries: config.Default().Registries.Allow, ExtraRegistries: entry.AllowRegistries, RepositoryOwner: entry.Source.Owner})
				if report.Blocked() {
					t.Fatalf("rendered file is blocked: %+v", report.Findings)
				}
				golden(t, filepath.Join(goldenDir, entry.Slug, variant.name+".compose.yaml"), compose)
				if variant.name != "default" {
					return
				}
				env, err := Env(plan, fixedValues(plan), "# test header\n")
				if err != nil {
					t.Fatal(err)
				}
				golden(t, filepath.Join(goldenDir, entry.Slug, "default.env"), env)
				golden(t, filepath.Join(goldenDir, entry.Slug, "DEPLOY.md"), Deploy(plan, options, "not validated yet"))
			})
		}
	}
}

// TestLargerMemory: a plan's figure raises the profile's default memory
// limit and never lowers it.
func TestLargerMemory(t *testing.T) {
	cases := []struct{ planned, profile, want string }{
		{"2g", "512m", "2g"},
		{"256m", "512m", "512m"},
		{"2g", "8g", "8g"},
		{"", "512m", "512m"},
		{"2g", "", "2g"},
	}
	for _, testCase := range cases {
		if got := largerMemory(testCase.planned, testCase.profile); got != testCase.want {
			t.Errorf("largerMemory(%q, %q) = %q, want %q", testCase.planned, testCase.profile, got, testCase.want)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/miniflux/expected.ir.json")
	options := testOptions(t, variants[0])
	first, _ := Compose(plan, options)
	second, _ := Compose(plan, options)
	if string(first) != string(second) {
		t.Fatal("rendering twice gives different output")
	}
}

func TestGrantedNeedsAreRenderedWithAComment(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/uptime-kuma/expected.ir.json")
	plan.Needs = []ir.Need{{Capability: ir.NeedHostNetwork, Service: "uptime-kuma", Reason: "ping hosts on the LAN"}}
	options := testOptions(t, variants[0])
	ungranted, _ := Compose(plan, options)
	if strings.Contains(string(ungranted), "network_mode") {
		t.Fatal("an ungranted need was rendered")
	}
	options.GrantedNeeds = []string{ir.NeedHostNetwork}
	granted, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	text := string(granted)
	if !strings.Contains(text, "# NEED GRANTED BY USER: host_network (ping hosts on the LAN)\n    network_mode: host") || strings.Contains(text, "ports:") {
		t.Fatalf("granted need not rendered as expected:\n%s", text)
	}
}

func TestOptionsResolve(t *testing.T) {
	profile := config.Default().Profile
	resolved, err := Options{Profile: profile}.Resolve()
	if err != nil || resolved.ExposeVia != ExposeLocal || resolved.Storage != config.StorageVolumes {
		t.Fatalf("defaults %+v %v", resolved, err)
	}
	profile.Networks = []config.ProfileNetwork{{Name: "npm_default", Kind: "proxy", Provider: "npm", Default: true},
		{Name: "newt", Kind: "tunnel", Provider: "pangolin", Default: true}, {Name: "vpn", Kind: "vpn"}}
	cases := []struct {
		name         string
		chosen       Options
		wantMode     string
		wantNetworks []string
	}{
		{"profile defaults", Options{}, ExposeNone, []string{"npm_default", "newt"}},
		{"ports only", Options{ExposeVia: ExposeLocal}, ExposeLocal, nil},
		{"older single network", Options{ExposeVia: "vpn"}, ExposeNone, []string{"vpn"}},
		{"networks in profile order", Options{ExposeVia: ExposeLAN, Networks: []string{"vpn", "npm_default"}}, ExposeLAN, []string{"npm_default", "vpn"}},
	}
	for _, chosen := range cases {
		chosen.chosen.Profile = profile
		resolved, err := chosen.chosen.Resolve()
		joined := []string{}
		for _, network := range resolved.networks() {
			joined = append(joined, network.Name)
		}
		if err != nil || resolved.ExposeVia != chosen.wantMode || strings.Join(joined, ",") != strings.Join(chosen.wantNetworks, ",") {
			t.Fatalf("%s: %s %v %v", chosen.name, resolved.ExposeVia, joined, err)
		}
	}
	for _, invalid := range []Options{{ExposeVia: "invented"}, {Networks: []string{"invented"}}} {
		invalid.Profile = profile
		if _, err := invalid.Resolve(); err == nil {
			t.Fatalf("a network outside the profile accepted: %+v", invalid)
		}
	}
	if _, err := (Options{Profile: profile, Storage: config.StorageBind}).Resolve(); err == nil {
		t.Fatal("bind storage without a base path accepted")
	}
}

func testOptions(t *testing.T, variant variant) Options {
	t.Helper()
	profile := config.Default().Profile
	if variant.profile != nil {
		variant.profile(&profile)
	}
	options, err := Options{Profile: profile, ExposeVia: variant.exposeVia, Networks: variant.networks, Storage: variant.storage,
		Header: " generated by composewright test"}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return options
}

// fixedValues replaces generated secrets with stable markers.
func fixedValues(plan *ir.IR) map[string]string {
	values := map[string]string{}
	for _, entry := range plan.EnvFile {
		values[entry.Key] = entry.Value
		if entry.Generate != "" {
			values[entry.Key] = "GENERATED-" + entry.Key
		}
	}
	return values
}

func loadPlan(t *testing.T, path string) *ir.IR {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ir.Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func golden(t *testing.T, path string, content []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/render -update)", err)
	}
	if string(want) != string(content) {
		t.Fatalf("%s changed; inspect and rerun with -update\n--- got ---\n%s", path, content)
	}
}

// TestDeployFolderOwner: bind-mounted folders go to the user a service runs
// as when that is fixed, else to the profile's owner.
func TestDeployFolderOwner(t *testing.T) {
	options := testOptions(t, variant{storage: config.StorageBind, profile: func(profile *config.Profile) {
		profile.Storage.BasePath, profile.Storage.Owner = "/srv/docker", "1000:1000"
	}})
	plan := &ir.IR{App: ir.App{Name: "demo"}, Services: []ir.Service{
		{Name: "app", User: "65532:65532", Volumes: []ir.Volume{{Name: "app-data", ContainerPath: "/data"}}},
		{Name: "cache", Volumes: []ir.Volume{{Name: "cache-data", ContainerPath: "/cache"}}},
	}}
	deploy := string(Deploy(plan, options, "not validated yet"))
	for _, want := range []string{"sudo chown 65532:65532 /srv/docker/demo/app-data", "sudo chown 1000:1000 /srv/docker/demo/cache-data"} {
		if !strings.Contains(deploy, want) {
			t.Errorf("DEPLOY.md lacks %q:\n%s", want, deploy)
		}
	}
}

// TestGrantedDockerSocketIsMountedWithAComment: the socket appears only once
// granted, from the host's socket to where the app asked for it, and the
// YAML gate accepts what the renderer writes.
func TestGrantedDockerSocketIsMountedWithAComment(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/uptime-kuma/expected.ir.json")
	plan.Needs = []ir.Need{{Capability: ir.NeedDockerSocket, Service: "uptime-kuma", Detail: "/var/run/docker.sock", Reason: "watches containers"}}
	options := testOptions(t, variants[0])
	ungranted, _ := Compose(plan, options)
	if strings.Contains(string(ungranted), "docker.sock") {
		t.Fatal("an ungranted socket was rendered")
	}
	options.GrantedNeeds = []string{ir.NeedDockerSocket}
	granted, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	text := string(granted)
	comment, mount := strings.Index(text, "# NEED GRANTED BY USER: docker_socket (watches containers)"), strings.Index(text, `- "/var/run/docker.sock:/var/run/docker.sock"`)
	if comment < 0 || mount < comment {
		t.Fatalf("granted socket not rendered under its comment:\n%s", text)
	}
	project, err := composefile.Load(context.Background(), "compose.yaml", granted, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	report := policy.EvaluateYAML(context.Background(), project, granted, policy.Input{Plan: plan, Profile: options.Profile, GrantedNeeds: options.GrantedNeeds,
		AllowRegistries: config.Default().Registries.Allow, RepositoryOwner: "louislam"})
	for _, finding := range report.Findings {
		if strings.Contains(finding.Path, "volumes") && finding.Severity == policy.Deny {
			t.Fatalf("the gate refuses the rendered socket: %+v", finding)
		}
	}
}

// TestOptionalSettingsArePassedThroughAndCommented: compose.yaml names each
// optional setting with no value, .env lists it commented out with its
// default, and one the plan sets as well is neither, so nothing is doubled.
func TestOptionalSettingsArePassedThroughAndCommented(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/uptime-kuma/expected.ir.json")
	plan.EnvFile = append(plan.EnvFile, ir.EnvFileEntry{Key: "ALREADY_SET", Value: "1"})
	plan.OptionalEnv = []ir.OptionalSetting{
		{Service: "uptime-kuma", Key: "UPTIME_KUMA_PORT", Default: "3001", Description: "Port to listen on", Provenance: ir.Provenance{SourceRef: "README.md:10"}},
		{Service: "uptime-kuma", Key: "ALREADY_SET", Default: "2"},
	}
	options := testOptions(t, variants[0])
	compose, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Env(plan, map[string]string{"ALREADY_SET": "1"}, "# test\n")
	if err != nil {
		t.Fatal(err)
	}
	deploy := string(Deploy(plan, options, "not validated yet"))
	checks := map[string]bool{
		"compose names the setting with no value":  strings.Contains(string(compose), "# "+optionalComposeComment+"\n      UPTIME_KUMA_PORT:\n"),
		"compose does not pass a set one twice":    strings.Count(string(compose), "ALREADY_SET") == 0,
		".env lists it commented out with default": strings.Contains(string(env), "# Port to listen on\n# for uptime-kuma · from README.md:10\n# UPTIME_KUMA_PORT=3001\n"),
		".env keeps the set one once":              strings.Count(string(env), "ALREADY_SET") == 1,
		"DEPLOY.md says how to use them":           strings.Contains(deploy, ".env lists 1 settings the upstream documents"),
	}
	for check, passed := range checks {
		if !passed {
			t.Errorf("%s:\n%s\n%s", check, compose, env)
		}
	}
	project, err := composefile.Load(context.Background(), "compose.yaml", compose, nil, false)
	if err != nil {
		t.Fatalf("compose-go rejects the pass-through: %v", err)
	}
	if value, set := project.Services["uptime-kuma"].Environment["UPTIME_KUMA_PORT"]; !set || value != nil {
		t.Fatalf("UPTIME_KUMA_PORT = %v (set %v), want no value", value, set)
	}
}
