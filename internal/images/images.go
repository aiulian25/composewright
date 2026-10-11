// Package images verifies image references against their registry: the
// digest a tag resolves to (a HEAD request, which Docker Hub does not count
// as a pull) and, unless disabled, the image configuration with its OCI
// labels, exposed ports, volumes and healthcheck (a GET, which it does).
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	SourceLabel = "org.opencontainers.image.source"
	cacheTTL    = 24 * time.Hour
	linuxOS     = "linux"
)

var ErrNotFound = errors.New("image not found in the registry")

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
	ConfigRead        bool              `json:"config_read"`
	FetchedAt         time.Time         `json:"fetched_at"`
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
}

// NewInspector inspects through the guard's registry purpose. readConfig
// controls the image-config GET used for labels and R3 facts.
func NewInspector(guard *netguard.Guard, cache Cache, readConfig bool) *Inspector {
	return &Inspector{guard: guard, cache: cache, readConfig: readConfig}
}

// Inspect resolves an image's digest and, if enabled, reads its config.
// Results are cached for a day.
func (inspector *Inspector) Inspect(ctx context.Context, image ir.Image) (Metadata, error) {
	reference := image.Reference() + ":" + image.Tag
	if cached, found := inspector.cached(ctx, reference); found {
		return cached, nil
	}
	parsed, err := name.ParseReference(reference)
	if err != nil {
		return Metadata{}, err
	}
	options := []remote.Option{
		remote.WithContext(ctx), remote.WithTransport(inspector.guard.Transport(netguard.Registry)),
		remote.WithAuth(authn.Anonymous), remote.WithPlatform(v1.Platform{OS: linuxOS, Architecture: runtime.GOARCH}),
	}
	descriptor, err := remote.Head(parsed, options...)
	if err != nil {
		return Metadata{}, classify(reference, err)
	}
	metadata := Metadata{Reference: reference, Digest: descriptor.Digest.String(), FetchedAt: time.Now().UTC()}
	if inspector.readConfig {
		if err := readConfig(parsed, options, &metadata); err != nil {
			return metadata, classify(reference, err)
		}
	}
	inspector.store(ctx, metadata)
	return metadata, nil
}

func readConfig(reference name.Reference, options []remote.Option, metadata *Metadata) error {
	image, err := remote.Image(reference, options...)
	if err != nil {
		return err
	}
	file, err := image.ConfigFile()
	if err != nil {
		return err
	}
	configuration := file.Config
	metadata.ConfigRead = true
	metadata.Labels = configuration.Labels
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
	return nil
}

func (inspector *Inspector) cached(ctx context.Context, reference string) (Metadata, bool) {
	if inspector.cache == nil {
		return Metadata{}, false
	}
	encoded, fetched, found, err := inspector.cache.CachedImage(ctx, reference)
	if err != nil || !found || time.Since(fetched) > cacheTTL {
		return Metadata{}, false
	}
	var metadata Metadata
	if json.Unmarshal([]byte(encoded), &metadata) != nil || (inspector.readConfig && !metadata.ConfigRead) {
		return Metadata{}, false
	}
	return metadata, true
}

func (inspector *Inspector) store(ctx context.Context, metadata Metadata) {
	if inspector.cache == nil {
		return
	}
	if encoded, err := json.Marshal(metadata); err == nil {
		_ = inspector.cache.CacheImage(ctx, metadata.Reference, string(encoded))
	}
}

// classify turns a registry error into ErrNotFound when the registry said
// the image or tag does not exist; anything else (network, rate limit) is
// reported as an unverified image.
func classify(reference string, err error) error {
	var registryError *transport.Error
	if errors.As(err, &registryError) && registryError.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", reference, ErrNotFound)
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
