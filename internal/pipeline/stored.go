package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/images"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/llm"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

// Stored is a project's latest generation with its decrypted .env values.
type Stored struct {
	Project    store.Project
	Generation store.Generation
	Facts      *extract.Facts
	Plan       *ir.IR
	Options    Options
	Report     policy.Report
	Values     map[string]string
	Images     []store.Image
}

func Load(ctx context.Context, workspace *workspace.Workspace, name string) (*Stored, error) {
	database := workspace.Store
	project, err := database.FindProject(ctx, name)
	if err != nil {
		return nil, err
	}
	generation, err := database.LatestGeneration(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	plan, err := ir.Decode([]byte(generation.IRJSON))
	if err != nil {
		return nil, fmt.Errorf("stored plan of %s: %w", name, err)
	}
	stored := &Stored{Project: project, Generation: generation, Plan: plan, Values: map[string]string{}}
	if err := json.Unmarshal([]byte(generation.OptionsJSON), &stored.Options); err != nil {
		return nil, err
	}
	if generation.FactsJSON != "" {
		stored.Facts = &extract.Facts{}
		if err := json.Unmarshal([]byte(generation.FactsJSON), stored.Facts); err != nil {
			return nil, fmt.Errorf("stored facts of %s: %w", name, err)
		}
	}
	if generation.PolicyJSON != "" {
		if err := json.Unmarshal([]byte(generation.PolicyJSON), &stored.Report); err != nil {
			return nil, err
		}
	}
	variables, err := database.EnvVars(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	for _, variable := range variables {
		stored.Values[variable.Key] = variable.Value
	}
	stored.Images, err = database.Images(ctx, generation.ID)
	return stored, err
}

// EnvFile renders the stored .env. A non-empty mask replaces every value
// that holds secret material, so the output is safe to show; values that
// only refer to other variables stay readable. An empty mask reveals all.
func (stored *Stored) EnvFile(mask string) ([]byte, error) {
	values := map[string]string{}
	for key, value := range stored.Values {
		values[key] = value
	}
	for _, key := range stored.MaskedKeys() {
		if mask != "" {
			values[key] = mask
		}
	}
	return render.Env(stored.Plan, values, envHeader(stored.Plan))
}

// MaskedKeys are the variables a mask hides: secrets with a value that a
// person cannot read or change in the plan.
func (stored *Stored) MaskedKeys() []string {
	var keys []string
	for _, entry := range stored.Plan.EnvFile {
		if stored.Values[entry.Key] != "" && !Editable(entry) {
			keys = append(keys, entry.Key)
		}
	}
	return keys
}

// EnvExample renders a shareable .env: secrets are left empty with a note.
func (stored *Stored) EnvExample() ([]byte, error) {
	values := map[string]string{}
	for key, value := range stored.Values {
		values[key] = value
	}
	for _, entry := range stored.Plan.EnvFile {
		if entry.Secret {
			values[entry.Key] = ""
		}
	}
	return render.Env(stored.Plan, values, "# "+stored.Plan.App.Name+" settings; fill in the secrets before starting\n")
}

// result rebuilds what a pipeline run knows from a stored generation, so it
// can be revised or validated without fetching or asking a model again.
// Images verified at generation time count as found; unverified ones are
// left for the policy gate to warn about, as an offline run would.
// source is where the project came from, as the store recorded it.
func (stored *Stored) source() *fetch.Source {
	project := stored.Project
	return &fetch.Source{Kind: project.SourceKind, URL: project.SourceURL, Ref: project.SourceRef, Commit: project.SourceCommit, Owner: project.SourceOwner}
}

func (stored *Stored) result() (*Result, error) {
	env, err := stored.EnvFile(Revealed)
	if err != nil {
		return nil, err
	}
	project := stored.Project
	result := &Result{Source: stored.source(), Facts: stored.Facts, Plan: stored.Plan.Clone(), Report: stored.Report, Values: stored.Values,
		Files:   Files{Compose: []byte(stored.Generation.ComposeYAML), Env: env, Deploy: []byte(stored.Generation.DeployMD)},
		Project: project, Generation: stored.Generation, Engine: stored.Generation.Engine,
		Call: llm.Call{Provider: stored.Generation.Provider, Model: stored.Generation.Model, PromptHash: stored.Generation.PromptHash}}
	result.Inspected, result.Checks = stored.verifiedImages()
	return result, nil
}

// verifiedImages is what image verification found when this version was
// made, without opening any secret.
func (stored *Stored) verifiedImages() (map[string]images.Metadata, map[string]policy.ImageCheck) {
	inspected, checks := map[string]images.Metadata{}, map[string]policy.ImageCheck{}
	verified := map[string]store.Image{}
	for _, image := range stored.Images {
		verified[image.Service] = image
	}
	for _, service := range stored.Plan.Services {
		image, found := verified[service.Name]
		if !found || image.VerifiedAt.IsZero() {
			continue
		}
		key := rules.ImageKey(service.Image)
		inspected[key] = images.Metadata{Reference: image.Reference, Digest: image.Digest, Source: image.OCISource, FetchedAt: image.VerifiedAt}
		checks[key] = policy.ImageCheck{Found: true}
	}
	return inspected, checks
}

// Revealed is the mask that shows every value.
const Revealed = ""
