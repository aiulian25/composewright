package pipeline

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
)

const minifluxRepository = "miniflux/miniflux"

var minifluxBuilt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// mirrorRegistry is a registry that Docker Hub images are read through,
// and an inspector that reads them there.
type mirrorRegistry struct {
	t    *testing.T
	host string
	// asked are the request paths the registry got.
	asked      []string
	askedMutex sync.Mutex
}

// wasAsked reports a request about repository after the first since.
func (mirror *mirrorRegistry) wasAsked(since int, repository string) bool {
	mirror.askedMutex.Lock()
	defer mirror.askedMutex.Unlock()
	return slices.ContainsFunc(mirror.asked[since:], func(path string) bool { return strings.Contains(path, repository) })
}

func (mirror *mirrorRegistry) requests() int {
	mirror.askedMutex.Lock()
	defer mirror.askedMutex.Unlock()
	return len(mirror.asked)
}

func newMirrorRegistry(t *testing.T, cache images.Cache) (*mirrorRegistry, *images.Inspector) {
	t.Helper()
	mirror := &mirrorRegistry{t: t}
	served := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mirror.askedMutex.Lock()
		mirror.asked = append(mirror.asked, request.URL.Path)
		mirror.askedMutex.Unlock()
		served.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	hostname, _, _ := strings.Cut(host, ":")
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {hostname}}, PrivateTargets: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	mirror.host = host
	return mirror, images.NewInspector(guard, cache, images.Options{ReadConfig: true, Mirror: host})
}

// push stores a new image under repository:tag, labelled with its version,
// and returns its digest.
func (mirror *mirrorRegistry) push(repository, tag string) string {
	mirror.t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		mirror.t.Fatal(err)
	}
	file, err := image.ConfigFile()
	if err != nil {
		mirror.t.Fatal(err)
	}
	file.Config.Labels = map[string]string{images.VersionLabel: tag}
	file.Created = v1.Time{Time: minifluxBuilt}
	if image, err = mutate.ConfigFile(image, file); err != nil {
		mirror.t.Fatal(err)
	}
	reference, err := name.ParseReference(mirror.host + "/" + repository + ":" + tag)
	if err != nil {
		mirror.t.Fatal(err)
	}
	if err := remote.Write(reference, image); err != nil {
		mirror.t.Fatal(err)
	}
	digest, _ := image.Digest()
	return digest.String()
}

func statusOf(t *testing.T, statuses []ImageStatus, service string) ImageStatus {
	t.Helper()
	index := slices.IndexFunc(statuses, func(status ImageStatus) bool { return status.Service == service })
	if index < 0 {
		t.Fatalf("no status for %s in %+v", service, statuses)
	}
	return statuses[index]
}

// TestCheckImages: a version made with a tag stores the image's labels and
// build time; checking finds the newer stable tags of its family, then
// sees the tag move, which using the tag again pins past the cache. Over a
// hand-edited compose.yaml the check reads the edit's image. Every check is
// audited.
func TestCheckImages(t *testing.T) {
	ctx := context.Background()
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	generated(t, dependencies)
	mirror, inspector := newMirrorRegistry(t, dependencies.Workspace.Store)
	pinned := mirror.push(minifluxRepository, "2.2.3")
	for _, tag := range []string{"2.2.4", "2.2.5", "2.2.6-rc1", "nightly", "dev", "3.0.0"} {
		mirror.push(minifluxRepository, tag)
	}
	if _, err := CheckImages(ctx, dependencies, "miniflux"); !errors.Is(err, ErrImagesOffline) {
		t.Fatalf("an offline check: %v", err)
	}
	dependencies.Config.Profile.Networks = append(dependencies.Config.Profile.Networks, config.ProfileNetwork{Name: "proxy"})
	kept := Revision{ExposeVia: "lan", Networks: []string{"proxy"}, AllowNeeds: []string{ir.NeedLowPort}}
	if _, err := Revise(ctx, dependencies, "miniflux", kept); err != nil {
		t.Fatal(err)
	}
	before, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Inspector = inspector
	for tag, want := range map[string]error{"bad tag": ErrInvalidTag, "x@" + pinned: ErrInvalidTag, "": ErrInvalidTag} {
		if _, err := UseTag(ctx, dependencies, "miniflux", "miniflux", tag); !errors.Is(err, want) {
			t.Fatalf("tag %q: %v", tag, err)
		}
	}
	if _, err := UseTag(ctx, dependencies, "miniflux", "nosuch", "2.2.3"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("an unknown service: %v", err)
	}
	result, err := UseTag(ctx, dependencies, "miniflux", "miniflux", "2.2.3")
	if err != nil {
		t.Fatal(err)
	}
	after, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(after.Options.ImageTags, map[string]string{"miniflux": "2.2.3"}) {
		t.Fatalf("the chosen tag was not kept with the options: %v", after.Options.ImageTags)
	}
	after.Options.ImageTags = nil
	if !reflect.DeepEqual(after.Options, before.Options) {
		t.Fatalf("the settings changed with the tag:\n%+v\n%+v", before.Options, after.Options)
	}
	service, _ := result.Plan.Service("miniflux")
	if service.Image.Tag != "2.2.3" || service.Image.Digest != pinned {
		t.Fatalf("the new tag was not pinned: %+v", service.Image)
	}
	recorded, err := dependencies.Workspace.Store.Images(ctx, result.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := recorded[slices.IndexFunc(recorded, func(image store.Image) bool { return image.Service == "miniflux" })]
	if app.Labels[images.VersionLabel] != "2.2.3" || !app.Created.Equal(minifluxBuilt) {
		t.Fatalf("image facts were not stored: %+v", app)
	}
	statuses, err := CheckImages(ctx, dependencies, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	checked := statusOf(t, statuses, "miniflux")
	if checked.State != ImageUnchanged || !slices.Equal(checked.Newer, []string{"2.2.4", "2.2.5"}) || checked.Version != "2.2.3" ||
		!checked.Created.Equal(minifluxBuilt) {
		t.Fatalf("check before the tag moved: %+v", checked)
	}
	moved := mirror.push(minifluxRepository, "2.2.3")
	statuses, err = CheckImages(ctx, dependencies, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	if checked = statusOf(t, statuses, "miniflux"); checked.State != ImageMoved || checked.Current != moved || checked.Pinned != pinned {
		t.Fatalf("check after the tag moved: %+v", checked)
	}
	if result, err = UseTag(ctx, dependencies, "miniflux", "miniflux", "2.2.3"); err != nil {
		t.Fatal(err)
	}
	if service, _ = result.Plan.Service("miniflux"); service.Image.Digest != moved {
		t.Fatalf("the moved tag was pinned from the cache: %+v", service.Image)
	}
	project, _ := dependencies.Workspace.Store.FindProject(ctx, "miniflux")
	checkOutsideTheAllowlist(t, dependencies, mirror)
	checkEditedCompose(t, dependencies, service.Image)
	audited, err := dependencies.Workspace.Store.AuditPage(ctx, store.AuditFilter{ProjectID: project.ID, Action: store.ActionCheckImages})
	if err != nil || len(audited) != 3 {
		t.Fatalf("%d checks audited: %v", len(audited), err)
	}
}

// checkEditedCompose: a version edited by hand keeps the image facts it
// was not inspected for, its check reads the tag the edit wrote, and no
// new tag is set over it.
func checkEditedCompose(t *testing.T, dependencies Dependencies, pinned ir.Image) {
	t.Helper()
	ctx := context.Background()
	latest, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	edited := ir.Image{Registry: pinned.Registry, Repository: pinned.Repository, Tag: "2.2.4"}
	compose := strings.Replace(latest.Generation.ComposeYAML, pinned.String(), edited.TaggedReference(), 1)
	result, err := EditCompose(ctx, dependencies, "miniflux", []byte(compose))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := dependencies.Workspace.Store.Images(ctx, result.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := recorded[slices.IndexFunc(recorded, func(image store.Image) bool { return image.Service == "miniflux" })]
	if app.Labels[images.VersionLabel] != "2.2.3" || !app.Created.Equal(minifluxBuilt) {
		t.Fatalf("an edited version lost its image facts: %+v", app)
	}
	statuses, err := CheckImages(ctx, dependencies, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	checked := statusOf(t, statuses, "miniflux")
	if checked.Tag != "2.2.4" || checked.State != ImageUnpinned || checked.Newest != "2.2.5" || checked.Version != "" {
		t.Fatalf("check of an edited compose.yaml: %+v", checked)
	}
	if _, err := UseTag(ctx, dependencies, "miniflux", "miniflux", "2.2.5"); !errors.Is(err, ErrEditedCompose) {
		t.Fatalf("a new tag over an edited compose.yaml: %v", err)
	}
}

// checkOutsideTheAllowlist: an image its namespace does not admit, from a
// repository of another owner, is reported and never asked about.
func checkOutsideTheAllowlist(t *testing.T, dependencies Dependencies, mirror *mirrorRegistry) {
	t.Helper()
	options := testOptions()
	options.RunSandbox, options.Name, options.Upstream = false, "elsewhere", "https://github.com/elsewhere/v2"
	if _, err := Generate(context.Background(), dependencies, options); err != nil {
		t.Fatal(err)
	}
	dependencies.Config.Registries.Allow = nil
	asked := mirror.requests()
	statuses, err := CheckImages(context.Background(), dependencies, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	outside := statusOf(t, statuses, "miniflux")
	if outside.State != ImageUnchecked || outside.Problem != outsideAllowlist || mirror.wasAsked(asked, minifluxRepository) {
		t.Fatalf("an image outside the allowlist: %+v", outside)
	}
}

// TestRegenerationKeepsAChosenTag: a tag picked with Use tag stays when the
// project is generated again, from Review or with gen, and is pinned anew.
func TestRegenerationKeepsAChosenTag(t *testing.T) {
	ctx := context.Background()
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	generated(t, dependencies)
	mirror, inspector := newMirrorRegistry(t, dependencies.Workspace.Store)
	for _, tag := range []string{"latest", "2.2.5"} {
		mirror.push(minifluxRepository, tag)
	}
	dependencies.Inspector = inspector
	if _, err := UseTag(ctx, dependencies, "miniflux", "miniflux", "2.2.5"); err != nil {
		t.Fatal(err)
	}
	stored, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	fromReview, err := stored.Regeneration(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	plainGen := testOptions()
	for name, options := range map[string]Options{"from Review": fromReview, "with gen": plainGen} {
		options.RunSandbox = false
		result, err := Generate(ctx, dependencies, options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if service, _ := result.Plan.Service("miniflux"); service.Image.Tag != "2.2.5" || service.Image.Digest == "" {
			t.Fatalf("%s: the chosen tag was not kept and pinned: %+v", name, service.Image)
		}
	}
}

// TestCheckedPlatformNeedsEveryImage: DEPLOY.md names the platform only when
// the registry listed it for every image of the plan.
func TestCheckedPlatformNeedsEveryImage(t *testing.T) {
	app := ir.Image{Registry: ir.DockerHubHost, Repository: minifluxRepository, Tag: "2.2.3"}
	database := ir.Image{Registry: ir.DockerHubHost, Repository: "library/postgres", Tag: "17"}
	plan := &ir.IR{Services: []ir.Service{{Name: "miniflux", Image: app}, {Name: "db", Image: database}}}
	listed := policy.ImageCheck{Found: true, Platforms: []string{"linux/amd64", "linux/arm64/v8"}}
	checks := map[string]policy.ImageCheck{rules.ImageKey(app): listed, rules.ImageKey(database): listed}
	if platform := checkedPlatform("linux/arm64", plan, checks); platform != "linux/arm64" {
		t.Errorf("every image listed: %q", platform)
	}
	if platform := checkedPlatform("", plan, checks); platform != "" {
		t.Errorf("no platform chosen: %q", platform)
	}
	delete(checks, rules.ImageKey(database))
	if platform := checkedPlatform("linux/arm64", plan, checks); platform != "" {
		t.Errorf("an image not checked: %q", platform)
	}
}
