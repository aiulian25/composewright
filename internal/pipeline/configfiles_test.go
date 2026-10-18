package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	bundledPath    = "config/nginx.conf"
	bundledContent = "events {}\nhttp { server { listen 80; location /ready { return 200; } } }\n"
)

// runFiles reads, when Compose starts a run, the bundled file and the
// override beside it.
type runFiles struct {
	*scriptedDocker
	bundled  []byte
	override []byte
}

func (docker *runFiles) run(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
	if strings.Contains(strings.Join(arguments, " "), " up ") {
		docker.bundled, _ = os.ReadFile(filepath.Join(directory, filepath.FromSlash(bundledPath)))
		docker.override, _ = os.ReadFile(filepath.Join(directory, "override.yaml"))
	}
	return docker.scriptedDocker.run(ctx, directory, live, arguments...)
}

// TestRepositoryFileTravelsWithTheStack: a file of the repository a
// service mounts is kept with the version, sealed, survives a change on
// the Review page, reaches the sandbox run mounted read-only from the run's
// copy, and an export writes it readable by the container's user.
func TestRepositoryFileTravelsWithTheStack(t *testing.T) {
	ctx := context.Background()
	docker := &runFiles{scriptedDocker: &scriptedDocker{states: []string{healthy}}}
	dependencies := testDependencies(t, docker.scriptedDocker)
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	dependencies.Sandbox = sandbox.New(docker.run, limits, 30*time.Second, "policy", dependencies.Workspace.Paths.ProjectsDir())
	repository := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repository, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"deploy/compose.yaml": "services:\n  web:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n    volumes:\n      - ./nginx.conf:/etc/nginx/nginx.conf:ro\n",
		"deploy/nginx.conf":   bundledContent,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repository, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Generate(ctx, dependencies, Options{Source: repository, Name: "web", MaxIterations: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Revise(ctx, dependencies, "web", Revision{ExposeVia: "lan"}); err != nil {
		t.Fatal(err)
	}
	stored, err := Load(ctx, dependencies.Workspace, "web")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Generation.Number != 2 || string(stored.Generation.Files[bundledPath]) != bundledContent {
		t.Fatalf("version %d keeps %q", stored.Generation.Number, stored.Generation.Files)
	}
	if !strings.Contains(stored.Generation.ComposeYAML, "./"+bundledPath+":/etc/nginx/nginx.conf:ro") {
		t.Fatalf("compose.yaml does not mount the bundled file:\n%s", stored.Generation.ComposeYAML)
	}
	if _, _, err := Revalidate(ctx, dependencies, "web"); err != nil {
		t.Fatal(err)
	}
	if string(docker.bundled) != bundledContent || !strings.Contains(string(docker.override), "./"+bundledPath+":/etc/nginx/nginx.conf:ro") {
		t.Fatalf("the run had %q and the override:\n%s", docker.bundled, docker.override)
	}
	bundle, err := stored.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	if _, err := export.Write(folder, dependencies.Config.Profile, bundle, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(folder, filepath.FromSlash(bundledPath)))
	if err != nil || info.Mode().Perm() != ir.ConfigFileMode {
		t.Fatalf("the exported file: %v %v", info, err)
	}
}
