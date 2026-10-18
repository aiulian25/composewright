package render

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/corpus"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

const (
	goldenDir      = "../../testdata/golden"
	traefikVariant = "traefik"
)

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
	{name: traefikVariant, exposeVia: "traefik", profile: func(profile *config.Profile) {
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

// TestDeployNamesTheCheckedPlatform: DEPLOY.md says which platform every
// image was verified for, and says nothing when none was checked.
func TestDeployNamesTheCheckedPlatform(t *testing.T) {
	options := testOptions(t, variant{})
	plan := &ir.IR{App: ir.App{Name: "demo"}, Services: []ir.Service{{Name: "app"}}}
	if deploy := string(Deploy(plan, options, "not validated yet")); strings.Contains(deploy, "Platform:") {
		t.Errorf("DEPLOY.md names a platform no image was checked for:\n%s", deploy)
	}
	options.CheckedPlatform = "linux/arm64"
	if deploy := string(Deploy(plan, options, "not validated yet")); !strings.Contains(deploy, "Platform: verified for linux/arm64, every image has a variant for it") {
		t.Errorf("DEPLOY.md lacks the platform line:\n%s", deploy)
	}
}

// TestDeployNeverOpensPortZero: an app with no published port is not sent
// to http://localhost:0.
func TestDeployNeverOpensPortZero(t *testing.T) {
	options := testOptions(t, variants[0])
	for name, ports := range map[string][]ir.Port{
		"an internal web port": {{Container: 8080, Protocol: ir.ProtocolTCP}},
		"no port at all":       nil,
	} {
		plan := &ir.IR{App: ir.App{Name: "demo"}, Services: []ir.Service{{Name: "app", Role: ir.RoleApp, Ports: ports}}}
		deploy := string(Deploy(plan, options, "not validated yet"))
		if strings.Contains(deploy, ":0") || !strings.Contains(deploy, "No port is published") {
			t.Errorf("%s: DEPLOY.md:\n%s", name, deploy)
		}
		if len(ports) > 0 && !strings.Contains(deploy, "http://app:8080") {
			t.Errorf("%s: DEPLOY.md does not say where another container reaches it:\n%s", name, deploy)
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

// TestUpstreamHardeningIsRendered: shm_size, read_only, tmpfs and cap_drop
// are written as upstream wrote them, and compose-go reads them back.
func TestUpstreamHardeningIsRendered(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/miniflux/expected.ir.json")
	app := plan.AppService()
	app.Resources.ShmSize = "128mb"
	app.Hardening = ir.Hardening{CapDrop: []string{"ALL"}, ReadOnly: true, Tmpfs: []string{"/tmp", "/run:size=64m"}}
	compose, err := Compose(plan, testOptions(t, variants[0]))
	if err != nil {
		t.Fatal(err)
	}
	want := "    security_opt: [\"no-new-privileges:true\"]\n    read_only: true\n    tmpfs: [\"/tmp\", \"/run:size=64m\"]\n    cap_drop: [\"ALL\"]\n"
	if !strings.Contains(string(compose), want) || !strings.Contains(string(compose), "    shm_size: 128mb\n") {
		t.Fatalf("hardening not rendered as upstream wrote it:\n%s", compose)
	}
	project, err := composefile.Load(context.Background(), "compose.yaml", compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	service := project.Services[app.Name]
	if !service.ReadOnly || service.ShmSize != 128*units.MiB || len(service.Tmpfs) != 2 || service.CapDrop[0] != "ALL" {
		t.Fatalf("compose-go reads %+v", service)
	}
}

// TestGrantedNeedsOfOneCapabilityMerge: several devices or capabilities of
// one service render as one key each, a device as upstream mapped it, and
// both gates accept what the renderer writes.
func TestGrantedNeedsOfOneCapabilityMerge(t *testing.T) {
	plan := loadPlan(t, "../../testdata/corpus/uptime-kuma/expected.ir.json")
	plan.Needs = []ir.Need{
		{Capability: ir.NeedDevices, Service: "uptime-kuma", Detail: "/dev/ttyUSB0:/dev/ttyACM0", Reason: "zigbee adapter"},
		{Capability: ir.NeedCapAdd, Service: "uptime-kuma", Detail: "NET_ADMIN", Reason: "upstream compose asks for it (compose.yaml:7)"},
		{Capability: ir.NeedDevices, Service: "uptime-kuma", Detail: "/dev/ttyUSB1", Reason: "second adapter"},
		{Capability: ir.NeedCapAdd, Service: "uptime-kuma", Detail: "NET_RAW", Reason: "upstream compose asks for it (compose.yaml:8)"},
		{Capability: ir.NeedPrivileged, Service: "uptime-kuma", Reason: "upstream compose asks for it (compose.yaml:9)"},
		{Capability: ir.NeedDevices, Service: "uptime-kuma", Detail: "/dev/ttyUSB1", Reason: "second adapter"},
	}
	options := testOptions(t, variants[0])
	ungranted, _ := Compose(plan, options)
	for _, key := range []string{"devices:", "cap_add:", "privileged:"} {
		if strings.Contains(string(ungranted), key) {
			t.Fatalf("an ungranted %s was rendered:\n%s", key, ungranted)
		}
	}
	options.GrantedNeeds = []string{ir.NeedDevices, ir.NeedCapAdd, ir.NeedPrivileged}
	granted, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	text := string(granted)
	for _, key := range []string{"devices:", "cap_add:", "privileged:"} {
		if strings.Count(text, key) != 1 {
			t.Fatalf("%s rendered %d times:\n%s", key, strings.Count(text, key), text)
		}
	}
	if strings.Count(text, "/dev/ttyUSB1:/dev/ttyUSB1") != 1 || strings.Count(text, "devices (second adapter)") != 1 {
		t.Fatalf("a repeated device should render once with one comment:\n%s", text)
	}
	deploy := string(Deploy(plan, options, "not validated yet"))
	if !strings.Contains(deploy, "- cap_add `NET_RAW` for uptime-kuma (granted by you)") {
		t.Fatalf("DEPLOY.md does not name the granted capability:\n%s", deploy)
	}
	for _, comment := range []string{"# NEED GRANTED BY USER: devices (zigbee adapter)\n    # NEED GRANTED BY USER: devices (second adapter)\n    devices:",
		"# NEED GRANTED BY USER: cap_add (upstream compose asks for it (compose.yaml:7))"} {
		if !strings.Contains(text, comment) {
			t.Fatalf("missing %q:\n%s", comment, text)
		}
	}
	project, err := composefile.Load(context.Background(), "compose.yaml", granted, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	service := project.Services["uptime-kuma"]
	devices := []string{}
	for _, device := range service.Devices {
		devices = append(devices, device.Source+":"+device.Target)
	}
	if strings.Join(devices, ",") != "/dev/ttyUSB0:/dev/ttyACM0,/dev/ttyUSB1:/dev/ttyUSB1" ||
		strings.Join(service.CapAdd, ",") != "NET_ADMIN,NET_RAW" || !service.Privileged {
		t.Fatalf("rendered devices %v, cap_add %v, privileged %v", devices, service.CapAdd, service.Privileged)
	}
	input := policy.Input{Plan: plan, Profile: options.Profile, GrantedNeeds: options.GrantedNeeds,
		AllowRegistries: config.Default().Registries.Allow, RepositoryOwner: "louislam"}
	findings := append(policy.EvaluateYAML(context.Background(), project, granted, input).Findings, policy.Evaluate(input).Findings...)
	for _, finding := range findings {
		if slices.Contains([]string{"P9", "P13"}, finding.Rule) && finding.Severity != policy.OK {
			t.Fatalf("a gate refuses the granted needs: %+v", finding)
		}
	}
}

// TestPublicHostRoutesAndAddresses checks that a public address set by the
// person feeds the Traefik rule, the URL variables and DEPLOY.md, a path
// adding a prefix rule stripped before the app, and blank keeps the
// derived host.
func TestPublicHostRoutesAndAddresses(t *testing.T) {
	cases := []struct {
		name, publicHost, wantURL string
		exposeVia                 string
		wantInCompose, notCompose []string
		wantInDeploy              string
	}{
		{name: "host", publicHost: "rss.example.com", exposeVia: "traefik", wantURL: "https://rss.example.com",
			wantInCompose: []string{"traefik.http.routers.miniflux.rule: \"Host(`rss.example.com`)\""}, notCompose: []string{"PathPrefix", "stripprefix"},
			wantInDeploy: "Traefik routes https://rss.example.com to it"},
		{name: "path", publicHost: "home.example.com/rss", exposeVia: "traefik", wantURL: "https://home.example.com/rss",
			wantInCompose: []string{"Host(`home.example.com`) && PathPrefix(`/rss`)", "traefik.http.middlewares.miniflux-stripprefix.stripprefix.prefixes: \"/rss\"",
				"traefik.http.routers.miniflux.middlewares: \"miniflux-stripprefix\""},
			wantInDeploy: "Traefik routes https://home.example.com/rss to it"},
		{name: "blank", exposeVia: "traefik", wantURL: "https://miniflux.home.arpa",
			wantInCompose: []string{"Host(`miniflux.home.arpa`)"}, notCompose: []string{"PathPrefix"}, wantInDeploy: "https://miniflux.home.arpa"},
		{name: "no network", publicHost: "rss.example.com", exposeVia: ExposeLocal, wantURL: "https://rss.example.com", notCompose: []string{"traefik"},
			wantInDeploy: "Point https://rss.example.com at the published port."},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := loadPlan(t, "../../testdata/corpus/miniflux/expected.ir.json")
			plan.EnvFile = append(plan.EnvFile, ir.EnvFileEntry{Key: "BASE_URL", Value: "http://localhost:8080", Provenance: ir.Provenance{Source: ir.SourceDefault}})
			traefik := variants[slices.IndexFunc(variants, func(candidate variant) bool { return candidate.name == traefikVariant })]
			traefik.exposeVia = testCase.exposeVia
			options := testOptions(t, traefik)
			options.PublicHost = testCase.publicHost
			compose, err := Compose(plan, options)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range testCase.wantInCompose {
				if !strings.Contains(string(compose), want) {
					t.Errorf("compose.yaml lacks %s:\n%s", want, compose)
				}
			}
			for _, unwanted := range testCase.notCompose {
				if strings.Contains(string(compose), unwanted) {
					t.Errorf("compose.yaml has %s:\n%s", unwanted, compose)
				}
			}
			values, err := Values(plan, options, nil)
			if err != nil {
				t.Fatal(err)
			}
			if values["BASE_URL"] != testCase.wantURL {
				t.Errorf("BASE_URL = %q, want %q", values["BASE_URL"], testCase.wantURL)
			}
			if deploy := string(Deploy(plan, options, "passed")); !strings.Contains(deploy, testCase.wantInDeploy) {
				t.Errorf("DEPLOY.md lacks %q:\n%s", testCase.wantInDeploy, deploy)
			}
		})
	}
}

func TestProxyStepUnderAPath(t *testing.T) {
	cases := map[string]string{
		providerNPM:        "custom location /rss/ that forwards to http://app:8080/",
		providerCaddy:      "redir /rss /rss/\n        handle_path /rss/* {",
		providerCloudflare: "path ^/rss",
		providerPangolin:   "resource for home.example.com/rss",
	}
	for provider, want := range cases {
		step := proxyStep(config.ProfileNetwork{Provider: provider}, "home.example.com", "/rss", "app", 8080)
		if !strings.Contains(step, want) {
			t.Errorf("%s step %q lacks %q", provider, step, want)
		}
	}
}

// TestBundledFilesAreMountedReadOnly: a bundled file is mounted from beside
// compose.yaml, read-only, the gate passes the rendered file, and DEPLOY.md
// lists it; another file bound from config/ is refused.
func TestBundledFilesAreMountedReadOnly(t *testing.T) {
	ctx := context.Background()
	options := testOptions(t, variant{})
	plan := &ir.IR{App: ir.App{Name: "demo"}, Services: []ir.Service{{Name: "web", Role: ir.RoleApp,
		Image: ir.Image{Registry: ir.DockerHubHost, Repository: "library/nginx", Tag: "1.27"}}},
		ConfigFiles: []ir.ConfigFile{{Service: "web", Path: "config/nginx.conf", Source: "deploy/nginx.conf", ContainerPath: "/etc/nginx/nginx.conf"}}}
	compose, err := Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), `- "./config/nginx.conf:/etc/nginx/nginx.conf:ro"`) {
		t.Fatalf("the bundled file is not mounted read-only:\n%s", compose)
	}
	input := policy.Input{Plan: plan, Profile: options.Profile, AllowRegistries: config.Default().Registries.Allow}
	project, err := composefile.Load(ctx, "compose.yaml", compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if report := policy.EvaluateYAML(ctx, project, compose, input); report.Blocked() {
		t.Fatalf("the rendered file is blocked: %+v", report.Findings)
	}
	if deploy := string(Deploy(plan, options, "not validated yet")); !strings.Contains(deploy, "- config/nginx.conf, from deploy/nginx.conf, at /etc/nginx/nginx.conf in web") {
		t.Errorf("DEPLOY.md does not list the bundled file:\n%s", deploy)
	}
	other := []byte(strings.Replace(string(compose), "./config/nginx.conf", "./config/other.conf", 1))
	if project, err = composefile.Load(ctx, "compose.yaml", other, nil, false); err != nil {
		t.Fatal(err)
	}
	if report := policy.EvaluateYAML(ctx, project, other, input); !report.Has("P9a", policy.Deny) {
		t.Fatalf("a file the plan does not bundle was mounted from config/: %+v", report.Findings)
	}
}
