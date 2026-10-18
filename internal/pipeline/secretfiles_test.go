package pipeline

import (
	"context"
	"io"
	"io/fs"
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
	secretFileKey  = "POSTGRES_PASSWORD"
	secretFilePath = "secrets/postgres_password"
)

// secretFileDocker reads the run's secret file when Compose starts the run.
type secretFileDocker struct {
	*scriptedDocker
	content []byte
	mode    fs.FileMode
}

func (docker *secretFileDocker) run(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
	if strings.Contains(strings.Join(arguments, " "), " up ") {
		path := filepath.Join(directory, filepath.FromSlash(secretFilePath))
		docker.content, _ = os.ReadFile(path)
		if info, err := os.Stat(path); err == nil {
			docker.mode = info.Mode().Perm()
		}
	}
	return docker.scriptedDocker.run(ctx, directory, live, arguments...)
}

// TestSecretPassedAsAFile: a database password whose *_FILE twin the
// repository documents reaches the sandbox run, the bundle and a folder
// export as a 0600 file holding the value, never .env or an Arcane template,
// and an edit of .env, which does not list it, keeps it.
func TestSecretPassedAsAFile(t *testing.T) {
	ctx := context.Background()
	docker := &secretFileDocker{scriptedDocker: &scriptedDocker{states: []string{healthy}}}
	dependencies := testDependencies(t, docker.scriptedDocker)
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	dependencies.Sandbox = sandbox.New(docker.run, limits, 30*time.Second, "policy", dependencies.Workspace.Paths.ProjectsDir())
	repository := t.TempDir()
	files := map[string]string{
		"compose.yaml": "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    environment:\n      " + secretFileKey + ": ${" + secretFileKey + "}\n" +
			"  db:\n    image: docker.io/library/postgres:17\n    environment:\n      " + secretFileKey + ": ${" + secretFileKey + "}\n",
		".env.example": secretFileKey + "=\n" + secretFileKey + "_FILE=\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Source: repository, Name: "notes", MaxIterations: 1}
	if _, err := Generate(ctx, dependencies, options); err != nil {
		t.Fatal(err)
	}
	stored, err := Load(ctx, dependencies.Workspace, "notes")
	if err != nil {
		t.Fatal(err)
	}
	value := stored.Values[secretFileKey]
	bundle, err := stored.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	if value == "" || string(bundle.Secrets[secretFilePath]) != value || strings.Contains(string(bundle.Env), value) || strings.Contains(string(bundle.EnvExample), value) {
		t.Fatalf("the bundle does not hold the password in %s alone: %v", secretFilePath, bundle.Secrets)
	}
	if _, _, err := Revalidate(ctx, dependencies, "notes"); err != nil {
		t.Fatal(err)
	}
	if string(docker.content) != value || docker.mode != 0o600 {
		t.Fatalf("the sandbox run's secret file is %q with mode %o", docker.content, docker.mode)
	}
	folder := t.TempDir()
	if _, err := export.Write(folder, dependencies.Config.Profile, bundle, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(folder, ir.SecretsDirectory))
	written, readErr := os.ReadFile(filepath.Join(folder, filepath.FromSlash(secretFilePath)))
	if err != nil || readErr != nil || info.Mode().Perm() != 0o700 || string(written) != value {
		t.Fatalf("the export wrote %q in a folder of mode %v: %v %v", written, info, err, readErr)
	}
	templates := t.TempDir()
	if _, err := export.Write("arcane:"+templates, dependencies.Config.Profile, bundle, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(templates, "notes", ir.SecretsDirectory)); !os.IsNotExist(err) {
		t.Fatalf("the Arcane template carries the secret files: %v", err)
	}
	shown, err := stored.EnvFile(Revealed)
	if err != nil {
		t.Fatal(err)
	}
	edited, _, err := SaveEdit(ctx, dependencies, "notes", Edit{Compose: []byte(stored.Generation.ComposeYAML), Env: string(shown) + "EXTRA=1\n", Mask: Revealed})
	if err != nil {
		t.Fatal(err)
	}
	if entry, found := edited.Plan.EnvEntry(secretFileKey); !found || !entry.AsFile() || edited.Values[secretFileKey] != value {
		t.Fatalf("an edit of .env dropped the secret passed as a file: %+v", entry)
	}
}
