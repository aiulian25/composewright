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

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
)

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

// fakeRegistry serves one image with labels, ports, volumes and a healthcheck.
func fakeRegistry(t *testing.T) (*httptest.Server, *atomic.Int64, string) {
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
	reference, _ := name.ParseReference(host + "/example/app:1.0")
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	digest, _ := image.Digest()
	requests.Store(0)
	return server, &requests, digest.String()
}

func inspector(t *testing.T, server *httptest.Server, cache Cache) *Inspector {
	t.Helper()
	target, _ := url.Parse(server.URL)
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {target.Hostname()}}, PrivateTargets: []string{target.Host}})
	if err != nil {
		t.Fatal(err)
	}
	return NewInspector(guard, cache, true)
}

func TestInspectReadsDigestAndConfig(t *testing.T) {
	server, requests, digest := fakeRegistry(t)
	host := strings.TrimPrefix(server.URL, "http://")
	cache := &memoryCache{entries: map[string]string{}}
	metadata, err := inspector(t, server, cache).Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
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
	if _, err := inspector(t, server, cache).Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Fatal("a cached image was fetched again")
	}
}

func TestInspectReportsMissingImages(t *testing.T) {
	server, _, _ := fakeRegistry(t)
	host := strings.TrimPrefix(server.URL, "http://")
	_, err := inspector(t, server, nil).Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/missing", Tag: "1.0"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
}

func TestInspectStaysBehindTheGuard(t *testing.T) {
	server, requests, _ := fakeRegistry(t)
	host := strings.TrimPrefix(server.URL, "http://")
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {"127.0.0.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewInspector(guard, nil, true).Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
	if err == nil || requests.Load() != 0 {
		t.Fatalf("an unlisted private registry was contacted: err=%v requests=%d", err, requests.Load())
	}
}
