package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
)

const (
	projectPrefix    = "cw-"
	suffixBytes      = 2
	runDirMode       = 0o700
	runFileMode      = 0o600
	composeFile      = "compose.yaml"
	overrideFile     = "override.yaml"
	envFile          = ".env"
	pullAllowance    = 10 * time.Minute
	cleanupTimeout   = 2 * time.Minute
	stopTimeout      = "10"
	janitorFactor    = 2
	labelPairs       = ","
	labelSeparator   = "="
	rootlessOption   = "name=rootless"
	cgroupNone       = "none"
	cgroupVersionTwo = "2"
)

var projectCleaner = regexp.MustCompile(`[^a-z0-9-]+`)

// Sandbox runs stacks in throw-away Compose projects named cw-<project>-<id>.
type Sandbox struct {
	run            Runner
	limits         Limits
	timeout        time.Duration
	tier           string
	root           string
	now            func() time.Time
	followInterval time.Duration
}

func New(run Runner, limits Limits, timeout time.Duration, tier, projectsDir string) *Sandbox {
	return &Sandbox{run: run, limits: limits, timeout: timeout, tier: tier, root: projectsDir, now: time.Now, followInterval: defaultFollowInterval}
}

// Input is one run: the user's rendered compose.yaml and .env, the secret
// values to scrub from logs, which services publish which ports, which
// one-shot services may exit 0, and where the app should answer from the
// network once every service is up.
type Input struct {
	Project     string
	Compose     []byte
	Env         []byte
	Secrets     []string
	Published   map[string][]string
	CompletedOK map[string]bool
	Probe       ir.Probe
	Iteration   int
	// Live receives redacted progress and container output during the run.
	Live func(Line)
}

// Validate runs the stack and judges it. The project is always taken down
// with its volumes, and the .env file is deleted, even when ctx is
// cancelled; a failed teardown is reported, and the janitor retries it.
func (sandbox *Sandbox) Validate(ctx context.Context, input Input) (verdict Verdict, err error) {
	started := sandbox.now()
	name, err := projectName(input.Project)
	if err != nil {
		return Verdict{}, err
	}
	directory := filepath.Join(sandbox.root, slug(input.Project), "run", name)
	output := newLiveOutput(input.Live, input.Secrets, sandbox.now)
	environment := sandbox.Environment(ctx)
	if err := sandbox.prepare(ctx, directory, name, input, started, environment.probesEarly()); err != nil {
		return Verdict{}, err
	}
	output.say("project %s: restart \"no\", no-new-privileges, at most %s memory, %g CPUs and %d processes per service, published ports on 127.0.0.1 only",
		name, units.BytesSize(float64(sandbox.limits.MemoryBytes)), sandbox.limits.CPUs, sandbox.limits.PIDs)
	defer func() {
		teardownErr := sandbox.teardown(directory, name)
		err = errors.Join(err, teardownErr)
		output.removed(name, teardownErr)
	}()
	upCtx, cancel := context.WithTimeout(ctx, sandbox.timeout+pullAllowance)
	defer cancel()
	waitTimeout := strconv.Itoa(int(sandbox.timeout.Seconds()))
	output.say("pulling images and starting; waiting up to %s for every service to be healthy%s", sandbox.timeout, probeNote(environment))
	stopFollowing := output.follow(upCtx, sandbox, directory, name, started)
	upOut, upErrOut, upErr := sandbox.compose(upCtx, directory, output.writer(SourceCompose), name, "up", "--wait", "--wait-timeout", waitTimeout,
		"--pull", "always", "--no-build", "--quiet-pull")
	stopFollowing()
	statesOut, _, statesErr := sandbox.compose(ctx, directory, nil, name, "ps", "-a", "--format", "json")
	logsOut, _, _ := sandbox.compose(ctx, directory, nil, name, "logs", "--no-color", "-t", "--since", started.UTC().Format(time.RFC3339))
	states, parseErr := parseStates(statesOut)
	if statesErr != nil || parseErr != nil {
		return Verdict{}, errors.Join(errors.New("could not read container states"), statesErr, parseErr)
	}
	output.logs(string(logsOut))
	sandbox.healthOutputs(ctx, states, input.Secrets, output)
	logs := scrub(string(logsOut), input.Secrets)
	startOutput := scrub(string(upOut)+string(upErrOut), input.Secrets)
	if errors.Is(upCtx.Err(), context.DeadlineExceeded) {
		startOutput += "\n" + timeoutMessage + " " + sandbox.timeout.String()
	}
	verdict = judge(states, upErr, startOutput, logs, input.CompletedOK)
	if verdict, err = sandbox.reach(ctx, directory, name, input, verdict, output); err != nil {
		return Verdict{}, err
	}
	verdict.Project, verdict.Iteration, verdict.Duration = name, input.Iteration, sandbox.now().Sub(started)
	verdict.Logs, verdict.StartOutput, verdict.LogTails = logs, startOutput, tails(logs)
	verdict.Sandbox = environment
	output.say("%s", verdictLine(verdict))
	return verdict, nil
}

// healthOutputs keeps what the last healthcheck printed for every
// container still starting or unhealthy, before the teardown removes it,
// and shows it in the live output.
func (sandbox *Sandbox) healthOutputs(ctx context.Context, states []ServiceState, secrets []string, output *liveOutput) {
	for index, state := range states {
		if state.Health != healthUnhealthy && state.Health != healthStarting {
			continue
		}
		inspected, _, err := sandbox.run(ctx, "", nil, "inspect", "--format", "{{json .State.Health}}", state.Name)
		said := lastHealthOutput(inspected, secrets)
		if err != nil || said == "" {
			continue
		}
		states[index].HealthOutput = said
		output.say("%s healthcheck said: %s", state.Service, said)
	}
}

func (sandbox *Sandbox) prepare(ctx context.Context, directory, name string, input Input, started time.Time, earlyProbes bool) error {
	project, err := composefile.Load(ctx, composeFile, input.Compose, nil, false)
	if err != nil {
		return fmt.Errorf("compose.yaml does not load: %w", err)
	}
	override, err := Override(project, input.Published, sandbox.limits, name, started.Unix(), earlyProbes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, runDirMode); err != nil {
		return err
	}
	files := map[string][]byte{composeFile: input.Compose, overrideFile: override, envFile: input.Env}
	for file, content := range files {
		if err := atomicfile.Write(filepath.Join(directory, file), content, runFileMode); err != nil {
			return err
		}
	}
	return nil
}

// teardown uses its own context so a cancelled run is still cleaned up.
func (sandbox *Sandbox) teardown(directory, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, stderr, downErr := sandbox.compose(ctx, directory, nil, name, "down", "-v", "--remove-orphans", "-t", stopTimeout)
	removeErr := os.RemoveAll(directory)
	if downErr != nil {
		return fmt.Errorf("sandbox %s was not removed (the janitor will retry): %w: %s", name, downErr, strings.TrimSpace(string(stderr)))
	}
	return removeErr
}

func (sandbox *Sandbox) compose(ctx context.Context, directory string, live io.Writer, name string, arguments ...string) ([]byte, []byte, error) {
	global := []string{"compose", "--ansi", "never", "--progress", "plain", "-p", name, "-f", composeFile, "-f", overrideFile, "--env-file", envFile}
	return sandbox.run(ctx, directory, live, append(global, arguments...)...)
}

// Janitor removes sandbox projects older than twice the validation timeout,
// found by the label every sandbox container carries.
func (sandbox *Sandbox) Janitor(ctx context.Context) ([]string, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "ps", "-a", "--filter", "label="+LabelProject, "--format", "{{json .Labels}}")
	if err != nil {
		return nil, fmt.Errorf("list sandbox containers: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	stale := map[string]bool{}
	cutoff := sandbox.now().Add(-janitorFactor * sandbox.timeout).Unix()
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		var text string
		if json.Unmarshal([]byte(line), &text) != nil {
			continue
		}
		labels := parseLabels(text)
		started, err := strconv.ParseInt(labels[LabelStarted], 10, 64)
		name := labels[LabelProject]
		if err == nil && started < cutoff && strings.HasPrefix(name, projectPrefix) {
			stale[name] = true
		}
	}
	var removed []string
	for name := range stale {
		if _, _, err := sandbox.run(ctx, "", nil, "compose", "-p", name, "down", "-v", "--remove-orphans", "-t", stopTimeout); err != nil {
			return removed, fmt.Errorf("remove stale sandbox %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// Environment asks the daemon whether it is rootless and how cgroups work,
// which decides whether --memory, --cpus and --pids-limit are enforced.
func (sandbox *Sandbox) Environment(ctx context.Context) Environment {
	environment, _ := sandbox.Info(ctx)
	return environment
}

// Info is Environment with the error when the daemon cannot be reached.
func (sandbox *Sandbox) Info(ctx context.Context) (Environment, error) {
	environment := Environment{Tier: sandbox.tier}
	output, stderr, err := sandbox.run(ctx, "", nil, "info", "--format", "{{json .}}")
	if err != nil {
		return environment, fmt.Errorf("the Docker daemon is not reachable: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var info struct {
		SecurityOptions []string
		CgroupVersion   string
		CgroupDriver    string
		ServerVersion   string
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return environment, err
	}
	for _, option := range info.SecurityOptions {
		environment.Rootless = environment.Rootless || option == rootlessOption
	}
	environment.CgroupVersion, environment.CgroupDriver, environment.EngineVersion = info.CgroupVersion, info.CgroupDriver, info.ServerVersion
	environment.LimitsEnforced = info.CgroupDriver != cgroupNone && (!environment.Rootless || delegated())
	return environment, nil
}

// delegated reports whether systemd delegates the cpu controller to this
// user, which rootless Docker needs to apply CPU limits.
func delegated() bool {
	uid := strconv.Itoa(os.Getuid())
	path := filepath.Join("/sys/fs/cgroup/user.slice", "user-"+uid+".slice", "user@"+uid+".service", "cgroup.controllers")
	content, err := os.ReadFile(path)
	return err == nil && strings.Contains(" "+string(content)+" ", " cpu ")
}

func projectName(project string) (string, error) {
	suffix := make([]byte, suffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return projectPrefix + slug(project) + "-" + hex.EncodeToString(suffix), nil
}

func slug(project string) string {
	return strings.Trim(projectCleaner.ReplaceAllString(strings.ToLower(project), "-"), "-")
}

func parseLabels(text string) map[string]string {
	labels := map[string]string{}
	for _, pair := range strings.Split(text, labelPairs) {
		key, value, found := strings.Cut(pair, labelSeparator)
		if found {
			labels[key] = value
		}
	}
	return labels
}

func scrub(text string, secrets []string) string {
	return redact.Text(redact.Values(ansiSequence.ReplaceAllString(text, ""), secrets))
}

// minimumCompose is the first Compose release with the !override and !reset
// merge tags the sandbox override relies on.
var minimumCompose = []int{2, 24, 4}

// ComposeVersion returns `docker compose version --short`.
func (sandbox *Sandbox) ComposeVersion(ctx context.Context) (string, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "compose", "version", "--short")
	if err != nil {
		return "", fmt.Errorf("docker compose is not available: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.TrimPrefix(strings.TrimSpace(string(output)), "v"), nil
}

// RequireCompose fails when the Compose plugin is too old for the override.
func (sandbox *Sandbox) RequireCompose(ctx context.Context) error {
	version, err := sandbox.ComposeVersion(ctx)
	if err != nil {
		return err
	}
	if !AtLeast(version, minimumCompose) {
		return fmt.Errorf("docker compose %s is too old: validation needs 2.24.4 or later", version)
	}
	return nil
}

// AtLeast compares dotted versions numerically.
func AtLeast(version string, minimum []int) bool {
	parts := strings.Split(strings.SplitN(version, "-", 2)[0], ".")
	for index, wanted := range minimum {
		if index >= len(parts) {
			return false
		}
		number, err := strconv.Atoi(parts[index])
		if err != nil || number != wanted {
			return err == nil && number > wanted
		}
	}
	return true
}

// Container is a running container on the daemon: what it runs and the
// networks it is attached to.
type Container struct {
	Name     string
	Image    string
	Networks []string
}

// Containers lists the running containers on the daemon.
func (sandbox *Sandbox) Containers(ctx context.Context) ([]Container, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "ps", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("list containers: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return parseContainers(output)
}

// parseContainers reads `docker ps --format '{{json .}}'`, one container a
// line; Networks there is a comma-separated list.
func parseContainers(output []byte) ([]Container, error) {
	var containers []Container
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var listed struct{ Names, Image, Networks string }
		if err := json.Unmarshal([]byte(line), &listed); err != nil {
			return nil, fmt.Errorf("unexpected docker ps output: %w", err)
		}
		networks := strings.FieldsFunc(listed.Networks, func(character rune) bool { return character == ',' })
		containers = append(containers, Container{Name: listed.Names, Image: listed.Image, Networks: networks})
	}
	return containers, nil
}

// Networks lists the Docker networks that exist on the daemon.
func (sandbox *Sandbox) Networks(ctx context.Context) ([]string, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("list networks: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.Fields(string(output)), nil
}
