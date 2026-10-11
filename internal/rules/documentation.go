package rules

import (
	"path"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
)

// fromDocumentation builds a single-service plan when the repository has no
// usable compose file (R6): the documented `docker run` command, else an
// image name the README or CI publishes.
func (builder *builder) fromDocumentation() {
	service := ir.Service{Name: slug(firstNonEmpty(builder.repositoryName(), "app")), Role: ir.RoleApp}
	run, found := builder.documentedRun()
	service.Image = builder.documentedImage(run, found)
	service.Provenance = service.Image.Provenance
	if found {
		builder.evidence("documented docker run command", run.Location)
		builder.runSettings(&service, run)
	}
	for _, sample := range builder.facts.EnvSamples {
		builder.rootSample(&service, sample)
	}
	builder.plan.Services = append(builder.plan.Services, service)
}

func (builder *builder) documentedRun() (extract.DockerRun, bool) {
	for _, run := range builder.facts.DockerRuns {
		image, err := ir.ParseImage(run.Image)
		if err == nil && builder.matchesIdentity(image) && strings.HasPrefix(run.Location.File, "README") {
			return run, true
		}
	}
	for _, run := range builder.facts.DockerRuns {
		image, err := ir.ParseImage(run.Image)
		if err == nil && builder.matchesIdentity(image) {
			return run, true
		}
	}
	return extract.DockerRun{}, false
}

// documentedImage prefers an image of the same application published in the
// repository owner's own namespace (trusted by policy P1 without a flag),
// then the image of the documented docker run command, then the
// application's image named anywhere else in the documentation, which P1 may
// ask the user to allow.
func (builder *builder) documentedImage(run extract.DockerRun, found bool) ir.Image {
	owner := strings.ToLower(builder.input.Source.Owner)
	ownNamespace := func(image ir.Image) bool {
		return owner != "" && strings.HasPrefix(strings.ToLower(image.Repository), owner+"/")
	}
	if image, published := builder.documentedImageOf(ownNamespace); published {
		return image
	}
	if found {
		return runImage(run)
	}
	if image, documented := builder.documentedImageOf(func(ir.Image) bool { return true }); documented {
		return image
	}
	return ir.Image{Provenance: ir.Provenance{Source: ir.SourceDefault, Confidence: ir.ConfidenceLow}}
}

func runImage(run extract.DockerRun) ir.Image {
	image, _ := ir.ParseImage(run.Image)
	if image.Tag == "" {
		image.Tag = "latest"
	}
	image.Provenance = provenance(ir.SourceReadme, run.Location, ir.ConfidenceMedium)
	return image
}

func (builder *builder) runSettings(service *ir.Service, run extract.DockerRun) {
	at := provenance(ir.SourceReadme, run.Location, ir.ConfidenceMedium)
	for _, mapping := range run.Ports {
		port, ok := parsePortMapping(mapping)
		if ok {
			port.Provenance = at
			service.Ports = append(service.Ports, port)
		}
	}
	for _, mapping := range run.Volumes {
		parts := strings.Split(mapping, ":")
		const sourceAndTarget = 2
		if len(parts) < sourceAndTarget || isHostPath(parts[0]) {
			continue
		}
		target := path.Clean(parts[1])
		addVolume(builder.plan, service, ir.Volume{Name: lastSegment(target), ContainerPath: target, Provenance: at})
	}
	for _, variable := range run.Env {
		builder.reference(service, variable.Key, variable.Key, variable.Value, true, at)
	}
}

func (builder *builder) rootSample(service *ir.Service, sample extract.EnvSample) {
	for _, entry := range sample.Entries {
		if entry.Commented {
			continue
		}
		builder.reference(service, entry.Key, entry.Key, entry.Value, false, provenance(ir.SourceEnvSample, entry.Location, ir.ConfidenceMedium))
	}
}

// parsePortMapping reads docker run -p forms: 8080, 8000:80, 127.0.0.1:8000:80/udp.
func parsePortMapping(mapping string) (ir.Port, bool) {
	specification, protocol, _ := strings.Cut(mapping, "/")
	if protocol == "" {
		protocol = ir.ProtocolTCP
	}
	parts := strings.Split(specification, ":")
	container, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return ir.Port{}, false
	}
	port := ir.Port{Container: container, Protocol: protocol, Publish: true}
	const hostAndContainer = 2
	if len(parts) >= hostAndContainer {
		host, _ := strconv.Atoi(parts[len(parts)-2])
		if host >= lowPortLimit {
			port.HostPort = host
		}
	}
	return port, true
}

func lastSegment(target string) string {
	parts := strings.Split(strings.Trim(target, "/"), "/")
	return parts[len(parts)-1]
}

func (builder *builder) repositoryName() string {
	return builder.input.Source.Repository
}
