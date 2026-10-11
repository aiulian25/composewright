package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	reviewSource     = "review"
	addedDescription = "added on the Review page"
	referenceMarker  = "${"
)

var (
	ErrUnknownVariable = errors.New("the plan has no such variable")
	ErrNotEditable     = errors.New("this value cannot be changed here")
)

// Revision is what a person changes on the Review page before approving.
type Revision struct {
	ExposeVia       string
	Networks        []string
	Storage         string
	AllowNeeds      []string
	AllowRegistries []string
	// Values are new values for variables that carry no secret material.
	Values map[string]string
	// Secrets are values a person types in for secrets the plan cannot
	// generate. They are stored sealed and never enter the plan.
	Secrets map[string]string
	// Regenerate lists generated secrets to replace with fresh values.
	Regenerate []string
	// Add are new variables for the app service.
	Add []Variable
}

type Variable struct {
	Key   string
	Value string
}

// Revise applies a Review page revision to a project's latest generation,
// gates and renders it again, and stores it as a new generation. Nothing is
// fetched and no model is asked; images are checked again when the run can
// reach registries, so a namespace trusted here gets its digests pinned.
func Revise(ctx context.Context, dependencies Dependencies, name string, revision Revision) (*Result, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	result, err := stored.result()
	if err != nil {
		return nil, err
	}
	options := stored.Options
	options.Edited = false
	options.ExposeVia, options.Networks, options.Storage, options.AllowNeeds = revision.ExposeVia, revision.Networks, revision.Storage, revision.AllowNeeds
	options.AllowRegistries = revision.AllowRegistries
	if err := options.Check(); err != nil {
		return nil, err
	}
	if err := revision.apply(result.Plan); err != nil {
		return nil, err
	}
	values, err := revision.values(result.Plan, stored.Values)
	if err != nil {
		return nil, err
	}
	if err := dependencies.Workspace.Store.ReplaceEnvVars(ctx, stored.Project.ID, envRecords(result.Plan, values)); err != nil {
		return nil, err
	}
	input := policyInput(dependencies.Config, result.Plan, result.Source, options)
	if dependencies.Inspector != nil {
		result.Inspected, result.Checks = verifyImages(ctx, dependencies.Inspector, result.Plan, input)
		rules.ApplyImages(result.Plan, result.Inspected, result.Source.URL)
	}
	result.Plan.Unfilled = rules.Gaps(result.Plan)
	input.Plan, input.Images = result.Plan, result.Checks
	if err := renderAndGate(ctx, dependencies, options, result, input, nil); err != nil {
		return nil, err
	}
	return result, persist(ctx, dependencies, options, result, 1, store.ActionRevise)
}

// Editable reports whether a variable's value may be shown and changed as
// plain text: it holds no secret, or only refers to other variables, such
// as a connection string built from a generated password.
func Editable(entry ir.EnvFileEntry) bool {
	return !entry.Secret || (entry.Generate == "" && strings.Contains(entry.Value, referenceMarker))
}

// Supplied reports whether a secret must be typed in by a person: the plan
// can neither generate it nor build it from other variables.
func Supplied(entry ir.EnvFileEntry) bool {
	return entry.Secret && entry.Generate == "" && entry.Value == ""
}

func (revision Revision) apply(plan *ir.IR) error {
	edited := ir.Provenance{Source: ir.SourceUser, SourceRef: reviewSource, Confidence: ir.ConfidenceHigh}
	for key, value := range revision.Values {
		entry, found := plan.EnvEntry(key)
		switch {
		case !found:
			return fmt.Errorf("%w: %s", ErrUnknownVariable, key)
		case !Editable(*entry):
			return fmt.Errorf("%w: %s holds a secret", ErrNotEditable, key)
		case entry.Value != value:
			entry.Value, entry.Provenance = value, edited
		}
	}
	for _, variable := range revision.Add {
		if err := addVariable(plan, variable, edited); err != nil {
			return err
		}
	}
	return nil
}

func addVariable(plan *ir.IR, variable Variable, provenance ir.Provenance) error {
	if _, exists := plan.EnvEntry(variable.Key); exists {
		return fmt.Errorf("%s is already in the plan", variable.Key)
	}
	service := plan.AppService()
	if service == nil {
		return errors.New("the plan has no app service to add a variable to")
	}
	secret := redact.IsSecretName(variable.Key)
	entry := ir.EnvFileEntry{Key: variable.Key, Secret: secret, Required: true, Description: addedDescription, Provenance: provenance}
	if !secret {
		entry.Value = variable.Value
	}
	plan.EnvFile = append(plan.EnvFile, entry)
	service.Env = append(service.Env, ir.EnvRef{Key: variable.Key, Ref: variable.Key, Secret: secret, Required: true, Provenance: provenance})
	return nil
}

// values are the stored .env values after the revision: typed-in secrets
// replace the old ones, regenerated secrets are dropped so rendering makes
// new ones.
func (revision Revision) values(plan *ir.IR, stored map[string]string) (map[string]string, error) {
	values, supplied := map[string]string{}, map[string]string{}
	maps.Copy(values, stored)
	maps.Copy(supplied, revision.Secrets)
	for _, variable := range revision.Add {
		if redact.IsSecretName(variable.Key) {
			supplied[variable.Key] = variable.Value
		}
	}
	for key, value := range supplied {
		entry, found := plan.EnvEntry(key)
		if !found || !Supplied(*entry) {
			return nil, fmt.Errorf("%w: %s is not a secret you supply", ErrNotEditable, key)
		}
		values[key] = value
	}
	for _, key := range revision.Regenerate {
		entry, found := plan.EnvEntry(key)
		if !found || entry.Generate == "" {
			return nil, fmt.Errorf("%w: %s is not a generated secret", ErrNotEditable, key)
		}
		delete(values, key)
	}
	return values, nil
}
