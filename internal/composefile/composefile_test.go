package composefile

import (
	"context"
	"testing"
)

// TestLoadReadsNothingElse: an untrusted file's include and extends are not
// followed, so loading it reads no other file, and its services are only the
// ones it declares, taking nothing from what they extend.
func TestLoadReadsNothingElse(t *testing.T) {
	content := []byte(`include:
  - ../secrets/compose.yaml
services:
  base:
    image: docker.io/library/nginx:1.27
  web:
    extends:
      file: ../secrets/base.yaml
      service: base
    command: ["serve"]
`)
	project, err := Load(context.Background(), "compose.yaml", content, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Services) != 2 {
		t.Fatalf("services %v, want only base and web", project.ServiceNames())
	}
	if image := project.Services["web"].Image; image != "" {
		t.Fatalf("web took the image %q from what it extends", image)
	}
}

// TestLoadKeepsReferencesVerbatim: without interpolation ${VAR} stays as
// written whatever the environment holds; with it, the value fills in.
func TestLoadKeepsReferencesVerbatim(t *testing.T) {
	content := []byte("services:\n  app:\n    image: docker.io/library/nginx:${TAG}\n")
	environment := map[string]string{"TAG": "1.27"}
	for interpolate, want := range map[bool]string{false: "docker.io/library/nginx:${TAG}", true: "docker.io/library/nginx:1.27"} {
		project, err := Load(context.Background(), "compose.yaml", content, environment, interpolate)
		if err != nil {
			t.Fatal(err)
		}
		if image := project.Services["app"].Image; image != want {
			t.Errorf("interpolate %v: image %q, want %q", interpolate, image, want)
		}
	}
}
