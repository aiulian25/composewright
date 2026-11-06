package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	projectPrefix  = "cw-"
	suffixBytes    = 2
	runDirMode     = 0o700
	runFileMode    = 0o600
	composeFile    = "compose.yaml"
	overrideFile   = "override.yaml"
	envFile        = ".env"
	pullAllowance  = 10 * time.Minute
	pullAlways     = "always"
	pullMissing    = "missing"
	cleanupTimeout = 2 * time.Minute
	// commandTimeout bounds each step of a run that only asks the daemon
	// something, so a stalled daemon ends the run instead of hanging it.
	commandTimeout = 2 * time.Minute
	stopTimeout    = "10"
	janitorFactor  = 2
	// filesHelper names the stopped container that fills the files volume,
	// mounted at filesMountPoint; stagedFileMode lets any container user
	// read a staged file, as a config's default mode does.
	filesHelper     = "cw-files-helper"
	filesMountPoint = "/files"
	stagedFileMode  = 0o444
	// composeProjectLabel and composeVolumeLabel are how Compose knows a
	// volume as its project's own.
	composeProjectLabel = "com.docker.compose.project"
	composeVolumeLabel  = "com.docker.compose.volume"
	// runFolder holds a project's run folders, one per sandbox run.
	runFolder = "run"
	// unknownStart marks a run whose start no label records.
	unknownStart     = int64(-1)
	labelPairs       = ","
	labelSeparator   = "="
	rootlessOption   = "name=rootless"
	cgroupNone       = "none"
	cgroupVersionTwo = "2"
	runscRuntime     = "runsc"
)

// ErrRunscMissing stops a gVisor run on a daemon that cannot give it one.
var ErrRunscMissing = errors.New("sandbox tier runsc, but this daemon has no runsc runtime: install gVisor and register it in the daemon's daemon.json")

// resourceKinds are the docker commands that list each kind of resource a
// run creates.
var resourceKinds = [][]string{{"ps", "-a"}, {"volume", "ls"}, {"network", "ls"}}

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

// Validate runs the stack and judges it. A lease marks the run as alive
// before it writes anything. The project is always taken down with its
// volumes, and its files, secrets included, are deleted, even when ctx is
// cancelled or preparing fails; a failed teardown is reported and keeps the
// lease, unlocked, so the janitor retries it.
func (sandbox *Sandbox) Validate(ctx context.Context, input Input) (verdict Verdict, err error) {
	started := sandbox.now()
	name, err := projectName(input.Project)
	if err != nil {
		return Verdict{}, err
	}
	directory := filepath.Join(sandbox.root, slug(input.Project), runFolder, name)
	output := newLiveOutput(input.Live, input.Secrets, sandbox.now)
	infoCtx, cancelInfo := context.WithTimeout(ctx, commandTimeout)
	environment, err := sandbox.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return Verdict{}, err
	}
	if err := environment.MissingRuntime(); err != nil {
		return Verdict{}, err
	}
	lease, err := sandbox.takeLease(name)
	if err != nil {
		return Verdict{}, err
	}
	released := false
	defer func() { err = errors.Join(err, endLease(lease, released)) }()
	project, staged, err := sandbox.prepare(ctx, directory, name, input, started, environment.probesEarly())
	if err != nil {
		released = true
		return Verdict{}, errors.Join(err, os.RemoveAll(directory))
	}
	pull := pullAlways
	if everyImagePinned(project) {
		pull = pullMissing
	}
	output.say("project %s: restart \"no\", no-new-privileges, %s, published ports on 127.0.0.1 only", name, limitsText(sandbox.limits, environment))
	if note := platformNote(sandbox.platform); note != "" {
		output.say("%s", note)
	}
	withheld := Withheld(project)
	if len(withheld) > 0 {
		output.say("withheld from the run, so untested: %s", strings.Join(withheld, "; "))
	}
	defer func() {
		teardownErr := sandbox.teardown(directory, name)
		err, released = errors.Join(err, teardownErr), teardownErr == nil
		output.removed(name, teardownErr)
	}()
	if staged {
		if err := sandbox.fillFiles(ctx, directory, name, started); err != nil {
			return Verdict{}, err
		}
	}
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
	afterCtx, cancelAfter := context.WithTimeout(ctx, commandTimeout)
	defer cancelAfter()
	statesOut, _, statesErr := sandbox.compose(afterCtx, directory, nil, name, "ps", "-a", "--format", "json")
	logsOut, _, _ := sandbox.compose(afterCtx, directory, nil, name, "logs", "--no-color", "-t", "--since", started.UTC().Format(time.RFC3339))
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
	sandbox.healthOutputs(afterCtx, states, input.Secrets, output)
	logs := scrub(string(logsOut), input.Secrets)
	startOutput := scrub(string(upOut)+string(upErrOut), input.Secrets)
	if errors.Is(upCtx.Err(), context.DeadlineExceeded) {
		startOutput += "\n" + timeoutMessage + " " + sandbox.timeout.String()
	}
	verdict = judge(states, upErr, startOutput, logs, input.CompletedOK, compileBenign(input.Benign))
	verdict = reconcile(verdict, slices.Sorted(maps.Keys(project.Services)), input.CompletedOK)
	if verdict, err = sandbox.reach(ctx, directory, name, input, verdict, output); err != nil {
		return Verdict{}, err
	}
	verdict = sandbox.stillUp(ctx, directory, name, input, verdict)
	verdict.Project, verdict.Iteration, verdict.Duration = name, input.Iteration, sandbox.now().Sub(started)
	verdict.Logs, verdict.StartOutput, verdict.LogTails = logs, startOutput, Tails(logs)
	verdict.Sandbox, verdict.Withheld = environment, withheld
	output.say("%s", verdictLine(verdict))
	return verdict, nil
}

// stillUp looks at every container again once the checks are done, so a
// service that crashed or turned unhealthy while they ran fails the run
// instead of passing on what it showed a moment before.
func (sandbox *Sandbox) stillUp(ctx context.Context, directory, name string, input Input, verdict Verdict) Verdict {
	if !verdict.Passed() {
		return verdict
	}
	checkCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	statesOut, _, statesErr := sandbox.compose(checkCtx, directory, nil, name, "ps", "-a", "--format", "json")
	states, parseErr := parseStates(statesOut)
	if statesErr != nil || parseErr != nil {
		return fail(verdict, ReasonStart, "could not read the containers again after the checks")
	}
	later := judge(states, nil, "", "", input.CompletedOK, nil)
	if !later.Passed() {
		return fail(verdict, later.Reason, later.Detail+" after the checks")
	}
	return verdict
}

// limitsText says what each service may use, or, when the daemon cannot
// enforce limits, that none is: a run is never called capped when it is not.
func limitsText(limits Limits, environment Environment) string {
	if !environment.LimitsEnforced {
		return "memory, CPU and process limits NOT enforced by this daemon"
	}
	return fmt.Sprintf("at most %s memory, %g CPUs and %d processes per service, one copy of each",
		units.BytesSize(float64(limits.MemoryBytes)), limits.CPUs, limits.PIDs)
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
func (sandbox *Sandbox) prepare(ctx context.Context, directory, name string, input Input, started time.Time, earlyProbes bool) (*types.Project, bool, error) {
	project, err := composefile.Load(ctx, composeFile, input.Compose, nil, false)
	if err != nil {
		return nil, false, fmt.Errorf("compose.yaml does not load: %w", err)
	}
	injected := map[string][]byte{}
	maps.Copy(injected, input.SecretFiles)
	maps.Copy(injected, input.ConfigFiles)
	override, staged, err := Override(project, input.Published, sandbox.limits, name, started.Unix(), earlyProbes, Runtime(sandbox.tier), injected)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(directory, runDirMode); err != nil {
		return nil, false, err
	}
	files := map[string][]byte{composeFile: input.Compose, overrideFile: override, envFile: input.Env}
	maps.Copy(files, input.SecretFiles)
	if err := writeRunFiles(directory, files, runFileMode); err != nil {
		return nil, false, err
	}
	if err := writeRunFiles(filepath.Join(directory, filesVolume), staged, stagedFileMode); err != nil {
		return nil, false, err
	}
	return project, len(staged) > 0, writeRunFiles(directory, input.ConfigFiles, ir.ConfigFileMode)
}

// fillFiles creates the run's files volume, labelled as the run's Compose
// project's own so down -v removes it with the rest, and copies the staged
// files into it through a stopped helper container: docker cp reads them
// where composewright runs and hands them to the daemon, which a bind
// mount from inside a container could not.
func (sandbox *Sandbox) fillFiles(ctx context.Context, directory, name string, started time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	volume, helper := name+resourceSeparator+filesVolume, name+resourceSeparator+filesHelper
	owned := []string{"--label", composeProjectLabel + "=" + name, "--label", LabelProject + "=" + name,
		"--label", LabelStarted + "=" + strconv.FormatInt(started.Unix(), 10)}
	steps := [][]string{
		append(append([]string{"volume", "create"}, owned...), "--label", composeVolumeLabel+"="+filesVolume, volume),
		append(append([]string{"create", "--name", helper}, owned...), "--volume", volume+":"+filesMountPoint, probeImage, "true"),
		{"cp", filepath.Join(directory, filesVolume) + "/.", helper + ":" + filesMountPoint},
	}
	var err error
	for _, step := range steps {
		_, stderr, stepErr := sandbox.run(ctx, "", nil, step...)
		if stepErr != nil {
			err = fmt.Errorf("handing the read-only services their files: %w: %s", stepErr, strings.TrimSpace(string(stderr)))
			break
		}
	}
	_, _, removeErr := sandbox.run(ctx, "", nil, "rm", "--force", helper)
	return errors.Join(err, removeErr)
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

// Janitor removes what runs that ended without cleaning up left behind:
// every sandbox project whose containers, volumes or networks carry the
// sandbox label, or that has a lease, unless a live run, in any process,
// holds its lease. A run from before leases is left alone until it is older
// than twice the validation timeout. Each removal also deletes the run's
// folder, its secrets included.
func (sandbox *Sandbox) Janitor(ctx context.Context) ([]string, error) {
	found, err := sandbox.labelledRuns(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range sandbox.leaseNames() {
		if _, seen := found[name]; !seen {
			found[name] = unknownStart
		}
	}
	cutoff := sandbox.now().Add(-janitorFactor * sandbox.timeout).Unix()
	var removed []string
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if !strings.HasPrefix(name, projectPrefix) || !sandbox.stale(name, found[name], cutoff) {
			continue
		}
		if err := sandbox.reap(ctx, name); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// labelledRuns are the sandbox projects that have a labelled container,
// volume or network, each with the earliest start any of them records.
func (sandbox *Sandbox) labelledRuns(ctx context.Context) (map[string]int64, error) {
	found := map[string]int64{}
	for _, kind := range resourceKinds {
		arguments := append(slices.Clone(kind), "--filter", "label="+LabelProject, "--format", "{{json .Labels}}")
		output, stderr, err := sandbox.run(ctx, "", nil, arguments...)
		if err != nil {
			return nil, fmt.Errorf("list sandbox %s: %w: %s", kind[0], err, strings.TrimSpace(string(stderr)))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			var text string
			if json.Unmarshal([]byte(line), &text) != nil {
				continue
			}
			labels := parseLabels(text)
			name := labels[LabelProject]
			started, parseErr := strconv.ParseInt(labels[LabelStarted], 10, 64)
			if parseErr != nil {
				started = unknownStart
			}
			if earlier, seen := found[name]; !seen || (started != unknownStart && (earlier == unknownStart || started < earlier)) {
				found[name] = started
			}
		}
	}
	return found, nil
}

// stale reports whether the run name is over: its lease is held by no one,
// or, for a run from before leases, it started before cutoff.
func (sandbox *Sandbox) stale(name string, started, cutoff int64) bool {
	if sandbox.leased(name) {
		return sandbox.abandoned(name)
	}
	return started != unknownStart && started < cutoff
}

// reap takes the run name down with its volumes and networks, deletes its
// folder whether or not that worked, and drops its lease once Docker has
// nothing of it left.
func (sandbox *Sandbox) reap(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	_, stderr, downErr := sandbox.run(ctx, "", nil, "compose", "-p", name, "down", "-v", "--remove-orphans", "-t", stopTimeout)
	folders, globErr := filepath.Glob(filepath.Join(sandbox.root, "*", runFolder, name))
	removeErr := errors.Join(globErr)
	for _, folder := range folders {
		removeErr = errors.Join(removeErr, os.RemoveAll(folder))
	}
	if downErr != nil {
		return errors.Join(fmt.Errorf("remove stale sandbox %s: %w: %s", name, downErr, strings.TrimSpace(string(stderr))), removeErr)
	}
	leaseErr := os.Remove(sandbox.leasePath(name))
	if errors.Is(leaseErr, fs.ErrNotExist) {
		leaseErr = nil
	}
	return errors.Join(removeErr, leaseErr)
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
// timeZoneFormat prints, for each container, its sandbox label and the
// value of its TZ variable alone: the rest of its environment never leaves
// the daemon.
const timeZoneFormat = `{{index .Config.Labels "` + LabelProject + `"}}` + timeZoneSeparator +
	`{{range .Config.Env}}{{if eq (index (split . "=") 0) "TZ"}}{{index (split . "=") 1}}{{end}}{{end}}`

// timeZoneSeparator parts a container's sandbox label from its time zone.
const timeZoneSeparator = "|"

// TimeZones are the TZ values the running containers set, one for each
// container that sets one; sandbox runs are left out.
func (sandbox *Sandbox) TimeZones(ctx context.Context) ([]string, error) {
	listed, stderr, err := sandbox.run(ctx, "", nil, "ps", "-q")
	if err != nil {
		return nil, fmt.Errorf("list containers: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	identifiers := strings.Fields(string(listed))
	if len(identifiers) == 0 {
		return nil, nil
	}
	output, stderr, err := sandbox.run(ctx, "", nil, append([]string{"inspect", "--format", timeZoneFormat}, identifiers...)...)
	if err != nil {
		return nil, fmt.Errorf("read time zones: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var zones []string
	for _, line := range strings.Split(string(output), "\n") {
		label, zone, _ := strings.Cut(strings.TrimSpace(line), timeZoneSeparator)
		if label == "" && zone != "" {
			zones = append(zones, zone)
		}
	}
	return zones, nil
}

func (sandbox *Sandbox) Networks(ctx context.Context) ([]string, error) {
	output, stderr, err := sandbox.run(ctx, "", nil, "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("list networks: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.Fields(string(output)), nil
}
