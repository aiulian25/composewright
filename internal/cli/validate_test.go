package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	healthyStates   = `{"Service":"miniflux","Name":"miniflux-1","State":"running","Health":"healthy"}\n{"Service":"db","Name":"db-1","State":"running","Health":"healthy"}`
	unhealthyStates = `{"Service":"miniflux","Name":"miniflux-1","State":"running","Health":"unhealthy"}\n{"Service":"db","Name":"db-1","State":"running","Health":"healthy"}`
	fakeDockerShape = `#!/bin/sh
echo "$*" >> "%[1]s/calls"
case "$*" in
  "compose version --short") echo 5.6.0 ;;
  "info --format"*) echo '{"CgroupVersion":"2","CgroupDriver":"systemd","SecurityOptions":["name=seccomp"]}' ;;
  "network ls"*) echo bridge ;;
  *" run --rm "*) echo '  HTTP/1.1 200 OK' ;;
  *" up "*) [ "%[3]s" = pass ] || { echo "container miniflux-1 is unhealthy" >&2; exit 1; } ;;
  *" ps -a --format json") printf '%[2]s\n' ;;
  "stats --no-stream"*) echo '{"Name":"miniflux-1","MemUsage":"300MiB / 1GiB","CPUPerc":"5%"}' ;;
  *" logs "*) cat "%[1]s/logs" 2>/dev/null ;;
esac
exit 0
`
)

// fakeDocker installs a docker CLI stand-in that records every call and
// reports the given container states.
func fakeDocker(t *testing.T, environment, outcome string) string {
	t.Helper()
	states := healthyStates
	if outcome != "pass" {
		states = unhealthyStates
	}
	script := filepath.Join(environment, "docker")
	content := strings.NewReplacer("%[1]s", environment, "%[2]s", states, "%[3]s", outcome).Replace(fakeDockerShape)
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if result := run(t, environment, "config", "set", "docker.cli_path", script); result.code != exitOK {
		t.Fatalf("set cli_path: %+v", result)
	}
	return filepath.Join(environment, "calls")
}

func TestGenAsksBeforeTheSandboxRuns(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	calls := fakeDocker(t, environment, "pass")
	declined := runWithInput(t, environment, "n\n", "gen", "--offline", "--no-llm", "--upstream", "https://github.com/miniflux/v2", minifluxFiles)
	recorded, _ := os.ReadFile(calls)
	if declined.code != exitOK || !strings.Contains(declined.stderr, "Run miniflux in an isolated sandbox") || strings.Contains(string(recorded), " up ") {
		t.Fatalf("sandbox ran without approval: %+v\ncalls:\n%s", declined, recorded)
	}
	approved := run(t, environment, "--yes", "gen", "--offline", "--no-llm", "--upstream", "https://github.com/miniflux/v2", minifluxFiles)
	recorded, _ = os.ReadFile(calls)
	if approved.code != exitOK || !strings.Contains(approved.stdout, "validation PASS") || !strings.Contains(approved.stdout, "peak 300MiB of 512MiB, 5% CPU") {
		t.Fatalf("approved gen: %+v", approved)
	}
	for _, want := range []string{"up --wait", "--pull always --no-build", "-f compose.yaml -f override.yaml", "down -v --remove-orphans"} {
		if !strings.Contains(string(recorded), want) {
			t.Errorf("no %q among the docker calls:\n%s", want, recorded)
		}
	}
	shown := run(t, environment, "--json", "show", "miniflux")
	if !strings.Contains(shown.stdout, `"status": "passed"`) {
		t.Fatalf("status not recorded: %s", shown.stdout)
	}
}

func TestValidateCommand(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	fakeDocker(t, environment, "fail")
	if result := generate(t, environment); result.code != exitOK {
		t.Fatalf("gen: %+v", result)
	}
	failed := run(t, environment, "validate", "miniflux")
	if failed.code != exitInvalid || !strings.Contains(failed.stdout, "validation FAIL (unhealthy)") {
		t.Fatalf("unhealthy stack: %+v", failed)
	}
	fakeDocker(t, environment, "pass")
	passed := run(t, environment, "--json", "validate", "miniflux")
	var report struct {
		Verdict struct{ Result string } `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(passed.stdout), &report); passed.code != exitOK || err != nil || report.Verdict.Result != "PASS" {
		t.Fatalf("healthy stack: %+v %v", passed, err)
	}
	deploy := run(t, environment, "show", "miniflux", "--file", "deploy")
	if !strings.Contains(deploy.stdout, "Validation: PASS") {
		t.Fatalf("DEPLOY.md not updated:\n%s", deploy.stdout)
	}
	if result := run(t, environment, "validate", "nonexistent"); result.code == exitOK {
		t.Fatal("validated a project that does not exist")
	}
}

func TestValidateIgnoreLog(t *testing.T) {
	const pattern = `^FATAL: optional plugin \w+, using defaults`
	environment := isolate(t)
	run(t, environment, "init")
	fakeDocker(t, environment, "pass")
	if result := generate(t, environment); result.code != exitOK {
		t.Fatalf("gen: %+v", result)
	}
	logs := "miniflux-1  | 2026-10-10T08:15:02.123456789Z FATAL: optional plugin missing, using defaults\n"
	if err := os.WriteFile(filepath.Join(environment, "logs"), []byte(logs), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := run(t, environment, "validate", "miniflux"); failed.code != exitInvalid || !strings.Contains(failed.stdout, "validation FAIL (logs)") {
		t.Fatalf("a fatal log line: %+v", failed)
	}
	if passed := run(t, environment, "validate", "miniflux", "--ignore-log", pattern); passed.code != exitOK || !strings.Contains(passed.stdout, "validation PASS") {
		t.Fatalf("with the line ignored: %+v", passed)
	}
	if again := run(t, environment, "validate", "miniflux"); again.code != exitOK {
		t.Fatalf("the pattern was not kept: %+v", again)
	}
	other := "miniflux-1  | 2026-10-10T08:15:03.123456789Z panic: no database, using defaults\n"
	if err := os.WriteFile(filepath.Join(environment, "logs"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := run(t, environment, "validate", "miniflux"); failed.code != exitInvalid {
		t.Fatalf("the pattern was split at its comma and hid another line: %+v", failed)
	}
	if plan := run(t, environment, "show", "miniflux", "--file", "plan"); plan.code != exitOK || strings.Contains(plan.stdout, "optional plugin") {
		t.Fatalf("the plan carries the pattern:\n%s", plan.stdout)
	}
}

func TestDoctor(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	fakeDocker(t, environment, "pass")
	result := run(t, environment, "--json", "doctor")
	var report struct {
		Checks []struct{ Name, Status string } `json:"checks"`
		Egress map[string][]string             `json:"egress"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &report); err != nil || len(report.Checks) == 0 {
		t.Fatalf("doctor json: %+v %v", result, err)
	}
	if len(report.Egress["registry"]) == 0 || len(report.Egress["scm"]) == 0 {
		t.Fatalf("egress allowlist missing: %v", report.Egress)
	}
	run(t, environment, "config", "set", "llm.provider", "anthropic")
	run(t, environment, "config", "set", "llm.base_url", "https://api.anthropic.com")
	if result := run(t, environment, "doctor"); !strings.Contains(result.stdout, "no API key at keyring:llm") {
		t.Fatalf("missing model key not reported:\n%s", result.stdout)
	}
	runWithInput(t, environment, "sk-ant-test\n", "config", "set-secret", "llm")
	if result := run(t, environment, "doctor"); !strings.Contains(result.stdout, "API key stored") || strings.Contains(result.stdout, "sk-ant-test") {
		t.Fatalf("stored model key not reported, or shown:\n%s", result.stdout)
	}
	recipe := run(t, environment, "doctor", "--setup-rootless")
	if recipe.code != exitOK || !strings.Contains(recipe.stdout, "dockerd-rootless-setuptool.sh install") || !strings.Contains(recipe.stdout, "composewright never does") {
		t.Fatalf("rootless recipe: %+v", recipe)
	}
	recipe = run(t, environment, "doctor", "--setup-runsc")
	if recipe.code != exitOK || !strings.Contains(recipe.stdout, "runsc install") || !strings.Contains(recipe.stdout, "composewright never does") {
		t.Fatalf("runsc recipe: %+v", recipe)
	}
}

// TestDoctorContactsModelOnlyWhenAsked: plain doctor sends the model
// nothing; --contact-model sends one request and prints what answered.
func TestDoctorContactsModelOnlyWhenAsked(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	fakeDocker(t, environment, "pass")
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(writer, `{"model":"qwen2.5-coder:7b","message":{"content":"{\"ok\":true}"},"done":true,"prompt_eval_count":23,"eval_count":4}`+"\n")
	}))
	t.Cleanup(model.Close)
	run(t, environment, "config", "set", "llm.provider", "ollama")
	run(t, environment, "config", "set", "llm.base_url", model.URL)
	run(t, environment, "config", "set", "llm.model", "qwen2.5-coder:7b")
	if result := run(t, environment, "doctor"); strings.Contains(result.stdout, "model (contacted)") || requests.Load() != 0 {
		t.Fatalf("doctor contacted the model unasked:\n%s", result.stdout)
	}
	result := run(t, environment, "doctor", "--contact-model")
	if result.code != exitOK || requests.Load() != 1 || !strings.Contains(result.stdout, "Reachable · answered as qwen2.5-coder:7b · 23 in / 4 out tokens") {
		t.Fatalf("doctor --contact-model: %+v", result)
	}
}

// TestRunscTierWithoutGVisor: the tier is accepted, but on a daemon without
// the runsc runtime doctor fails it and no sandbox run starts.
func TestRunscTierWithoutGVisor(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	calls := fakeDocker(t, environment, "pass")
	if set := run(t, environment, "config", "set", "docker.sandbox_tier", "runsc"); set.code != exitOK {
		t.Fatalf("set tier: %+v", set)
	}
	checked := run(t, environment, "doctor")
	if checked.code == exitOK || !strings.Contains(checked.stdout, "no runsc runtime") || !strings.Contains(checked.stdout, "--setup-runsc") {
		t.Fatalf("doctor: %+v", checked)
	}
	generated := run(t, environment, "--yes", "gen", "--offline", "--no-llm", "--upstream", "https://github.com/miniflux/v2", minifluxFiles)
	recorded, _ := os.ReadFile(calls)
	if generated.code == exitOK || !strings.Contains(generated.stderr+generated.stdout, "no runsc runtime") || strings.Contains(string(recorded), " up ") {
		t.Fatalf("gen ran without gVisor: %+v\ncalls:\n%s", generated, recorded)
	}
}
