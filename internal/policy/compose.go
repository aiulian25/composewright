package policy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/aiulian25/composewright/internal/ir"
)

// EvaluateYAML is the second gate (P9a, P13): it inspects the compose model
// re-parsed from the rendered file, independently of the plan it came from.
func EvaluateYAML(project *types.Project, input Input) Report {
	check := &checker{input: input}
	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		check.composeService(name, project.Services[name])
	}
	check.composeResources(project)
	return Report{Findings: check.findings}
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

func (check *checker) composeGrants(name string, service types.ServiceConfig) {
	if service.Privileged {
		check.grantedOrDeny(ir.NeedPrivileged, name, "privileged: true")
	}
	if len(service.CapAdd) > 0 {
		check.grantedOrDeny(ir.NeedCapAdd, name, "cap_add "+strings.Join(service.CapAdd, ","))
	}
	if len(service.Devices) > 0 {
		check.grantedOrDeny(ir.NeedDevices, name, "devices")
	}
	switch service.NetworkMode {
	case "":
	case networkModeHost:
		check.grantedOrDeny(ir.NeedHostNetwork, name, "network_mode: host")
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
	for _, volume := range service.Volumes {
		field := "services." + name + ".volumes"
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

// bindSource is P9a: bind mounts come only from the profile base path (after
// resolving symlinks) or from a bind_mount need the user granted.
func (check *checker) bindSource(service, field, source string) {
	if !filepath.IsAbs(source) {
		check.add("P9a", Deny, service, field, "bind source %q must be an absolute path", source)
		return
	}
	base := check.input.Profile.Storage.BasePath
	if base != "" && within(resolveExisting(source), resolveExisting(base)) {
		return
	}
	if check.granted(ir.NeedBindMount) && check.requested(ir.NeedBindMount, service, source) {
		check.add("P9a", OK, service, field, "bind mount %s granted by the user", source)
		return
	}
	check.add("P9a", Deny, service, field, "bind mount %s is outside the profile base path %q", source, base)
}

func (check *checker) composePorts(name string, service types.ServiceConfig) {
	for _, port := range service.Ports {
		published, err := strconv.Atoi(port.Published)
		if err != nil || published == 0 || published >= lowPortLimit {
			continue
		}
		check.need("P4", ir.NeedLowPort, name, "services."+name+".ports", "%s publishes host port %d, below %d", name, published, lowPortLimit)
	}
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

func (check *checker) grantedOrDeny(capability, service, what string) {
	if check.granted(capability) && check.requested(capability, service, "") {
		check.add("P13", OK, service, "services."+service, "%s granted by the user", what)
		return
	}
	check.add("P13", Deny, service, "services."+service, "%s requires the %s need to be requested and granted", what, capability)
}

func (check *checker) requested(capability, service, detail string) bool {
	if check.input.Plan == nil {
		return true
	}
	return slices.ContainsFunc(check.input.Plan.Needs, func(need ir.Need) bool {
		return need.Capability == capability && need.Service == service && (detail == "" || need.Detail == detail)
	})
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
