// Package render writes compose.yaml, .env and DEPLOY.md from a plan and the
// environment profile. It is the only package that writes compose YAML.
package render

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/rules"
)

const (
	ExposeLocal = "local"
	ExposeLAN   = "lan"
	ExposeNone  = "none"

	loopback           = "127.0.0.1"
	defaultNetwork     = "default"
	logDriver          = "json-file"
	noNewPrivileges    = "no-new-privileges:true"
	providerTraefik    = "traefik"
	providerNPM        = "npm"
	providerCaddy      = "caddy"
	providerCloudflare = "cloudflare"
	providerPangolin   = "pangolin"
	providerTailscale  = "tailscale"
	keyLabels          = "labels"
	keyExpose          = "expose"
	pathSeparator      = "/"
	// relativePrefix makes a path beside compose.yaml one Compose reads as
	// relative to the file, as the gate requires.
	relativePrefix = "./"
	// readOnlyMount ends a mount the container may only read.
	readOnlyMount   = ":ro"
	stripMiddleware = "-stripprefix"
)

var exposeModes = []string{ExposeLocal, ExposeLAN, ExposeNone}

// Options are the choices the review step makes on top of the plan.
type Options struct {
	Profile config.Profile
	// ExposeVia is how the app's ports are published: local, lan or none.
	ExposeVia string
	// Networks are the external networks from the profile the app joins.
	Networks []string
	// PublicHost is the address a person set for the app, a host name with
	// an optional path as home.example.com/rss; blank derives one.
	PublicHost   string
	Storage      string
	GrantedNeeds []string
	Header       string
	// CheckedPlatform is the platform the registry listed for every image;
	// empty when the images were not checked for one.
	CheckedPlatform string
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
	if secrets := secretFiles(plan); secrets != nil {
		root.Content = append(root.Content, key("secrets"), secrets)
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
	if environment := environment(plan, service); environment != nil {
		node.Content = append(node.Content, key("environment"), environment)
	}
	if secrets := serviceSecrets(plan, service); len(secrets) > 0 {
		node.Content = append(node.Content, key("secrets"), flowSequence(secrets...))
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
	hardening(service.Hardening, node)
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
		host, path := options.networkHost(network, project), options.publicPath()
		switch network.Provider {
		case providerTraefik:
			node.Content = appendOnce(node.Content, keyLabels, traefikLabels(project, host, path, network.Name, port))
		case providerNPM:
			node.Content = appendOnce(node.Content, keyExpose, flowSequence(strconv.Itoa(port)))
		default:
			notes = append(notes, fmt.Sprintf("%s (%s): route %s to http://%s:%d", network.Name, network.Kind, host+path, service.Name, port))
		}
	}
	return notes
}

// traefikLabels route the host to the app; under a path, the path is
// stripped before the request reaches it.
func traefikLabels(router, host, path, network string, port int) *yaml.Node {
	rule := "Host(`" + host + "`)"
	if path != "" {
		rule += " && PathPrefix(`" + path + "`)"
	}
	labels := mapping(
		key("traefik.enable"), quoted("true"),
		key("traefik.docker.network"), quoted(network),
		key("traefik.http.routers."+router+".rule"), quoted(rule),
		key("traefik.http.services."+router+".loadbalancer.server.port"), quoted(strconv.Itoa(port)),
	)
	if path == "" {
		return labels
	}
	middleware := router + stripMiddleware
	labels.Content = append(labels.Content,
		key("traefik.http.middlewares."+middleware+".stripprefix.prefixes"), quoted(path),
		key("traefik.http.routers."+router+".middlewares"), quoted(middleware),
	)
	return labels
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
	for _, file := range plan.ConfigFiles {
		if file.Service == service.Name {
			items = append(items, quoted(relativePrefix+file.Path+":"+file.ContainerPath+readOnlyMount))
		}
	}
	for _, need := range plan.Needs {
		if need.Service != service.Name || !options.grants(need) {
			continue
		}
		switch need.Capability {
		case ir.NeedBindMount:
			source, target := splitMount(need.Detail)
			items = append(items, quoted(source+":"+target))
		case ir.NeedDockerSocket:
			socket := quoted(ir.DockerSocket + ":" + cmp.Or(need.Detail, ir.DockerSocket))
			socket.HeadComment = grantedComment(need)
			items = append(items, socket)
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
	node.Content = append(node.Content, key(composefile.KeyMemLimit), scalar(memory), key(composefile.KeyCPUs), quoted(cpus), key(composefile.KeyPIDsLimit), integer(pids))
	if service.Resources.ShmSize != "" {
		node.Content = append(node.Content, key(composefile.KeyShmSize), scalar(service.Resources.ShmSize))
	}
}

// hardening writes what upstream takes away from the service as it wrote it.
func hardening(hardening ir.Hardening, node *yaml.Node) {
	if hardening.ReadOnly {
		node.Content = append(node.Content, key(composefile.KeyReadOnly), boolean(true))
	}
	if len(hardening.Tmpfs) > 0 {
		node.Content = append(node.Content, key(composefile.KeyTmpfs), flowSequence(hardening.Tmpfs...))
	}
	if len(hardening.CapDrop) > 0 {
		node.Content = append(node.Content, key(composefile.KeyCapDrop), flowSequence(hardening.CapDrop...))
	}
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

// needs renders only what the user granted, each with a loud comment. The
// granted needs of one capability merge into one key, so several devices
// or capabilities of one service are one sequence.
func (options Options) needs(plan *ir.IR, service *ir.Service, node *yaml.Node) {
	granted := map[string][]ir.Need{}
	for _, need := range plan.Needs {
		if need.Service == service.Name && options.grants(need) {
			granted[need.Capability] = append(granted[need.Capability], need)
		}
	}
	for _, capability := range ir.Capabilities {
		needs := granted[capability]
		value := needValue(capability, needs)
		if capability == ir.NeedCapAdd {
			value = withRestored(service.Hardening.Restored, value)
		}
		if value == nil {
			continue
		}
		field := key(capabilityKeys[capability])
		comments := make([]string, 0, len(needs))
		for _, need := range needs {
			comments = append(comments, grantedComment(need))
		}
		field.HeadComment = strings.Join(distinct(comments), "\n")
		node.Content = append(node.Content, field, value)
	}
}

// capabilityKeys are the service keys the needs rendered here set; the
// mounts are rendered with the volumes.
var capabilityKeys = map[string]string{ir.NeedHostNetwork: composefile.KeyNetworkMode, ir.NeedPrivileged: composefile.KeyPrivileged,
	ir.NeedDevices: composefile.KeyDevices, ir.NeedCapAdd: composefile.KeyCapAdd}

// withRestored is cap_add with the default capabilities the hardening adds
// back first, each once, or granted alone when it restores none.
func withRestored(restored []string, granted *yaml.Node) *yaml.Node {
	if len(restored) == 0 {
		return granted
	}
	names := slices.Clone(restored)
	if granted != nil {
		for _, item := range granted.Content {
			names = append(names, item.Value)
		}
	}
	return flowSequence(distinct(names)...)
}

// needValue is what the granted needs of one capability set, or nil when
// they set nothing here.
func needValue(capability string, needs []ir.Need) *yaml.Node {
	if len(needs) == 0 {
		return nil
	}
	switch capability {
	case ir.NeedHostNetwork:
		return scalar(composefile.NetworkModeHost)
	case ir.NeedPrivileged:
		return boolean(true)
	case ir.NeedDevices, ir.NeedCapAdd:
		return detailSequence(capability, needs)
	}
	return nil
}

// detailSequence lists each device or capability the needs name once.
func detailSequence(capability string, needs []ir.Need) *yaml.Node {
	var details []string
	for _, need := range needs {
		if need.Detail != "" {
			details = append(details, needDetail(capability, need.Detail))
		}
	}
	if len(details) == 0 {
		return nil
	}
	return flowSequence(distinct(details)...)
}

// needDetail is a device as host:container, as upstream wrote it; a bare
// path maps to the same path in the container.
func needDetail(capability, detail string) string {
	if capability != ir.NeedDevices || strings.Contains(detail, composefile.DeviceSeparator) {
		return detail
	}
	return detail + composefile.DeviceSeparator + detail
}

// distinct keeps the first of each value, in order.
func distinct(values []string) []string {
	seen := map[string]bool{}
	return slices.DeleteFunc(values, func(value string) bool {
		repeated := seen[value]
		seen[value] = true
		return repeated
	})
}

func grantedComment(need ir.Need) string {
	return fmt.Sprintf("NEED GRANTED BY USER: %s (%s)", need.Capability, singleLine(need.Reason))
}

func (options Options) granted(plan *ir.IR, service *ir.Service, capability string) bool {
	return slices.ContainsFunc(plan.Needs, func(need ir.Need) bool {
		return need.Capability == capability && need.Service == service.Name && options.grants(need)
	})
}

// grants reports whether the user granted this exact request.
func (options Options) grants(need ir.Need) bool {
	return slices.Contains(options.GrantedNeeds, need.Grant())
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

func environment(plan *ir.IR, service *ir.Service) *yaml.Node {
	optional := optionalFor(plan, service.Name)
	if len(service.Env) == 0 && len(optional) == 0 {
		return nil
	}
	node := mapping()
	for _, variable := range service.Env {
		name, value := variable.Key, variable.Literal
		if variable.Ref != "" {
			value = "${" + variable.Ref + "}"
		}
		if entry, found := plan.EnvEntry(variable.Ref); found && entry.AsFile() {
			name, value = entry.FileEnvKey, path.Join(ir.SecretMountDirectory, entry.SecretName())
		}
		node.Content = append(node.Content, key(name), quoted(value))
	}
	for index, setting := range optional {
		name := key(setting.Key)
		if index == 0 {
			name.HeadComment = optionalComposeComment
		}
		node.Content = append(node.Content, name, unset())
	}
	return node
}

// serviceSecrets are the secrets passed as files that the service reads.
func serviceSecrets(plan *ir.IR, service *ir.Service) []string {
	var secrets []string
	for _, variable := range service.Env {
		if entry, found := plan.EnvEntry(variable.Ref); found && entry.AsFile() {
			secrets = append(secrets, entry.SecretName())
		}
	}
	return secrets
}

// secretFiles is the top-level secrets block: each secret passed as a file,
// read from its file beside compose.yaml.
func secretFiles(plan *ir.IR) *yaml.Node {
	node := mapping()
	for _, entry := range plan.EnvFile {
		if entry.AsFile() {
			node.Content = append(node.Content, key(entry.SecretName()), mapping(key("file"), scalar(relativePrefix+entry.SecretPath())))
		}
	}
	if len(node.Content) == 0 {
		return nil
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

// networkHost is the name the app is reached by through one network: the
// host a person set, else under the network's own domain, else under the
// profile's domain suffix.
func (options Options) networkHost(network config.ProfileNetwork, project string) string {
	if host, _, _ := strings.Cut(options.PublicHost, pathSeparator); host != "" {
		return host
	}
	return HostName(project, cmp.Or(network.Domain, options.Profile.Defaults.DomainSuffix))
}

// publicPath is the path prefix of the host a person set, as /rss; "" for
// none.
func (options Options) publicPath() string {
	if _, path, found := strings.Cut(options.PublicHost, pathSeparator); found {
		return pathSeparator + path
	}
	return ""
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
