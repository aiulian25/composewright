// Package render writes compose.yaml, .env and DEPLOY.md from a plan and the
// environment profile. It is the only package that writes compose YAML.
package render

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/rules"
)

const (
	ExposeLocal = "local"
	ExposeLAN   = "lan"
	ExposeNone  = "none"

	loopback        = "127.0.0.1"
	defaultNetwork  = "default"
	logDriver       = "json-file"
	noNewPrivileges = "no-new-privileges:true"
	providerTraefik = "traefik"
	providerNPM     = "npm"
	keyLabels       = "labels"
	keyExpose       = "expose"
)

var exposeModes = []string{ExposeLocal, ExposeLAN, ExposeNone}

// Options are the choices the review step makes on top of the plan.
type Options struct {
	Profile config.Profile
	// ExposeVia is how the app's ports are published: local, lan or none.
	ExposeVia string
	// Networks are the external networks from the profile the app joins.
	Networks     []string
	Storage      string
	GrantedNeeds []string
	Header       string
}

// Exposure applies the defaults to a choice: with nothing chosen, the app
// joins the profile's default networks and publishes no port, or publishes
// on loopback when the profile has none. A network name as the mode is the
// older way to choose that one network.
func Exposure(profile config.Profile, mode string, networks []string) (string, []string) {
	if _, isNetwork := profile.Network(mode); isNetwork {
		networks, mode = append(slices.Clone(networks), mode), ExposeNone
	}
	if mode != "" {
		return mode, networks
	}
	if len(networks) == 0 {
		networks = profile.DefaultNetworks()
	}
	if len(networks) > 0 {
		return ExposeNone, networks
	}
	return ExposeLocal, networks
}

// Resolve fills defaults (see Exposure, and the profile's storage style)
// and checks that every network is in the profile.
func (options Options) Resolve() (Options, error) {
	if options.Storage == "" {
		options.Storage = options.Profile.Storage.Style
	}
	if options.Storage != config.StorageVolumes && options.Storage != config.StorageBind {
		return options, fmt.Errorf("--storage %q must be volumes or bind", options.Storage)
	}
	if options.Storage == config.StorageBind && options.Profile.Storage.BasePath == "" {
		return options, fmt.Errorf("--storage bind needs profile.storage.base_path in the config")
	}
	options.ExposeVia, options.Networks = Exposure(options.Profile, options.ExposeVia, options.Networks)
	if !slices.Contains(exposeModes, options.ExposeVia) {
		return options, fmt.Errorf("--expose-via %q is neither local, lan, none nor a network in profile.networks", options.ExposeVia)
	}
	for _, name := range options.Networks {
		if _, found := options.Profile.Network(name); !found {
			return options, fmt.Errorf("network %q is not in profile.networks", name)
		}
	}
	return options, nil
}

// networks are the chosen networks in the order the profile lists them,
// each once.
func (options Options) networks() []config.ProfileNetwork {
	var chosen []config.ProfileNetwork
	for _, network := range options.Profile.Networks {
		if slices.Contains(options.Networks, network.Name) {
			chosen = append(chosen, network)
		}
	}
	return chosen
}

// Compose renders compose.yaml. Values never appear in it: every variable is
// a ${REFERENCE} into .env.
func Compose(plan *ir.IR, options Options) ([]byte, error) {
	services := mapping()
	volumes := mapping()
	for index := range plan.Services {
		service := &plan.Services[index]
		services.Content = append(services.Content, key(service.Name), options.service(plan, service, volumes))
	}
	root := mapping(key("name"), scalar(plan.App.Name), key("services"), services)
	root.HeadComment = options.Header
	if len(volumes.Content) > 0 {
		root.Content = append(root.Content, key("volumes"), volumes)
	}
	if networks := options.networks(); len(networks) > 0 {
		external := mapping()
		for _, network := range networks {
			external.Content = append(external.Content, key(network.Name), mapping(key("external"), boolean(true)))
		}
		root.Content = append(root.Content, key("networks"), external)
	}
	return encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}})
}

func (options Options) service(plan *ir.IR, service *ir.Service, volumes *yaml.Node) *yaml.Node {
	node := mapping(key("image"), scalar(service.Image.String()))
	hostNetwork := options.granted(plan, service, ir.NeedHostNetwork)
	options.needs(plan, service, node)
	if len(service.Entrypoint) > 0 {
		node.Content = append(node.Content, key("entrypoint"), flowSequence(service.Entrypoint...))
	}
	if len(service.Command) > 0 {
		node.Content = append(node.Content, key("command"), flowSequence(service.Command...))
	}
	if service.User != "" {
		node.Content = append(node.Content, key("user"), quoted(service.User))
	}
	node.Content = append(node.Content, key("restart"), scalar(options.Profile.Defaults.Restart))
	if dependencies := dependsOn(service); dependencies != nil {
		node.Content = append(node.Content, key("depends_on"), dependencies)
	}
	if environment := environment(service); environment != nil {
		node.Content = append(node.Content, key("environment"), environment)
	}
	isApp := service.Role == ir.RoleApp
	if !hostNetwork {
		options.publishing(plan, service, isApp, node)
	}
	if mounts := options.mounts(plan, service, volumes); mounts != nil {
		node.Content = append(node.Content, key("volumes"), mounts)
	}
	if healthcheck := healthcheckNode(service.Healthcheck); healthcheck != nil {
		node.Content = append(node.Content, key("healthcheck"), healthcheck)
	}
	node.Content = append(node.Content, key("security_opt"), flowSequence(noNewPrivileges))
	options.limits(service, node)
	node.Content = append(node.Content, key("logging"), mapping(key("driver"), scalar(logDriver), key("options"),
		mapping(key("max-size"), scalar(options.Profile.Defaults.LogMaxSize), key("max-file"), quoted(strconv.Itoa(options.Profile.Defaults.LogMaxFile)))))
	return node
}

// publishing decides how the app is reached: through the external proxy
// and tunnel networks it joins (app service only; databases never join
// them), and by ports published on loopback, the LAN or not at all.
func (options Options) publishing(plan *ir.IR, service *ir.Service, isApp bool, node *yaml.Node) {
	if isApp {
		options.join(plan, service, node)
	}
	if isApp && options.ExposeVia == ExposeNone {
		return
	}
	var ports []*yaml.Node
	for _, port := range service.Ports {
		if !port.Publish {
			continue
		}
		hostIP := loopback + ":"
		if isApp && options.ExposeVia == ExposeLAN {
			hostIP = ""
		}
		ports = append(ports, quoted(fmt.Sprintf("%s%d:%d/%s", hostIP, rules.HostPort(port), port.Container, port.Protocol)))
	}
	if len(ports) > 0 {
		node.Content = append(node.Content, key("ports"), sequence(ports...))
	}
}

// join puts the app on the default network and every external network
// chosen for it.
func (options Options) join(plan *ir.IR, service *ir.Service, node *yaml.Node) {
	networks := options.networks()
	if len(networks) == 0 {
		return
	}
	names := []*yaml.Node{scalar(defaultNetwork)}
	for _, network := range networks {
		names = append(names, scalar(network.Name))
	}
	field := key("networks")
	node.Content = append(node.Content, field, sequence(names...))
	field.HeadComment = strings.Join(options.providerExtras(service, networks, node, plan.App.Name), "\n")
}

// providerExtras adds what each network's provider needs to route to the
// app: Traefik labels or an exposed port for Nginx Proxy Manager. For the
// rest it returns a note on what to route where.
func (options Options) providerExtras(service *ir.Service, networks []config.ProfileNetwork, node *yaml.Node, project string) []string {
	port, found := webPort(service)
	if !found {
		return nil
	}
	var notes []string
	for _, network := range networks {
		host := options.networkHost(network, project)
		switch network.Provider {
		case providerTraefik:
			node.Content = appendOnce(node.Content, keyLabels, traefikLabels(project, host, network.Name, port))
		case providerNPM:
			node.Content = appendOnce(node.Content, keyExpose, flowSequence(strconv.Itoa(port)))
		default:
			notes = append(notes, fmt.Sprintf("%s (%s): route %s to http://%s:%d", network.Name, network.Kind, host, service.Name, port))
		}
	}
	return notes
}

func traefikLabels(router, host, network string, port int) *yaml.Node {
	return mapping(
		key("traefik.enable"), quoted("true"),
		key("traefik.docker.network"), quoted(network),
		key("traefik.http.routers."+router+".rule"), quoted("Host(`"+host+"`)"),
		key("traefik.http.services."+router+".loadbalancer.server.port"), quoted(strconv.Itoa(port)),
	)
}

// appendOnce adds a key to a mapping's content unless it is already there:
// the first network that needs a key sets it.
func appendOnce(content []*yaml.Node, name string, value *yaml.Node) []*yaml.Node {
	if hasKey(&yaml.Node{Content: content}, name) {
		return content
	}
	return append(content, key(name), value)
}

func (options Options) mounts(plan *ir.IR, service *ir.Service, volumes *yaml.Node) *yaml.Node {
	var items []*yaml.Node
	for _, volume := range service.Volumes {
		if options.Storage == config.StorageBind {
			items = append(items, quoted(BindSource(options.Profile.Storage, plan.App.Name, volume.Name)+":"+volume.ContainerPath))
			continue
		}
		items = append(items, quoted(volume.Name+":"+volume.ContainerPath))
		if !hasKey(volumes, volume.Name) {
			volumes.Content = append(volumes.Content, key(volume.Name), emptyMapping())
		}
	}
	for _, need := range plan.Needs {
		if need.Capability == ir.NeedBindMount && need.Service == service.Name && options.isGranted(ir.NeedBindMount) {
			source, target := splitMount(need.Detail)
			items = append(items, quoted(source+":"+target))
		}
	}
	if len(items) == 0 {
		return nil
	}
	return sequence(items...)
}

// BindSource expands the profile's layout template for one volume.
func BindSource(storage config.Storage, project, volume string) string {
	return strings.NewReplacer(config.LayoutBase, storage.BasePath, config.LayoutProject, project, config.LayoutVolume, volume).Replace(storage.Layout)
}

func (options Options) limits(service *ir.Service, node *yaml.Node) {
	defaults := options.Profile.Defaults
	memory := largerMemory(service.Resources.MemoryLimit, defaults.MemoryLimit)
	cpus := firstNonEmpty(service.Resources.CPUs, strconv.FormatFloat(defaults.CPUs, 'f', -1, 64))
	pids := service.Resources.PIDsLimit
	if pids == 0 {
		pids = defaults.PIDsLimit
	}
	node.Content = append(node.Content, key("mem_limit"), scalar(memory), key("cpus"), quoted(cpus), key("pids_limit"), integer(pids))
}

// largerMemory is the profile's default unless the plan needs more: a
// figure such as immich's 2g raises a 512m default and never lowers a larger
// one.
func largerMemory(planned, profile string) string {
	plannedBytes, plannedErr := units.RAMInBytes(planned)
	if planned == "" || plannedErr != nil {
		return profile
	}
	profileBytes, profileErr := units.RAMInBytes(profile)
	if profileErr != nil || plannedBytes > profileBytes {
		return planned
	}
	return profile
}

// needs renders only what the user granted, each with a loud comment.
func (options Options) needs(plan *ir.IR, service *ir.Service, node *yaml.Node) {
	for _, need := range plan.Needs {
		if need.Service != service.Name || !options.isGranted(need.Capability) {
			continue
		}
		var field, value *yaml.Node
		switch need.Capability {
		case ir.NeedHostNetwork:
			field, value = key("network_mode"), scalar("host")
		case ir.NeedPrivileged:
			field, value = key("privileged"), boolean(true)
		case ir.NeedDevices:
			field, value = key("devices"), flowSequence(need.Detail+":"+need.Detail)
		case ir.NeedCapAdd:
			field, value = key("cap_add"), flowSequence(need.Detail)
		default:
			continue
		}
		field.HeadComment = fmt.Sprintf("NEED GRANTED BY USER: %s (%s)", need.Capability, singleLine(need.Reason))
		node.Content = append(node.Content, field, value)
	}
}

func (options Options) granted(plan *ir.IR, service *ir.Service, capability string) bool {
	return options.isGranted(capability) && slices.ContainsFunc(plan.Needs, func(need ir.Need) bool {
		return need.Capability == capability && need.Service == service.Name
	})
}

func (options Options) isGranted(capability string) bool {
	return slices.Contains(options.GrantedNeeds, capability)
}

func dependsOn(service *ir.Service) *yaml.Node {
	if len(service.DependsOn) == 0 {
		return nil
	}
	node := mapping()
	for _, dependency := range service.DependsOn {
		node.Content = append(node.Content, key(dependency.Service), mapping(key("condition"), scalar(dependency.Condition)))
	}
	return node
}

func environment(service *ir.Service) *yaml.Node {
	if len(service.Env) == 0 {
		return nil
	}
	node := mapping()
	for _, variable := range service.Env {
		value := variable.Literal
		if variable.Ref != "" {
			value = "${" + variable.Ref + "}"
		}
		node.Content = append(node.Content, key(variable.Key), quoted(value))
	}
	return node
}

func healthcheckNode(healthcheck ir.Healthcheck) *yaml.Node {
	if !healthcheck.Defined() {
		return nil
	}
	node := mapping(key("test"), flowSequence(healthcheck.Test...))
	for _, field := range []struct{ name, value string }{
		{"interval", healthcheck.Interval}, {"timeout", healthcheck.Timeout}, {"start_period", healthcheck.StartPeriod},
	} {
		if field.value != "" {
			node.Content = append(node.Content, key(field.name), scalar(field.value))
		}
	}
	if healthcheck.Retries > 0 {
		node.Content = append(node.Content, key("retries"), integer(healthcheck.Retries))
	}
	return node
}

// webPort is the port a proxy should forward to: the first published one.
func webPort(service *ir.Service) (int, bool) {
	for _, port := range service.Ports {
		if port.Publish && port.Protocol == ir.ProtocolTCP {
			return port.Container, true
		}
	}
	return 0, false
}

// networkHost is the name the app is reached by through one network: under
// the network's own domain, else under the profile's domain suffix.
func (options Options) networkHost(network config.ProfileNetwork, project string) string {
	return HostName(project, cmp.Or(network.Domain, options.Profile.Defaults.DomainSuffix))
}

// HostName is the name a proxy or tunnel serves the app under.
func HostName(project, suffix string) string {
	if suffix == "" {
		return project + ".localhost"
	}
	return project + "." + suffix
}

func splitMount(detail string) (string, string) {
	source, target, found := strings.Cut(detail, ":")
	if !found {
		return detail, detail
	}
	return source, target
}

func hasKey(node *yaml.Node, name string) bool {
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == name {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
