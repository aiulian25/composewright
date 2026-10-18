package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/pipeline"
)

const minifluxRepository = "miniflux/miniflux"

var minifluxBuilt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// pushTo stores a new image labelled with its tag as its version in the
// registry at host, and returns its digest.
func pushTo(t *testing.T, host, repository, tag string) string {
	t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	file.Config.Labels = map[string]string{images.VersionLabel: tag, images.SourceLabel: "https://github.com/miniflux/v2"}
	file.Created = v1.Time{Time: minifluxBuilt}
	if image, err = mutate.ConfigFile(image, file); err != nil {
		t.Fatal(err)
	}
	reference, err := name.ParseReference(host + "/" + repository + ":" + tag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	digest, _ := image.Digest()
	return digest.String()
}

// readThroughMirror makes every run read Docker Hub images from a fake
// registry, returning its host and the registries the latest run's egress
// guard was told the project trusts.
func (harness *harness) readThroughMirror() (string, *[]string) {
	harness.t.Helper()
	server := httptest.NewServer(registry.New())
	harness.t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	hostname, _, _ := strings.Cut(host, ":")
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Registry: {hostname}}, PrivateTargets: []string{host}})
	if err != nil {
		harness.t.Fatal(err)
	}
	inspector := images.NewInspector(guard, nil, images.Options{ReadConfig: true, Mirror: host})
	var trusted []string
	harness.server.options.Connect = func(ctx context.Context, setup pipeline.Setup) (pipeline.Connection, error) {
		trusted = setup.ExtraRegistries
		connection, err := offline(ctx, setup)
		connection.Dependencies.Inspector = inspector
		return connection, err
	}
	return host, &trusted
}

// TestCheckImagesAndUseANewerTag: the project page checks the images and
// offers the newest stable tag of the family; using it makes a version
// pinned to that image and tests it, and the image's facts show on the
// Review's compose pane and the Analysis.
func TestCheckImagesAndUseANewerTag(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	harness.waitForJob(response.Header.Get("Location"))
	response, page = harness.post("/projects/miniflux/review", url.Values{trustField: {"docker.io/miniflux/*"}})
	status(t, response, http.StatusSeeOther, page)
	host, trusted := harness.readThroughMirror()
	pinned := pushTo(t, host, minifluxRepository, "2.2.3")
	for _, tag := range []string{"2.2.4", "nightly", "2.2.6-rc1"} {
		pushTo(t, host, minifluxRepository, tag)
	}
	newest := pushTo(t, host, minifluxRepository, "2.2.5")
	use := func(tag string) {
		t.Helper()
		response, page := harness.post("/projects/miniflux/images/use", url.Values{serviceField: {"miniflux"}, tagField: {tag}})
		status(t, response, http.StatusSeeOther, page)
		if run := harness.waitForJob(response.Header.Get("Location")); run.Failure != "" {
			t.Fatalf("the run with %s: %s", tag, run.Failure)
		}
	}
	for service, tag := range map[string]string{"nosuch": "2.2.3", "miniflux": "bad tag"} {
		response, page = harness.post("/projects/miniflux/images/use", url.Values{serviceField: {service}, tagField: {tag}})
		status(t, response, http.StatusBadRequest, page)
	}
	use("2.2.3")
	response, page = harness.post("/projects/miniflux/images", nil)
	status(t, response, http.StatusOK, page)
	for _, want := range []string{"docker.io/miniflux/miniflux:2.2.3", "same image as pinned", "2.2.4, 2.2.5", "Use 2.2.5 and re-test", "1 Sep 2026"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the images card lacks %q:\n%s", want, page)
		}
	}
	if strings.Count(page, pipeline.ShortDigest(pinned)) < 2 {
		t.Fatal("the images card does not show the pinned digest next to the current one")
	}
	if !slices.Equal(*trusted, []string{"docker.io/miniflux/*"}) {
		t.Fatalf("the check's egress guard trusted %v, not the project's registries", *trusted)
	}
	if strings.Contains(page, "nightly") || strings.Contains(page, "2.2.6-rc1") {
		t.Fatal("the images card offers a development build")
	}
	use("2.2.5")
	stored, err := pipeline.Load(context.Background(), harness.server.workspace(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	service, _ := stored.Plan.Service("miniflux")
	if service.Image.Tag != "2.2.5" || service.Image.Digest != newest || stored.Generation.ValidationJSON == "" {
		t.Fatalf("the latest version runs %+v, tested: %v", service.Image, stored.Generation.ValidationJSON != "")
	}
	response, page = harness.get("/projects/miniflux/review")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `title="2.2.5 · 1 Sep 2026 · https://github.com/miniflux/v2"`) {
		t.Fatalf("the compose pane does not show the image's facts:\n%s", page)
	}
	response, page = harness.get("/projects/miniflux/analyse")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, imageVersionRow) {
		t.Fatal("the analysis lacks the image version")
	}
	pushTo(t, host, minifluxRepository, "2.2.6")
	connection, err := harness.server.connect(context.Background(), "", pipeline.EngineRules, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.EditCompose(context.Background(), connection.Dependencies, "miniflux", []byte(stored.Generation.ComposeYAML+"# kept by hand\n")); err != nil {
		t.Fatal(err)
	}
	response, page = harness.post("/projects/miniflux/images", nil)
	status(t, response, http.StatusOK, page)
	if strings.Contains(page, "Use 2.2.6 and re-test") || !strings.Contains(page, editedTagHint) {
		t.Fatalf("the images card offers a new tag over an edited compose.yaml:\n%s", page)
	}
	response, page = harness.post("/projects/miniflux/images/use", url.Values{serviceField: {"miniflux"}, tagField: {"2.2.6"}})
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, editedTagHint) {
		t.Fatalf("using a tag over an edited compose.yaml does not say why it is refused:\n%s", page)
	}
}
