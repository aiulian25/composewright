// Package pipeline turns a repository into a stack end to end: fetch,
// extract, generate, gate, render, approve, validate, store. It holds no
// presentation logic; callers receive results and print them.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	StageFacts  = "facts"
	StageIR     = "ir"
	StageRender = "render"

	shortCommit = 7
)

var Stages = []string{StageFacts, StageIR, StageRender}

type Options struct {
	Name      string `json:"name,omitempty"`
	Engine    string `json:"engine"`
	Source    string `json:"source"`
	Ref       string `json:"ref,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	StopAfter string `json:"-"`
	ExposeVia string `json:"expose_via,omitempty"`
	// Networks are the profile's external networks the app joins; with no
	// port mode and none chosen, the profile's default networks.
	Networks        []string `json:"networks,omitempty"`
	Storage         string   `json:"storage,omitempty"`
	ProfileFile     string   `json:"profile_file,omitempty"`
	AllowNeeds      []string `json:"allow_needs,omitempty"`
	AllowRegistries []string `json:"allow_registries,omitempty"`
	RunSandbox      bool     `json:"run_sandbox"`
	MaxIterations   int      `json:"max_iterations,omitempty"`
	ApprovedByFlag  bool     `json:"approved_by_flag,omitempty"`
	// Edited marks a version whose compose.yaml a person wrote on the
	// Review page instead of the renderer.
	Edited bool `json:"edited,omitempty"`
	// Uploaded marks a source of files someone gave, kept in a folder only
	// for the run: the folder's name says nothing about the app.
	Uploaded bool `json:"uploaded,omitempty"`
}

// Files are the rendered artefacts. Env contains secrets in plain text.
type Files struct {
	Compose []byte
	Env     []byte
	Deploy  []byte
}

type Result struct {
	Source     *fetch.Source
	Facts      *extract.Facts
	Inspected  map[string]images.Metadata
	Checks     map[string]policy.ImageCheck
	Verdicts   []sandbox.Verdict
	Notices    []string
	Engine     string
	Call       llm.Call
	Plan       *ir.IR
	Report     policy.Report
	Render     render.Options
	Files      Files
	Values     map[string]string
	Project    store.Project
	Generation store.Generation
}

// Dependencies are what the pipeline needs from the outside world.
type Dependencies struct {
	Config      config.Config
	Workspace   *workspace.Workspace
	Guard       *netguard.Guard
	Inspector   *images.Inspector
	Sandbox     *sandbox.Sandbox
	Assistant   *llm.Assistant
	Observer    Observer
	Approve     Approval
	Repairer    Repairer
	GitHubToken string
	Version     string
}

func (options Options) Check() error {
	if options.StopAfter != "" && !slices.Contains(Stages, options.StopAfter) {
		return fmt.Errorf("--stop-after %q must be one of %v", options.StopAfter, Stages)
	}
	if options.Engine != "" && !slices.Contains(Engines, options.Engine) {
		return fmt.Errorf("--engine %q must be one of %s", options.Engine, strings.Join(Engines, ", "))
	}
	for _, capability := range options.AllowNeeds {
		if !slices.Contains(ir.Capabilities, capability) {
			return fmt.Errorf("--allow-need %q must be one of %s", capability, strings.Join(ir.Capabilities, ", "))
		}
	}
	return nil
}

// Generate runs the pipeline up to options.StopAfter. Facts and the plan need
// no local state; rendering and storing need an initialised workspace.
func Generate(ctx context.Context, dependencies Dependencies, options Options) (*Result, error) {
	if err := options.Check(); err != nil {
		return nil, err
	}
	observer := dependencies.Observer
	observer.step(StepFetch, StateRunning, "")
	source, err := openSource(ctx, dependencies, options)
	if err != nil {
		return nil, observer.failed(StepFetch, err)
	}
	defer source.Close()
	if options.Owner != "" {
		source.Owner = options.Owner
	}
	observer.step(StepFetch, StateDone, "")
	observer.step(StepExtract, StateRunning, "")
	facts, err := extract.Extract(ctx, source.Directory)
	if err != nil {
		return nil, observer.failed(StepExtract, err)
	}
	observer.step(StepExtract, StateDone, "")
	observer.facts(facts, source)
	result := &Result{Source: source, Facts: facts}
	if options.StopAfter == StageFacts {
		return result, nil
	}
	result.Notices = append(result.Notices, facts.Problems...)
	input := plan(ctx, dependencies, options, result)
	if options.StopAfter == StageIR {
		return result, nil
	}
	if dependencies.Workspace == nil {
		return nil, fmt.Errorf("%w: rendering needs `composewright init` first", workspace.ErrNotInitialised)
	}
	observer.step(StepRender, StateRunning, "")
	own := rules.OwnSecrets(result.Facts, result.Source)
	if err := renderAndGate(ctx, dependencies, options, result, input, own); err != nil {
		return nil, observer.failed(StepRender, err)
	}
	if err := persist(ctx, dependencies, options, result, 1, store.ActionGenerate); err != nil {
		return nil, observer.failed(StepRender, err)
	}
	observer.step(StepRender, StateDone, "")
	if !options.RunSandbox || result.Report.Blocked() || options.StopAfter == StageRender {
		return result, nil
	}
	return result, approveAndValidate(ctx, dependencies, options, result)
}

// plan builds the plan with the chosen engine, verifies the images and runs
// the policy gate, reporting each stage.
func plan(ctx context.Context, dependencies Dependencies, options Options, result *Result) policy.Input {
	observer := dependencies.Observer
	observer.step(StepPlan, StateRunning, "")
	draft(ctx, dependencies, options, result)
	observer.step(StepPlan, StateDone, "")
	input := policyInput(dependencies.Config, result.Plan, result.Source, options)
	imagesState, imagesNote := StateRunning, ""
	if dependencies.Inspector == nil {
		imagesState, imagesNote = StateSkipped, "offline: images are not verified or pinned"
	}
	observer.step(StepImages, imagesState, imagesNote)
	result.Inspected, result.Checks = verifyImages(ctx, dependencies.Inspector, result.Plan, input)
	rules.ApplyImages(result.Plan, result.Inspected, result.Source.URL)
	if dependencies.Inspector != nil {
		observer.step(StepImages, StateDone, "")
	}
	fill(ctx, dependencies, options, result)
	observer.step(StepPolicy, StateRunning, "")
	input.Plan, input.Images = result.Plan, result.Checks
	result.Report = policy.Evaluate(input)
	observer.step(StepPolicy, StateDone, "")
	return input
}

// approveAndValidate asks for approval; nothing runs without it. A --yes
// approval is recorded as such in the audit log.
func approveAndValidate(ctx context.Context, dependencies Dependencies, options Options, result *Result) error {
	approved, err := dependencies.Approve(result)
	if err != nil || !approved {
		return err
	}
	details := map[string]any{"generation": result.Generation.ID, "approved_by_yes_flag": options.ApprovedByFlag, "allow_needs": options.AllowNeeds}
	if err := dependencies.Workspace.Store.Audit(ctx, dependencies.Workspace.Actor, store.ActionApprove, result.Project.ID, details); err != nil {
		return err
	}
	return validationLoop(ctx, dependencies, options, result, result.Generation.Iteration)
}

// openSource fetches a GitHub repository as a tarball, or uses a local
// directory as it is.
func openSource(ctx context.Context, dependencies Dependencies, options Options) (*fetch.Source, error) {
	owner, repository, isGitHub := fetch.ParseGitHub(options.Source)
	if isGitHub && dependencies.Guard == nil {
		return nil, errors.New("fetching from GitHub needs network access through netguard")
	}
	if isGitHub {
		return fetch.GitHub(ctx, dependencies.Guard, dependencies.GitHubToken, owner, repository, options.Ref)
	}
	if strings.Contains(options.Source, "://") || strings.HasPrefix(options.Source, "git@") {
		return nil, fmt.Errorf("%s: only https://github.com/<owner>/<repository> URLs and local directories are supported", options.Source)
	}
	source, err := fetch.Local(options.Source, options.Upstream)
	if err == nil && options.Uploaded {
		source.Repository = ""
	}
	return source, err
}

// verifyImages asks the registry about every image the policy would allow
// (images outside the allowlist are never contacted). A missing image
// becomes a deny; an unreachable registry only a warning.
func verifyImages(ctx context.Context, inspector *images.Inspector, plan *ir.IR, input policy.Input) (map[string]images.Metadata, map[string]policy.ImageCheck) {
	inspected := map[string]images.Metadata{}
	checks := map[string]policy.ImageCheck{}
	if inspector == nil {
		return inspected, checks
	}
	for _, service := range plan.Services {
		image := service.Image
		key := rules.ImageKey(image)
		_, done := checks[key]
		allowed := image.Registry != "" && policy.RegistryAllowed(image.Reference(), input.AllowRegistries, input.ExtraRegistries, input.RepositoryOwner)
		if done || !allowed {
			continue
		}
		metadata, err := inspector.Inspect(ctx, image)
		switch {
		case errors.Is(err, images.ErrNotFound):
			checks[key] = policy.ImageCheck{Found: false}
		case err != nil && metadata.Digest != "":
			inspected[key] = metadata
			checks[key] = policy.ImageCheck{Found: true, Message: "digest pinned, but the image config could not be read: " + err.Error()}
		case err != nil:
			checks[key] = policy.ImageCheck{Found: true, Message: err.Error()}
		default:
			inspected[key] = metadata
			checks[key] = policy.ImageCheck{Found: true}
		}
	}
	return inspected, checks
}

func policyInput(configuration config.Config, plan *ir.IR, source *fetch.Source, options Options) policy.Input {
	return policy.Input{Plan: plan, Profile: configuration.Profile, AllowRegistries: configuration.Registries.Allow,
		ExtraRegistries: options.AllowRegistries, RepositoryOwner: source.Owner, GrantedNeeds: options.AllowNeeds}
}

// renderAndGate runs the plan gate, renders the files and runs the second
// gate on the compose model compose-go reads back from the rendered YAML.
// renderAndGate renders the files and runs the YAML gate. own are secrets
// from the person's own .env, given only on a first generation: later runs
// read stored, redacted facts, whose masks must never become values.
func renderAndGate(ctx context.Context, dependencies Dependencies, options Options, result *Result, input policy.Input, own map[string]string) error {
	result.Report = policy.Evaluate(input)
	profile, err := loadProfile(dependencies.Config.Profile, options.ProfileFile)
	if err != nil {
		return err
	}
	renderOptions, err := render.Options{Profile: profile, ExposeVia: options.ExposeVia, Networks: options.Networks, Storage: options.Storage,
		GrantedNeeds: options.AllowNeeds, Header: header(dependencies.Version, result.Source, result.Plan)}.Resolve()
	if err != nil {
		return err
	}
	result.Render = renderOptions
	stored, err := storedValues(ctx, dependencies.Workspace.Store, result.Plan.App.Name)
	if err != nil {
		return err
	}
	known := map[string]string{}
	maps.Copy(known, stored)
	maps.Copy(known, own)
	if result.Values, err = render.Values(result.Plan, renderOptions, known); err != nil {
		return err
	}
	if result.Files.Compose, err = render.Compose(result.Plan, renderOptions); err != nil {
		return err
	}
	if result.Files.Env, err = render.Env(result.Plan, result.Values, envHeader(result.Plan)); err != nil {
		return err
	}
	result.Files.Deploy = render.Deploy(result.Plan, renderOptions, "not validated yet")
	project, err := composefile.Load(ctx, "compose.yaml", result.Files.Compose, nil, false)
	if err != nil {
		return fmt.Errorf("the rendered compose.yaml does not load: %w", err)
	}
	input.Profile = profile
	result.Report = result.Report.Merge(policy.EvaluateYAML(project, input))
	return nil
}

func loadProfile(configured config.Profile, file string) (config.Profile, error) {
	if file == "" {
		return configured, nil
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return config.Profile{}, err
	}
	profile, err := config.DecodeProfile(content)
	if err != nil {
		return config.Profile{}, fmt.Errorf("profile %s: %w", file, err)
	}
	return profile, profile.Validate()
}

// storedValues returns the .env values kept for an existing project, so a
// regeneration reuses secrets that may already be deployed.
func storedValues(ctx context.Context, database *store.Store, name string) (map[string]string, error) {
	project, err := database.FindProject(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	variables, err := database.EnvVars(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(variables))
	for _, variable := range variables {
		values[variable.Key] = variable.Value
	}
	return values, nil
}

// persist stores the result as a new generation and audits it as action.
func persist(ctx context.Context, dependencies Dependencies, options Options, result *Result, iteration int, action string) error {
	database := dependencies.Workspace.Store
	status := store.StatusGenerated
	if result.Report.Blocked() {
		status = store.StatusBlocked
	}
	source := result.Source
	project, err := database.SaveProject(ctx, store.Project{Name: result.Plan.App.Name, Slug: result.Plan.App.Name, SourceKind: source.Kind,
		SourceURL: source.URL, SourceRef: source.Ref, SourceCommit: source.Commit, SourceOwner: source.Owner, Status: status})
	if err != nil {
		return err
	}
	generation, err := database.AddGeneration(ctx, generationRecord(project.ID, options, result, iteration), imageRecords(result.Plan, result.Inspected))
	if err != nil {
		return err
	}
	if err := database.ReplaceEnvVars(ctx, project.ID, envRecords(result.Plan, result.Values)); err != nil {
		return err
	}
	result.Project, result.Generation = project, generation
	details := map[string]any{"generation": generation.ID, "engine": result.Engine, "model": result.Call.Model, "source": source.URL, "commit": source.Commit,
		"status": status, "denied": result.Report.Count(policy.Deny), "needs": result.Report.Count(policy.Need)}
	if err := database.Audit(ctx, dependencies.Workspace.Actor, action, project.ID, details); err != nil {
		return err
	}
	for _, capability := range options.AllowNeeds {
		grant := map[string]any{"capability": capability, "generation": generation.ID}
		if err := database.Audit(ctx, dependencies.Workspace.Actor, store.ActionGrantNeed, project.ID, grant); err != nil {
			return err
		}
	}
	return nil
}

func generationRecord(projectID string, options Options, result *Result, iteration int) store.Generation {
	plan, _ := json.Marshal(result.Plan)
	settings, _ := json.Marshal(options)
	report, _ := json.Marshal(result.Report)
	return store.Generation{ProjectID: projectID, Iteration: iteration, Engine: result.Engine, Provider: result.Call.Provider,
		Model: result.Call.Model, PromptHash: result.Call.PromptHash, IRJSON: string(plan),
		ComposeYAML: string(result.Files.Compose), DeployMD: string(result.Files.Deploy), OptionsJSON: string(settings), PolicyJSON: string(report),
		FactsJSON: factsJSON(result.Facts)}
}

// factsJSON keeps the redacted facts with the generation, so the Analyse
// and project pages can show them later.
func factsJSON(facts *extract.Facts) string {
	if facts == nil {
		return ""
	}
	redacted, err := facts.Redacted()
	if err != nil {
		return ""
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func imageRecords(plan *ir.IR, inspected map[string]images.Metadata) []store.Image {
	records := make([]store.Image, 0, len(plan.Services))
	for _, service := range plan.Services {
		metadata := inspected[rules.ImageKey(service.Image)]
		records = append(records, store.Image{Service: service.Name, Reference: service.Image.String(), Digest: service.Image.Digest,
			Registry: service.Image.Registry, OCISource: metadata.Source, VerifiedAt: metadata.FetchedAt})
	}
	return records
}

func envRecords(plan *ir.IR, values map[string]string) []store.EnvVar {
	variables := make([]store.EnvVar, 0, len(plan.EnvFile))
	for _, entry := range plan.EnvFile {
		variables = append(variables, store.EnvVar{Key: entry.Key, Value: values[entry.Key], Secret: entry.Secret, Required: entry.Required,
			Source: entry.Provenance.Source, SourceRef: entry.Provenance.SourceRef, Description: entry.Description})
	}
	return variables
}

func header(version string, source *fetch.Source, plan *ir.IR) string {
	origin := source.URL
	if source.Commit != "" {
		origin += " @ " + source.Commit[:min(len(source.Commit), shortCommit)]
	}
	return fmt.Sprintf("generated by composewright %s from %s\nno privileges unless marked NEED GRANTED BY USER · re-validate with: composewright validate %s",
		version, origin, plan.App.Name)
}

func envHeader(plan *ir.IR) string {
	return fmt.Sprintf("# %s environment, generated by composewright\n# Keep this file private (mode 0600): it holds generated secrets.\n", plan.App.Name)
}
