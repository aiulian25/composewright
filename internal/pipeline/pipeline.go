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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	Name       string `json:"name,omitempty"`
	Engine     string `json:"engine"`
	Source     string `json:"source"`
	Ref        string `json:"ref,omitempty"`
	Owner      string `json:"owner,omitempty"`
	SourceRoot string `json:"source_root,omitempty"`
	Upstream   string `json:"upstream,omitempty"`
	StopAfter  string `json:"-"`
	ExposeVia  string `json:"expose_via,omitempty"`
	// Networks are the profile's external networks the app joins; with no
	// port mode and none chosen, the profile's default networks.
	Networks []string `json:"networks,omitempty"`
	// PublicHost is the address people open the app at, a host name with an
	// optional path as home.example.com/rss; blank derives one from the
	// networks it joins.
	PublicHost  string `json:"public_host,omitempty"`
	Storage     string `json:"storage,omitempty"`
	ProfileFile string `json:"profile_file,omitempty"`
	// AllowNeeds are the grant keys of the exact requests a person granted,
	// as ir.Need.Grant makes them.
	AllowNeeds []string `json:"allow_needs,omitempty"`
	// GrantRequested are capabilities named on the command line: the
	// requests of them this run's plan makes are granted, once, before the
	// first render, so a repair can never widen them.
	GrantRequested  []string `json:"-"`
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
	// BenignLogPatterns are log lines a person said are normal for this
	// app: a matching line never fails a run.
	BenignLogPatterns []string `json:"benign_log_patterns,omitempty"`
	// EnabledOptional are the optional services, those upstream starts only
	// with a Compose profile, that a person switched on.
	EnabledOptional []string `json:"enabled_optional,omitempty"`
	// ComposeFile is the upstream compose file to build the plan from, by
	// its path in the repository; empty, the rules choose. The generation
	// stores the one the plan was built from.
	ComposeFile string `json:"compose_file,omitempty"`
	// Ports are the host ports a person chose, keyed by service and
	// container port as app:8080.
	Ports map[string]int `json:"ports,omitempty"`
	// ImageTags are the image tags a person chose with Use tag, by service:
	// every regeneration keeps them until another is chosen.
	ImageTags map[string]string `json:"image_tags,omitempty"`
	// Regenerating generates an existing project again from its own
	// source: a source read elsewhere is refused, never moved to.
	Regenerating bool `json:"-"`
}

// Files are the rendered artefacts. Env contains secrets in plain text.
type Files struct {
	Compose []byte
	Env     []byte
	Deploy  []byte
	// Config are the repository's files the plan bundles, by their path
	// beside compose.yaml.
	Config map[string][]byte
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
	Config    config.Config
	Workspace *workspace.Workspace
	Guard     *netguard.Guard
	Inspector *images.Inspector
	Sandbox   *sandbox.Sandbox
	Assistant *llm.Assistant
	Observer  Observer
	Approve   Approval
	Repairer  Repairer
	// Tokens are the code hosts' tokens, by host name.
	Tokens  map[string]string
	Version string
}

func (options Options) Check() error {
	if options.StopAfter != "" && !slices.Contains(Stages, options.StopAfter) {
		return fmt.Errorf("--stop-after %q must be one of %v", options.StopAfter, Stages)
	}
	if options.Engine != "" && !slices.Contains(Engines, options.Engine) {
		return fmt.Errorf("--engine %q must be one of %s", options.Engine, strings.Join(Engines, ", "))
	}
	if err := validPublicHost(options.PublicHost); err != nil {
		return err
	}
	for _, capability := range options.GrantRequested {
		if !slices.Contains(ir.Capabilities, capability) {
			return fmt.Errorf("--allow-need %q must be one of %s", capability, strings.Join(ir.Capabilities, ", "))
		}
	}
	for _, grant := range options.AllowNeeds {
		if !slices.Contains(ir.Capabilities, ir.GrantCapability(grant)) {
			return fmt.Errorf("granted need %q is not one of %s", grant, strings.Join(ir.Capabilities, ", "))
		}
	}
	return nil
}

// grantRequested grants each request the plan makes of a capability named
// on the command line, by its exact key, and forgets the names.
func grantRequested(options *Options, input *policy.Input) {
	if len(options.GrantRequested) == 0 {
		return
	}
	for _, finding := range policy.Evaluate(*input).Findings {
		named := finding.Severity == policy.Need && slices.Contains(options.GrantRequested, finding.Capability)
		if named && !slices.Contains(options.AllowNeeds, finding.Grant) {
			options.AllowNeeds = append(options.AllowNeeds, finding.Grant)
		}
	}
	options.GrantRequested, input.GrantedNeeds = nil, options.AllowNeeds
}

// Generate runs the pipeline up to options.StopAfter. Facts and the plan need
// no local state; rendering and storing need an initialised workspace.
func Generate(ctx context.Context, dependencies Dependencies, options Options) (*Result, error) {
	if err := options.Check(); err != nil {
		return nil, err
	}
	if err := absolutePaths(&options); err != nil {
		return nil, err
	}
	observer := dependencies.Observer
	observer.step(StepFetch, StateRunning, "")
	intakeTimeout := dependencies.Config.Network.IntakeTimeout
	if intakeTimeout <= 0 {
		intakeTimeout = 10 * time.Minute
	}
	intakeCtx, cancelIntake := context.WithTimeout(ctx, intakeTimeout)
	defer cancelIntake()
	source, err := openSource(intakeCtx, dependencies, options)
	if err != nil && intakeCtx.Err() != nil {
		err = fmt.Errorf("repository intake exceeded its time budget or was cancelled: %w", intakeCtx.Err())
	}
	if err != nil {
		return nil, observer.failed(StepFetch, err)
	}
	defer source.Close()
	if options.Owner != "" {
		source.Owner = options.Owner
	}
	observer.step(StepFetch, StateDone, source.Note)
	observer.step(StepExtract, StateRunning, "")
	facts, err := extract.ExtractSource(intakeCtx, source)
	if intakeCtx.Err() != nil {
		err = fmt.Errorf("repository analysis exceeded its time budget or was cancelled: %w", intakeCtx.Err())
	}
	cancelIntake()
	if err != nil {
		return nil, observer.failed(StepExtract, err)
	}
	options.SourceRoot = source.Subdir
	if source.Subdir != "" && options.Name == "" {
		candidate := strings.ToLower(path.Base(source.Subdir))
		if store.ValidProjectName(candidate) {
			options.Name = candidate
		}
	}
	if source.SelectedFile != "" && options.ComposeFile == "" {
		for _, file := range facts.ComposeFiles {
			if file.Path == source.SelectedFile {
				options.ComposeFile = file.Path
				break
			}
		}
	}
	observer.step(StepExtract, StateDone, "")
	observer.facts(facts, source)
	result := &Result{Source: source, Facts: facts}
	if options.StopAfter == StageFacts {
		return result, nil
	}
	result.Notices = append(result.Notices, facts.Problems...)
	input, err := plan(ctx, dependencies, &options, result)
	if err != nil {
		return nil, observer.failed(StepPlan, err)
	}
	if err := observer.project(result.Plan.App.Name); err != nil {
		return nil, observer.failed(StepPlan, err)
	}
	if options.StopAfter == StageIR {
		return result, nil
	}
	if dependencies.Workspace == nil {
		return nil, fmt.Errorf("%w: rendering needs `composewright init` first", workspace.ErrNotInitialised)
	}
	observer.step(StepRender, StateRunning, "")
	regeneration, err := sameSource(ctx, dependencies.Workspace.Store, options, result)
	if err != nil {
		return nil, observer.failed(StepRender, err)
	}
	if regeneration {
		if err := keepDecisions(ctx, dependencies.Workspace.Store, result.Plan); err != nil {
			return nil, observer.failed(StepRender, err)
		}
	}
	known, err := knownValues(ctx, dependencies, result.Plan.App.Name, rules.OwnSecrets(result.Facts, result.Source))
	if err != nil {
		return nil, observer.failed(StepRender, err)
	}
	grantRequested(&options, &input)
	if err := renderAndGate(ctx, dependencies, options, result, input, known); err != nil {
		return nil, observer.failed(StepRender, err)
	}
	if result.Files.Config, err = configContents(source, result.Plan); err != nil {
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

var (
	// ErrNameTaken means another source owns the project name already.
	ErrNameTaken = errors.New("project name taken")
	// ErrSourceMoved means generating a project again read another source
	// than the one it came from.
	ErrSourceMoved = errors.New("the project's source is no longer where it was")
)

// sameSource keeps a second repository from taking over a project, and its
// stored secrets, only because both derive the same name. A name given on
// purpose moves the project to the new source, with a notice saying so.
// Uploads are not compared: each is a new temporary folder. regeneration
// is true for an existing project from the same source, or an upload.
func sameSource(ctx context.Context, database *store.Store, options Options, result *Result) (regeneration bool, err error) {
	name := result.Plan.App.Name
	existing, err := database.FindProject(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	moved := existing.SourceURL != "" && existing.SourceURL != result.Source.URL
	if !moved || options.Uploaded {
		return true, nil
	}
	if options.Regenerating {
		return false, fmt.Errorf("%w: %s came from %s, but %s was read", ErrSourceMoved, name, existing.SourceURL, result.Source.URL)
	}
	if options.Name == "" {
		return false, fmt.Errorf("%w: %s comes from %s; give this one another name (--name, or Project name on the New page)", ErrNameTaken, name, existing.SourceURL)
	}
	result.Notices = append(result.Notices, fmt.Sprintf("%s came from %s and now comes from %s; its stored secrets were kept",
		name, existing.SourceURL, result.Source.URL))
	return false, nil
}

// keepDecisions carries onto a regenerated plan what a person decided on
// the project's latest version: a value they set for a variable the plan
// still has, also one of a service switched off since, and a variable they
// added, which comes back when the plan lacks it. render.Values keeps the
// secrets they typed in, by key.
func keepDecisions(ctx context.Context, database *store.Store, plan *ir.IR) error {
	_, generation, found, err := latestGeneration(ctx, database, plan.App.Name)
	if err != nil || !found {
		return err
	}
	stored, err := storedVariables(ctx, database, plan.App.Name)
	if err != nil {
		return err
	}
	// A stored plan that no longer loads keeps nothing: a regeneration is
	// how such a version is mended.
	previous, decodeErr := ir.Decode([]byte(generation.IRJSON))
	if decodeErr != nil {
		previous = &ir.IR{}
	}
	for _, entry := range userDecisions(previous, stored) {
		current, inPlan := plan.EnvEntry(entry.Key)
		if inPlan && Editable(*current) {
			current.Value, current.Provenance = entry.Value, entry.Provenance
		}
		if inPlan || !slices.Contains(addedDescriptions, entry.Description) {
			continue
		}
		// A plan without an app service has nowhere to put it.
		if addVariable(plan, Variable{Key: entry.Key, Value: entry.Value}, entry.Provenance) == nil {
			added, _ := plan.EnvEntry(entry.Key)
			added.Description = entry.Description
		}
	}
	return nil
}

// userDecisions are the variables a person set on the previous plan, and
// those they set for a service that is off, which only the stored .env
// records still hold.
func userDecisions(previous *ir.IR, stored []store.EnvVar) []ir.EnvFileEntry {
	decisions := slices.DeleteFunc(slices.Clone(previous.EnvFile), func(entry ir.EnvFileEntry) bool {
		return entry.Provenance.Source != ir.SourceUser
	})
	for _, variable := range stored {
		_, inPrevious := previous.EnvEntry(variable.Key)
		if inPrevious || variable.Source != ir.SourceUser {
			continue
		}
		decisions = append(decisions, ir.EnvFileEntry{Key: variable.Key, Value: variable.Value, Description: variable.Description,
			Provenance: ir.Provenance{Source: variable.Source, SourceRef: variable.SourceRef, Confidence: ir.ConfidenceHigh}})
	}
	return decisions
}

// plan builds the plan with the chosen engine, verifies the images and runs
// the policy gate, reporting each stage. options.EnabledOptional becomes the
// optional services switched on, as the plan names them.
func plan(ctx context.Context, dependencies Dependencies, options *Options, result *Result) (policy.Input, error) {
	observer := dependencies.Observer
	observer.step(StepPlan, StateRunning, "")
	if err := draft(ctx, dependencies, options, result); err != nil {
		return policy.Input{}, err
	}
	if err := keepChosenTags(ctx, dependencies.Workspace, options, result); err != nil {
		return policy.Input{}, err
	}
	observer.step(StepPlan, StateDone, "")
	input := policyInput(ctx, dependencies, result.Plan, result.Source, *options)
	imagesState, imagesNote := StateRunning, ""
	if dependencies.Inspector == nil {
		imagesState, imagesNote = StateSkipped, "offline: images are not verified or pinned"
	}
	observer.step(StepImages, imagesState, imagesNote)
	result.Inspected, result.Checks = verifyImages(ctx, dependencies.Inspector, result.Plan, input, nil)
	rules.ApplyImages(result.Plan, result.Inspected, result.Source.URL)
	if dependencies.Inspector != nil {
		observer.step(StepImages, StateDone, "")
	}
	fill(ctx, dependencies, *options, result)
	for _, key := range setHostPorts(result.Plan, options.Ports, chosenPort) {
		result.Notices = append(result.Notices, fmt.Sprintf(unmatchedPortNote, key))
	}
	observer.step(StepPolicy, StateRunning, "")
	input.Plan, input.Images = result.Plan, result.Checks
	result.Report = policy.Evaluate(input)
	observer.step(StepPolicy, StateDone, "")
	return input, nil
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

// offlineFetchProblem and unsupportedURLProblem say why a repository URL
// is not read.
const (
	offlineFetchProblem   = "fetching from %s needs network access through netguard"
	unsupportedURLProblem = "%s: only https://<host>/<owner>/<repository> URLs of a GitHub, GitLab or Gitea host in scm.allow_hosts (%s), " +
		"local directories and .zip or .tar.gz archives are supported"
)

// openSource fetches a repository from its code host as a tarball, or uses
// a local directory as it is, or unpacks a local .zip or .tar.gz.
func openSource(ctx context.Context, dependencies Dependencies, options Options) (*fetch.Source, error) {
	tree, isTree, err := fetch.ParseGitHubLocation(options.Source)
	if err != nil {
		return nil, err
	}
	if isTree {
		if dependencies.Guard == nil {
			return nil, fmt.Errorf(offlineFetchProblem, fetch.GitHubHost)
		}
		var pinned *string
		if options.Regenerating {
			pinned = &options.SourceRoot
		}
		return fetch.GitHubSubproject(ctx, dependencies.Guard, dependencies.Tokens[fetch.GitHubHost], tree, options.Ref, pinned)
	}
	location, isRemote := fetch.ParseRepository(options.Source)
	if isRemote && dependencies.Guard == nil {
		return nil, fmt.Errorf(offlineFetchProblem, location.Host)
	}
	if isRemote {
		return fetch.Remote(ctx, dependencies.Guard, dependencies.Tokens[location.Host], location, options.Ref)
	}
	if remoteURL(options.Source) {
		return nil, fmt.Errorf(unsupportedURLProblem, options.Source, strings.Join(dependencies.Config.SCM.AllowHosts, ", "))
	}
	source, err := fetch.Local(options.Source, options.Upstream)
	if err == nil && options.Uploaded {
		source.Repository = ""
	}
	return source, err
}

func remoteURL(source string) bool {
	return strings.Contains(source, "://") || strings.HasPrefix(source, "git@")
}

// absolutePaths stores a local source and the profile file as absolute
// paths, so generating again from another working directory, as the web
// server does, reads the same files; the compose file, a path in the
// repository, it spells as the facts do.
func absolutePaths(options *Options) error {
	var err error
	if options.ComposeFile != "" {
		options.ComposeFile = path.Clean(filepath.ToSlash(options.ComposeFile))
	}
	if options.ProfileFile != "" {
		if options.ProfileFile, err = filepath.Abs(options.ProfileFile); err != nil {
			return err
		}
	}
	_, _, isGitHub := fetch.ParseGitHub(options.Source)
	_, isTree, treeErr := fetch.ParseGitHubLocation(options.Source)
	if treeErr != nil {
		return treeErr
	}
	if isTree || isGitHub || remoteURL(options.Source) {
		return nil
	}
	options.Source, err = filepath.Abs(options.Source)
	return err
}

// verifyImages asks the registry about every image the policy would allow
// (images outside the allowlist are never contacted). A missing image
// becomes a deny; an unreachable registry only a warning. Services in
// retagged are read past the cache: their tag is new to this version.
func verifyImages(ctx context.Context, inspector *images.Inspector, plan *ir.IR, input policy.Input, retagged map[string]string) (map[string]images.Metadata, map[string]policy.ImageCheck) {
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
		inspect := inspector.Inspect
		if _, found := retagged[service.Name]; found {
			inspect = inspector.Fresh
		}
		metadata, err := inspect(ctx, image)
		switch {
		case errors.Is(err, images.ErrNotFound):
			checks[key] = policy.ImageCheck{Found: false}
		case err != nil && metadata.Digest != "":
			inspected[key] = metadata
			checks[key] = policy.ImageCheck{Found: true, Message: "digest pinned, but the image config could not be read: " + err.Error(), Platforms: metadata.Platforms}
		case err != nil:
			checks[key] = policy.ImageCheck{Found: true, Message: err.Error()}
		default:
			inspected[key] = metadata
			checks[key] = policy.ImageCheck{Found: true, Platforms: metadata.Platforms}
		}
	}
	return inspected, checks
}

func policyInput(ctx context.Context, dependencies Dependencies, plan *ir.IR, source *fetch.Source, options Options) policy.Input {
	configuration := dependencies.Config
	return policy.Input{Plan: plan, Profile: configuration.Profile, AllowRegistries: configuration.Registries.Allow,
		ExtraRegistries: options.AllowRegistries, RepositoryOwner: source.Owner, GrantedNeeds: options.AllowNeeds,
		TakenPorts: takenPorts(ctx, dependencies, plan.App.Name)}
}

// renderAndGate runs the plan gate, renders the files and runs the YAML gate
// on the compose model compose-go reads back from them. known are the values
// already chosen, by variable: nothing is read from or written to the store
// here, so a failure leaves the project as it was.
func renderAndGate(ctx context.Context, dependencies Dependencies, options Options, result *Result, input policy.Input, known map[string]string) error {
	result.Report = policy.Evaluate(input)
	profile, err := loadProfile(dependencies.Config.Profile, options.ProfileFile)
	if err != nil {
		return err
	}
	renderOptions, err := render.Options{Profile: profile, ExposeVia: options.ExposeVia, Networks: options.Networks, PublicHost: options.PublicHost,
		Storage: options.Storage, GrantedNeeds: options.AllowNeeds, Header: header(dependencies.Version, result.Source, result.Plan),
		CheckedPlatform: checkedPlatform(dependencies.Config.Profile.Platform, result.Plan, result.Checks)}.Resolve()
	if err != nil {
		return err
	}
	result.Render = renderOptions
	if result.Values, err = render.Values(result.Plan, renderOptions, known); err != nil {
		return err
	}
	if result.Files.Compose, err = render.Compose(result.Plan, renderOptions); err != nil {
		return err
	}
	if result.Files.Env, err = render.Env(result.Plan, result.Values, envHeader(result.Plan)); err != nil {
		return err
	}
	result.Files.Deploy = render.Deploy(result.Plan, renderOptions, notValidated)
	project, err := composefile.Load(ctx, composeFileName, result.Files.Compose, nil, false)
	if err != nil {
		return fmt.Errorf("the rendered compose.yaml does not load: %w", err)
	}
	input.Profile = profile
	result.Report = result.Report.Merge(gateYAML(ctx, project, result.Files.Compose, result.Files.Env, secretValues(result.Plan, result.Values), input))
	return nil
}

// checkedPlatform is the profile's platform when the registry listed it for
// every image, so DEPLOY.md can say the stack was verified for it.
func checkedPlatform(platform string, plan *ir.IR, checks map[string]policy.ImageCheck) string {
	if platform == "" {
		return ""
	}
	for _, service := range plan.Services {
		if !policy.PlatformListed(checks[rules.ImageKey(service.Image)].Platforms, platform) {
			return ""
		}
	}
	return platform
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
// knownValues are a project's stored values with own, secrets from the
// person's own .env given only on a first generation, over them: later runs
// read stored, redacted facts, whose masks must never become values.
func knownValues(ctx context.Context, dependencies Dependencies, name string, own map[string]string) (map[string]string, error) {
	known, err := storedValues(ctx, dependencies.Workspace.Store, name)
	if err != nil {
		return nil, err
	}
	maps.Copy(known, own)
	return known, nil
}

func storedValues(ctx context.Context, database *store.Store, name string) (map[string]string, error) {
	variables, err := storedVariables(ctx, database, name)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(variables))
	for _, variable := range variables {
		values[variable.Key] = variable.Value
	}
	return values, nil
}

// storedVariables are the .env records kept for an existing project.
func storedVariables(ctx context.Context, database *store.Store, name string) ([]store.EnvVar, error) {
	project, err := database.FindProject(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return database.EnvVars(ctx, project.ID)
}

// keptVariables are the stored .env records a new version may keep for keys
// its plan lacks. An editor save drops those the edited .env showed, as the
// person removed them; the others belong to a service that is off, which the
// editor never showed.
func keptVariables(ctx context.Context, database *store.Store, name, action string) ([]store.EnvVar, error) {
	variables, err := storedVariables(ctx, database, name)
	if err != nil || action != store.ActionEdit {
		return variables, err
	}
	_, generation, found, err := latestGeneration(ctx, database, name)
	if err != nil || !found {
		return variables, err
	}
	shown, err := ir.Decode([]byte(generation.IRJSON))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(variables, func(variable store.EnvVar) bool {
		_, wasShown := shown.EnvEntry(variable.Key)
		return wasShown
	}), nil
}

// persist stores the result as a new generation, audited as action, in one
// transaction with its project, images and .env.
func persist(ctx context.Context, dependencies Dependencies, options Options, result *Result, iteration int, action string) error {
	status := store.StatusGenerated
	if result.Report.Blocked() {
		status = store.StatusBlocked
	}
	source := result.Source
	project := store.Project{Name: result.Plan.App.Name, Slug: result.Plan.App.Name, SourceKind: source.Kind,
		SourceURL: source.URL, SourceRef: source.Ref, SourceCommit: source.Commit, SourceOwner: source.Owner, Status: status}
	events := func(_ store.Project, generation store.Generation) []store.Event {
		recorded := []store.Event{{Action: action, Details: map[string]any{"generation": generation.ID, "engine": result.Engine,
			"model": result.Call.Model, "source": source.URL, "commit": source.Commit, "status": status,
			"denied": result.Report.Count(policy.Deny), "needs": result.Report.Count(policy.Need)}}}
		for _, capability := range options.AllowNeeds {
			recorded = append(recorded, store.Event{Action: store.ActionGrantNeed, Details: map[string]any{"capability": capability, "generation": generation.ID}})
		}
		return recorded
	}
	stored, err := keptVariables(ctx, dependencies.Workspace.Store, project.Name, action)
	if err != nil {
		return err
	}
	saved, generation, err := dependencies.Workspace.Store.RecordGeneration(ctx, project, generationRecord(options, result, iteration),
		imageRecords(result.Plan, result.Inspected), envRecords(result.Plan, result.Values, stored), dependencies.Workspace.Actor, events)
	if err != nil {
		return err
	}
	result.Project, result.Generation = saved, generation
	return nil
}

func generationRecord(options Options, result *Result, iteration int) store.Generation {
	plan, _ := json.Marshal(result.Plan)
	settings, _ := json.Marshal(options)
	report, _ := json.Marshal(result.Report)
	return store.Generation{Iteration: iteration, Engine: result.Engine, Provider: result.Call.Provider,
		Model: result.Call.Model, PromptHash: result.Call.PromptHash, IRJSON: string(plan),
		ComposeYAML: string(result.Files.Compose), DeployMD: untestedDeploy(string(result.Files.Deploy), result.Generation, options.Edited),
		OptionsJSON: string(settings), PolicyJSON: string(report),
		FactsJSON: factsJSON(result.Facts), Files: result.Files.Config, Base: result.Generation.ID}
}

// configContents reads the repository's files the plan bundles, which
// extraction found within the size it keeps of such a file.
func configContents(source *fetch.Source, plan *ir.IR) (map[string][]byte, error) {
	if len(plan.ConfigFiles) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(source.Directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	contents := map[string][]byte{}
	for _, file := range plan.ConfigFiles {
		content, err := root.ReadFile(filepath.FromSlash(file.Source))
		if err == nil && int64(len(content)) > fetch.Limit(fetch.Config) {
			err = fetch.ErrTooLarge
		}
		if err != nil {
			return nil, fmt.Errorf("bundle %s: %w", file.Source, err)
		}
		contents[file.Path] = content
	}
	return contents, nil
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
			Registry: service.Image.Registry, OCISource: metadata.Source, VerifiedAt: metadata.FetchedAt, Labels: metadata.Labels, Created: metadata.Created})
	}
	return records
}

// envRecords are the .env records of plan with values, followed by the
// stored secrets and values a person set of keys the plan no longer has: a
// service switched off keeps them for when it is switched on again.
func envRecords(plan *ir.IR, values map[string]string, stored []store.EnvVar) []store.EnvVar {
	variables := make([]store.EnvVar, 0, len(plan.EnvFile))
	for _, entry := range plan.EnvFile {
		variables = append(variables, store.EnvVar{Key: entry.Key, Value: values[entry.Key], Secret: entry.Secret, Required: entry.Required,
			Source: entry.Provenance.Source, SourceRef: entry.Provenance.SourceRef, Description: entry.Description})
	}
	for _, variable := range stored {
		_, inPlan := plan.EnvEntry(variable.Key)
		if !inPlan && keptWhileOff(variable) {
			variables = append(variables, variable)
		}
	}
	return variables
}

// keptWhileOff reports whether a stored record outlives its key leaving the
// plan: a secret, or a value a person set.
func keptWhileOff(variable store.EnvVar) bool {
	return variable.Secret || variable.Source == ir.SourceUser
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
