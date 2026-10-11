package export

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
)

func bundle() Bundle {
	return Bundle{Project: "miniflux", Compose: []byte("services: {}\n"), Env: []byte("PASSWORD=s3cret\n"),
		EnvExample: []byte("PASSWORD=\n"), Deploy: []byte("# miniflux\n")}
}

func TestWritePlainFolder(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "miniflux")
	written, err := Write(directory, config.Default().Profile, bundle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Files) != 3 || !written.ContainsSecret {
		t.Fatalf("written %+v", written)
	}
	for _, name := range []string{ComposeName, EnvName, DeployName} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != fileMode {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	if _, err := Write(directory, config.Default().Profile, bundle(), false); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite without --overwrite: %v", err)
	}
	if _, err := Write(directory, config.Default().Profile, bundle(), true); err != nil {
		t.Fatalf("--overwrite refused: %v", err)
	}
}

func TestWriteArcaneTemplateHasNoSecrets(t *testing.T) {
	templates := t.TempDir()
	written, err := Write("arcane:"+templates, config.Default().Profile, bundle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if written.ContainsSecret || written.Directory != filepath.Join(templates, "miniflux") {
		t.Fatalf("written %+v", written)
	}
	if _, err := os.Stat(filepath.Join(templates, "miniflux", EnvName)); err == nil {
		t.Fatal("a template contains .env with secrets")
	}
	content, _ := os.ReadFile(filepath.Join(templates, "miniflux", EnvExampleName))
	if string(content) != "PASSWORD=\n" {
		t.Fatalf(".env.example %q", content)
	}
}

func TestDefaultDestinationUsesTheProfile(t *testing.T) {
	profile := config.Default().Profile
	if _, err := Write("", profile, bundle(), false); err == nil {
		t.Fatal("exported without a destination")
	}
	profile.Storage.BasePath = t.TempDir()
	written, err := Write("", profile, bundle(), false)
	if err != nil || written.Directory != filepath.Join(profile.Storage.BasePath, "miniflux") {
		t.Fatalf("profile save_dir not used: %+v %v", written, err)
	}
}
