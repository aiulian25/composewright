package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
)

// TestImagesPrintsTheTable: the images command reads Docker Hub images
// through the configured mirror and prints each service's image and what
// its tag points at now, as a table or as JSON.
func TestImagesPrintsTheTable(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	if result := generate(t, environment); result.code != exitOK {
		t.Fatalf("gen: %+v", result)
	}
	opened := openTestWorkspace(t, environment)
	stored, err := pipeline.Load(context.Background(), opened, "miniflux")
	opened.Close()
	if err != nil {
		t.Fatal(err)
	}
	service, _ := stored.Plan.Service("miniflux")
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	reference, _ := name.ParseReference(host + "/" + service.Image.Repository + ir.TagSeparator + service.Image.Tag)
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	run(t, environment, "config", "set", "profile.registry_mirror", host)
	run(t, environment, "config", "set", "network.allow_private_targets", `["`+host+`"]`)
	if result := run(t, environment, "images", "miniflux"); result.code != exitOK || !strings.Contains(result.stdout, "NEWER") ||
		!strings.Contains(result.stdout, service.Image.TaggedReference()) || !strings.Contains(result.stdout, "not pinned before sha256:") {
		t.Fatalf("images: %+v", result)
	}
	if result := run(t, environment, "images", "miniflux", "--json"); result.code != exitOK || !strings.Contains(result.stdout, `"state": "unpinned"`) {
		t.Fatalf("images --json: %+v", result)
	}
}

// TestImagesTrustsTheProjectsRegistries: an image on a registry only the
// project trusts, through gen --allow-registry, is looked up rather than
// refused by the egress guard.
func TestImagesTrustsTheProjectsRegistries(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	reference, _ := name.ParseReference(host + "/example/app:1.0")
	if err := remote.Write(reference, image); err != nil {
		t.Fatal(err)
	}
	files := t.TempDir()
	compose := "services:\n  app:\n    image: " + reference.String() + "\n"
	if err := os.WriteFile(filepath.Join(files, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, environment, "config", "set", "network.allow_private_targets", `["`+host+`"]`)
	generated := run(t, environment, "gen", "--offline", "--no-llm", "--no-validate", "--name", "app", "--allow-registry", host+"/example/*", files)
	if generated.code != exitOK {
		t.Fatalf("gen: %+v", generated)
	}
	if result := run(t, environment, "images", "app"); result.code != exitOK || !strings.Contains(result.stdout, "not pinned before sha256:") {
		t.Fatalf("images: %+v", result)
	}
}
