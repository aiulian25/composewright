package sandbox

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
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
	pullAlways       = "always"
	pullMissing      = "missing"
	cleanupTimeout   = 2 * time.Minute
	stopTimeout      = "10"
	janitorFactor    = 2
	labelPairs       = ","
	labelSeparator   = "="
	rootlessOption   = "name=rootless"
	cgroupNone       = "none"
	cgroupVersionTwo = "2"
	runscRuntime     = "runsc"
)

// ErrRunscMissing stops a gVisor run on a daemon that cannot give it one.
var ErrRunscMissing = errors.New("sandbox tier runsc, but this daemon has no runsc runtime: install gVisor and register it in the daemon's daemon.json")

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
	// platform is the profile's, the one the files target; empty is this
	// machine's.
	platform string
}

func New(run Runner, limits Limits, timeout time.Duration, tier, projectsDir string) *Sandbox {
	return &Sandbox{run: run, limits: limits, timeout: timeout, tier: tier, root: projectsDir, now: time.Now, followInterval: defaultFollowInterval}
}

// Input is one run: the user's rendered compose.yaml and .env, the secret
// values to scrub from logs, which services publish which ports, which
// one-shot services may exit 0, where the app should answer from the
// network once every service is up, and which log lines are normal for it.
type Input struct {
	Project string
	Compose []byte
	Env     []byte
	// SecretFiles are the secrets passed as files and ConfigFiles the
	// repository's files the services mount, each by its path beside
	// compose.yaml.
	SecretFiles map[string][]byte
	ConfigFiles map[string][]byte
	Secrets     []string
	Published   map[string][]string
	CompletedOK map[string]bool
	Probe       ir.Probe
	Benign      []string
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
	environment, infoErr := sandbox.Info(ctx)
	if err := environment.MissingRuntime(); err != nil {
		return Verdict{}, cmp.Or(infoErr, err)
	}
	project, err := sandbox.prepare(ctx, directory, name, input, started, environment.probesEarly())
	if err != nil {
		return Verdict{}, err
	}
	pull := pullAlways
	if everyImagePinned(project) {
		pull = pullMissing
	}
	output.say("project %s: restart \"no\", no-new-privileges, at most %s memory, %g CPUs and %d processes per service, published ports on 127.0.0.1 only",
		name, units.BytesSize(float64(sandbox.limits.MemoryBytes)), sandbox.limits.CPUs, sandbox.limits.PIDs)
	if note := platformNote(sandbox.platform); note != "" {
		output.say("%s", note)
	}
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
	kept := newPeaks()
	stopSampling := sandbox.sampling(upCtx, directory, name, kept)
	upOut, upErrOut, upErr := sandbox.compose(upCtx, directory, output.writer(SourceCompose), name, "up", "--wait", "--wait-timeout", waitTimeout,
		"--pull", pull, "--no-build", "--quiet-pull")
	stopFollowing()
	stopSampling()
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		sandbox.sample(ctx, directory, name, kept)
	}()
	statesOut, _, statesErr := sandbox.compose(ctx, directory, nil, name, "ps", "-a", "--format", "json")
	logsOut, _, _ := sandbox.compose(ctx, directory, nil, name, "logs", "--no-color", "-t", "--since", started.UTC().Format(time.RFC3339))
	states, parseErr := parseStates(statesOut)
	<-sampled
	if statesErr != nil || parseErr != nil {
		return Verdict{}, errors.Join(errors.New("could not read container states"), statesErr, parseErr)
	}
	kept.record(states)
	for index, state := range states {
		states[index].MemoryLimitBytes = int64(project.Services[state.Service].MemLimit)
	}
	output.logs(string(logsOut))
	sandbox.healthOutputs(ctx, states, input.Secrets, output)
	logs := scrub(string(logsOut), input.Secrets)
	startOutput := scrub(string(upOut)+string(upErrOut), input.Secrets)
	if errors.Is(upCtx.Err(), context.DeadlineExceeded) {
		startOutput += "\n" + timeoutMessage + " " + sandbox.timeout.String()
	}
	verdict = judge(states, upErr, startOutput, logs, input.CompletedOK, compileBenign(input.Benign))
	if verdict, err = sandbox.reach(ctx, directory, name, input, verdict, output); err != nil {
		return Verdict{}, err
	}
	verdict.Project, verdict.Iteration, verdict.Duration = name, input.Iteration, sandbox.now().Sub(started)
	verdict.Logs, verdict.StartOutput, verdict.LogTails = logs, startOutput, Tails(logs)
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

// prepare writes the run's files and returns the compose project they hold.
func (sandbox *Sandbox) prepare(ctx context.Context, directory, name string, input Input, started time.Time, earlyProbes bool) (*types.Project, error) {
	project, err := composefile.Load(ctx, composeFile, input.Compose, nil, false)
	if err != nil {
		return nil, fmt.Errorf("compose.yaml does not load: %w", err)
	}
	override, err := Override(project, input.Published, sandbox.limits, name, started.Unix(), earlyProbes, Runtime(sandbox.tier))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, runDirMode); err != nil {
		return nil, err
	}
	files := map[string][]byte{composeFile: input.Compose, overrideFile: override, envFile: input.Env}
	maps.Copy(files, input.SecretFiles)
	if err := writeRunFiles(directory, files, runFileMode); err != nil {
		return nil, err
	}
	return project, writeRunFiles(directory, input.ConfigFiles, ir.ConfigFileMode)
}

// writeRunFiles writes files, by their path in the run's folder, with mode.
func writeRunFiles(directory string, files map[string][]byte, mode os.FileMode) error {
	for file, content := range files {
		path := filepath.Join(directory, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), runDirMode); err != nil {
			return err
		}
		if err := atomicfile.Write(path, content, mode); err != nil {
			return err
		}
	}
	return nil
}

// everyImagePinned reports whether each service names its image by digest.
// A digest names exact content, so a local copy is the same image and the
// run need not ask the registry, nor count against its pull limits.
func everyImagePinned(project *types.Project) bool {
	for _, service := range project.Services {
		image, err := ir.ParseImage(service.Image)
		if err != nil || image.Digest == "" {
			return false
		}
	}
	return true
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

// Info asks the daemon whether it is rootless, how cgroups work and which
// runtimes it offers, which decides whether --memory, --cpus and
// --pids-limit are enforced; the error says when the daemon cannot be reached.
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
		Runtimes        map[string]json.RawMessage
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return environment, err
	}
	environment.Runtimes = slices.Sorted(maps.Keys(info.Runtimes))
	for _, option := range info.SecurityOptions {
		environment.Rootless = environment.Rootless || option == rootlessOption
	}
	environment.CgroupVersion, environment.CgroupDriver, environment.EngineVersion = info.CgroupVersion, info.CgroupDriver, info.ServerVersion
	environment.LimitsEnforced = info.CgroupDriver != cgroupNone && (!environment.Rootless || delegated())
	return environment, nil
}

// platformNote says when the files target another platform than the one
// the sandbox tests them on, which runs each image's variant for itself.
// ponytail: this machine's platform, not a remote Docker host's; read the
// daemon's architecture from docker info if remote daemons matter.
func platformNote(target string) string {
	own := config.HostPlatform()
	if target == "" || target == own {
		return ""
	}
	return "this sandbox runs " + own + "; the files target " + target
}

// Runtime is the runtime every sandbox container runs under: runsc for the
// gVisor tier, else empty for the daemon's default.
func Runtime(tier string) string {
	if tier == config.SandboxTierRunsc {
		return runscRuntime
	}
	return ""
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

// Container is a running container on the daemon: what it runs, the
// networks it is attached to, the host ports it binds and its labels.
type Container struct {
	Name     string
	Image    string
	Networks []string
	Ports    []int
	Labels   map[string]string
}

// LabelComposeProject names the Compose project a container belongs to.
const LabelComposeProject = "com.docker.compose.project"

const (
	// listedPortsSeparator parts the entries of docker ps's Ports, as in
	// "0.0.0.0:8080->8080/tcp, [::]:8080->8080/tcp".
	listedPortsSeparator = ", "
	// publishedArrow follows the host side of a published entry; an
	// entry without one, as "8080/tcp", publishes nothing.
	publishedArrow = "->"
	hostPortColon  = ":"
)

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
		var listed struct{ Names, Image, Networks, Ports, Labels string }
		if err := json.Unmarshal([]byte(line), &listed); err != nil {
			return nil, fmt.Errorf("unexpected docker ps output: %w", err)
		}
		networks := strings.FieldsFunc(listed.Networks, func(character rune) bool { return character == ',' })
		containers = append(containers, Container{Name: listed.Names, Image: listed.Image, Networks: networks,
			Ports: listedHostPorts(listed.Ports), Labels: parseLabels(listed.Labels)})
	}
	return containers, nil
}

// listedHostPorts are the host ports of docker ps's Ports, each once
// though IPv4 and IPv6 both list it.
func listedHostPorts(listed string) []int {
	var ports []int
	for _, entry := range strings.Split(listed, listedPortsSeparator) {
		host, _, published := strings.Cut(entry, publishedArrow)
		if !published {
			continue
		}
		ports = append(ports, composefile.HostPorts(host[strings.LastIndex(host, hostPortColon)+1:])...)
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// Networks lists the Docker networks that exist on the daemon.
func (sandbox *Sandbox) Networks(ctx context.Context) ([]string, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("list networks: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.Fields(string(output)), nil
}
