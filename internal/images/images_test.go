package images

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
)

const testImage = "example/app:1.0"

type memoryCache struct {
	entries map[string]string
	fetched time.Time
}

func (cache *memoryCache) CachedImage(_ context.Context, reference string) (string, time.Time, bool, error) {
	value, found := cache.entries[reference]
	return value, cache.fetched, found, nil
}

func (cache *memoryCache) CacheImage(_ context.Context, reference, metadata string) error {
	cache.entries[reference] = metadata
	cache.fetched = time.Now()
	return nil
}

// fakeRegistry serves one image, named repository:tag, with labels, ports,
// volumes and a healthcheck.
func fakeRegistry(t *testing.T, repositoryAndTag string) (*httptest.Server, *atomic.Int64, string) {
	t.Helper()
	var requests atomic.Int64
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := image.ConfigFile()
	file.Config.Labels = map[string]string{SourceLabel: "https://github.com/example/app"}
	file.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}}
	file.Config.Volumes = map[string]struct{}{"/data": {}}
	file.Config.Healthcheck = &v1.HealthConfig{Test: []string{"CMD-SHELL", "wget -qO- http://127.0.0.1:$PORT/health"}, Interval: 30 * time.Second, Retries: 3}
	image, err = mutate.ConfigFile(image, file)
	if err != nil {
		t.Fatal(err)
	}
	reference, _ := name.ParseReference(host + "/" + repositoryAndTag)
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	digest, _ := image.Digest()
	requests.Store(0)
	return server, &requests, digest.String()
}

func inspector(t *testing.T, server *httptest.Server, cache Cache, mirror string) *Inspector {
	t.Helper()
	target, _ := url.Parse(server.URL)
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {target.Hostname()}}, PrivateTargets: []string{target.Host}})
	if err != nil {
		t.Fatal(err)
	}
	return NewInspector(guard, cache, true, mirror)
}

func TestInspectReadsDigestAndConfig(t *testing.T) {
	server, requests, digest := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	cache := &memoryCache{entries: map[string]string{}}
	metadata, err := inspector(t, server, cache, "").Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Digest != digest || metadata.Source != "https://github.com/example/app" || !metadata.ConfigRead {
		t.Fatalf("metadata %+v", metadata)
	}
	if metadata.ExposedPorts[0] != "8080/tcp" || metadata.Volumes[0] != "/data" || metadata.HealthInterval != "30s" || metadata.HealthRetries != 3 {
		t.Fatalf("config facts %+v", metadata)
	}
	before := requests.Load()
	if _, err := inspector(t, server, cache, "").Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Fatal("a cached image was fetched again")
	}
}

func TestInspectReportsMissingImages(t *testing.T) {
	server, _, _ := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	_, err := inspector(t, server, nil, "").Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/missing", Tag: "1.0"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
}

func TestInspectStaysBehindTheGuard(t *testing.T) {
	server, requests, _ := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {"127.0.0.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewInspector(guard, nil, true, "").Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
	if err == nil || requests.Load() != 0 {
		t.Fatalf("an unlisted private registry was contacted: err=%v requests=%d", err, requests.Load())
	}
}

// TestInspectReadsDockerHubThroughTheMirror: with a mirror set, a Docker Hub
// image is read from the mirror alone (the guard allows no Docker Hub host)
// and recorded under its docker.io name with the mirror's digest.
func TestInspectReadsDockerHubThroughTheMirror(t *testing.T) {
	server, requests, digest := fakeRegistry(t, "library/postgres:17")
	mirror := strings.TrimPrefix(server.URL, "http://")
	metadata, err := inspector(t, server, nil, mirror).Inspect(context.Background(), ir.Image{Registry: ir.DockerHubHost, Repository: "library/postgres", Tag: "17"})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Reference != "docker.io/library/postgres:17" || metadata.Digest != digest || !metadata.ConfigRead || requests.Load() == 0 {
		t.Fatalf("metadata %+v after %d mirror requests", metadata, requests.Load())
	}
}

// TestPublicMirrorUsesHTTPS: a mirror that is not a private target is read
// over https; plain HTTP is only for one listed in allow_private_targets.
func TestPublicMirrorUsesHTTPS(t *testing.T) {
	hub := ir.Image{Registry: ir.DockerHubHost, Repository: "library/postgres", Tag: "17"}
	cases := map[string]string{"mirror.gcr.io": "https", "mirror.lan:5000": "http"}
	guard, err := netguard.New(netguard.Policy{PrivateTargets: []string{"mirror.lan:5000"}})
	if err != nil {
		t.Fatal(err)
	}
	for mirror, scheme := range cases {
		reference, err := NewInspector(guard, nil, true, mirror).remoteReference(hub, "docker.io/library/postgres:17")
		if err != nil {
			t.Fatal(err)
		}
		if got := reference.Context().Scheme(); got != scheme {
			t.Errorf("mirror %s: scheme %s, want %s", mirror, got, scheme)
		}
	}
}

// TestSingleWordMirrorIsTheHost: a LAN mirror named with one word, such as
// nas, is the host asked, not a Docker Hub namespace.
func TestSingleWordMirrorIsTheHost(t *testing.T) {
	hub := ir.Image{Registry: ir.DockerHubHost, Repository: "library/postgres", Tag: "17"}
	guard, err := netguard.New(netguard.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewInspector(guard, nil, true, "nas").remoteReference(hub, "docker.io/library/postgres:17")
	if err != nil {
		t.Fatal(err)
	}
	if got := reference.Context().RegistryStr(); got != "nas" {
		t.Fatalf("single-word mirror resolved to %s", got)
	}
	if got := reference.Context().RepositoryStr(); got != hub.Repository {
		t.Fatalf("repository %s, want %s", got, hub.Repository)
	}
}

// TestMirrorLackingAnImageIsNotMissing: a mirror holds only what it cached,
// so its 404 leaves the image unverified rather than not found.
func TestMirrorLackingAnImageIsNotMissing(t *testing.T) {
	server, _, _ := fakeRegistry(t, "library/postgres:17")
	mirror := strings.TrimPrefix(server.URL, "http://")
	_, err := inspector(t, server, nil, mirror).Inspect(context.Background(), ir.Image{Registry: ir.DockerHubHost, Repository: "library/redis", Tag: "7"})
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "the mirror") {
		t.Fatalf("an image the mirror lacks: %v", err)
	}
}

// TestRateLimitNamesTheMirrorKey: only Docker Hub asked directly gets the
// hint; a mirror's or another registry's limit is reported as it came.
func TestRateLimitNamesTheMirrorKey(t *testing.T) {
	limited := &transport.Error{StatusCode: http.StatusTooManyRequests}
	hub := ir.Image{Registry: ir.DockerHubHost, Repository: "library/postgres", Tag: "17"}
	err := NewInspector(nil, nil, true, "").classify(hub, "docker.io/library/postgres:17", limited)
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "profile.registry_mirror") {
		t.Fatalf("Docker Hub's limit: %v", err)
	}
	if err := NewInspector(nil, nil, true, "mirror.gcr.io").classify(hub, "docker.io/library/postgres:17", limited); errors.Is(err, ErrRateLimited) {
		t.Fatalf("a mirror's limit points at the mirror setting: %v", err)
	}
	other := ir.Image{Registry: "ghcr.io", Repository: "example/app", Tag: "1.0"}
	if err := NewInspector(nil, nil, true, "").classify(other, "ghcr.io/example/app:1.0", limited); errors.Is(err, ErrRateLimited) {
		t.Fatalf("another registry's limit blames Docker Hub: %v", err)
	}
}
