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
	scoreDepth     = 2
	scorePreferred = 5
	scoreMentioned = 8
	scoreIdentity  = 3
	scoreAddon     = 5
	lowPortLimit   = 1024
	volumeBind     = "bind"
	volumeTmpfs    = "tmpfs"
)

var (
	excludedVariant = regexp.MustCompile(`(?i)(^|[/._-])(dev|devel|development|test|tests|testing|ci|debug|local|e2e|example-dev|hacks?|experiments?|experimental|fixtures?|playground|scratch|staging)([/._-]|$)`)
	proxyVariant    = regexp.MustCompile(`(?i)(traefik|caddy|nginx|proxy|swag|portainer|letsencrypt|https)`)
	addonVariant    = regexp.MustCompile(`(?i)(tika|gotenberg|ldap|oidc|authelia|keycloak|monitoring|prometheus|grafana|backup)`)
	preferredNames  = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml", "basic.yml", "basic.yaml"}
	// unstableTag marks tags that follow development builds; a documented
	// stable tag is preferred over them.
	unstableTag = regexp.MustCompile(`(?i)(^|[._-])(nightly|dev|develop|edge|beta|alpha|rc[0-9]*|canary|unstable|snapshot|preview|main|master)([._-]|$)`)
	// hostPaths are bind sources that expose the host itself; they are never
	// turned into volumes.
	hostPaths = []string{"/var/run/docker.sock", "/run/docker.sock", "/etc/localtime", "/etc/timezone", "/proc", "/sys", "/dev", "/run", "/var/run"}
)

// chooseCompose is R1's variant selection: among the candidate files, prefer
// the canonical name, files the docs mention, shallow paths and plain
// variants over add-on ones.
func chooseCompose(facts *extract.Facts, identity []string) *extract.ComposeFile {
	var best *extract.ComposeFile
	bestScore := 0
	for index := range facts.ComposeFiles {
		file := &facts.ComposeFiles[index]
		if !candidate(file, identity) {
			continue
		}
		score := composeScore(file, facts, identity)
		better := best == nil || score > bestScore || (score == bestScore && len(file.Path) < len(best.Path))
		if better {
			best, bestScore = file, score
		}
	}
	return best
}

func composeScore(file *extract.ComposeFile, facts *extract.Facts, identity []string) int {
	base := path.Base(file.Path)
	score := -scoreDepth * strings.Count(file.Path, "/")
	score -= len(file.Services)
	if slices.Contains(preferredNames, base) {
		score += scorePreferred
	}
	if slices.ContainsFunc(facts.FileMentions, func(note extract.Note) bool { return note.Text == base }) {
		score += scoreMentioned
	}
	if addonVariant.MatchString(base) {
		score -= scoreAddon
	}
	for _, service := range file.Services {
		image, err := ir.ParseImage(service.Image)
		if err == nil && containsAny(strings.ToLower(image.Repository), identity) {
			score += scoreIdentity
			break
		}
	}
	return score
}

// candidate is false for the files no deployment starts from: test,
// development and staging variants by name; development files by what they
// mount, the repository itself; proxy variants, since the profile adds the
// proxy; add-on files without an app; and files that only build another
// program kept in the same repository.
func candidate(file *extract.ComposeFile, identity []string) bool {
	variant := excludedVariant.MatchString(file.Path) || proxyVariant.MatchString(path.Base(file.Path))
	return !variant && !mountsRepository(file) && hasAppService(file) && !buildsAnotherProgram(file, identity)
}

// mountsRepository is true for a development file: it mounts the checked-out
// repository into a container to run the code being edited.
func mountsRepository(file *extract.ComposeFile) bool {
	return slices.ContainsFunc(file.Services, func(service extract.ComposeService) bool {
		return slices.ContainsFunc(service.Volumes, func(volume extract.ComposeVolume) bool {
			return volume.Type == volumeBind && !path.IsAbs(volume.Source) && strings.Trim(path.Clean(volume.Source), "./") == ""
		})
	})
}

// buildsAnotherProgram is true when the file builds services from source,
// and every service that is not a database, cache or proxy is built under a
// local name and none of them carries the project's name.
func buildsAnotherProgram(file *extract.ComposeFile, identity []string) bool {
	builds := false
	for _, service := range file.Services {
		image, err := ir.ParseImage(service.Image)
		if _, sidecar := known.family(image); err == nil && sidecar {
			continue
		}
		if !builtLocally(service) || containsAny(strings.ToLower(service.Name), identity) || containsAny(strings.ToLower(service.Image), identity) {
			return false
		}
		builds = true
	}
	return builds
}

// builtLocally is true for a service compose builds from the repository and
// tags with a bare local name, such as mealie:dev, which no registry serves.
func builtLocally(service extract.ComposeService) bool {
	return service.Build && !strings.Contains(service.Image, "/")
}

// hasAppService is false for add-on files that only define databases and
// caches to merge into the main file. A proxy counts: a file with nothing but
// nginx deploys nginx.
func hasAppService(file *extract.ComposeFile) bool {
	return slices.ContainsFunc(file.Services, func(service extract.ComposeService) bool {
		if service.Build {
			return true
		}
		image, err := ir.ParseImage(service.Image)
		if err != nil {
			return false
		}
		family, sidecar := known.family(image)
		return !sidecar || family.Role == ir.RoleProxy
	})
}

func (builder *builder) fromCompose(file *extract.ComposeFile) {
	builder.evidence("compose file "+file.Path+" chosen among "+strconv.Itoa(len(builder.facts.ComposeFiles))+" upstream compose files",
		extract.Location{File: file.Path, Line: 1})
	app := builder.appService(file)
	for _, service := range file.Services {
		for _, volume := range service.Volumes {
			if volume.Type != volumeBind && volume.Source != "" {
				builder.namedVolumes[volumeName(volume.Source, service.Name)] = true
			}
		}
	}
	for _, service := range file.Services {
		builder.plan.Services = append(builder.plan.Services, builder.composeService(file, service, service.Name == app))
	}
}

// appService picks the service people use: the one running the project's
// own image, else the one built from the repository, else the first service
// that is not a known database, cache or proxy, else the first proxy.
func (builder *builder) appService(file *extract.ComposeFile) string {
	var built, firstOther, firstProxy string
	for _, service := range file.Services {
		image, err := ir.ParseImage(service.Image)
		if err == nil && builder.matchesIdentity(image) {
			return service.Name
		}
		if service.Build && built == "" {
			built = service.Name
		}
		family, sidecar := known.family(image)
		if err == nil && !sidecar && firstOther == "" {
			firstOther = service.Name
		}
		if err == nil && sidecar && family.Role == ir.RoleProxy && firstProxy == "" {
			firstProxy = service.Name
		}
	}
	return firstNonEmpty(built, firstOther, firstProxy, file.Services[0].Name)
}

func (builder *builder) composeService(file *extract.ComposeFile, service extract.ComposeService, isApp bool) ir.Service {
	high := provenance(ir.SourceCompose, service.Location, ir.ConfidenceHigh)
	// Lower-cased now, as Normalize would later, so names made from it are too.
	name := strings.ToLower(service.Name)
	result := ir.Service{Name: name, Role: ir.RoleWorker, Provenance: high, Command: service.Command, Entrypoint: service.Entrypoint}
	result.Image = builder.composeImage(service)
	if isApp {
		result.Role = ir.RoleApp
		builder.evidence("app image "+result.Image.Reference(), service.ImageAt)
	} else if family, found := known.family(result.Image); found {
		result.Role = family.Role
	}
	result.User = numericUser(service.User)
	if service.User != "" && result.User == "" {
		builder.note(&result, "upstream runs it as user %q; the image default user is kept", service.User)
	}
	builder.composePorts(&result, service)
	builder.composeVolumes(&result, service)
	builder.composeEnvironment(file, &result, service)
	for _, dependency := range service.DependsOn {
		result.DependsOn = append(result.DependsOn, ir.Dependency{Service: strings.ToLower(dependency.Service), Condition: dependency.Condition})
	}
	if service.Healthcheck != nil {
		result.Healthcheck = composeHealthcheck(*service.Healthcheck)
	}
	return result
}

func (builder *builder) composeImage(service extract.ComposeService) ir.Image {
	image, err := ir.ParseImage(service.Image)
	if err != nil || service.Image == "" || builtLocally(service) {
		return builder.publishedImage(service)
	}
	if image.Tag == "" && image.Digest == "" {
		image.Tag = "latest"
	}
	image.Provenance = provenance(ir.SourceCompose, service.ImageAt, ir.ConfidenceHigh)
	return image
}

// publishedImage replaces build: with the image the project publishes, found
// in its CI workflows or documentation. Building from source is out of scope.
func (builder *builder) publishedImage(service extract.ComposeService) ir.Image {
	if image, found := builder.documentedImageOf(func(ir.Image) bool { return true }); found {
		return image
	}
	return ir.Image{Provenance: ir.Provenance{Source: ir.SourceDefault, Confidence: ir.ConfidenceLow}}
}

// documentedImageOf is the project's own image that CI workflows, then the
// documentation, name and accept takes, preferring a stable tag over one
// that follows development builds.
func (builder *builder) documentedImageOf(accept func(ir.Image) bool) (ir.Image, bool) {
	var images []ir.Image
	for _, kind := range []string{extract.KindWorkflow, extract.KindReadme} {
		for _, mention := range builder.facts.Images {
			image, err := ir.ParseImage(mention.Reference)
			if err != nil || mention.Kind != kind || !builder.matchesIdentity(image) || !accept(image) {
				continue
			}
			if image.Tag == "" {
				image.Tag = "latest"
			}
			image.Provenance = provenance(sourceOfKind(kind), mention.Location, ir.ConfidenceMedium)
			images = append(images, image)
		}
	}
	if stable := slices.IndexFunc(images, func(image ir.Image) bool { return !unstableTag.MatchString(image.Tag) }); stable >= 0 {
		return images[stable], true
	}
	if len(images) > 0 {
		return images[0], true
	}
	return ir.Image{}, false
}

func (builder *builder) composePorts(result *ir.Service, service extract.ComposeService) {
	for _, port := range service.Ports {
		published, _ := strconv.Atoi(port.Published)
		converted := ir.Port{
			Container: port.Target, Protocol: port.Protocol, Publish: port.Published != "",
			Provenance: provenance(ir.SourceCompose, port.Location, ir.ConfidenceHigh),
		}
		if published >= lowPortLimit {
			converted.HostPort = published
		}
		if published > 0 && published < lowPortLimit {
			builder.note(result, "upstream publishes host port %d; a port above 1024 is used so no extra privilege is needed", published)
		}
		result.Ports = append(result.Ports, converted)
	}
}

// composeVolumes keeps named volumes and turns bind mounts of directories
// into named volumes; the profile can render them back as bind mounts under
// its base path. Host system paths are dropped.
func (builder *builder) composeVolumes(result *ir.Service, service extract.ComposeService) {
	for _, volume := range service.Volumes {
		at := provenance(ir.SourceCompose, volume.Location, ir.ConfidenceHigh)
		switch {
		case volume.Type == volumeTmpfs:
			continue
		case volume.Type == volumeBind && isHostPath(volume.Source):
			builder.note(result, "upstream mounts the host path %s; it is left out", volume.Source)
			continue
		case volume.Type == volumeBind && looksLikeFile(volume.Target):
			builder.note(result, "upstream mounts the file %s at %s; provide it yourself after export", volume.Source, volume.Target)
			continue
		case volume.Type == volumeBind:
			builder.bindNote(result, volume)
		}
		mountVolume(result, ir.Volume{Name: builder.composeVolumeName(result, volume), ContainerPath: volume.Target, Provenance: at})
	}
}

// composeVolumeName names the volume behind an upstream mount. A named
// volume keeps its name: upstream shares it on purpose. A bind-mounted
// folder becomes a volume named after the folder, one per folder, so
// services that share the folder share the volume. An anonymous volume
// gets a name nothing else uses.
func (builder *builder) composeVolumeName(service *ir.Service, volume extract.ComposeVolume) string {
	if volume.Type != volumeBind && volume.Source != "" {
		return volumeName(volume.Source, service.Name)
	}
	if volume.Type != volumeBind {
		return freeVolumeName(path.Base(volume.Target), service.Name, builder.usedVolumeNames(service))
	}
	folder := path.Clean(volume.Source)
	if name, seen := builder.bindVolumes[folder]; seen {
		return name
	}
	name := freeVolumeName(path.Base(folder), service.Name, builder.usedVolumeNames(service))
	builder.bindVolumes[folder] = name
	return name
}

// bindNote says what became of a bind mount. A read-only folder held
// content upstream expects from the user, and the volume starts empty.
func (builder *builder) bindNote(service *ir.Service, volume extract.ComposeVolume) {
	if volume.ReadOnly {
		builder.note(service, "upstream mounts %s read-only at %s with content you provide; the named volume here starts empty, so copy it in or use --storage bind", volume.Source, volume.Target)
		return
	}
	builder.note(service, "upstream bind mount %s:%s is a named volume here (use --storage bind for a host folder)", volume.Source, volume.Target)
}

// usedVolumeNames adds the names upstream gives its named volumes, which
// later services may still mount, to those already mounted.
func (builder *builder) usedVolumeNames(service *ir.Service) map[string]bool {
	used := planVolumeNames(builder.plan, service)
	for name := range builder.namedVolumes {
		used[name] = true
	}
	return used
}

func planVolumeNames(plan *ir.IR, service *ir.Service) map[string]bool {
	used := map[string]bool{}
	for _, existing := range plan.Services {
		for _, volume := range existing.Volumes {
			used[volume.Name] = true
		}
	}
	for _, volume := range service.Volumes {
		used[volume.Name] = true
	}
	return used
}

// freeVolumeName keeps one volume to one purpose: a candidate already in
// use gets the service's name in front, then a number.
func freeVolumeName(candidate, service string, used map[string]bool) string {
	name := volumeName(candidate, service)
	if !used[name] {
		return name
	}
	prefixed := volumeName(service+"-"+name, service)
	name = prefixed
	for number := 2; used[name]; number++ {
		name = prefixed + "-" + strconv.Itoa(number)
	}
	return name
}

// addVolume mounts a volume that holds this service's own data, under a
// name no other mount of the plan uses.
func addVolume(plan *ir.IR, service *ir.Service, volume ir.Volume) {
	volume.Name = freeVolumeName(volume.Name, service.Name, planVolumeNames(plan, service))
	mountVolume(service, volume)
}

// mountVolume adds volume unless the service already mounts that path.
func mountVolume(service *ir.Service, volume ir.Volume) {
	target := path.Clean(volume.ContainerPath)
	if slices.ContainsFunc(service.Volumes, func(existing ir.Volume) bool { return path.Clean(existing.ContainerPath) == target }) {
		return
	}
	service.Volumes = append(service.Volumes, volume)
}

var volumeCleaner = regexp.MustCompile(`[^a-z0-9_-]+`)

func volumeName(name, service string) string {
	cleaned := strings.Trim(volumeCleaner.ReplaceAllString(strings.ToLower(name), "-"), "-_")
	const minimumLength = 2
	if len(cleaned) < minimumLength {
		cleaned = slug(service) + "-data"
	}
	return cleaned
}

func isHostPath(source string) bool {
	clean := path.Clean(source)
	return slices.ContainsFunc(hostPaths, func(host string) bool { return clean == host || strings.HasPrefix(clean, host+"/") })
}

func looksLikeFile(target string) bool {
	base := path.Base(target)
	return strings.Contains(base, ".") && !strings.HasPrefix(base, ".")
}

func numericUser(user string) string {
	for _, character := range user {
		if (character < '0' || character > '9') && character != ':' {
			return ""
		}
	}
	return user
}

func composeHealthcheck(healthcheck extract.Healthcheck) ir.Healthcheck {
	at := provenance(ir.SourceCompose, healthcheck.Location, ir.ConfidenceHigh)
	if healthcheck.Disabled {
		return ir.Healthcheck{OmittedReason: "disabled upstream", Provenance: at}
	}
	return ir.Healthcheck{Test: normaliseTest(healthcheck.Test), Interval: healthcheck.Interval, Timeout: healthcheck.Timeout,
		Retries: healthcheck.Retries, StartPeriod: healthcheck.StartPeriod, Provenance: at}
}

// normaliseTest turns compose's string form into CMD-SHELL so the policy and
// renderer only ever see the exec forms.
func normaliseTest(test []string) []string {
	if len(test) == 0 || test[0] == ir.HealthcheckCommand || test[0] == ir.HealthcheckShell {
		return test
	}
	if test[0] == "NONE" {
		return nil
	}
	return []string{ir.HealthcheckShell, strings.Join(test, " ")}
}

func sourceOfKind(kind string) string {
	switch kind {
	case extract.KindWorkflow:
		return ir.SourceCIWorkflow
	case extract.KindDockerfile:
		return ir.SourceDockerfile
	case extract.KindEnvSample:
		return ir.SourceEnvSample
	case extract.KindCompose:
		return ir.SourceCompose
	case extract.KindCode:
		return ir.SourceCode
	case extract.KindHelm:
		return ir.SourceHelm
	}
	return ir.SourceReadme
}

func containsAny(text string, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return strings.Contains(text, name) })
}
