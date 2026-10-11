package rules

import (
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
)

func TestApplyImages(t *testing.T) {
	appImage := ir.Image{Registry: "docker.io", Repository: "example/app", Tag: "1.0"}
	dbImage := ir.Image{Registry: "docker.io", Repository: "library/postgres", Tag: "18"}
	plan := &ir.IR{Services: []ir.Service{
		{Name: "app", Role: ir.RoleApp, Image: appImage,
			Healthcheck: ir.Healthcheck{Test: []string{"CMD", "guess"}, Provenance: ir.Provenance{Source: ir.SourceDockerfile}}},
		{Name: "db", Role: ir.RoleDB, Image: dbImage,
			Volumes:     []ir.Volume{{Name: "db-data", ContainerPath: "/var/lib/postgresql/data", Provenance: ir.Provenance{Source: ir.SourceSidecarRule}}},
			Healthcheck: ir.Healthcheck{Test: []string{"CMD", "pg_isready"}, Provenance: ir.Provenance{Source: ir.SourceCompose}}},
	}}
	digest := "sha256:" + strings.Repeat("a", 64)
	inspected := map[string]images.Metadata{
		ImageKey(appImage): {Reference: "docker.io/example/app:1.0", Digest: digest, ConfigRead: true, Source: "https://github.com/someone/else",
			ExposedPorts: []string{"8080/tcp"}, Volumes: []string{"/data"}, Healthcheck: []string{"CMD-SHELL", "curl -f http://localhost:$PORT/"}},
		ImageKey(dbImage): {Reference: "docker.io/library/postgres:18", Digest: digest, ConfigRead: true, Source: "https://github.com/docker-library/postgres",
			Volumes: []string{"/var/lib/postgresql"}, Healthcheck: []string{"CMD", "image-check"}},
	}
	ApplyImages(plan, inspected, "https://github.com/example/app")
	app, db := plan.Services[0], plan.Services[1]
	if app.Image.Digest != digest || app.Healthcheck.Test[1] != "curl -f http://localhost:$$PORT/" || app.Healthcheck.Provenance.Source != ir.SourceOCILabel {
		t.Fatalf("app not enriched: %+v", app)
	}
	if len(app.Ports) != 1 || !app.Ports[0].Publish || app.Volumes[0].ContainerPath != "/data" {
		t.Fatalf("app ports/volumes: %+v", app)
	}
	if len(app.Notes) != 1 || !strings.Contains(app.Notes[0], "someone/else") {
		t.Fatalf("source label mismatch not reported: %v", app.Notes)
	}
	if db.Healthcheck.Test[1] != "pg_isready" || db.Volumes[0].ContainerPath != "/var/lib/postgresql" || len(db.Notes) != 0 {
		t.Fatalf("db: compose healthcheck must win, data path must follow the image, no label note: %+v", db)
	}
}

// TestImageVolumesStayPrivate: two images that both declare /data get two
// volumes, not one shared by accident.
func TestImageVolumesStayPrivate(t *testing.T) {
	first := ir.Image{Registry: "docker.io", Repository: "example/first", Tag: "1"}
	second := ir.Image{Registry: "docker.io", Repository: "example/second", Tag: "1"}
	plan := &ir.IR{Services: []ir.Service{{Name: "first", Image: first}, {Name: "second", Image: second}}}
	inspected := map[string]images.Metadata{
		ImageKey(first):  {Reference: "docker.io/example/first:1", ConfigRead: true, Volumes: []string{"/data"}},
		ImageKey(second): {Reference: "docker.io/example/second:1", ConfigRead: true, Volumes: []string{"/data"}},
	}
	ApplyImages(plan, inspected, "")
	if plan.Services[0].Volumes[0].Name != "data" || plan.Services[1].Volumes[0].Name != "second-data" {
		t.Fatalf("volumes: %+v / %+v", plan.Services[0].Volumes, plan.Services[1].Volumes)
	}
}
