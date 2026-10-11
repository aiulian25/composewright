package rules

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	catalogueInterval    = "10s"
	catalogueTimeout     = "5s"
	catalogueRetries     = 5
	catalogueStartPeriod = "30s"
	scoreRootDockerfile  = 6
	scoreDockerDirectory = 3
	// scoreServiceDirectory outweighs a root Dockerfile: a folder named
	// after the service says which image it builds.
	scoreServiceDirectory = 10
)

// genericFolders hold Dockerfiles without naming what they build.
var genericFolders = []string{"docker", "build", "deploy", "deployment", "packaging", "container", "containers", "dockerfiles", "images", "contrib", "scripts", "src"}

var excludedDockerfile = regexp.MustCompile(`(?i)(dev|test|builder|base|ci|\.j2$|release|wix|latest-warning|push)`)

// enrich applies R2 (Dockerfile), R4 (sidecars) and R5 (healthcheck
// catalogue) after the services exist, then links connection strings.
func (builder *builder) enrich() {
	builder.addDatabase()
	for index := range builder.plan.Services {
		service := &builder.plan.Services[index]
		dockerfile, found := chooseDockerfile(builder.facts, service)
		if found && service.Role == ir.RoleApp && builder.matchesIdentity(service.Image) {
			builder.fromDockerfile(service, dockerfile)
		}
		builder.sidecar(service)
		builder.catalogueHealthcheck(service)
		if service.Resources.MemoryLimit == "" {
			service.Resources.MemoryLimit = known.appMemory(service.Image)
		}
	}
	builder.waitForHealthy()
	builder.linkSecrets()
	builder.publicURLs()
	builder.containerListen()
}

func chooseDockerfile(facts *extract.Facts, service *ir.Service) (extract.Dockerfile, bool) {
	best, bestScore := extract.Dockerfile{}, 0
	found := false
	for _, dockerfile := range facts.Dockerfiles {
		if excludedDockerfile.MatchString(dockerfile.Path) {
			continue
		}
		score := -strings.Count(dockerfile.Path, "/")
		if buildsService(dockerfile.Path, service) {
			score += scoreServiceDirectory
		}
		if !strings.Contains(dockerfile.Path, "/") {
			score += scoreRootDockerfile
		}
		if strings.HasPrefix(dockerfile.Path, "docker/") || strings.HasPrefix(dockerfile.Path, "Docker/") {
			score += scoreDockerDirectory
		}
		if !found || score > bestScore || (score == bestScore && dockerfile.Path < best.Path) {
			best, bestScore, found = dockerfile, score, true
		}
	}
	return best, found
}

// buildsService is true when a folder of the Dockerfile's path names the
// service: in a monorepo server/Dockerfile builds immich-server and
// machine-learning/Dockerfile builds immich-machine-learning.
func buildsService(dockerfilePath string, service *ir.Service) bool {
	name := strings.ToLower(service.Name + " " + service.Image.Repository)
	return slices.ContainsFunc(strings.Split(path.Dir(dockerfilePath), "/"), func(folder string) bool {
		folder = strings.ToLower(folder)
		return len(folder) >= minIdentityLength && !slices.Contains(genericFolders, folder) && strings.Contains(name, folder)
	})
}

// fromDockerfile is R2: the image's own EXPOSE, VOLUME and HEALTHCHECK fill
// what the compose file or README left out.
func (builder *builder) fromDockerfile(service *ir.Service, dockerfile extract.Dockerfile) {
	if len(service.Ports) == 0 {
		for _, exposed := range dockerfile.Exposed {
			service.Ports = append(service.Ports, ir.Port{Container: exposed.Port, Protocol: exposed.Protocol, Publish: true,
				Provenance: provenance(ir.SourceDockerfile, exposed.Location, ir.ConfidenceMedium)})
		}
	}
	if len(service.Volumes) == 0 {
		for _, volume := range dockerfile.Volumes {
			addVolume(builder.plan, service, ir.Volume{Name: path.Base(volume.Path), ContainerPath: volume.Path,
				Provenance: provenance(ir.SourceDockerfile, volume.Location, ir.ConfidenceMedium)})
		}
	}
	healthcheck := dockerfile.Healthcheck
	if service.Healthcheck.Defined() || healthcheck == nil || healthcheck.Disabled || len(healthcheck.Test) == 0 {
		return
	}
	service.Healthcheck = ir.Healthcheck{Test: escapeDollars(healthcheck.Test), Interval: healthcheck.Interval, Timeout: healthcheck.Timeout,
		Retries: healthcheck.Retries, StartPeriod: healthcheck.StartPeriod,
		Provenance: provenance(ir.SourceDockerfile, healthcheck.Location, ir.ConfidenceMedium)}
}

// escapeDollars keeps a Dockerfile healthcheck's $VARIABLES for the
// container's shell: in compose.yaml an unescaped $ is interpolated by
// Compose from the host's .env instead.
func escapeDollars(test []string) []string {
	escaped := make([]string, len(test))
	for index, part := range test {
		escaped[index] = strings.ReplaceAll(part, "$", "$$")
	}
	return escaped
}

// sidecar is R4: known databases and caches get their role and a named
// volume for their data directory when upstream forgot one.
func (builder *builder) sidecar(service *ir.Service) {
	family, found := known.family(service.Image)
	if !found || service.Role == ir.RoleApp {
		return
	}
	service.Role = family.Role
	dataPath := family.dataPathOf(service.Image)
	if dataPath == "" {
		return
	}
	builder.followMovedData(service, family, dataPath)
	covered := slices.ContainsFunc(service.Volumes, func(volume ir.Volume) bool {
		return overlaps(path.Clean(volume.ContainerPath), dataPath)
	})
	if !covered {
		addVolume(builder.plan, service, ir.Volume{Name: service.Name + "-data", ContainerPath: dataPath,
			Description: family.Name + " data", Provenance: ir.Provenance{Source: ir.SourceSidecarRule, Confidence: ir.ConfidenceMedium}})
	}
}

// followMovedData mounts a volume upstream wrote for the old data path at the
// one this image version uses, unless upstream set where the data goes.
// nextcloud's example mounts postgres:latest, now 18, at the old path, which
// the image refuses.
func (builder *builder) followMovedData(service *ir.Service, candidate family, dataPath string) {
	setByUpstream := slices.ContainsFunc(service.Env, func(variable ir.EnvRef) bool {
		return candidate.DataPathSince != nil && variable.Key == candidate.DataPathSince.UnlessEnv
	})
	if dataPath == candidate.DataPath || setByUpstream {
		return
	}
	for index := range service.Volumes {
		volume := &service.Volumes[index]
		if path.Clean(volume.ContainerPath) != candidate.DataPath {
			continue
		}
		builder.note(service, "upstream mounts %s at %s; this version of the image keeps its data in %s, so it is mounted there",
			volume.Name, volume.ContainerPath, dataPath)
		volume.ContainerPath = dataPath
	}
}

// catalogueHealthcheck is R5: verified healthchecks for known images.
func (builder *builder) catalogueHealthcheck(service *ir.Service) {
	if service.Healthcheck.Defined() || service.Healthcheck.OmittedReason != "" {
		return
	}
	test := known.appHealthcheck(service.Image)
	if family, found := known.family(service.Image); found && test == nil {
		test = family.Healthcheck
	}
	if test == nil {
		return
	}
	service.Healthcheck = ir.Healthcheck{Test: test, Interval: catalogueInterval, Timeout: catalogueTimeout, Retries: catalogueRetries,
		StartPeriod: catalogueStartPeriod, Provenance: ir.Provenance{Source: ir.SourceHealthchecks, Confidence: ir.ConfidenceMedium}}
}

// waitForHealthy makes the app wait until its databases and caches answer,
// not merely until their containers started; it also downgrades a
// service_healthy wait on a service that has no healthcheck.
func (builder *builder) waitForHealthy() {
	for index := range builder.plan.Services {
		service := &builder.plan.Services[index]
		for dependency := range service.DependsOn {
			target, found := builder.plan.Service(service.DependsOn[dependency].Service)
			if !found {
				continue
			}
			sidecar := target.Role == ir.RoleDB || target.Role == ir.RoleCache
			switch {
			case target.Healthcheck.Defined() && sidecar && service.DependsOn[dependency].Condition == ir.ConditionStarted:
				service.DependsOn[dependency].Condition = ir.ConditionHealthy
			case !target.Healthcheck.Defined() && service.DependsOn[dependency].Condition == ir.ConditionHealthy:
				service.DependsOn[dependency].Condition = ir.ConditionStarted
				builder.note(service, "waits for %s to start; %s has no healthcheck to wait for", target.Name, target.Name)
			}
		}
	}
}

// publicURLs points URL-style settings such as BASE_URL or DOMAIN at the
// published app port when upstream left a placeholder.
func (builder *builder) publicURLs() {
	app := builder.plan.AppService()
	if app == nil {
		return
	}
	address := "http://localhost:" + strconv.Itoa(publishedPort(app))
	for index := range builder.plan.EnvFile {
		entry := &builder.plan.EnvFile[index]
		if !builder.ownAddress(entry.Key, app) || !isPlaceholderURL(entry.Value) {
			continue
		}
		entry.Value = address
		entry.Provenance = ir.Provenance{Source: ir.SourceDefault, SourceRef: entry.Provenance.SourceRef, Confidence: ir.ConfidenceMedium}
	}
}

// containerListen points the app's own listen address at every interface.
// Samples written for running the binary use loopback, which inside a
// container nothing outside it can reach: not the published port, not a
// proxy on a shared network. Addresses of other services, such as DB_HOST,
// stay as written.
func (builder *builder) containerListen() {
	app := builder.plan.AppService()
	if app == nil {
		return
	}
	for index := range builder.plan.EnvFile {
		entry := &builder.plan.EnvFile[index]
		setting := listenSetting.FindStringSubmatch(entry.Key)
		loopback := loopbackListen.FindStringSubmatch(entry.Value)
		if setting == nil || loopback == nil || !builder.ownPrefix(setting[1], app) {
			continue
		}
		entry.Value = allInterfaces + loopback[2]
		entry.Provenance = ir.Provenance{Source: ir.SourceDefault, SourceRef: entry.Provenance.SourceRef, Confidence: ir.ConfidenceMedium}
		builder.note(app, "%s listens on every interface: in a container, %s cannot be reached from outside", entry.Key, loopback[1])
	}
}

// IsPublicURL reports whether a variable holds the address people open.
func IsPublicURL(name string) bool {
	return publicURLName.MatchString(name)
}

// ownAddress keeps the rewrite to the app's own address. A prefixed name
// counts only when the prefix is generic or names this app: OLLAMA_BASE_URL
// is the address of another service and stays as written.
func (builder *builder) ownAddress(name string, app *ir.Service) bool {
	match := prefixedAddress.FindStringSubmatch(name)
	if match == nil {
		return IsPublicURL(name)
	}
	return builder.ownPrefix(match[1], app)
}

// ownPrefix reports whether a variable's prefix means this app: none, a
// generic one such as APP, or the app's own name.
func (builder *builder) ownPrefix(prefix string, app *ir.Service) bool {
	ownNames := append([]string{strings.ToLower(app.Name)}, builder.identity...)
	return prefix == "" || slices.Contains(genericAddressPrefixes, prefix) || slices.Contains(ownNames, strings.ToLower(prefix))
}

func isPlaceholderURL(value string) bool {
	lowered := strings.ToLower(value)
	placeholders := []string{"example", "domain.tld", "your", "changeme", "localhost", "my.domain"}
	return value == "" || slices.ContainsFunc(placeholders, func(placeholder string) bool { return strings.Contains(lowered, placeholder) })
}

// publishedPort is the host port people will open: the explicit host port,
// else the container port, moved above 1024 when it is privileged.
func publishedPort(service *ir.Service) int {
	for _, port := range service.Ports {
		if !port.Publish {
			continue
		}
		return HostPort(port)
	}
	if len(service.Ports) > 0 {
		return HostPort(service.Ports[0])
	}
	const fallback = 8080
	return fallback
}

// HostPort is the host side of a published port when the plan leaves it to
// composewright: the container port, or 8000+port for ports below 1024.
func HostPort(port ir.Port) int {
	if port.HostPort > 0 {
		return port.HostPort
	}
	if port.Container < lowPortLimit {
		const offset = 8000
		return port.Container + offset
	}
	return port.Container
}

func overlaps(target, dataPath string) bool {
	return target == dataPath || strings.HasPrefix(dataPath, target+"/") || strings.HasPrefix(target, dataPath+"/")
}
