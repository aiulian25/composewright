package images

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
)

const testImage = "example/app:1.0"

var testBuilt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

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
	file.Created = v1.Time{Time: testBuilt}
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
	return NewInspector(guard, cache, Options{ReadConfig: true, Mirror: mirror})
}

func TestInspectReadsDigestAndConfig(t *testing.T) {
	server, requests, digest := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	cache := &memoryCache{entries: map[string]string{}}
	metadata, err := inspector(t, server, cache, "").Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Digest != digest || metadata.Source != "https://github.com/example/app" || !metadata.ConfigRead || !metadata.Created.Equal(testBuilt) {
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

// TestInspectReadsAgainWithoutBuildDate: an entry cached before the build
// date was kept is fetched again rather than served without one.
func TestInspectReadsAgainWithoutBuildDate(t *testing.T) {
	server, requests, digest := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	image := ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"}
	cache := &memoryCache{entries: map[string]string{}, fetched: time.Now()}
	key := inspector(t, server, cache, "").cacheKey(image.TaggedReference())
	if err := cache.CacheImage(context.Background(), key, `{"digest":"`+digest+`","config_read":true}`); err != nil {
		t.Fatal(err)
	}
	metadata, err := inspector(t, server, cache, "").Inspect(context.Background(), image)
	if err != nil || !metadata.Created.Equal(testBuilt) {
		t.Fatalf("metadata %+v: %v", metadata, err)
	}
	// An image built without a date is still served from the cache.
	if err := cache.CacheImage(context.Background(), key, `{"digest":"`+digest+`","config_facts_read":true}`); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	if _, err := inspector(t, server, cache, "").Inspect(context.Background(), image); err != nil || requests.Load() != before {
		t.Fatalf("an image without a build date was fetched again: %v", err)
	}
}

// platformIndex serves example/app:1.0 as an image index with one image per
// platform, each labelled with its platform, and returns the index digest.
func platformIndex(t *testing.T, server *httptest.Server, platforms ...string) string {
	t.Helper()
	var index v1.ImageIndex = empty.Index
	for _, text := range platforms {
		platform, err := v1.ParsePlatform(text)
		if err != nil {
			t.Fatal(err)
		}
		image, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		file, _ := image.ConfigFile()
		file.OS, file.Architecture, file.Variant = platform.OS, platform.Architecture, platform.Variant
		file.Config.Labels = map[string]string{VersionLabel: text}
		if image, err = mutate.ConfigFile(image, file); err != nil {
			t.Fatal(err)
		}
		index = mutate.AppendManifests(index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: platform}})
	}
	reference, _ := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/" + testImage)
	if err := remote.WriteIndex(reference, index); err != nil {
		t.Fatal(err)
	}
	digest, _ := index.Digest()
	return digest.String()
}

func platformInspector(t *testing.T, server *httptest.Server, cache Cache, platform string) *Inspector {
	t.Helper()
	return NewInspector(inspector(t, server, nil, "").guard, cache, Options{ReadConfig: true, Platform: platform})
}

// TestInspectReadsThePlatformVariant: with a platform in the profile, the
// digest pinned is still the index's, so one compose.yaml runs on every
// platform it lists, while the facts come from that platform's image.
func TestInspectReadsThePlatformVariant(t *testing.T) {
	server, _, _ := fakeRegistry(t, "example/other:1.0")
	digest := platformIndex(t, server, "linux/amd64", "linux/arm64/v8")
	image := ir.Image{Registry: strings.TrimPrefix(server.URL, "http://"), Repository: "example/app", Tag: "1.0"}
	metadata, err := platformInspector(t, server, nil, "linux/arm64").Inspect(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Digest != digest || !slices.Equal(metadata.Platforms, []string{"linux/amd64", "linux/arm64/v8"}) || metadata.Labels[VersionLabel] != "linux/arm64/v8" {
		t.Fatalf("metadata %+v, want the index digest %s, both platforms and the arm64 image's labels", metadata, digest)
	}
}

// TestInspectListsAMissingPlatformWithoutFailing: an index without the chosen
// platform is no error, and the platforms it lists are kept for the policy.
// Facts cached for one platform are never served for another.
func TestInspectListsAMissingPlatformWithoutFailing(t *testing.T) {
	server, requests, _ := fakeRegistry(t, "example/other:1.0")
	platformIndex(t, server, "linux/amd64")
	image := ir.Image{Registry: strings.TrimPrefix(server.URL, "http://"), Repository: "example/app", Tag: "1.0"}
	cache := &memoryCache{entries: map[string]string{}}
	if _, err := platformInspector(t, server, cache, "linux/amd64").Inspect(context.Background(), image); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	metadata, err := platformInspector(t, server, cache, "linux/arm64").Inspect(context.Background(), image)
	if err != nil || !slices.Equal(metadata.Platforms, []string{"linux/amd64"}) || metadata.ConfigRead {
		t.Fatalf("metadata %+v: %v", metadata, err)
	}
	if requests.Load() == before {
		t.Fatal("facts cached for linux/amd64 served an inspection for linux/arm64")
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
	_, err = NewInspector(guard, nil, Options{ReadConfig: true}).Inspect(context.Background(), ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"})
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
		reference, err := NewInspector(guard, nil, Options{ReadConfig: true, Mirror: mirror}).remoteReference(hub, "docker.io/library/postgres:17")
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
	reference, err := NewInspector(guard, nil, Options{ReadConfig: true, Mirror: "nas"}).remoteReference(hub, "docker.io/library/postgres:17")
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
	err := NewInspector(nil, nil, Options{ReadConfig: true}).classify(hub, "docker.io/library/postgres:17", limited)
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "profile.registry_mirror") {
		t.Fatalf("Docker Hub's limit: %v", err)
	}
	if err := NewInspector(nil, nil, Options{ReadConfig: true, Mirror: "mirror.gcr.io"}).classify(hub, "docker.io/library/postgres:17", limited); errors.Is(err, ErrRateLimited) {
		t.Fatalf("a mirror's limit points at the mirror setting: %v", err)
	}
	other := ir.Image{Registry: "ghcr.io", Repository: "example/app", Tag: "1.0"}
	if err := NewInspector(nil, nil, Options{ReadConfig: true}).classify(other, "ghcr.io/example/app:1.0", limited); errors.Is(err, ErrRateLimited) {
		t.Fatalf("another registry's limit blames Docker Hub: %v", err)
	}
}

// TestNewerTags: newer tags keep the pinned tag's prefix, number count,
// suffix and major version, and development builds are left out.
func TestNewerTags(t *testing.T) {
	tags := []string{"2.2.1", "2.2.3", "2.2.5", "2.2.4", "2.3.0", "3.0.0", "2.2.5-rc1", "2.2.6-nightly", "2.2.7.dev", "2.2", "2.2.4.1",
		"v2.2.9", "latest", "nightly", "2.2.6-alpine", "2.2.4-rc1", "2.2.3-ls45", "2.2.3-ls46", "2.2.4-ls46", "2.2.4-ls7",
		"2.2.3-php7", "2.2.3-php8", "2.2.4-php7", "2.2.5-php8", "1.25.3-java11", "1.25.3-java17", "1.25.4-java11",
		"2.2.4-alpine", "2.2.4-alpine3", "10.1.30-jdk17", "10.1.30-jdk21", "10.1.31-jdk11", "10.1.31-jdk17",
		"7.2.4-alpine3.19", "7.2.5-alpine3.18", "7.2.5-alpine3.19", "16.1.0-debian-12-r5", "16.1.0-debian-12-r6", "16.1.1-debian-12"}
	cases := []struct {
		pinned string
		want   []string
	}{
		{"2.2.3", []string{"2.2.4", "2.2.5", "2.3.0"}},
		{"2.2", []string{}},
		{"2.1", []string{"2.2"}},
		{"v2.2.3", []string{"v2.2.9"}},
		{"2.2.5-alpine", []string{"2.2.6-alpine"}},
		{"2.2.3-alpine", []string{"2.2.4-alpine", "2.2.6-alpine"}},
		{"2.2.3-php7", []string{"2.2.4-php7"}},
		{"1.25.3-java11", []string{"1.25.4-java11"}},
		{"10.1.30-jdk17", []string{"10.1.31-jdk17"}},
		{"7.2.4-alpine3.19", []string{"7.2.5-alpine3.19"}},
		{"16.1.0-debian-12-r5", []string{"16.1.0-debian-12-r6"}},
		{"2.2.3-ls45", []string{"2.2.3-ls46", "2.2.4-ls7", "2.2.4-ls46"}},
		{"latest", nil},
		{"3.0.0", []string{}},
		// Only another release candidate is written like one; it is no
		// stable tag to move to.
		{"2.2.3-rc1", []string{}},
	}
	for _, testCase := range cases {
		got := Newer(testCase.pinned, tags)
		if !slices.Equal(got, testCase.want) || (testCase.want == nil) != (got == nil) {
			t.Errorf("newer than %s: %q, want %q", testCase.pinned, got, testCase.want)
		}
	}
}

// TestDigestAndTagsAskTheRegistry: a tag moved since it was cached is seen
// at once by Digest and Fresh, and the repository's tags are listed.
func TestDigestAndTagsAskTheRegistry(t *testing.T) {
	server, _, first := fakeRegistry(t, testImage)
	host := strings.TrimPrefix(server.URL, "http://")
	image := ir.Image{Registry: host, Repository: "example/app", Tag: "1.0"}
	cache := &memoryCache{entries: map[string]string{}}
	checker := inspector(t, server, cache, "")
	if _, err := checker.Inspect(context.Background(), image); err != nil {
		t.Fatal(err)
	}
	moved, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"1.0", "1.1"} {
		reference, _ := name.ParseReference(host + "/example/app:" + tag)
		if err := remote.Write(reference, moved); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := checker.Digest(context.Background(), image)
	if wanted, _ := moved.Digest(); err != nil || digest != wanted.String() || digest == first {
		t.Fatalf("digest after the tag moved: %s %v", digest, err)
	}
	tags, err := checker.Tags(context.Background(), image)
	if err != nil || !slices.Equal(tags, []string{"1.0", "1.1"}) {
		t.Fatalf("tags %v: %v", tags, err)
	}
	if cached, _ := checker.Inspect(context.Background(), image); cached.Digest != first {
		t.Fatalf("Inspect did not serve the cache: %s", cached.Digest)
	}
	if _, err := checker.Fresh(context.Background(), image); err != nil {
		t.Fatal(err)
	}
	if cached, _ := checker.Inspect(context.Background(), image); cached.Digest != digest {
		t.Fatalf("Fresh did not cache what it read: %s", cached.Digest)
	}
}
