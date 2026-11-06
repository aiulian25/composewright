package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/composefile"
	exportfiles "github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/sandbox"
)

func TestImportedDotenvSecretSurvivesStorageAndExport(t *testing.T) {
	const own = "dummy-quote\"-dollar$-line\nsecond-line"
	directory := t.TempDir()
	files := map[string]string{
		"compose.yaml": "services:\n  app:\n    image: example/app:1\n    environment:\n      APP_PASSWORD: ${APP_PASSWORD}\n",
		".env":         "APP_PASSWORD='" + own + "'\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	ctx := context.Background()
	result, err := Generate(ctx, dependencies, Options{Source: directory, Name: "dotenv"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["APP_PASSWORD"] != own {
		t.Fatalf("import changed synthetic credential: %q", result.Values["APP_PASSWORD"])
	}
	plan, err := json.Marshal(result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan), "dummy-quote") {
		t.Fatal("credential in IR")
	}
	values, err := storedValues(ctx, dependencies.Workspace.Store, "dotenv")
	if err != nil {
		t.Fatal(err)
	}
	if values["APP_PASSWORD"] != own {
		t.Fatal("encrypted-store round trip changed value")
	}
	env, err := render.Env(result.Plan, values, "")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "export")
	_, err = exportfiles.Write(target, result.Render.Profile, exportfiles.Bundle{Project: "dotenv", Compose: result.Files.Compose, Env: env}, false)
	if err != nil {
		t.Fatal(err)
	}
	env, err = os.ReadFile(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := sandbox.ComposeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := composefile.Load(ctx, "compose.yaml", result.Files.Compose, resolved, true)
	if err != nil {
		t.Fatal(err)
	}
	if *parsed.Services["app"].Environment["APP_PASSWORD"] != own {
		t.Fatal("exported credential changed")
	}
}
