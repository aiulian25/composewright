package policy

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/ir"
)

var (
	appNamePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)
	serviceNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	volumeNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)
	envKeyPattern       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	registryHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)
	repositoryPattern   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	tagPattern          = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	userPattern         = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)
	referencePattern    = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}`)
	urlHost             = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(?:[^@/\s"']*@)?(\[[^\]]*\]|[^/:\s"'?#]*)`)
	dangerousCommand    = regexp.MustCompile(`(?i)(\b(curl|wget|nc|ncat|netcat|socat)\b|/dev/(tcp|udp)/|docker\.sock|--privileged|\b(ba|da|z)?sh\s+-c\b.*https?://)`)
	dangerousProbe      = regexp.MustCompile(`(?i)(/dev/(tcp|udp)/|docker\.sock|--privileged)`)
	dockerSocketMention = regexp.MustCompile(`(?i)docker\.sock`)
	// shellToken splits a healthcheck into words and the operators that end
	// a command; quotes are dropped.
	shellToken = regexp.MustCompile(`[|;&()]+|[^\s"'|;&()]+`)
	// bareHost is a target written without a scheme, as curl and wget take
	// it: a dotted name, an IPv4 or a bracketed IPv6 address, after any user
	// info and before a port or a path.
	bareHost = regexp.MustCompile(`(?i)^(?:[^@/\s]+@)?(\[[0-9a-f:.]+\]|\d{1,3}(?:\.\d{1,3}){3}|(?:[a-z0-9-]+\.)+[a-z]{2,})(?::\d+)?(?:/|$)`)
)

var (
	loopbackHosts  = []string{"localhost", "127.0.0.1", "0.0.0.0", "::1", "::"}
	networkClients = []string{"curl", "wget", "nc", "ncat", "netcat", "socat", "telnet"}
)

const shellOperators = "|;&()"

func (check *checker) consistency(plan *ir.IR) {
	if plan.IRVersion != ir.Version {
		check.add("C", Deny, "", "ir_version", "ir_version %q is not supported (expected %q)", plan.IRVersion, ir.Version)
	}
	if !appNamePattern.MatchString(plan.App.Name) {
		check.add("C", Deny, "", "app.name", "app name %q must be 2-40 characters of a-z, 0-9 and -", plan.App.Name)
	}
	if !slices.Contains(ir.Confidences, plan.Confidence) {
		check.add("C", Deny, "", "confidence", "confidence %q must be high, medium or low", plan.Confidence)
	}
	seen := map[string]bool{}
	apps := 0
	for index, service := range plan.Services {
		field := fmt.Sprintf("services.%d", index)
		if !serviceNamePattern.MatchString(service.Name) {
			check.add("C", Deny, service.Name, field+".name", "service name %q must be 1-63 characters of a-z, 0-9, _ and -", service.Name)
		}
		if seen[service.Name] {
			check.add("C", Deny, service.Name, field+".name", "service name %q is used twice", service.Name)
		}
		seen[service.Name] = true
		if !slices.Contains(ir.Roles, service.Role) {
			check.add("C", Deny, service.Name, field+".role", "role %q is not one of %s", service.Role, strings.Join(ir.Roles, ", "))
		}
		if service.Role == ir.RoleApp {
			apps++
		}
	}
	if apps != 1 {
		check.add("C", Deny, "", "services", "exactly one service must have the app role, found %d", apps)
	}
	check.probe(plan)
}

func (check *checker) probe(plan *ir.IR) {
	probe := plan.Probe
	if probe.Service == "" && probe.Port == 0 {
		return
	}
	service, found := plan.Service(probe.Service)
	if !found {
		check.add("C", Deny, "", "probe.service", "probe targets %q, which is not a service", probe.Service)
		return
	}
	listens := slices.ContainsFunc(service.Ports, func(port ir.Port) bool { return port.Container == probe.Port })
	if !listens {
		check.add("C", Deny, service.Name, "probe.port", "probe port %d is not a port of %s", probe.Port, service.Name)
	}
	if probe.Path != "" && !strings.HasPrefix(probe.Path, "/") {
		check.add("C", Deny, service.Name, "probe.path", "probe path %q must start with /", probe.Path)
	}
}

func (check *checker) size(plan *ir.IR) {
	if len(plan.Services) < 1 || len(plan.Services) > maxServices {
		check.add("P12", Deny, "", "services", "a stack has 1 to %d services, this plan has %d", maxServices, len(plan.Services))
	}
}

func (check *checker) image(service *ir.Service, field string) {
	image := service.Image
	field += ".image"
	if !registryHostPattern.MatchString(image.Registry) || !repositoryPattern.MatchString(image.Repository) {
		check.add("P1", Deny, service.Name, field, "image %q is not a valid registry/repository reference", image.Reference())
		return
	}
	if !RegistryAllowed(image.Reference(), check.input.AllowRegistries, check.input.ExtraRegistries, check.input.RepositoryOwner) {
		check.add("P1", Deny, service.Name, field, "image %s is "+OutsideAllowlist+"; allow %s to use it",
			image.Reference(), namespacePattern(image))
	}
	if trustedByNameOnly(image.Reference(), check.input.AllowRegistries, check.input.ExtraRegistries, check.input.RepositoryOwner) {
		check.add("P1", Warn, service.Name, field, "image %s is trusted only because its namespace matches the GitHub owner %s; %s "+NotGitHubAccounts+
			", so trust %s if it is the project's own", image.Reference(), check.input.RepositoryOwner, image.Registry, namespacePattern(image))
	}
	switch {
	case image.Tag == "":
		check.add("P2", Deny, service.Name, field+".tag", "image %s has no tag", image.Reference())
	case !tagPattern.MatchString(image.Tag):
		check.add("P2", Deny, service.Name, field+".tag", "image tag %q is not a valid tag", image.Tag)
	case image.Tag == latestTag:
		check.add("P2", Warn, service.Name, field+".tag", "image %s uses the latest tag; the digest is pinned when the image is verified", image.Reference())
	}
	if image.Digest != "" && !digestPattern.MatchString(image.Digest) {
		check.add("P2", Deny, service.Name, field+".digest", "image digest %q must be sha256 followed by 64 hex characters", image.Digest)
	}
	verification, checked := check.input.Images[image.Reference()+":"+image.Tag]
	switch {
	case checked && !verification.Found:
		check.add("P1", Deny, service.Name, field, "image %s:%s does not exist in the registry", image.Reference(), image.Tag)
	case checked && verification.Message != "":
		check.add("P1", Warn, service.Name, field, "image %s:%s could not be verified: %s", image.Reference(), image.Tag, verification.Message)
	}
}

// ValidEnvKey reports whether key can name a .env variable.
func ValidEnvKey(key string) bool {
	return envKeyPattern.MatchString(key)
}

// RegistryAllowed applies P1. Official images and the repository owner's own
// namespace are trusted; host-wide allowlist entries such as ghcr.io/* are
// narrowed to that owner when the owner is known; --allow-registry entries
// are taken literally.
func RegistryAllowed(reference string, allow, extra []string, owner string) bool {
	reference = strings.ToLower(reference)
	owner = strings.ToLower(owner)
	if strings.HasPrefix(reference, officialNamespace) {
		return true
	}
	ownNamespace := func(host string) bool { return owner != "" && strings.HasPrefix(reference, host+"/"+owner+"/") }
	if slices.ContainsFunc(ownerRegistries, ownNamespace) {
		return true
	}
	if slices.ContainsFunc(extra, func(pattern string) bool { return patternMatches(reference, pattern) }) {
		return true
	}
	return slices.ContainsFunc(allow, func(pattern string) bool {
		host, hostWide := hostOnly(pattern)
		if hostWide && owner != "" {
			return ownNamespace(host)
		}
		return patternMatches(reference, pattern)
	})
}

// trustedByNameOnly reports an image RegistryAllowed admits only because its
// namespace on Docker Hub or Quay carries the repository owner's name, which
// someone else may hold there. An --allow-registry entry, or an allowlist
// entry narrower than a whole host, is a person's own choice and settles it.
func trustedByNameOnly(reference string, allow, extra []string, owner string) bool {
	reference, owner = strings.ToLower(reference), strings.ToLower(owner)
	byName := owner != "" && slices.ContainsFunc(nameOnlyRegistries, func(host string) bool { return strings.HasPrefix(reference, host+"/"+owner+"/") })
	if !byName || strings.HasPrefix(reference, officialNamespace) {
		return false
	}
	matches := func(pattern string) bool { return patternMatches(reference, pattern) }
	chosen := func(pattern string) bool {
		_, hostWide := hostOnly(pattern)
		return !hostWide && matches(pattern)
	}
	return !slices.ContainsFunc(extra, matches) && !slices.ContainsFunc(allow, chosen)
}

// hostOnly is the host of a pattern covering a whole host, such as ghcr.io/*,
// which is narrowed to the repository owner when the owner is known.
func hostOnly(pattern string) (string, bool) {
	host, hostWide := strings.CutSuffix(strings.ToLower(pattern), hostWideSuffix)
	return host, hostWide && !strings.Contains(host, "/")
}

func patternMatches(reference, pattern string) bool {
	pattern = strings.ToLower(pattern)
	prefix, wildcarded := strings.CutSuffix(pattern, wildcard)
	if wildcarded {
		return strings.HasPrefix(reference, prefix)
	}
	return reference == pattern
}

func namespacePattern(image ir.Image) string {
	namespace, _, _ := strings.Cut(image.Repository, "/")
	return image.Registry + "/" + namespace + hostWideSuffix
}

func (check *checker) healthcheck(service *ir.Service, field string) {
	healthcheck := service.Healthcheck
	field += ".healthcheck"
	if !healthcheck.Defined() {
		needsOne := service.Role == ir.RoleApp || service.Role == ir.RoleDB
		if needsOne && healthcheck.OmittedReason == "" {
			check.add("P3", Warn, service.Name, field, "%s has no healthcheck, so validation can only confirm it keeps running", service.Name)
		}
		return
	}
	kind := healthcheck.Test[0]
	if (kind != ir.HealthcheckCommand && kind != ir.HealthcheckShell) || len(healthcheck.Test) < 2 {
		check.add("P3", Deny, service.Name, field+".test", "healthcheck test must start with CMD or CMD-SHELL followed by a command")
	}
	for name, value := range map[string]string{"interval": healthcheck.Interval, "timeout": healthcheck.Timeout, "start_period": healthcheck.StartPeriod} {
		if _, err := time.ParseDuration(value); value != "" && err != nil {
			check.add("P3", Deny, service.Name, field+"."+name, "healthcheck %s %q is not a duration such as 30s", name, value)
		}
	}
	if healthcheck.Retries < 0 {
		check.add("P3", Deny, service.Name, field+".retries", "healthcheck retries must not be negative")
	}
	check.probeTargets(service, field, strings.Join(healthcheck.Test[1:], " "))
}

// probeTargets is P6 for healthchecks: they may only contact the container
// itself, otherwise a healthcheck becomes an exfiltration channel.
func (check *checker) probeTargets(service *ir.Service, field, test string) {
	if dangerousProbe.MatchString(test) {
		check.add("P6", Deny, service.Name, field+".test", "healthcheck %q reaches a device, the Docker socket or privileges", test)
	}
	for _, match := range urlHost.FindAllStringSubmatch(test, -1) {
		check.probeHost(service, field, match[1])
	}
	check.networkClientTargets(service, field, test)
}

// networkClientTargets is P6 for targets written without a scheme: what
// curl, wget, nc and the like are given must be the container itself, and
// command substitution could hide what that is. It guards against injected
// healthchecks; it is no boundary, since the container can reach the
// network anyway.
func (check *checker) networkClientTargets(service *ir.Service, field, test string) {
	clientSeen, arguments := false, false
	for _, token := range shellToken.FindAllString(test, -1) {
		switch {
		case strings.Trim(token, shellOperators) == "":
			arguments = false
		case slices.Contains(networkClients, path.Base(token)):
			clientSeen, arguments = true, true
		case arguments:
			if match := bareHost.FindStringSubmatch(token); match != nil {
				check.probeHost(service, field, match[1])
			}
		}
	}
	if clientSeen && (strings.Contains(test, "$(") || strings.Contains(test, "`")) {
		check.add("P6", Deny, service.Name, field+".test", "healthcheck runs a network client with shell command substitution, which can hide what it contacts")
	}
}

func (check *checker) probeHost(service *ir.Service, field, host string) {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if !slices.Contains(loopbackHosts, host) && host != service.Name {
		check.add("P6", Deny, service.Name, field+".test", "healthcheck contacts %q; it may only probe the container itself", host)
	}
}

func (check *checker) ports(service *ir.Service, field string) {
	for index, port := range service.Ports {
		portField := fmt.Sprintf("%s.ports.%d", field, index)
		if port.Container < 1 || port.Container > maxPort {
			check.add("P4", Deny, service.Name, portField, "container port %d is outside 1-%d", port.Container, maxPort)
		}
		if port.HostPort < 0 || port.HostPort > maxPort {
			check.add("P4", Deny, service.Name, portField, "host port %d is outside 0-%d", port.HostPort, maxPort)
		}
		if !slices.Contains(ir.Protocols, port.Protocol) {
			check.add("P4", Deny, service.Name, portField, "protocol %q must be tcp or udp", port.Protocol)
		}
		if port.Publish && port.HostPort > 0 && port.HostPort < lowPortLimit {
			check.need("P4", ir.NeedLowPort, service.Name, portField, "%s publishes host port %d, below %d", service.Name, port.HostPort, lowPortLimit)
		}
	}
}

func (check *checker) user(service *ir.Service, field string) {
	user := service.User
	field += ".user"
	uid, _, _ := strings.Cut(user, ":")
	switch {
	case user == "":
	case user == rootUser || uid == rootUID:
		check.add("P5", Warn, service.Name, field, "%s runs as root; the image default user is preferred", service.Name)
	case !userPattern.MatchString(user):
		check.add("P5", Deny, service.Name, field, "user %q must be a numeric uid or uid:gid", user)
	}
}

func (check *checker) commands(service *ir.Service, field string) {
	// A service granted the socket may name it, as in -H unix:///var/run/docker.sock.
	socketGranted := check.granted(ir.NeedDockerSocket) && check.requested(ir.NeedDockerSocket, service.Name, "")
	for name, argv := range map[string][]string{"command": service.Command, "entrypoint": service.Entrypoint} {
		joined := strings.Join(argv, " ")
		if socketGranted {
			joined = dockerSocketMention.ReplaceAllString(joined, "")
		}
		if match := dangerousCommand.FindString(joined); match != "" {
			check.add("P6", Deny, service.Name, field+"."+name, "%s contains %q, which can reach the network, a device or the Docker socket", name, match)
		}
	}
}

func (check *checker) serviceEnv(plan *ir.IR, service *ir.Service, field string) {
	seen := map[string]bool{}
	for index, variable := range service.Env {
		envField := fmt.Sprintf("%s.env.%d", field, index)
		if !envKeyPattern.MatchString(variable.Key) {
			check.add("P7", Deny, service.Name, envField, "variable name %q is not valid", variable.Key)
		}
		if seen[variable.Key] {
			check.add("C", Deny, service.Name, envField, "variable %s is set twice", variable.Key)
		}
		seen[variable.Key] = true
		hasRef, hasLiteral := variable.Ref != "", variable.Literal != ""
		if hasRef == hasLiteral {
			check.add("P7", Deny, service.Name, envField, "variable %s needs exactly one of ref and literal", variable.Key)
			continue
		}
		if _, found := plan.EnvEntry(variable.Ref); hasRef && !found {
			check.add("P7", Deny, service.Name, envField, "variable %s refers to %s, which is not in env_file", variable.Key, variable.Ref)
		}
		if !hasLiteral {
			continue
		}
		check.value(plan, service.Name, envField, variable.Literal)
		if variable.Secret && !referencesOnly(variable.Literal) {
			check.add("P7", Deny, service.Name, envField, "secret %s must come from env_file, not a literal", variable.Key)
		}
	}
}

func (check *checker) envFile(plan *ir.IR) {
	seen := map[string]bool{}
	for index, entry := range plan.EnvFile {
		field := fmt.Sprintf("env_file.%d", index)
		if !envKeyPattern.MatchString(entry.Key) {
			check.add("P7", Deny, "", field, "variable name %q is not valid", entry.Key)
		}
		if seen[entry.Key] {
			check.add("C", Deny, "", field, "env_file defines %s twice", entry.Key)
		}
		seen[entry.Key] = true
		if !slices.Contains(ir.Generators, entry.Generate) {
			check.add("P7", Deny, "", field, "generator %q is not one of %q", entry.Generate, ir.Generators)
		}
		check.value(plan, "", field, entry.Value)
		check.secretEntry(entry, field)
		if slices.Contains(referenceNames(entry.Value), entry.Key) {
			check.add("P7", Deny, "", field, "%s refers to itself", entry.Key)
		}
	}
}

// optionalEnv is P7 for settings the upstream documents and the plan leaves
// unset: compose.yaml names each one, and .env shows its default commented
// out, so a default never carries a newline or a command.
func (check *checker) optionalEnv(plan *ir.IR) {
	for index, setting := range plan.OptionalEnv {
		field := fmt.Sprintf("optional_env.%d", index)
		if !envKeyPattern.MatchString(setting.Key) {
			check.add("P7", Deny, setting.Service, field, "variable name %q is not valid", setting.Key)
		}
		if _, found := plan.Service(setting.Service); !found {
			check.add("P7", Deny, setting.Service, field, "%s is documented for %q, which is not a service", setting.Key, setting.Service)
		}
		unsafe := strings.ContainsAny(setting.Default, "\n\r`") || strings.Contains(setting.Default, "$(")
		if unsafe {
			check.add("P7", Deny, setting.Service, field, "the documented default of %s has a newline or a shell command", setting.Key)
		}
	}
}

func (check *checker) secretEntry(entry ir.EnvFileEntry, field string) {
	generated := entry.Generate != ""
	switch {
	case generated && !entry.Secret:
		check.add("P7", Deny, "", field, "%s is generated, so it must be marked secret", entry.Key)
	case generated && entry.Value != "":
		check.add("P7", Deny, "", field, "%s is generated, so the plan must not carry a value", entry.Key)
	case entry.Secret && !generated && entry.Value != "" && !hasReference(entry.Value):
		check.add("P7", Deny, "", field, "secret %s must be generated, never written into the plan", entry.Key)
	case entry.Secret && !generated && entry.Value == "" && entry.Required:
		check.add("P7", Warn, "", field, "required secret %s has no value; set it before deploying", entry.Key)
	}
	if entry.FileEnvKey == "" {
		return
	}
	if !strings.HasSuffix(entry.FileEnvKey, "_FILE") || !envKeyPattern.MatchString(entry.FileEnvKey) || !entry.Secret {
		check.add("P7", Deny, "", field, "file_env_key %q must be a secret's *_FILE variable", entry.FileEnvKey)
	}
}

func (check *checker) value(plan *ir.IR, service, field, value string) {
	if strings.ContainsAny(value, "\n\r") {
		check.add("P7", Deny, service, field, "value contains a newline")
	}
	if strings.Contains(value, "$(") || strings.Contains(value, "`") {
		check.add("P7", Deny, service, field, "value contains shell command substitution")
	}
	for _, name := range referenceNames(value) {
		if _, found := plan.EnvEntry(name); !found {
			check.add("P7", Deny, service, field, "value refers to ${%s}, which is not in env_file", name)
		}
	}
}

func referenceNames(value string) []string {
	var names []string
	for _, match := range referencePattern.FindAllStringSubmatch(value, -1) {
		names = append(names, match[1])
	}
	return names
}

func hasReference(value string) bool {
	return referencePattern.MatchString(value)
}

func referencesOnly(value string) bool {
	return hasReference(value) && strings.TrimSpace(referencePattern.ReplaceAllString(value, "")) == ""
}

func (check *checker) volumes(service *ir.Service, field string) {
	targets := map[string]bool{}
	for index, volume := range service.Volumes {
		volumeField := fmt.Sprintf("%s.volumes.%d", field, index)
		if !volumeNamePattern.MatchString(volume.Name) {
			check.add("P8", Deny, service.Name, volumeField, "volume name %q must be 2-63 characters of a-z, 0-9, _ and -", volume.Name)
		}
		target := path.Clean(volume.ContainerPath)
		if !path.IsAbs(volume.ContainerPath) || target == "/" {
			check.add("P8", Deny, service.Name, volumeField, "container path %q must be an absolute path below /", volume.ContainerPath)
		}
		if ForbiddenTarget(target) {
			check.add("P8", Deny, service.Name, volumeField, "container path %s is reserved for the host", target)
		}
		if targets[target] {
			check.add("P8", Deny, service.Name, volumeField, "container path %s is mounted twice", target)
		}
		targets[target] = true
	}
}

// ForbiddenTarget reports container paths that must never receive a mount.
func ForbiddenTarget(target string) bool {
	clean := path.Clean(target)
	return slices.ContainsFunc(forbiddenTargets, func(forbidden string) bool {
		return clean == forbidden || strings.HasPrefix(clean, forbidden+"/")
	})
}

func (check *checker) resources(service *ir.Service, field string) {
	resources := service.Resources
	field += ".resources"
	if resources.MemoryLimit == "" || resources.CPUs == "" || resources.PIDsLimit == 0 {
		check.add("P10", Fix, service.Name, field, "missing limits for %s are filled from the profile defaults", service.Name)
	}
	if _, err := units.RAMInBytes(resources.MemoryLimit); resources.MemoryLimit != "" && err != nil {
		check.add("P10", Deny, service.Name, field+".memory_limit", "memory limit %q is not a size such as 512m", resources.MemoryLimit)
	}
	if cpus, err := strconv.ParseFloat(resources.CPUs, 64); resources.CPUs != "" && (err != nil || cpus <= 0) {
		check.add("P10", Deny, service.Name, field+".cpus", "cpus %q must be a positive number", resources.CPUs)
	}
	if resources.PIDsLimit < 0 {
		check.add("P10", Deny, service.Name, field+".pids_limit", "pids limit must not be negative")
	}
}

func (check *checker) needs(plan *ir.IR) {
	for index, need := range plan.Needs {
		field := fmt.Sprintf("needs.%d", index)
		if !slices.Contains(ir.Capabilities, need.Capability) {
			check.add("P9", Deny, need.Service, field, "capability %q is not one of %s", need.Capability, strings.Join(ir.Capabilities, ", "))
			continue
		}
		if _, found := plan.Service(need.Service); !found {
			check.add("P9", Deny, need.Service, field, "%s is requested for %q, which is not a service", need.Capability, need.Service)
			continue
		}
		check.need("P9", need.Capability, need.Service, field, "%s requests %s: %s", need.Service, need.Capability, need.Reason)
	}
}

// need records a request that blocks the run until a human grants the
// capability; once granted it stays in the report as an OK finding.
func (check *checker) need(rule, capability, service, field, format string, arguments ...any) {
	severity := Need
	suffix := fmt.Sprintf(" (grant with --allow-need %s)", capability)
	if check.granted(capability) {
		severity, suffix = OK, " (granted by the user)"
	}
	check.findings = append(check.findings, Finding{
		Rule: rule, Severity: severity, Service: service, Path: field, Capability: capability,
		Message: fmt.Sprintf(format, arguments...) + suffix,
	})
}

func (check *checker) dependencies(plan *ir.IR) {
	for serviceIndex, service := range plan.Services {
		for index, dependency := range service.DependsOn {
			field := fmt.Sprintf("services.%d.depends_on.%d", serviceIndex, index)
			check.dependency(plan, service, dependency, field)
		}
	}
	if cycle := dependencyCycle(plan); len(cycle) > 0 {
		check.add("P11", Deny, "", "services", "depends_on forms a cycle: %s", strings.Join(cycle, " -> "))
	}
}

func (check *checker) dependency(plan *ir.IR, service ir.Service, dependency ir.Dependency, field string) {
	if dependency.Service == service.Name {
		check.add("P11", Deny, service.Name, field, "%s depends on itself", service.Name)
		return
	}
	target, found := plan.Service(dependency.Service)
	if !found {
		check.add("P11", Deny, service.Name, field, "%s depends on %q, which is not a service", service.Name, dependency.Service)
		return
	}
	if !slices.Contains(ir.Conditions, dependency.Condition) {
		check.add("P11", Deny, service.Name, field, "condition %q is not one of %s", dependency.Condition, strings.Join(ir.Conditions, ", "))
	}
	if dependency.Condition == ir.ConditionHealthy && !target.Healthcheck.Defined() {
		check.add("P11", Deny, service.Name, field, "%s waits for %s to be healthy, but %s has no healthcheck", service.Name, target.Name, target.Name)
	}
}

func dependencyCycle(plan *ir.IR) []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(name string) []string
	visit = func(name string) []string {
		state[name] = visiting
		stack = append(stack, name)
		service, _ := plan.Service(name)
		for _, dependency := range service.DependsOn {
			if _, found := plan.Service(dependency.Service); !found || dependency.Service == name {
				continue
			}
			if state[dependency.Service] == visiting {
				start := slices.Index(stack, dependency.Service)
				return append(slices.Clone(stack[start:]), dependency.Service)
			}
			if state[dependency.Service] == unvisited {
				if cycle := visit(dependency.Service); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		return nil
	}
	for _, service := range plan.Services {
		if state[service.Name] != unvisited {
			continue
		}
		if cycle := visit(service.Name); cycle != nil {
			return cycle
		}
	}
	return nil
}
