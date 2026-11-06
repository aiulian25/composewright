package policy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
)

// EvaluateYAML is the second gate (P9a, P13): it inspects the compose model
// re-parsed from the rendered file, independently of the plan it came from.
func EvaluateYAML(ctx context.Context, project *types.Project, content []byte, input Input) Report {
	check := &checker{input: input}
	check.composeInclude(ctx, content)
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	check.ownPorts = publishedPorts(project)
	for _, name := range names {
		check.composeService(name, project.Services[name])
	}
	check.composeResources(project)
	check.composeSize(project)
	return Report{Findings: check.findings}
}

// composeSize is P12 for the compose file as it runs: as many services as a
// plan may have, and few copies of each, so per-container limits add up to
// a bounded whole even when the file was written by hand.
func (check *checker) composeSize(project *types.Project) {
	if count := len(project.Services); count > maxServices {
		check.add("P12", Deny, "", "services", "a stack has 1 to %d services, this file has %d", maxServices, count)
	}
	for _, name := range sortedKeys(project.Services) {
		service := project.Services[name]
		if replicas := service.GetScale(); replicas > maxReplicas {
			check.add("P12", Deny, name, "services."+name+".deploy.replicas", "%s runs %d copies; at most %d are allowed", name, replicas, maxReplicas)
		}
	}
}

// composeInclude denies a top-level include. Loading skips it, so the project
// never shows the files it names, yet docker compose up would follow them,
// from this host or a registry, and run their services without this gate.
// compose-go's own model is read, so an include in a later document or
// behind a merge key or an alias is found exactly as docker compose finds it.
func (check *checker) composeInclude(ctx context.Context, content []byte) {
	model, err := composefile.LoadModel(ctx, composeFileName, content)
	if err != nil {
		check.add("P13", Deny, "", includeKey, "the compose file could not be read to look for include: %v", err)
		return
	}
	if _, included := model[includeKey]; included {
		check.add("P13", Deny, "", includeKey, "include is not allowed in a generated stack: the services it brings in would run unchecked")
	}
}

func (check *checker) composeService(name string, service types.ServiceConfig) {
	field := "services." + name
	deny := func(format string, arguments ...any) {
		check.add("P13", Deny, name, field, format, arguments...)
	}
	forbidden := map[string]bool{
		"build":               service.Build != nil,
		"extends":             service.Extends != nil,
		"provider":            service.Provider != nil,
		"lifecycle hooks":     len(service.PreStart)+len(service.PostStart)+len(service.PreStop) > 0,
		"pid":                 service.Pid != "",
		"ipc":                 service.Ipc != "",
		"userns_mode":         service.UserNSMode != "",
		"uts":                 service.Uts != "",
		"cgroup":              service.Cgroup != "" || service.CgroupParent != "",
		"runtime":             service.Runtime != "",
		"isolation":           service.Isolation != "",
		"sysctls":             len(service.Sysctls) > 0,
		"ulimits":             len(service.Ulimits) > 0,
		"volumes_from":        len(service.VolumesFrom) > 0,
		"group_add":           len(service.GroupAdd) > 0,
		"use_api_socket":      service.UseAPISocket,
		"device_cgroup_rules": len(service.DeviceCgroupRules) > 0,
		"gpus":                len(service.Gpus) > 0,
		"oom settings":        service.OomKillDisable || service.OomScoreAdj != 0,
		"storage_opt":         len(service.StorageOpt) > 0 || service.VolumeDriver != "",
		"configs":             len(service.Configs) > 0,
		"models":              len(service.Models) > 0,
		"external_links":      len(service.ExternalLinks) > 0 || service.Net != "",
		// Loading never reads these, but docker compose up reads them on this
		// host and hands their contents to the container.
		"env_file":   len(service.EnvFiles) > 0,
		"label_file": len(service.LabelFiles) > 0,
	}
	for _, key := range sortedTrue(forbidden) {
		deny("%s is not allowed in a generated stack", key)
	}
	check.composeGrants(name, service)
	check.composeSecurityOptions(name, service)
	check.composeLogging(name, service)
	check.composeVolumes(name, service)
	check.composePorts(name, service)
	check.composeImage(name, service)
}

// EvaluateObserved is the part of P10 that needs a sandbox run: each
// service's mem_limit in the compose model against the peak memory in
// bytes a passing run measured for it. A tight limit gets a larger one to
// use, a generous one is pointed out.
func EvaluateObserved(project *types.Project, peaks map[string]int64) Report {
	check := &checker{}
	for _, name := range sortedKeys(project.Services) {
		check.observed(name, project.Services[name], peaks[name])
	}
	return Report{Findings: check.findings}
}

func (check *checker) observed(name string, service types.ServiceConfig, peak int64) {
	limit := int64(service.MemLimit)
	if peak <= 0 || limit <= 0 {
		return
	}
	field := "services." + name + ".mem_limit"
	sizes := fmt.Sprintf("observed peak %s; limit %s", units.BytesSize(float64(peak)), units.BytesSize(float64(limit)))
	switch {
	case float64(limit) < tightFactor*float64(peak):
		check.add("P10", Warn, name, field, "%s: the limit is tight: OOM kills likely", sizes)
		check.findings[len(check.findings)-1].MemoryLimit = SuggestedLimit(peak)
	case float64(limit) > generousFactor*float64(peak):
		check.add("P10", Warn, name, field, "%s: the limit is %.1f× what was used", sizes, float64(limit)/float64(peak))
	default:
		check.add("P10", OK, name, field, "%s", sizes)
	}
}

// SuggestedLimit is tightFactor times the peak, rounded up to whole
// limitStep mebibytes, as a mem_limit value.
func SuggestedLimit(peak int64) string {
	wanted := int64(math.Ceil(tightFactor * float64(peak)))
	mebibytes := (wanted + limitStep - 1) / limitStep * limitStep / units.MiB
	return strconv.FormatInt(mebibytes, 10) + mebibyteSuffix
}

func (check *checker) composeGrants(name string, service types.ServiceConfig) {
	if service.Privileged {
		check.grantedOrDeny(ir.NeedPrivileged, name, "privileged: true", anyDetail)
	}
	for _, capability := range service.CapAdd {
		check.grantedOrDeny(ir.NeedCapAdd, name, "cap_add "+capability, func(detail string) bool { return ir.SameCapability(detail, capability) })
	}
	for _, device := range service.Devices {
		check.grantedOrDeny(ir.NeedDevices, name, "device "+device.Source+composefile.DeviceSeparator+device.Target,
			func(detail string) bool { return sameDevice(detail, device) })
	}
	switch service.NetworkMode {
	case "":
	case composefile.NetworkModeHost:
		check.grantedOrDeny(ir.NeedHostNetwork, name, "network_mode: host", anyDetail)
	default:
		check.add("P13", Deny, name, "services."+name, "network_mode %q is not allowed", service.NetworkMode)
	}
}

func (check *checker) composeSecurityOptions(name string, service types.ServiceConfig) {
	for _, option := range service.SecurityOpt {
		if !slices.Contains(allowedNoNewPrivileges, option) {
			check.add("P13", Deny, name, "services."+name+".security_opt", "security_opt %q is not allowed; only %s", option, noNewPrivileges)
		}
	}
}

// composeLogging warns about a logging driver that lets the Docker daemon
// send logs elsewhere: the sandbox resets it, but the exported file keeps it.
func (check *checker) composeLogging(name string, service types.ServiceConfig) {
	if service.Logging == nil || slices.Contains(localLogDrivers, service.Logging.Driver) {
		return
	}
	check.add("P13", Warn, name, "services."+name+".logging", "logging driver %q can send logs off this host, from the Docker daemon", service.Logging.Driver)
}

func (check *checker) composeVolumes(name string, service types.ServiceConfig) {
	for _, entry := range service.Tmpfs {
		if target, _, _ := strings.Cut(entry, composefile.TmpfsOptionsSeparator); ForbiddenTarget(target) {
			check.add("P13", Deny, name, "services."+name+".tmpfs", "tmpfs target %s is reserved for the host", target)
		}
	}
	for _, volume := range service.Volumes {
		field := "services." + name + ".volumes"
		if check.grantedSocket(name, volume) {
			check.add("P13", OK, name, field, "the Docker socket at %s, granted by the user", volume.Target)
			continue
		}
		if ForbiddenTarget(volume.Target) {
			check.add("P13", Deny, name, field, "mount target %s is reserved for the host", volume.Target)
		}
		if volume.Bind != nil && volume.Bind.Propagation != "" {
			check.add("P13", Deny, name, field, "bind propagation %q is not allowed", volume.Bind.Propagation)
		}
		switch volume.Type {
		case volumeTypeVolume, volumeTypeTmpfs:
		case volumeTypeBind:
			check.bindSource(name, field, volume.Source)
		default:
			check.add("P13", Deny, name, field, "mount type %q is not allowed", volume.Type)
		}
	}
}

// grantedSocket is a docker_socket the user granted: the host's socket,
// bind-mounted where the service asked for it.
func (check *checker) grantedSocket(service string, volume types.ServiceVolumeConfig) bool {
	socket := volume.Type == volumeTypeBind && ir.IsDockerSocket(volume.Source)
	return socket && check.grantedRequest(ir.NeedDockerSocket, service, exactly(volume.Target))
}

// bindSource is P9a: bind mounts come only from the profile base path (after
// resolving symlinks), from a bind_mount need the user granted, or from
// beside compose.yaml for a file the plan bundles for that service.
func (check *checker) bindSource(service, field, source string) {
	if check.bundled(service, source) {
		return
	}
	if !filepath.IsAbs(source) {
		check.add("P9a", Deny, service, field, "bind source %q must be an absolute path", source)
		return
	}
	base := check.input.Profile.Storage.BasePath
	if base != "" && within(resolveExisting(source), resolveExisting(base)) {
		return
	}
	if check.grantedRequest(ir.NeedBindMount, service, exactly(source)) {
		check.add("P9a", OK, service, field, "bind mount %s granted by the user", source)
		return
	}
	check.add("P9a", Deny, service, field, "bind mount %s is outside the profile base path %q", source, base)
}

// bundled is a bind of a file the plan bundles beside compose.yaml, into
// the service that mounts it.
func (check *checker) bundled(service, source string) bool {
	return check.input.Plan != nil && slices.ContainsFunc(check.input.Plan.ConfigFiles, func(file ir.ConfigFile) bool {
		return file.Service == service && source == relativePrefix+file.Path
	})
}

func (check *checker) composePorts(name string, service types.ServiceConfig) {
	for _, port := range service.Ports {
		hostPorts := composefile.HostPorts(port.Published)
		check.portTaken(name, port, hostPorts)
		if len(hostPorts) == 0 || hostPorts[0] >= lowPortLimit {
			continue
		}
		request := ir.Need{Capability: ir.NeedLowPort, Service: name, Detail: strconv.Itoa(hostPorts[0])}
		check.need("P4", request, "services."+name+".ports", "%s publishes host port %s, below %d", name, port.Published, lowPortLimit)
	}
}

// portTaken warns about each host port of port another project or a
// running container publishes too; docker compose up would fail on it. A
// single port gets the next free one to use instead.
func (check *checker) portTaken(name string, port types.ServicePortConfig, hostPorts []int) {
	for _, hostPort := range hostPorts {
		takers := check.input.TakenPorts[hostPort]
		if len(takers) == 0 {
			continue
		}
		finding := Finding{Rule: "P4", Severity: Warn, Service: name, Path: "services." + name + ".ports",
			Message: fmt.Sprintf(portTakenMessage, hostPort, strings.Join(takers, portTakersSeparator))}
		if len(hostPorts) == 1 {
			finding.ContainerPort, finding.FreePort = int(port.Target), check.nextFreePort(hostPort)
		}
		check.findings = append(check.findings, finding)
	}
}

// nextFreePort is the first host port above port that neither another
// stack nor this one publishes; 0 when none is left.
func (check *checker) nextFreePort(port int) int {
	for candidate := port + 1; candidate <= composefile.MaxPort; candidate++ {
		_, taken := check.input.TakenPorts[candidate]
		if !taken && !check.ownPorts[candidate] {
			return candidate
		}
	}
	return 0
}

// publishedPorts are the host ports the project publishes.
func publishedPorts(project *types.Project) map[int]bool {
	ports := map[int]bool{}
	for _, service := range project.Services {
		for _, port := range service.Ports {
			for _, hostPort := range composefile.HostPorts(port.Published) {
				ports[hostPort] = true
			}
		}
	}
	return ports
}

func (check *checker) composeImage(name string, service types.ServiceConfig) {
	image, err := ir.ParseImage(service.Image)
	if err != nil {
		check.add("P1", Deny, name, "services."+name+".image", "%v", err)
		return
	}
	if !RegistryAllowed(image.Reference(), check.input.AllowRegistries, check.input.ExtraRegistries, check.input.RepositoryOwner) {
		check.add("P1", Deny, name, "services."+name+".image", "image %s is outside the registry allowlist", image.Reference())
	}
}

func (check *checker) composeResources(project *types.Project) {
	for _, name := range sortedKeys(project.Networks) {
		network := project.Networks[name]
		external := firstNonEmpty(network.Name, name)
		if _, listed := check.input.Profile.Network(external); bool(network.External) && !listed {
			check.add("P9a", Deny, "", "networks."+name, "external network %s is not in the profile; add it to profile.networks first", external)
		}
		if len(network.DriverOpts) > 0 || (network.Driver != "" && network.Driver != "bridge") {
			check.add("P13", Deny, "", "networks."+name, "network driver %q and driver_opts are not allowed", network.Driver)
		}
	}
	for _, name := range sortedKeys(project.Volumes) {
		volume := project.Volumes[name]
		if volume.External {
			check.add("P13", Deny, "", "volumes."+name, "external volume %s is not allowed", name)
		}
		if len(volume.DriverOpts) > 0 || (volume.Driver != "" && volume.Driver != localVolumeDriver) {
			check.add("P13", Deny, "", "volumes."+name, "volume driver options can mount host paths and are not allowed")
		}
	}
	for _, name := range sortedKeys(project.Secrets) {
		secret := project.Secrets[name]
		inSecretsDir := strings.HasPrefix(secret.File, secretsDirPrefix) && !strings.Contains(secret.File, "..")
		if bool(secret.External) || (secret.File != "" && !inSecretsDir) {
			check.add("P13", Deny, "", "secrets."+name, "secret %s must be a file under %s", name, secretsDirPrefix)
		}
	}
	if len(project.Configs) > 0 || len(project.Models) > 0 || len(project.Jobs) > 0 {
		check.add("P13", Deny, "", "", "top-level configs, models and jobs are not allowed in a generated stack")
	}
}

// grantedOrDeny allows a privilege only where the plan requests it for this
// service with a detail matches accepts, and the user granted that request.
func (check *checker) grantedOrDeny(capability, service, what string, matches func(detail string) bool) {
	if check.grantedRequest(capability, service, matches) {
		check.add("P13", OK, service, "services."+service, "%s granted by the user", what)
		return
	}
	check.add("P13", Deny, service, "services."+service, "%s requires the %s need to be requested for %s and granted", what, capability, service)
}

// grantedRequest reports whether the plan requests capability for service
// with a detail matches accepts, and the user granted that exact request.
func (check *checker) grantedRequest(capability, service string, matches func(detail string) bool) bool {
	return check.input.Plan != nil && slices.ContainsFunc(check.input.Plan.Needs, func(need ir.Need) bool {
		return need.Capability == capability && need.Service == service && matches(need.Detail) && check.grants(need)
	})
}

func anyDetail(string) bool { return true }

func exactly(wanted string) func(string) bool {
	return func(detail string) bool { return detail == wanted }
}

// sameDevice reports whether a devices need, written as upstream wrote it
// (a path, host:container, or with permissions), is this device mapping.
func sameDevice(detail string, device types.DeviceMapping) bool {
	parts := strings.Split(detail, composefile.DeviceSeparator)
	source, target, permissions := parts[0], parts[0], defaultDevicePermissions
	if len(parts) > 1 {
		target = parts[1]
	}
	if len(parts) > 2 {
		permissions = parts[2]
	}
	return source == device.Source && target == device.Target && permissions == cmp.Or(device.Permissions, defaultDevicePermissions)
}

// resolveExisting resolves symlinks in the longest existing prefix of path,
// so a bind source that does not exist yet is still checked against where
// its parent really points.
func resolveExisting(path string) string {
	clean := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(clean)
		if err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(clean)
		if !errors.Is(err, fs.ErrNotExist) || parent == clean {
			return clean
		}
		missing = append([]string{filepath.Base(clean)}, missing...)
		clean = parent
	}
}

func within(path, base string) bool {
	relative, err := filepath.Rel(base, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func sortedTrue(flags map[string]bool) []string {
	var keys []string
	for key, set := range flags {
		if set {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
