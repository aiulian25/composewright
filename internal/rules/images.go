package rules

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	healthcheckNone = "NONE"
	userSeparator   = ":"
	rootUID         = "0"
)

// ImageKey identifies an image the way verification results are keyed.
func ImageKey(image ir.Image) string {
	return image.Reference() + ":" + image.Tag
}

// ApplyImages is R3: what the registry says about each image pins its
// digest and fills ports, volumes and healthchecks the repository left out.
// Upstream compose values still win; Dockerfile and catalogue guesses do not.
func ApplyImages(plan *ir.IR, inspected map[string]images.Metadata, upstream string) {
	for index := range plan.Services {
		service := &plan.Services[index]
		metadata, found := inspected[ImageKey(service.Image)]
		if !found {
			continue
		}
		service.Image.Digest = metadata.Digest
		if !metadata.ConfigRead {
			continue
		}
		at := ir.Provenance{Source: ir.SourceOCILabel, SourceRef: metadata.Reference, Confidence: ir.ConfidenceHigh}
		imageHealthcheck(service, metadata, at)
		imagePorts(service, metadata, at)
		imageVolumes(plan, service, metadata, at)
		imageUser(service, metadata)
		sourceLabel(plan, service, metadata, upstream)
	}
	plan.Unfilled = Gaps(plan)
}

// imageUser makes a fixed non-root user the image declares explicit, so the
// deploy notes hand bind-mounted folders to the user that writes them.
// Upstream compose's user still wins, and root stays implicit.
func imageUser(service *ir.Service, metadata images.Metadata) {
	user := numericUser(metadata.User)
	uid, _, _ := strings.Cut(user, userSeparator)
	if service.User != "" || user == "" || uid == rootUID {
		return
	}
	service.User = user
}

func imageHealthcheck(service *ir.Service, metadata images.Metadata, at ir.Provenance) {
	test := metadata.Healthcheck
	if len(test) == 0 || test[0] == healthcheckNone || service.Healthcheck.Provenance.Source == ir.SourceCompose {
		return
	}
	service.Healthcheck = ir.Healthcheck{Test: escapeDollars(test), Interval: metadata.HealthInterval, Timeout: metadata.HealthTimeout,
		StartPeriod: metadata.HealthStartPeriod, Retries: metadata.HealthRetries, Provenance: at}
}

func imagePorts(service *ir.Service, metadata images.Metadata, at ir.Provenance) {
	if len(service.Ports) > 0 {
		return
	}
	for _, exposed := range metadata.ExposedPorts {
		number, protocol, _ := strings.Cut(exposed, "/")
		port, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		if protocol == "" {
			protocol = ir.ProtocolTCP
		}
		service.Ports = append(service.Ports, ir.Port{Container: port, Protocol: protocol, Publish: service.Role == ir.RoleApp, Provenance: at})
	}
}

// imageVolumes names the image's declared volumes, and moves a sidecar
// rule's guessed data path onto the path the image really declares.
func imageVolumes(plan *ir.IR, service *ir.Service, metadata images.Metadata, at ir.Provenance) {
	for index := range service.Volumes {
		volume := &service.Volumes[index]
		if volume.Provenance.Source != ir.SourceSidecarRule {
			continue
		}
		for _, declared := range metadata.Volumes {
			if overlaps(path.Clean(declared), path.Clean(volume.ContainerPath)) {
				volume.ContainerPath, volume.Provenance = declared, at
			}
		}
	}
	if len(service.Volumes) > 0 {
		return
	}
	for _, declared := range metadata.Volumes {
		addVolume(plan, service, ir.Volume{Name: path.Base(declared), ContainerPath: declared, Provenance: at})
	}
}

// sourceLabel compares the app image's org.opencontainers.image.source with
// the repository the plan was generated from; a mismatch is reported, not
// fatal. Sidecars come from other projects by design.
func sourceLabel(plan *ir.IR, service *ir.Service, metadata images.Metadata, upstream string) {
	if metadata.Source == "" || upstream == "" || service.Role != ir.RoleApp {
		return
	}
	if normaliseRepositoryURL(metadata.Source) == normaliseRepositoryURL(upstream) {
		plan.Evidence = append(plan.Evidence, ir.Evidence{Claim: "image " + service.Image.Reference() + " source label matches the repository"})
		return
	}
	service.Notes = append(service.Notes, fmt.Sprintf("the image's source label says %s, not %s; check it is the official image", metadata.Source, upstream))
}

func normaliseRepositoryURL(text string) string {
	lowered := strings.ToLower(strings.TrimSpace(text))
	lowered = strings.TrimPrefix(strings.TrimPrefix(lowered, "https://"), "http://")
	lowered = strings.TrimSuffix(strings.TrimSuffix(lowered, "/"), ".git")
	return strings.TrimPrefix(lowered, "www.")
}
