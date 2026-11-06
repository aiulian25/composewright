package pipeline

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

const gatedSecret = `services:
  app:
    image: docker.io/library/nginx:1.27
    secrets: [token]
secrets:
  token:
    file: SOURCE
`

func gateInput() policy.Input {
	return policy.Input{Profile: config.Default().Profile, AllowRegistries: config.Default().Registries.Allow, Plan: &ir.IR{}}
}

// TestGateChecksComposeAsItRuns: a secret file that is safe as written but
// resolves outside ./secrets/ with .env is refused, as is one a default
// sends there and a file that cannot resolve; an escaped $ stays literal,
// as Compose keeps it, and a safe value passes.
func TestGateChecksComposeAsItRuns(t *testing.T) {
	cases := []struct {
		name, file, env string
		refused         bool
	}{
		{"resolves inside", "./secrets/${NAME}", "NAME=token\n", false},
		{"resolves outside", "./secrets/${NAME}", "NAME=../../../etc/hostname\n", true},
		{"default outside", "./secrets/${UNSET:-../../etc/hostname}", "", true},
		{"empty value", "./secrets/${NAME}token", "NAME=\n", false},
		{"escaped dollar", "./secrets/$${NAME}", "NAME=../../etc/hostname\n", false},
		{"does not resolve", "./secrets/${NAME:?set it}", "", true},
	}
	for _, test := range cases {
		compose := []byte(strings.Replace(gatedSecret, "SOURCE", test.file, 1))
		project, err := composefile.Load(context.Background(), composeFileName, compose, nil, false)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		report := gateYAML(context.Background(), project, compose, []byte(test.env), nil, gateInput())
		if refused := report.Has("P13", policy.Deny); refused != test.refused {
			t.Errorf("%s: refused = %v, want %v: %+v", test.name, refused, test.refused, report.Findings)
		}
	}
}

// leakingEdit adds to compose.yaml a secret whose file a .env value names.
func leakingEdit(t *testing.T, stored *Stored) []byte {
	t.Helper()
	compose := strings.Replace(stored.Generation.ComposeYAML, "  miniflux:\n", "  miniflux:\n    secrets: [leak]\n", 1)
	if compose == stored.Generation.ComposeYAML {
		t.Fatal("no miniflux service to edit")
	}
	return []byte(compose + "secrets:\n  leak:\n    file: ./secrets/${OUTSIDE}\n")
}

// TestEveryEntryPointGatesTheResolvedCompose: an edit whose .env sends a
// secret file outside ./secrets/ is refused when saved, and a stored
// version whose value was changed to do so is refused before a test run
// ever reaches Docker, whatever its stored report says.
func TestEveryEntryPointGatesTheResolvedCompose(t *testing.T) {
	ctx := context.Background()
	docker := &scriptedDocker{states: []string{healthy}}
	dependencies := testDependencies(t, docker)
	generated(t, dependencies)
	stored, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	shown, err := stored.EnvFile(Revealed)
	if err != nil {
		t.Fatal(err)
	}
	compose := leakingEdit(t, stored)
	if _, _, err := SaveEdit(ctx, dependencies, "miniflux", Edit{Compose: compose, Env: string(shown) + "OUTSIDE=../../../etc/hostname\n", Mask: Revealed}); !errors.Is(err, ErrEditRefused) {
		t.Fatalf("an edit leaving ./secrets/ was saved: %v", err)
	}
	if _, _, err := SaveEdit(ctx, dependencies, "miniflux", Edit{Compose: compose, Env: string(shown) + "OUTSIDE=token\n", Mask: Revealed}); err != nil {
		t.Fatalf("a safe edit was refused: %v", err)
	}
	edited, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil || edited.Report.Blocked() {
		t.Fatalf("the safe edit was stored blocked: %v", err)
	}
	variables, err := storedVariables(ctx, dependencies.Workspace.Store, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	values := maps.Clone(edited.Values)
	values["OUTSIDE"] = "../../../etc/hostname"
	if err := dependencies.Workspace.Store.ReplaceEnvVars(ctx, edited.Project.ID, envRecords(edited.Plan, values, variables)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Revalidate(ctx, dependencies, "miniflux"); !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "secret leak must be a file under ./secrets/") {
		t.Fatalf("a run resolving outside ./secrets/ was not refused: %v", err)
	}
	if docker.runs != 0 {
		t.Fatalf("Docker started the refused stack %d times", docker.runs)
	}
}

// TestGateHidesSecretValues: a finding about a resolved value never shows a
// secret it was built from.
func TestGateHidesSecretValues(t *testing.T) {
	compose := []byte(strings.Replace(gatedSecret, "SOURCE", "${TOKEN_PATH}", 1))
	project, err := composefile.Load(context.Background(), composeFileName, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	report := gateYAML(context.Background(), project, compose, []byte("TOKEN_PATH=/etc/s3cret-value\n"), []string{"s3cret-value"}, gateInput())
	shown := slices.ContainsFunc(report.Findings, func(finding policy.Finding) bool { return strings.Contains(finding.Message, "s3cret-value") })
	if !report.Has("P13", policy.Deny) || shown {
		t.Fatalf("findings %+v", report.Findings)
	}
}
