// Package images verifies image references against their registry: the
// digest a tag resolves to (a HEAD request, which Docker Hub does not count
// as a pull) and, unless disabled, the image configuration with its OCI
// labels, exposed ports, volumes and healthcheck (a GET, which it does).
package images

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	SourceLabel   = "org.opencontainers.image.source"
	VersionLabel  = "org.opencontainers.image.version"
	RevisionLabel = "org.opencontainers.image.revision"
	cacheTTL      = 24 * time.Hour
	// cacheKeySeparator joins a reference and the platform its facts were
	// read for into the cache key.
	cacheKeySeparator = " "
	// linuxOS is the only system a profile targets, so an index's Windows
	// images and attestation manifests are not listed.
	linuxOS = "linux"
	// tagPageSize and maxTagsRead bound a tag listing: popular images
	// have thousands of tags, and only a version-shaped few matter.
	tagPageSize = 1000
	maxTagsRead = 5000
)

var (
	ErrNotFound = errors.New("image not found in the registry")
	// ErrRateLimited is Docker Hub refusing anonymous requests for a while.
	ErrRateLimited = errors.New("hit Docker Hub's anonymous pull limit; set profile.registry_mirror or wait")
)

type Metadata struct {
	Reference         string            `json:"reference"`
	Digest            string            `json:"digest"`
	Source            string            `json:"source"`
	ExposedPorts      []string          `json:"exposed_ports"`
	Volumes           []string          `json:"volumes"`
	Env               []string          `json:"env"`
	User              string            `json:"user"`
	Healthcheck       []string          `json:"healthcheck"`
	HealthInterval    string            `json:"health_interval"`
	HealthTimeout     string            `json:"health_timeout"`
	HealthStartPeriod string            `json:"health_start_period"`
	HealthRetries     int               `json:"health_retries"`
	Labels            map[string]string `json:"labels"`
	Created           time.Time         `json:"created"`
	// Platforms are the platforms the tag's image index lists, or the
	// image's own when the tag names a single image.
	Platforms []string `json:"platforms"`
	// ConfigRead's key changed when the build date was kept, so an entry
	// cached before then is read again.
	ConfigRead bool      `json:"config_facts_read"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// Cache keeps metadata between runs; the store implements it.
type Cache interface {
	CachedImage(ctx context.Context, reference string) (string, time.Time, bool, error)
	CacheImage(ctx context.Context, reference, metadata string) error
}

type Inspector struct {
	guard      *netguard.Guard
	cache      Cache
	readConfig bool
	mirror     string
	platform   v1.Platform
	// platformChosen says the profile names the platform, so an image
	// lacking it is the policy's to report, not an unreadable config.
	platformChosen bool
}

// Options are how an inspector reads images.
type Options struct {
	// ReadConfig controls the image-config GET used for labels and R3 facts.
	ReadConfig bool
	// Mirror, when set, is the host Docker Hub images are read from instead.
	Mirror string
	// Platform is the profile's platform, such as linux/arm64, whose variant
	// facts are read; empty is this machine's.
	Platform string
}

// NewInspector inspects through the guard's registry purpose.
func NewInspector(guard *netguard.Guard, cache Cache, options Options) *Inspector {
	inspector := &Inspector{guard: guard, cache: cache, readConfig: options.ReadConfig, mirror: options.Mirror}
	// Validation accepts only platforms ParsePlatform reads.
	platform, _ := v1.ParsePlatform(cmp.Or(options.Platform, config.HostPlatform()))
	inspector.platform, inspector.platformChosen = *platform, options.Platform != ""
	return inspector
}

// Inspect resolves an image's digest and, if enabled, reads its config.
// Results are cached for a day.
func (inspector *Inspector) Inspect(ctx context.Context, image ir.Image) (Metadata, error) {
	if cached, found := inspector.cached(ctx, image.TaggedReference()); found {
		return cached, nil
	}
	return inspector.Fresh(ctx, image)
}

// Fresh is Inspect past the cache, for a tag that may have moved since it
// was cached; what it reads is cached again.
func (inspector *Inspector) Fresh(ctx context.Context, image ir.Image) (Metadata, error) {
	reference := image.TaggedReference()
	parsed, options, err := inspector.registryRequest(ctx, image, reference)
	if err != nil {
		return Metadata{}, err
	}
	descriptor, err := remote.Head(parsed, options...)
	if err != nil {
		return Metadata{}, inspector.classify(image, reference, err)
	}
	metadata := Metadata{Reference: reference, Digest: descriptor.Digest.String(), FetchedAt: time.Now().UTC()}
	if inspector.readConfig || inspector.platformChosen {
		if err := inspector.readManifest(parsed, options, &metadata); err != nil {
			return metadata, inspector.classify(image, reference, err)
		}
	}
	inspector.store(ctx, metadata)
	return metadata, nil
}

// Digest is what the image's tag resolves to now: one HEAD, past the cache
// and without reading the config, so Docker Hub counts no pull.
func (inspector *Inspector) Digest(ctx context.Context, image ir.Image) (string, error) {
	reference := image.TaggedReference()
	parsed, options, err := inspector.registryRequest(ctx, image, reference)
	if err != nil {
		return "", err
	}
	descriptor, err := remote.Head(parsed, options...)
	if err != nil {
		return "", inspector.classify(image, reference, err)
	}
	return descriptor.Digest.String(), nil
}

// Tags lists the image repository's tags, at most maxTagsRead of them.
// ponytail: a repository with more tags than that may hide newer ones past
// the cap; ask for them by name if that matters.
func (inspector *Inspector) Tags(ctx context.Context, image ir.Image) ([]string, error) {
	reference := image.TaggedReference()
	parsed, options, err := inspector.registryRequest(ctx, image, reference)
	if err != nil {
		return nil, err
	}
	puller, err := remote.NewPuller(append(options, remote.WithPageSize(tagPageSize))...)
	if err != nil {
		return nil, err
	}
	lister, err := puller.Lister(ctx, parsed.Context())
	if err != nil {
		return nil, inspector.classify(image, reference, err)
	}
	var tags []string
	for lister.HasNext() && len(tags) < maxTagsRead {
		page, err := lister.Next(ctx)
		if err != nil {
			return nil, inspector.classify(image, reference, err)
		}
		tags = append(tags, page.Tags...)
	}
	return tags[:min(len(tags), maxTagsRead)], nil
}

// registryRequest is where an image's registry requests go and how they
// are made.
func (inspector *Inspector) registryRequest(ctx context.Context, image ir.Image, reference string) (name.Reference, []remote.Option, error) {
	parsed, err := inspector.remoteReference(image, reference)
	if err != nil {
		return nil, nil, err
	}
	options := []remote.Option{
		remote.WithContext(ctx), remote.WithTransport(inspector.guard.Transport(netguard.Registry)),
		remote.WithAuth(authn.Anonymous), remote.WithPlatform(inspector.platform),
	}
	return parsed, options, nil
}

// remoteReference is where the registry requests go: a Docker Hub image is
// read through the mirror when one is set. Digests address content, so the
// mirror's digest is Docker Hub's and the plan still names docker.io.
func (inspector *Inspector) remoteReference(image ir.Image, reference string) (name.Reference, error) {
	if !inspector.throughMirror(image) {
		return name.ParseReference(reference)
	}
	// name.Insecure makes every request plain HTTP, so it is only for a
	// mirror listed in network.allow_private_targets; others use https.
	var options []name.Option
	if inspector.guard.PlainHTTPAllowed(inspector.mirror) {
		options = append(options, name.Insecure)
	}
	// Built from a Registry, not parsed from text: parsing reads a single-word
	// host such as "nas" as a Docker Hub namespace and never reaches the mirror.
	registry, err := name.NewRegistry(inspector.mirror, options...)
	if err != nil {
		return nil, err
	}
	return registry.Repo(image.Repository).Tag(image.Tag), nil
}

func (inspector *Inspector) throughMirror(image ir.Image) bool {
	return image.Registry == ir.DockerHubHost && inspector.mirror != ""
}

// readManifest reads what the tag points at: the platforms an image index
// lists and, when enabled, the config of the variant for the platform. An
// index lacking a chosen platform has no such config to read.
func (inspector *Inspector) readManifest(reference name.Reference, options []remote.Option, metadata *Metadata) error {
	descriptor, err := remote.Get(reference, options...)
	if err != nil {
		return err
	}
	if descriptor.MediaType.IsIndex() {
		listed, err := indexPlatforms(descriptor)
		if err != nil {
			return err
		}
		metadata.Platforms = platformNames(listed)
		lacksChosen := inspector.platformChosen && !slices.ContainsFunc(listed, func(platform v1.Platform) bool { return platform.Satisfies(inspector.platform) })
		if !inspector.readConfig || lacksChosen {
			return nil
		}
	}
	image, err := descriptor.Image()
	if err != nil {
		return err
	}
	file, err := image.ConfigFile()
	if err != nil {
		return err
	}
	if own := file.Platform(); len(metadata.Platforms) == 0 && own != nil {
		metadata.Platforms = []string{own.String()}
	}
	if inspector.readConfig {
		keepConfig(file, metadata)
	}
	return nil
}

// indexPlatforms are the Linux platforms an image index has images for.
func indexPlatforms(descriptor *remote.Descriptor) ([]v1.Platform, error) {
	index, err := descriptor.ImageIndex()
	if err != nil {
		return nil, err
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}
	var platforms []v1.Platform
	for _, child := range manifest.Manifests {
		if child.Platform != nil && child.Platform.OS == linuxOS {
			platforms = append(platforms, *child.Platform)
		}
	}
	return platforms, nil
}

func platformNames(platforms []v1.Platform) []string {
	names := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		names = append(names, platform.String())
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func keepConfig(file *v1.ConfigFile, metadata *Metadata) {
	configuration := file.Config
	metadata.ConfigRead = true
	metadata.Labels = configuration.Labels
	metadata.Created = file.Created.Time
	metadata.Source = configuration.Labels[SourceLabel]
	metadata.Env, metadata.User = configuration.Env, configuration.User
	metadata.ExposedPorts = sortedKeys(configuration.ExposedPorts)
	metadata.Volumes = sortedKeys(configuration.Volumes)
	if health := configuration.Healthcheck; health != nil {
		metadata.Healthcheck = health.Test
		metadata.HealthInterval = durationText(health.Interval)
		metadata.HealthTimeout = durationText(health.Timeout)
		metadata.HealthStartPeriod = durationText(health.StartPeriod)
		metadata.HealthRetries = health.Retries
	}
}

func (inspector *Inspector) cached(ctx context.Context, reference string) (Metadata, bool) {
	if inspector.cache == nil {
		return Metadata{}, false
	}
	encoded, fetched, found, err := inspector.cache.CachedImage(ctx, inspector.cacheKey(reference))
	if err != nil || !found || time.Since(fetched) > cacheTTL {
		return Metadata{}, false
	}
	var metadata Metadata
	if json.Unmarshal([]byte(encoded), &metadata) != nil {
		return Metadata{}, false
	}
	if inspector.readConfig && !metadata.ConfigRead {
		return Metadata{}, false
	}
	return metadata, true
}

// cacheKey keeps facts read for one platform's variant from serving
// another's.
func (inspector *Inspector) cacheKey(reference string) string {
	return reference + cacheKeySeparator + inspector.platform.String()
}

func (inspector *Inspector) store(ctx context.Context, metadata Metadata) {
	if inspector.cache == nil {
		return
	}
	if encoded, err := json.Marshal(metadata); err == nil {
		_ = inspector.cache.CacheImage(ctx, inspector.cacheKey(metadata.Reference), string(encoded))
	}
}

// classify turns a registry error into ErrNotFound when the image's own
// registry said the image or tag does not exist, and into ErrRateLimited when
// Docker Hub, asked directly, says to slow down; anything else (network,
// another registry's limit, a mirror lacking the image) is reported as an
// unverified image.
func (inspector *Inspector) classify(image ir.Image, reference string, err error) error {
	var registryError *transport.Error
	if !errors.As(err, &registryError) {
		return fmt.Errorf("verify %s: %w", reference, err)
	}
	notFound := registryError.StatusCode == http.StatusNotFound
	if notFound && inspector.throughMirror(image) {
		return fmt.Errorf("verify %s: the mirror %s does not have it: %w", reference, inspector.mirror, err)
	}
	if notFound {
		return fmt.Errorf("%s: %w", reference, ErrNotFound)
	}
	hubAskedDirectly := image.Registry == ir.DockerHubHost && inspector.mirror == ""
	if hubAskedDirectly && registryError.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("verify %s: %w", reference, ErrRateLimited)
	}
	return fmt.Errorf("verify %s: %w", reference, err)
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func durationText(value time.Duration) string {
	if value == 0 {
		return ""
	}
	return value.String()
}
