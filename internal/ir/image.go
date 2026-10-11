package ir

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

const dockerHubHost = "docker.io"

// ParseImage normalises a reference the way Docker does: "postgres:17"
// becomes docker.io/library/postgres:17. A missing tag stays empty so the
// policy can insist on an explicit one.
func ParseImage(text string) (Image, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(text))
	if err != nil {
		return Image{}, fmt.Errorf("image %q: %w", text, err)
	}
	image := Image{Registry: reference.Domain(named), Repository: reference.Path(named)}
	if image.Registry == "index.docker.io" || image.Registry == "registry-1.docker.io" {
		image.Registry = dockerHubHost
	}
	if tagged, ok := named.(reference.Tagged); ok {
		image.Tag = tagged.Tag()
	}
	if digested, ok := named.(reference.Digested); ok {
		image.Digest = digested.Digest().String()
	}
	return image, nil
}
