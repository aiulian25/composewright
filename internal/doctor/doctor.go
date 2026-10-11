// Package doctor inspects the host and recommends how to run composewright
// safely. It changes nothing; every fix is printed for a human to apply.
package doctor

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type Inputs struct {
	Loaded  *config.Loaded
	Sandbox *sandbox.Sandbox
	Docker  error
}

// Run performs every check and returns them in a fixed order.
func Run(ctx context.Context, inputs Inputs) []Check {
	checks := []Check{workspaceCheck(ctx, inputs.Loaded), auditCheck(ctx, inputs.Loaded), modelCheck(ctx, inputs.Loaded)}
	if inputs.Docker != nil {
		return append(checks, Check{Name: "docker cli", Status: StatusFail, Detail: inputs.Docker.Error(),
			Fix: "install Docker Engine with the compose plugin, or set docker.cli_path"})
	}
	checks = append(checks, composeCheck(ctx, inputs.Sandbox))
	environment, err := inputs.Sandbox.Info(ctx)
	if err != nil {
		return append(checks, Check{Name: "docker daemon", Status: StatusFail, Detail: err.Error(),
			Fix: "start Docker, or point docker.host at a reachable daemon (DOCKER_HOST)"})
	}
	checks = append(checks, Check{Name: "docker daemon", Status: StatusOK, Detail: fmt.Sprintf("reachable, cgroup v%s (%s driver)", environment.CgroupVersion, environment.CgroupDriver)})
	checks = append(checks, tierCheck(environment), limitsCheck(environment))
	return append(checks, networkChecks(ctx, inputs)...)
}

func workspaceCheck(ctx context.Context, loaded *config.Loaded) Check {
	opened, err := workspace.Open(ctx, loaded)
	if err != nil {
		return Check{Name: "data and key", Status: StatusFail, Detail: err.Error(), Fix: "run `composewright init`, or apply the chmod shown"}
	}
	defer opened.Close()
	return Check{Name: "data and key", Status: StatusOK,
		Detail: fmt.Sprintf("%s is private, master key %s (0600)", loaded.Paths.DataDir, opened.Key.ID)}
}

// auditCheck recomputes the audit log's hash chain, as audit verify does.
func auditCheck(ctx context.Context, loaded *config.Loaded) Check {
	opened, err := workspace.Open(ctx, loaded)
	if err != nil {
		return Check{Name: "audit log", Status: StatusWarn, Detail: "not checked: the data directory or key does not open"}
	}
	defer opened.Close()
	count, err := opened.Store.VerifyAudit(ctx)
	if err != nil {
		return Check{Name: "audit log", Status: StatusFail, Detail: err.Error(),
			Fix: "the database was changed outside composewright; keep a copy of it for inspection"}
	}
	noun := "entries"
	if count == 1 {
		noun = "entry"
	}
	return Check{Name: "audit log", Status: StatusOK, Detail: fmt.Sprintf("hash chain intact, %d %s", count, noun)}
}

// modelCheck reads the model settings and whether its API key is stored;
// it never contacts the model.
func modelCheck(ctx context.Context, loaded *config.Loaded) Check {
	settings := loaded.Config.LLM
	if settings.Provider == config.ProviderNone {
		return Check{Name: "model", Status: StatusOK, Detail: "llm.provider is none: the rules make every plan"}
	}
	detail := fmt.Sprintf("%s %s at %s (not contacted)", settings.Provider, settings.Model, settings.BaseURL)
	keyStored := apiKey(ctx, loaded) != ""
	switch {
	case keyStored:
		return Check{Name: "model", Status: StatusOK, Detail: detail + ", API key stored"}
	case settings.Provider == config.ProviderAnthropic:
		return Check{Name: "model", Status: StatusWarn, Detail: detail + ", no API key at " + settings.APIKeyRef,
			Fix: "composewright config set-secret llm, or set llm.api_key_ref to env:ANTHROPIC_API_KEY; until then the rules work alone"}
	}
	return Check{Name: "model", Status: StatusOK, Detail: detail}
}

func apiKey(ctx context.Context, loaded *config.Loaded) string {
	reference := loaded.Config.LLM.APIKeyRef
	opened, err := workspace.Open(ctx, loaded)
	if err != nil {
		return workspace.ResolveEnvironmentSecret(reference)
	}
	defer opened.Close()
	key, err := opened.ResolveSecret(ctx, reference)
	if err != nil {
		return ""
	}
	return key
}

func composeCheck(ctx context.Context, runner *sandbox.Sandbox) Check {
	version, err := runner.ComposeVersion(ctx)
	if err != nil {
		return Check{Name: "docker compose", Status: StatusFail, Detail: err.Error(), Fix: "install the docker compose v2 plugin (2.24.4 or later)"}
	}
	if err := runner.RequireCompose(ctx); err != nil {
		return Check{Name: "docker compose", Status: StatusFail, Detail: err.Error(), Fix: "upgrade the docker compose plugin to 2.24.4 or later"}
	}
	return Check{Name: "docker compose", Status: StatusOK, Detail: "version " + version}
}

func tierCheck(environment sandbox.Environment) Check {
	if environment.Rootless {
		return Check{Name: "sandbox tier", Status: StatusOK, Detail: "rootless daemon: containers run as an unprivileged user (tier " + environment.Tier + ")"}
	}
	return Check{Name: "sandbox tier", Status: StatusWarn,
		Detail: "rootful daemon: whoever can use its socket is effectively root on this host (tier " + environment.Tier + ")",
		Fix:    "use a dedicated rootless daemon: composewright doctor --setup-rootless"}
}

func limitsCheck(environment sandbox.Environment) Check {
	if environment.LimitsEnforced {
		return Check{Name: "resource limits", Status: StatusOK, Detail: "memory, CPU and process limits are enforced"}
	}
	return Check{Name: "resource limits", Status: StatusWarn, Detail: "this daemon ignores memory, CPU or process limits",
		Fix: "rootless Docker needs cgroup v2 and Delegate=cpu cpuset io memory pids (see composewright doctor --setup-rootless)"}
}

func networkChecks(ctx context.Context, inputs Inputs) []Check {
	wanted := inputs.Loaded.Config.Profile.Networks
	if len(wanted) == 0 {
		return []Check{{Name: "profile networks", Status: StatusOK, Detail: "none configured; apps are published on 127.0.0.1"}}
	}
	existing, err := inputs.Sandbox.Networks(ctx)
	if err != nil {
		return []Check{{Name: "profile networks", Status: StatusWarn, Detail: err.Error()}}
	}
	var checks []Check
	for _, network := range wanted {
		check := Check{Name: "network " + network.Name, Status: StatusOK, Detail: network.Kind + " network exists"}
		if !slices.Contains(existing, network.Name) {
			check.Detail = "a placeholder: not on this daemon. Compose files declare it external, so it must exist where you deploy them " +
				"(docker network create " + network.Name + ", unless your proxy creates it)"
		}
		checks = append(checks, check)
	}
	return checks
}

// RootlessRecipe is what `doctor --setup-rootless` prints: the steps on the
// host, where the Docker daemon runs, for a dedicated rootless daemon the
// composewright container then uses. Nothing here is ever executed by
// composewright.
func RootlessRecipe() string {
	return strings.Join([]string{
		"Rootless Docker for your user on the host. Run these there yourself; composewright never does:",
		"",
		"  sudo apt-get install -y uidmap docker-ce-rootless-extras        # Debian/Ubuntu (dnf on Fedora)",
		`  grep "^$USER:" /etc/subuid /etc/subgid || sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"`,
		"  dockerd-rootless-setuptool.sh install",
		`  sudo loginctl enable-linger "$USER"`,
		"",
		"CPU limits need cgroup v2 delegation (memory and pids are delegated by default):",
		"",
		"  sudo mkdir -p /etc/systemd/system/user@.service.d",
		"  printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf",
		"  sudo systemctl daemon-reload",
		"",
		"Then start the composewright container on that daemon (docker context use rootless) as root",
		"inside it, which is your own unprivileged user on the host, with its socket instead of",
		"/var/run/docker.sock and a data folder you own: a volume belongs to the image's user (65532),",
		"which composewright refuses when it runs as another. --group-add is not needed:",
		"",
		"  mkdir -p -m 0700 ~/.local/share/composewright",
		`  -v "$XDG_RUNTIME_DIR/docker.sock:/var/run/docker.sock" -v ~/.local/share/composewright:/data --user 0:0`,
		"",
		"and record the tier:",
		"",
		"  docker exec composewright composewright config set docker.sandbox_tier rootless",
		"  docker exec composewright composewright doctor",
	}, "\n")
}
