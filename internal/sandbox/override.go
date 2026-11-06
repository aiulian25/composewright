package sandbox

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/ir"
)

const (
	LabelProject  = "io.composewright.sandbox"
	LabelStarted  = "io.composewright.started"
	tagOverride   = "!override"
	tagReset      = "!reset"
	sandboxVolume = "cw-bind-"
	// bundledPrefix starts the source of a file bundled beside compose.yaml.
	bundledPrefix  = relativePrefix + ir.ConfigDirectory + "/"
	relativePrefix = "./"
	// secretsMount is where Compose mounts secrets in a container, and
	// injectedPrefix names the configs a run hands its services.
	secretsMount   = "/run/secrets"
	injectedPrefix = "cw-file-"
	// filesVolume is the run's volume that holds what a read-only service
	// is handed, which Compose cannot copy into it as a config.
	filesVolume      = "cw-run-files"
	emptyFile        = "/dev/null"
	noNewPrivileges  = "no-new-privileges:true"
	logDriver        = "json-file"
	logMaxSize       = "10m"
	logMaxFile       = "3"
	restartNever     = "no"
	defaultNetwork   = "default"
	loopbackAddress  = "127.0.0.1"
	volumeTypeBind   = "bind"
	volumeTypeVolume = "volume"
	readOnlySuffix   = ":ro"
	allCapabilities  = "ALL"
	yamlTrue         = "true"
	yamlFalse        = "false"
	yamlNull         = "null"
	singleCopy       = "1"
	// resourceSeparator joins a run's project and a resource key, as
	// Compose names the resources it scopes to a project.
	resourceSeparator = "_"
	yamlIndent        = 2
	// earlyProbe is how often a healthcheck runs while an app starts, so a
	// run ends seconds after the app is ready instead of an interval later.
	earlyProbe          = 2 * time.Second
	startIntervalEngine = 25
	dockerInterval      = 30 * time.Second
	dockerRetries       = 3
)

// owner is the run a sandbox resource belongs to: its project and when it
// started, labelled on every container, volume and network it creates, so
// the janitor finds whatever an interrupted run left.
type owner struct {
	name    string
	started int64
}

func (run owner) labels() *yaml.Node {
	return mapping(scalar(LabelProject), quoted(run.name), scalar(LabelStarted), quoted(strconv.FormatInt(run.started, 10)))
}

// resource renames a volume or network the run creates to one scoped by
// its project and labels it as the run's: an explicit name is global on
// the daemon, so it could attach, and down -v delete, real data.
func (run owner) resource(owned *yaml.Node, key string) {
	owned.Content = append(owned.Content, scalar(key), mapping(scalar("name"), quoted(run.name+resourceSeparator+key), scalar("labels"), run.labels()))
}

// reservedNames refuses a file that uses a name the run gives its own
// helpers, which the override would otherwise merge into the user's.
func reservedNames(project *types.Project) error {
	if _, taken := project.Volumes[filesVolume]; taken {
		return fmt.Errorf("volume name %s is reserved for the files the sandbox hands a read-only service", filesVolume)
	}
	if _, taken := project.Services[probeService]; taken {
		return fmt.Errorf("service name %s is reserved for the sandbox's reachability probe", probeService)
	}
	for key := range project.Volumes {
		if strings.HasPrefix(key, sandboxVolume) {
			return fmt.Errorf("volume name %s is reserved: names starting with %s are the sandbox's", key, sandboxVolume)
		}
	}
	return nil
}

// Limits are the per-service ceilings for a sandbox run.
type Limits struct {
	MemoryBytes int64
	CPUs        float64
	PIDs        int64
}

// Override builds the file layered on the user's compose.yaml for a
// sandbox run. It reads the compose model of the rendered file, so the run
// always validates exactly what the user deploys, and it:
//   - disables restarts so a crash loop surfaces as an exited container,
//   - keeps only no-new-privileges and caps memory, shared memory, CPUs
//     and processes,
//   - rebinds every published port to an ephemeral loopback port,
//   - leaves external proxy networks and host bind mounts out of the run,
//   - names every volume and network it creates after the run, labelled as
//     its own, and drops container_name, so it never reuses a real one,
//   - labels containers so the janitor can find stale runs,
//   - with earlyProbes, runs healthchecks every two seconds while the app
//     starts, for at least as long as its own settings tolerate failures,
//   - adds the reachability probe's service, which only `run` starts,
//   - with a runtime, runs every container, the probe too, under it.
//
// published lists, per service, the "port/protocol" the plan publishes; they
// are bound on loopback even when the user's file reaches the app through a
// proxy network instead. files are the secrets and bundled config files
// beside compose.yaml, by path, which the run hands the services as configs.
func Override(project *types.Project, published map[string][]string, limits Limits, sandboxName string, started int64, earlyProbes bool, runtime string,
	files map[string][]byte) ([]byte, map[string][]byte, error) {
	if err := reservedNames(project); err != nil {
		return nil, nil, err
	}
	run := owner{name: sandboxName, started: started}
	services := mapping()
	volumes := mapping()
	for _, key := range slices.Sorted(maps.Keys(project.Volumes)) {
		if !project.Volumes[key].External {
			run.resource(volumes, key)
		}
	}
	configs := mapping()
	staged := map[string][]byte{}
	for _, name := range sortedServices(project) {
		service := project.Services[name]
		node := serviceOverride(service, published[name], limits, run, volumes)
		injectFiles(project, service, files, node, configs, staged)
		if probes := startProbes(service.HealthCheck); earlyProbes && probes != nil {
			node.Content = append(node.Content, scalar("healthcheck"), probes)
		}
		services.Content = append(services.Content, scalar(name), node)
	}
	services.Content = append(services.Content, scalar(probeService), probeOverride(run))
	if runtime != "" {
		for index := 1; index < len(services.Content); index += 2 {
			service := services.Content[index]
			service.Content = append(service.Content, scalar("runtime"), quoted(runtime))
		}
	}
	root := mapping(scalar("services"), services)
	if networks := sandboxNetworks(project, run); len(networks.Content) > 0 {
		root.Content = append(root.Content, scalar("networks"), networks)
	}
	if len(staged) > 0 {
		run.resource(volumes, filesVolume)
	}
	if len(volumes.Content) > 0 {
		root.Content = append(root.Content, scalar("volumes"), volumes)
	}
	if len(configs.Content) > 0 {
		root.Content = append(root.Content, scalar("configs"), configs)
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(yamlIndent)
	if err := encoder.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return nil, nil, err
	}
	return buffer.Bytes(), staged, encoder.Close()
}

// withholdPrivileges are the keys that take from a test run the host
// privileges the user granted for the deployment: privileged mode, host
// networking, devices and every capability Docker does not give a plain
// container. Final consent never makes a throw-away run safe.
func withholdPrivileges(service types.ServiceConfig) []*yaml.Node {
	var keys []*yaml.Node
	if service.Privileged {
		keys = append(keys, scalar("privileged"), scalar(yamlFalse))
	}
	if service.NetworkMode != "" {
		keys = append(keys, scalar("network_mode"), tagged(tagReset, scalar(yamlNull)))
	}
	if len(service.Devices) > 0 {
		keys = append(keys, scalar("devices"), tagged(tagReset, sequence()))
	}
	kept := sequence()
	for _, capability := range service.CapAdd {
		if ir.DefaultCapability(capability) {
			kept.Content = append(kept.Content, quoted(capability))
		}
	}
	if len(kept.Content) < len(service.CapAdd) {
		keys = append(keys, scalar("cap_add"), tagged(tagOverride, kept))
	}
	return keys
}

// singleReplica runs one copy of a service the file scales, so the run's
// per-container limits are its whole budget.
func singleReplica(service types.ServiceConfig) []*yaml.Node {
	var keys []*yaml.Node
	if service.Scale != nil {
		keys = append(keys, scalar("scale"), scalar(singleCopy))
	}
	if service.Deploy != nil && service.Deploy.Replicas != nil {
		keys = append(keys, scalar("deploy"), mapping(scalar("replicas"), scalar(singleCopy)))
	}
	return keys
}

// Withheld names, per service, each host privilege the compose file grants
// that a test run goes without, and each scaled service it runs one copy
// of, so a pass says what it never exercised.
func Withheld(project *types.Project) []string {
	var withheld []string
	for _, name := range sortedServices(project) {
		service := project.Services[name]
		if service.Privileged {
			withheld = append(withheld, name+": privileged mode")
		}
		if service.NetworkMode != "" {
			withheld = append(withheld, name+": network_mode "+service.NetworkMode)
		}
		for _, device := range service.Devices {
			withheld = append(withheld, name+": device "+device.Source)
		}
		for _, capability := range service.CapAdd {
			if !ir.DefaultCapability(capability) {
				withheld = append(withheld, name+": capability "+capability)
			}
		}
		if replicas := service.GetScale(); replicas > 1 {
			withheld = append(withheld, fmt.Sprintf("%s: %d copies, tested as one", name, replicas))
		}
		for _, volume := range service.Volumes {
			if volume.Source == ir.DockerSocket {
				withheld = append(withheld, name+": the Docker socket")
			}
		}
	}
	return withheld
}

func serviceOverride(service types.ServiceConfig, published []string, limits Limits, run owner, volumes *yaml.Node) *yaml.Node {
	node := mapping(
		scalar("restart"), quoted(restartNever),
		scalar("security_opt"), tagged(tagOverride, sequence(quoted(noNewPrivileges))),
		scalar("mem_limit"), scalar(strconv.FormatInt(capped(int64(service.MemLimit), limits.MemoryBytes), 10)),
		scalar("cpus"), quoted(strconv.FormatFloat(cappedFloat(float64(service.CPUS), limits.CPUs), 'f', -1, 64)),
		scalar("pids_limit"), scalar(strconv.FormatInt(capped(service.PidsLimit, limits.PIDs), 10)),
		scalar("labels"), run.labels(),
		scalar("logging"), tagged(tagOverride, mapping(scalar("driver"), scalar(logDriver),
			scalar("options"), mapping(scalar("max-size"), quoted(logMaxSize), scalar("max-file"), quoted(logMaxFile)))),
	)
	if service.ShmSize > 0 {
		node.Content = append(node.Content, scalar("shm_size"), scalar(strconv.FormatInt(capped(int64(service.ShmSize), limits.MemoryBytes), 10)))
	}
	node.Content = append(node.Content, withholdPrivileges(service)...)
	node.Content = append(node.Content, singleReplica(service)...)
	if service.ContainerName != "" {
		node.Content = append(node.Content, scalar("container_name"), tagged(tagReset, scalar(yamlNull)))
	}
	node.Content = append(node.Content, scalar("ports"), tagged(tagOverride, loopbackPorts(published)))
	node.Content = append(node.Content, scalar("networks"), tagged(tagOverride, sequence(scalar(defaultNetwork))))
	if mounts := sandboxMounts(service, volumes, run); mounts != nil {
		node.Content = append(node.Content, scalar("volumes"), tagged(tagOverride, mounts))
	}
	return node
}

// probeOverride is the reachability probe's service: in a profile of its
// own, read-only, without capabilities, as nobody, with small limits and
// the sandbox labels, on the sandbox network.
func probeOverride(run owner) *yaml.Node {
	node := serviceOverride(types.ServiceConfig{Name: probeService}, nil, probeLimits, run, mapping())
	node.Content = append(node.Content,
		scalar("image"), quoted(probeImage),
		scalar("profiles"), sequence(quoted(probeService)),
		scalar("user"), quoted(probeUser),
		scalar("read_only"), scalar(yamlTrue),
		scalar("cap_drop"), sequence(scalar(allCapabilities)),
	)
	return node
}

// startProbes keeps the app's healthcheck and only changes when it runs:
// every two seconds during a start period as long as the failures its own
// interval and retries tolerate, so success is seen at once and no app is
// judged sooner than its settings allow. A failing app is reported after
// that period plus its usual retries.
func startProbes(check *types.HealthCheckConfig) *yaml.Node {
	if check == nil || check.Disable || len(check.Test) == 0 || check.Test[0] == "NONE" {
		return nil
	}
	interval, retries, period := dockerInterval, uint64(dockerRetries), time.Duration(0)
	if check.Interval != nil {
		interval = time.Duration(*check.Interval)
	}
	if check.Retries != nil {
		retries = *check.Retries
	}
	if check.StartPeriod != nil {
		period = time.Duration(*check.StartPeriod)
	}
	period = max(period, interval*time.Duration(retries))
	return mapping(scalar("start_period"), quoted(period.String()), scalar("start_interval"), quoted(earlyProbe.String()))
}

func probeNote(environment Environment) string {
	if !environment.probesEarly() {
		return ""
	}
	return " (healthchecks run every " + earlyProbe.String() + " while it starts)"
}

func loopbackPorts(published []string) *yaml.Node {
	ports := sequence()
	for _, port := range published {
		ports.Content = append(ports.Content, quoted(loopbackAddress+":0:"+port))
	}
	return ports
}

// sandboxMounts replaces bind mounts with throw-away named volumes so a run
// never writes into the user's folders; named volumes, tmpfs mounts and
// read-only flags are kept as they are.
func sandboxMounts(service types.ServiceConfig, volumes *yaml.Node, run owner) *yaml.Node {
	hasBind := false
	for _, volume := range service.Volumes {
		hasBind = hasBind || volume.Type == volumeTypeBind
	}
	if !hasBind {
		return nil
	}
	mounts := sequence()
	for index, volume := range service.Volumes {
		if _, bundled := bundledPath(volume); !bundled {
			mounts.Content = append(mounts.Content, sandboxMount(service.Name, index, volume, volumes, run))
		}
	}
	return mounts
}

// bundledPath is the path beside compose.yaml of a file bundled with the
// stack that a volume mounts.
func bundledPath(volume types.ServiceVolumeConfig) (string, bool) {
	if volume.Type != volumeTypeBind || !strings.HasPrefix(volume.Source, bundledPrefix) {
		return "", false
	}
	return strings.TrimPrefix(volume.Source, relativePrefix), true
}

// injectFiles hands a service the secrets and bundled files beside
// compose.yaml without a bind mount, which a containerized composewright
// could not make: as configs Compose copies in, or, for a read-only service,
// which Compose cannot copy into, from the run's files volume, each file by
// its subpath. staged collects what that volume must hold.
func injectFiles(project *types.Project, service types.ServiceConfig, files map[string][]byte, node, configs *yaml.Node, staged map[string][]byte) {
	hand := func(content []byte, target string) *yaml.Node {
		if service.ReadOnly {
			return stagedMount(staged, content, target)
		}
		return injected(configs, content, target)
	}
	mounts := sequence()
	for _, volume := range service.Volumes {
		if path, bundled := bundledPath(volume); bundled {
			mounts.Content = append(mounts.Content, hand(files[path], volume.Target))
		}
	}
	secrets := sequence()
	for _, secret := range service.Secrets {
		content, found := files[strings.TrimPrefix(project.Secrets[secret.Source].File, relativePrefix)]
		if !found {
			secrets = nil
			break
		}
		secrets.Content = append(secrets.Content, hand(content, secretTarget(secret)))
	}
	if secrets != nil && len(secrets.Content) > 0 {
		node.Content = append(node.Content, scalar("secrets"), tagged(tagReset, sequence()))
		mounts.Content = append(mounts.Content, secrets.Content...)
	}
	if len(mounts.Content) == 0 {
		return
	}
	if service.ReadOnly {
		appendVolumes(node, mounts)
		return
	}
	node.Content = append(node.Content, scalar("configs"), mounts)
}

// stagedMount mounts one file of the run's files volume read-only at
// target, and stages content for it.
func stagedMount(staged map[string][]byte, content []byte, target string) *yaml.Node {
	name := injectedPrefix + strconv.Itoa(len(staged))
	staged[name] = content
	return mapping(scalar("type"), scalar(volumeTypeVolume), scalar("source"), scalar(filesVolume), scalar("target"), quoted(target),
		scalar("read_only"), scalar(yamlTrue), scalar("volume"), mapping(scalar("subpath"), quoted(name)))
}

// appendVolumes adds mounts to the service's volumes in its override, or
// as its volumes, which Compose merges with the file's by target.
func appendVolumes(node, mounts *yaml.Node) {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == "volumes" {
			node.Content[index+1].Content = append(node.Content[index+1].Content, mounts.Content...)
			return
		}
	}
	node.Content = append(node.Content, scalar("volumes"), mounts)
}

// secretTarget is where Compose mounts a secret in the container.
func secretTarget(secret types.ServiceSecretConfig) string {
	target := cmp.Or(secret.Target, secret.Source)
	if path.IsAbs(target) {
		return target
	}
	return path.Join(secretsMount, target)
}

// injected adds a config holding content and mounts it at target; a $ is
// doubled, since Compose interpolates the override. Compose reads empty
// content as none and binds its project folder instead, so an empty file,
// such as a secret not supplied yet, is the host's /dev/null.
func injected(configs *yaml.Node, content []byte, target string) *yaml.Node {
	name := injectedPrefix + strconv.Itoa(len(configs.Content)/2)
	source := mapping(scalar("content"), quoted(strings.ReplaceAll(string(content), "$", "$$")))
	if len(content) == 0 {
		source = mapping(scalar("file"), quoted(emptyFile))
	}
	configs.Content = append(configs.Content, scalar(name), source)
	return mapping(scalar("source"), scalar(name), scalar("target"), quoted(target))
}

// sandboxMount writes one mount as source:target, read-only when it was.
// A tmpfs or an anonymous volume has no source, so it takes the long form.
// Any bind becomes a throw-away volume.
func sandboxMount(serviceName string, index int, volume types.ServiceVolumeConfig, volumes *yaml.Node, run owner) *yaml.Node {
	source := volume.Source
	if volume.Type == volumeTypeBind {
		source = sandboxVolume + serviceName + "-" + strconv.Itoa(index)
		run.resource(volumes, source)
	}
	if source == "" {
		mount := mapping(scalar("type"), scalar(volume.Type), scalar("target"), quoted(volume.Target))
		if volume.ReadOnly {
			mount.Content = append(mount.Content, scalar("read_only"), scalar(yamlTrue))
		}
		return mount
	}
	mount := source + ":" + volume.Target
	if volume.ReadOnly {
		mount += readOnlySuffix
	}
	return quoted(mount)
}

// sandboxNetworks resets every external network so the sandbox never joins
// the user's real proxy or tunnel network, and gives every other network a
// name of the run's own.
func sandboxNetworks(project *types.Project, run owner) *yaml.Node {
	networks := mapping()
	for _, name := range slices.Sorted(maps.Keys(project.Networks)) {
		if project.Networks[name].External {
			networks.Content = append(networks.Content, scalar(name), tagged(tagReset, scalar(yamlNull)))
			continue
		}
		run.resource(networks, name)
	}
	if _, declared := project.Networks[defaultNetwork]; !declared {
		run.resource(networks, defaultNetwork)
	}
	return networks
}

func capped(value, ceiling int64) int64 {
	if value <= 0 || value > ceiling {
		return ceiling
	}
	return value
}

func cappedFloat(value, ceiling float64) float64 {
	if value <= 0 || value > ceiling {
		return ceiling
	}
	return value
}

// ParseLimits reads the config's sandbox ceilings.
func ParseLimits(memory string, cpus float64, pids int) (Limits, error) {
	bytes, err := units.RAMInBytes(memory)
	if err != nil {
		return Limits{}, fmt.Errorf("docker.validate.memory_limit %q: %w", memory, err)
	}
	return Limits{MemoryBytes: bytes, CPUs: cpus, PIDs: int64(pids)}, nil
}

func sortedServices(project *types.Project) []string {
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func mapping(content ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Content: content}
}

func sequence(content ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Content: content}
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

func quoted(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value, Style: yaml.DoubleQuotedStyle}
}

func tagged(tag string, node *yaml.Node) *yaml.Node {
	node.Tag = tag
	return node
}
