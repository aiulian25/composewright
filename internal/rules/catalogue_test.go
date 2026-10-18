package rules

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
)

func parsedImage(t *testing.T, reference string) ir.Image {
	t.Helper()
	image, err := ir.ParseImage(reference)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// TestLaterOverridesWin: override files merge over the shipped catalogue in
// name order. A later app entry wins for the images it lists, written as
// short as people write them, while the shipped entry still serves its
// other images; a family replaces the shipped one of its name. The shipped
// catalogue is left as it was, and a plan built with the overrides cites
// their healthcheck as the catalogue's.
func TestLaterOverridesWin(t *testing.T) {
	merged := LoadCatalogue(map[string][]byte{
		"20-later.yaml": []byte("apps:\n  - images: [ghcr.io/x/bookorbit, miniflux/miniflux]\n    healthcheck: [\"CMD\", \"/app\", \"later\"]\n"),
		"10-earlier.yaml": []byte(`families:
  - name: redis
    role: cache
    images: [redis]
    healthcheck: ["CMD", "redis-cli", "-p", "6380", "ping"]
apps:
  - images: [ghcr.io/x/bookorbit]
    healthcheck: ["CMD", "/app", "earlier"]
    memory_limit: 1g
`),
	})
	if want := []Override{{Name: "10-earlier.yaml", Families: 1, Apps: 1}, {Name: "20-later.yaml", Apps: 1}}; len(merged.Problems) != 0 || !slices.Equal(merged.Overrides, want) {
		t.Fatalf("overrides %+v, problems %q", merged.Overrides, merged.Problems)
	}
	later := []string{"CMD", "/app", "later"}
	bookorbit, miniflux := parsedImage(t, "ghcr.io/x/bookorbit:1.0"), parsedImage(t, "miniflux/miniflux:2.2.0")
	if !slices.Equal(merged.appHealthcheck(bookorbit), later) || merged.appMemory(bookorbit) != "" {
		t.Fatalf("bookorbit: healthcheck %q, memory %q", merged.appHealthcheck(bookorbit), merged.appMemory(bookorbit))
	}
	if !slices.Equal(merged.appHealthcheck(miniflux), later) || merged.appProbePath(miniflux) != "" {
		t.Fatalf("miniflux: healthcheck %q, probe path %q", merged.appHealthcheck(miniflux), merged.appProbePath(miniflux))
	}
	if path := merged.appProbePath(parsedImage(t, "ghcr.io/miniflux/miniflux")); path != "/healthcheck" {
		t.Fatalf("the shipped entry's other image lost its probe path: %q", path)
	}
	redis, found := merged.family(parsedImage(t, "redis:7"))
	if !found || !slices.Contains(redis.Healthcheck, "6380") || len(merged.Families) != len(shipped.Families) {
		t.Fatalf("redis %+v, %d families", redis, len(merged.Families))
	}
	if _, found := merged.family(parsedImage(t, "redis/redis-stack-server")); found {
		t.Fatal("the replaced family still claims an image only the shipped one listed")
	}
	if shippedRedis, _ := shipped.family(parsedImage(t, "redis")); slices.Contains(shippedRedis.Healthcheck, "6380") || shipped.appHealthcheck(bookorbit) != nil {
		t.Fatal("merging changed the shipped catalogue")
	}
	facts := testFacts(t, map[string]string{"compose.yaml": "services:\n  bookorbit:\n    image: ghcr.io/x/bookorbit:1.0\n    ports: [\"8080:8080\"]\n"})
	source := &fetch.Source{Owner: "x", Repository: "bookorbit"}
	app, _ := Build(context.Background(), Input{Facts: facts, Source: source, Catalogue: merged}).Service("bookorbit")
	if app == nil || !slices.Equal(app.Healthcheck.Test, later) || app.Healthcheck.Provenance.Source != ir.SourceHealthchecks {
		t.Fatalf("app %+v", app)
	}
	if app, _ := Build(context.Background(), Input{Facts: facts, Source: source}).Service("bookorbit"); app.Healthcheck.Defined() {
		t.Fatalf("a plan without the overrides has their healthcheck: %+v", app.Healthcheck)
	}
}

// TestOverrideProblems: a file the rules cannot use is left out, with a
// one-line problem that starts with its name: not YAML, a key the catalogue
// has not, an image that does not parse, an entry without images, a memory
// limit that is not a size, an empty or misspelt healthcheck, and a family
// without a name, a known role or a sidecar it can run. The others and the
// shipped catalogue still apply.
func TestOverrideProblems(t *testing.T) {
	cases := map[string]struct{ content, problem string }{
		"not yaml":          {"apps: [", "yaml:"},
		"unknown key":       {"apps:\n  - images: [x/app]\n    memory: 2g\n", "field memory not found"},
		"image":             {"apps:\n  - images: [\"Not An Image\"]\n", `image "Not An Image"`},
		"no images":         {"apps:\n  - healthcheck: [\"CMD\", \"true\"]\n", "lists no images"},
		"memory":            {"apps:\n  - images: [x/app]\n    memory_limit: lots\n", `memory_limit "lots" is not a size`},
		"empty healthcheck": {"apps:\n  - images: [x/app]\n    healthcheck: []\n", "healthcheck is an empty list"},
		"healthcheck text":  {"apps:\n  - images: [x/app]\n    healthcheck: CMD true\n", "cannot unmarshal"},
		"unnamed family":    {"families:\n  - role: db\n    images: [x/db]\n", "a family has no name"},
		"role":              {"families:\n  - name: x\n    role: database\n    images: [x/db]\n", `role "database" is not one of`},
		"family image":      {"families:\n  - name: x\n    role: db\n    images: [\"x/db:\"]\n", "family x: image"},
		"sidecar":           {"families:\n  - name: x\n    role: db\n    images: [x/db]\n    sidecar: {port: 5432}\n", "the sidecar needs a port and a tag"},
	}
	files := map[string][]byte{"good.yaml": []byte("apps:\n  - images: [x/app]\n    memory_limit: 1g\n")}
	for name, testCase := range cases {
		files[name+".yaml"] = []byte(testCase.content)
	}
	merged := LoadCatalogue(files)
	if len(merged.Overrides) != 1 || merged.Overrides[0].Name != "good.yaml" || len(merged.Problems) != len(cases) {
		t.Fatalf("overrides %+v, problems %q", merged.Overrides, merged.Problems)
	}
	for name, testCase := range cases {
		prefix := name + ".yaml" + problemSeparator
		found := slices.IndexFunc(merged.Problems, func(problem string) bool { return strings.HasPrefix(problem, prefix) })
		if found < 0 || !strings.Contains(merged.Problems[found], testCase.problem) || strings.Contains(merged.Problems[found], "\n") {
			t.Errorf("%s: problems %q", name, merged.Problems)
		}
	}
	if merged.appMemory(parsedImage(t, "x/app")) != "1g" || merged.appProbePath(parsedImage(t, "miniflux/miniflux")) != "/healthcheck" {
		t.Fatal("a good file or the shipped catalogue no longer applies")
	}
}
