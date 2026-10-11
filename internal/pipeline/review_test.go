package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

// recorder keeps everything an observer is told.
type recorder struct {
	mutex sync.Mutex
	steps []string
	facts int
	lines []sandbox.Line
}

func (recorder *recorder) observer() Observer {
	return Observer{
		Step: func(step Step) {
			recorder.mutex.Lock()
			defer recorder.mutex.Unlock()
			recorder.steps = append(recorder.steps, step.Name+":"+step.State)
		},
		Facts: func(facts *extract.Facts, _ *fetch.Source) {
			recorder.mutex.Lock()
			defer recorder.mutex.Unlock()
			recorder.facts++
			if strings.Contains(facts.Summary.Text+strings.Join(facts.Files, ""), "test123") {
				panic("unredacted facts")
			}
		},
		Line: func(line sandbox.Line) {
			recorder.mutex.Lock()
			defer recorder.mutex.Unlock()
			recorder.lines = append(recorder.lines, line)
		},
	}
}

func generated(t *testing.T, dependencies Dependencies) *Result {
	t.Helper()
	options := testOptions()
	options.RunSandbox = false
	result, err := Generate(context.Background(), dependencies, options)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGenerateReportsEveryStage(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	recorded := &recorder{}
	dependencies.Observer = recorded.observer()
	result := generated(t, dependencies)
	want := "fetch:running fetch:done extract:running extract:done plan:running plan:done images:skipped model:skipped policy:running policy:done render:running render:done"
	if got := strings.Join(recorded.steps, " "); got != want || recorded.facts != 1 {
		t.Fatalf("steps %q (facts %d), want %q", got, recorded.facts, want)
	}
	if result.Generation.FactsJSON == "" || strings.Contains(result.Generation.FactsJSON, "test123") {
		t.Fatal("facts were not stored, or stored unredacted")
	}
	stored, err := Load(context.Background(), dependencies.Workspace, "miniflux")
	if err != nil || stored.Facts == nil || len(stored.Facts.ComposeFiles) == 0 {
		t.Fatalf("stored facts: %v", err)
	}
}

func TestReviseRendersANewGeneration(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	first := generated(t, dependencies)
	ctx := context.Background()
	before, _ := Load(ctx, dependencies.Workspace, "miniflux")
	revision := Revision{ExposeVia: "lan", Values: map[string]string{"ADMIN_USERNAME": "boss", "DATABASE_URL": "postgres://miniflux:${POSTGRES_PASSWORD}@db/feeds"},
		Regenerate: []string{"ADMIN_PASSWORD"}, Add: []Variable{{Key: "LOG_LEVEL", Value: "debug"}, {Key: "SMTP_PASSWORD", Value: "mail-secret-123"}}}
	result, err := Revise(ctx, dependencies, "miniflux", revision)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"a new generation":             after.Generation.ID != first.Generation.ID && result.Generation.ID == after.Generation.ID,
		"value changed":                after.Values["ADMIN_USERNAME"] == "boss",
		"connection string changed":    strings.HasSuffix(after.Values["DATABASE_URL"], "@db/feeds"),
		"secret regenerated":           after.Values["ADMIN_PASSWORD"] != before.Values["ADMIN_PASSWORD"] && len(after.Values["ADMIN_PASSWORD"]) == 24,
		"other secret kept":            after.Values["POSTGRES_PASSWORD"] == before.Values["POSTGRES_PASSWORD"],
		"typed secret stored":          after.Values["SMTP_PASSWORD"] == "mail-secret-123",
		"typed secret not in the plan": !strings.Contains(after.Generation.IRJSON, "mail-secret-123"),
		"variable added to the app":    strings.Contains(after.Generation.ComposeYAML, `LOG_LEVEL: "${LOG_LEVEL}"`),
		"exposure applied":             after.Options.ExposeVia == "lan" && strings.Contains(after.Generation.ComposeYAML, `- "8080:8080/tcp"`),
		"facts carried over":           after.Generation.FactsJSON == before.Generation.FactsJSON,
		"engine carried over":          after.Generation.Engine == before.Generation.Engine,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	if t.Failed() {
		t.Logf("compose after the revision:\n%s", after.Generation.ComposeYAML)
	}
	entries, err := dependencies.Workspace.Store.AuditEntries(ctx, after.Project.ID, 10)
	if err != nil || entries[0].Action != store.ActionRevise {
		t.Fatalf("revision not audited: %+v %v", entries, err)
	}
	for name, bad := range map[string]Revision{
		"generated secret as a value": {Values: map[string]string{"ADMIN_PASSWORD": "x"}},
		"unknown variable":            {Values: map[string]string{"NOPE": "x"}},
		"generated secret typed in":   {Secrets: map[string]string{"POSTGRES_PASSWORD": "x"}},
		"plain value regenerated":     {Regenerate: []string{"ADMIN_USERNAME"}},
		"duplicate variable":          {Add: []Variable{{Key: "DEBUG", Value: "0"}}},
		"unknown need":                {AllowNeeds: []string{"everything"}},
	} {
		if _, err := Revise(ctx, dependencies, "miniflux", bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestApproveAndValidate(t *testing.T) {
	docker := &scriptedDocker{states: []string{unhealthy, healthy}}
	dependencies := testDependencies(t, docker)
	dependencies.Repairer = &fakeRepairer{}
	generated(t, dependencies)
	recorded := &recorder{}
	dependencies.Observer = recorded.observer()
	result, err := ApproveAndValidate(context.Background(), dependencies, "miniflux", "web")
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(recorded.steps, " ")
	if len(result.Verdicts) != 2 || !result.Verdicts[1].Passed() || status(t, dependencies) != store.StatusPassed {
		t.Fatalf("verdicts %d, status %s", len(result.Verdicts), status(t, dependencies))
	}
	if steps != "validate:running validate:failed repair:running repair:done validate:running validate:done" {
		t.Fatalf("steps %q", steps)
	}
	if len(recorded.lines) == 0 || recorded.lines[0].Source != sandbox.SourceSandbox {
		t.Fatalf("no live lines: %+v", recorded.lines)
	}
	entries, _ := dependencies.Workspace.Store.AuditEntries(context.Background(), result.Project.ID, 20)
	approvals := 0
	for _, entry := range entries {
		if entry.Action == store.ActionApprove && entry.Details["via"] == "web" {
			approvals++
		}
	}
	if approvals != 1 {
		t.Fatalf("%d web approvals audited", approvals)
	}
}

func TestApproveRefusesABlockedPlan(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	options := Options{Source: minifluxFiles, RunSandbox: false}
	if _, err := Generate(context.Background(), dependencies, options); err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveAndValidate(context.Background(), dependencies, "miniflux", "web"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a blocked plan was approved: %v", err)
	}
}

// TestOwnEnvValuesAreKept: in a person's own folder or upload, real secrets
// from their .env are kept, stored encrypted and never put in the plan;
// placeholders are generated; the same file from a repository is a sample.
func TestOwnEnvValuesAreKept(t *testing.T) {
	const own = "MyOwnDbPassword-7c2f"
	directory := t.TempDir()
	files := map[string]string{
		"compose.yaml": "services:\n  app:\n    image: docker.io/library/nextcloud:31\n    environment:\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n" +
			"      NEXTCLOUD_ADMIN_PASSWORD: ${NEXTCLOUD_ADMIN_PASSWORD}\n      POSTGRES_HOST: db\n    depends_on: [db]\n" +
			"  db:\n    image: docker.io/library/postgres:17\n    environment:\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n",
		".env": "POSTGRES_PASSWORD=" + own + "\nNEXTCLOUD_ADMIN_PASSWORD=changeme\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	result, err := Generate(context.Background(), dependencies, Options{Source: directory, Name: "own"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["POSTGRES_PASSWORD"] != own || !strings.Contains(string(result.Files.Env), own) {
		t.Fatalf("the person's own password was not kept: %q", result.Values["POSTGRES_PASSWORD"])
	}
	if admin := result.Values["NEXTCLOUD_ADMIN_PASSWORD"]; admin == "" || admin == "changeme" {
		t.Fatalf("a placeholder was kept: %q", admin)
	}
	plan, _ := json.Marshal(result.Plan)
	if strings.Contains(string(plan), own) {
		t.Fatal("an own secret reached the plan")
	}
	if kept := rules.OwnSecrets(result.Facts, &fetch.Source{Kind: fetch.SourceGitHub}); len(kept) != 0 {
		t.Fatalf("a repository's .env was treated as the person's own: %v", kept)
	}
}

// TestSaveEdit: compose.yaml and .env edited side by side save as a version
// made by hand, and a secret's placeholder keeps the stored value; YAML
// Compose cannot load, a privilege, a variable the .env lacks, a tab and a
// line that is no assignment are refused at their lines.
func TestSaveEdit(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{})
	first := generated(t, dependencies)
	ctx := context.Background()
	stored, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	const mask = "<hidden>"
	shown, err := stored.EnvFile(mask)
	if err != nil || !strings.Contains(string(shown), mask) {
		t.Fatalf("no secret is masked: %v\n%s", err, shown)
	}
	compose := strings.Replace(string(first.Files.Compose), "restart: unless-stopped", "restart: always", 1)
	env := strings.Replace(string(shown), "ADMIN_USERNAME=admin", "ADMIN_USERNAME=boss", 1) + "EXTRA_FLAG=on\n"
	_, check, err := SaveEdit(ctx, dependencies, "miniflux", Edit{Compose: []byte(compose), Env: env, Mask: mask})
	if err != nil || check.Refused() || !check.Parses || !check.Defined || check.RulesPassed != check.Rules {
		t.Fatalf("a harmless edit: %v %+v", err, check)
	}
	saved, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	generations, err := dependencies.Workspace.Store.Generations(ctx, saved.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, added := saved.Plan.EnvEntry("EXTRA_FLAG")
	checks := map[string]bool{
		"compose as written":   saved.Generation.ComposeYAML == compose,
		"made by hand":         saved.Generation.Engine == EngineManual && saved.Options.Edited,
		"value changed":        saved.Values["ADMIN_USERNAME"] == "boss",
		"variable added":       added && saved.Values["EXTRA_FLAG"] == "on",
		"secret kept":          saved.Values["ADMIN_PASSWORD"] != "" && saved.Values["ADMIN_PASSWORD"] == stored.Values["ADMIN_PASSWORD"],
		"earlier version kept": len(generations) == 2,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	privileged := strings.Replace(compose, "restart: always", "restart: always\n    privileged: true", 1)
	undefined := strings.Replace(compose, `DEBUG: "${DEBUG}"`, `DEBUG: "${NOT_IN_ENV}"`, 1)
	refused := map[string]Edit{
		"broken YAML":       {Compose: []byte("services: [not, a, map"), Env: env, Mask: mask},
		"privileged":        {Compose: []byte(privileged), Env: env, Mask: mask},
		"missing variable":  {Compose: []byte(undefined), Env: env, Mask: mask},
		"tab":               {Compose: []byte(strings.Replace(compose, "restart: always", "restart:\talways", 1)), Env: env, Mask: mask},
		"not an assignment": {Compose: []byte(compose), Env: env + "not an assignment\n", Mask: mask},
	}
	results := map[string]EditCheck{}
	for name, edit := range refused {
		_, check, err := SaveEdit(ctx, dependencies, "miniflux", edit)
		if !errors.Is(err, ErrEditRefused) || !check.Refused() {
			t.Errorf("%s was saved: %v", name, err)
		}
		results[name] = check
	}
	if results["broken YAML"].Parses || results["broken YAML"].Problems[0].Line != 1 {
		t.Errorf("broken YAML: %+v", results["broken YAML"])
	}
	review := slices.ContainsFunc(results["privileged"].Problems, func(problem EditProblem) bool { return problem.Review })
	if !review || results["privileged"].RulesPassed == results["privileged"].Rules {
		t.Errorf("privileged: true was not sent to the Review step: %+v", results["privileged"])
	}
	line := slices.Index(strings.Split(undefined, "\n"), `      DEBUG: "${NOT_IN_ENV}"`) + 1
	missing := slices.ContainsFunc(results["missing variable"].Problems, func(problem EditProblem) bool {
		return problem.Line == line && strings.Contains(problem.Message, "NOT_IN_ENV")
	})
	if results["missing variable"].Defined || !missing {
		t.Errorf("an undefined variable was not pointed at line %d: %+v", line, results["missing variable"])
	}
	latest, _ := Load(ctx, dependencies.Workspace, "miniflux")
	if latest.Generation.ID != saved.Generation.ID {
		t.Fatal("a refused edit was stored")
	}
	shownAgain, _ := latest.EnvFile(mask)
	if _, _, err := SaveEdit(ctx, dependencies, "miniflux", Edit{Compose: []byte(compose), Env: string(shownAgain), Mask: mask}); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(ctx, dependencies.Workspace, "miniflux"); again.Generation.ID != saved.Generation.ID {
		t.Fatal("saving the version as it was added another")
	}
}

// TestEditCompose: a person's compose.yaml becomes the version the test
// runs; an edit that adds a privilege is blocked by the gate, YAML Compose
// cannot load is refused, and a Review change renders from the plan again.
func TestEditCompose(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{})
	first := generated(t, dependencies)
	ctx := context.Background()
	edited := strings.Replace(string(first.Files.Compose), "restart: unless-stopped", "restart: always", 1)
	if edited == string(first.Files.Compose) {
		t.Fatal("the rendered file has no restart policy to edit")
	}
	result, err := EditCompose(ctx, dependencies, "miniflux", []byte(edited))
	if err != nil || result.Report.Blocked() {
		t.Fatalf("a harmless edit: %v %+v", err, result.Report.Findings)
	}
	stored, err := Load(ctx, dependencies.Workspace, "miniflux")
	if err != nil || stored.Generation.ComposeYAML != edited || !stored.Options.Edited {
		t.Fatalf("the edit is not the stored version: %v edited=%v", err, stored.Options.Edited)
	}
	privileged := strings.Replace(edited, "restart: always", "restart: always\n    privileged: true", 1)
	result, err = EditCompose(ctx, dependencies, "miniflux", []byte(privileged))
	if err != nil || !result.Report.Blocked() {
		t.Fatalf("privileged: true passed the gate: %v %+v", err, result.Report.Findings)
	}
	if _, err := EditCompose(ctx, dependencies, "miniflux", []byte("services: [not, a, map")); !errors.Is(err, ErrComposeInvalid) {
		t.Fatalf("broken YAML: %v", err)
	}
	if _, err := Revise(ctx, dependencies, "miniflux", Revision{}); err != nil {
		t.Fatal(err)
	}
	stored, _ = Load(ctx, dependencies.Workspace, "miniflux")
	if stored.Options.Edited || stored.Generation.ComposeYAML == privileged {
		t.Fatal("a Review change kept the edited file instead of rendering the plan")
	}
}
