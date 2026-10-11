package sandbox

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"
)

const (
	LabelProject    = "io.composewright.sandbox"
	LabelStarted    = "io.composewright.started"
	tagOverride     = "!override"
	tagReset        = "!reset"
	sandboxVolume   = "cw-bind-"
	noNewPrivileges = "no-new-privileges:true"
	logDriver       = "json-file"
	logMaxSize      = "10m"
	logMaxFile      = "3"
	restartNever    = "no"
	defaultNetwork  = "default"
	loopbackAddress = "127.0.0.1"
	volumeTypeBind  = "bind"
	readOnlySuffix  = ":ro"
	allCapabilities = "ALL"
	yamlTrue        = "true"
	yamlIndent      = 2
	// earlyProbe is how often a healthcheck runs while an app starts, so a
	// run ends seconds after the app is ready instead of an interval later.
	earlyProbe          = 2 * time.Second
	startIntervalEngine = 25
	dockerInterval      = 30 * time.Second
	dockerRetries       = 3
)

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
//   - keeps only no-new-privileges and caps memory, CPUs and processes,
//   - rebinds every published port to an ephemeral loopback port,
//   - leaves external proxy networks and host bind mounts out of the run,
//   - labels containers so the janitor can find stale runs,
//   - with earlyProbes, runs healthchecks every two seconds while the app
//     starts, for at least as long as its own settings tolerate failures,
//   - adds the reachability probe's service, which only `run` starts.
//
// published lists, per service, the "port/protocol" the plan publishes; they
// are bound on loopback even when the user's file reaches the app through a
// proxy network instead.
func Override(project *types.Project, published map[string][]string, limits Limits, sandboxName string, started int64, earlyProbes bool) ([]byte, error) {
	services := mapping()
	volumes := mapping()
	for _, name := range sortedServices(project) {
		service := project.Services[name]
		node := serviceOverride(service, published[name], limits, sandboxName, started, volumes)
		if probes := startProbes(service.HealthCheck); earlyProbes && probes != nil {
			node.Content = append(node.Content, scalar("healthcheck"), probes)
		}
		services.Content = append(services.Content, scalar(name), node)
	}
	services.Content = append(services.Content, scalar(probeService), probeOverride(sandboxName, started))
	root := mapping(scalar("services"), services)
	if networks := externalNetworks(project); len(networks.Content) > 0 {
		root.Content = append(root.Content, scalar("networks"), networks)
	}
	if len(volumes.Content) > 0 {
		root.Content = append(root.Content, scalar("volumes"), volumes)
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(yamlIndent)
	if err := encoder.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), encoder.Close()
}

func serviceOverride(service types.ServiceConfig, published []string, limits Limits, sandboxName string, started int64, volumes *yaml.Node) *yaml.Node {
	node := mapping(
		scalar("restart"), quoted(restartNever),
		scalar("security_opt"), tagged(tagOverride, sequence(quoted(noNewPrivileges))),
		scalar("mem_limit"), scalar(strconv.FormatInt(capped(int64(service.MemLimit), limits.MemoryBytes), 10)),
		scalar("cpus"), quoted(strconv.FormatFloat(cappedFloat(float64(service.CPUS), limits.CPUs), 'f', -1, 64)),
		scalar("pids_limit"), scalar(strconv.FormatInt(capped(service.PidsLimit, limits.PIDs), 10)),
		scalar("labels"), mapping(scalar(LabelProject), quoted(sandboxName), scalar(LabelStarted), quoted(strconv.FormatInt(started, 10))),
		scalar("logging"), tagged(tagOverride, mapping(scalar("driver"), scalar(logDriver),
			scalar("options"), mapping(scalar("max-size"), quoted(logMaxSize), scalar("max-file"), quoted(logMaxFile)))),
	)
	if service.NetworkMode == "" {
		node.Content = append(node.Content, scalar("ports"), tagged(tagOverride, loopbackPorts(published)))
		node.Content = append(node.Content, scalar("networks"), tagged(tagOverride, sequence(scalar(defaultNetwork))))
	}
	if mounts := sandboxMounts(service, volumes); mounts != nil {
		node.Content = append(node.Content, scalar("volumes"), tagged(tagOverride, mounts))
	}
	return node
}

// probeOverride is the reachability probe's service: in a profile of its
// own, read-only, without capabilities, as nobody, with small limits and
// the sandbox labels, on the sandbox network.
func probeOverride(sandboxName string, started int64) *yaml.Node {
	node := serviceOverride(types.ServiceConfig{Name: probeService}, nil, probeLimits, sandboxName, started, mapping())
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
func sandboxMounts(service types.ServiceConfig, volumes *yaml.Node) *yaml.Node {
	hasBind := false
	for _, volume := range service.Volumes {
		hasBind = hasBind || volume.Type == volumeTypeBind
	}
	if !hasBind {
		return nil
	}
	mounts := sequence()
	for index, volume := range service.Volumes {
		mounts.Content = append(mounts.Content, sandboxMount(service.Name, index, volume, volumes))
	}
	return mounts
}

// sandboxMount writes one mount as source:target, read-only when it was.
// A tmpfs or an anonymous volume has no source, so it takes the long form.
func sandboxMount(serviceName string, index int, volume types.ServiceVolumeConfig, volumes *yaml.Node) *yaml.Node {
	source := volume.Source
	if volume.Type == volumeTypeBind {
		source = sandboxVolume + serviceName + "-" + strconv.Itoa(index)
		volumes.Content = append(volumes.Content, scalar(source), &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle})
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

// externalNetworks resets every external network so the sandbox never joins
// the user's real proxy or tunnel network.
func externalNetworks(project *types.Project) *yaml.Node {
	networks := mapping()
	names := make([]string, 0, len(project.Networks))
	for name, network := range project.Networks {
		if network.External {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		networks.Content = append(networks.Content, scalar(name), &yaml.Node{Kind: yaml.ScalarNode, Tag: tagReset, Value: "null"})
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
