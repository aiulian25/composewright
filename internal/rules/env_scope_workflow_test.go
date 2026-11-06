package rules_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	exportfiles "github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
)

func TestScopedValuesSurviveReviewReopenAndExport(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	raw := `services:
  api:
    image: example/api:1
    environment: {PORT: "8080", PASSWORD: alpha-secret, SHARED: "${TOKEN:-shared}"}
  worker:
    image: example/worker:1
    environment: {PORT: "9090", PASSWORD: beta-secret, SHARED: "${TOKEN:-shared}"}
`
	if err := os.WriteFile(filepath.Join(source, "compose.yaml"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	facts, err := extract.Extract(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := rules.Build(ctx, rules.Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "api"}})
	options := render.Options{Profile: config.Default().Profile}
	values, err := render.Values(plan, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A Review edit addresses only the api's allocated output identifier.
	entry, _ := plan.EnvEntry("API_PORT")
	entry.Value = "8181"
	entry.Provenance.Source = ir.SourceUser
	values, err = render.Values(plan, options, values)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = ir.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	// Rename an existing service: references and stored secrets keep their identity.
	api, _ := plan.Service("api")
	api.Name = "renamed"
	reopened, err := render.Values(plan, options, values)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"API_PASSWORD", "WORKER_PASSWORD"} {
		if reopened[key] != values[key] || reopened[key] == "" {
			t.Fatalf("secret regenerated: %s", key)
		}
	}
	compose, err := render.Compose(plan, options)
	if err != nil {
		t.Fatal(err)
	}
	env, err := render.Env(plan, reopened, "")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "export")
	_, err = exportfiles.Write(target, options.Profile, exportfiles.Bundle{Project: "scoped", Compose: compose, Env: env}, false)
	if err != nil {
		t.Fatal(err)
	}
	compose, err = os.ReadFile(filepath.Join(target, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env, err = os.ReadFile(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := sandbox.ComposeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := composefile.Load(ctx, "compose.yaml", compose, environment, true)
	if err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string]string{"renamed": "8181", "worker": "9090"} {
		actual := resolved.Services[service].Environment
		if actual["PORT"] == nil || *actual["PORT"] != want {
			t.Fatalf("%s PORT: %v", service, actual["PORT"])
		}
		if actual["SHARED"] == nil || *actual["SHARED"] != reopened["TOKEN"] {
			t.Fatalf("%s shared source lost", service)
		}
	}
	if *resolved.Services["renamed"].Environment["PASSWORD"] == *resolved.Services["worker"].Environment["PASSWORD"] {
		t.Fatal("distinct credentials collapsed")
	}
}
