package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
)

// Preflight checks the complete archive against this install before any key,
// settings file or project is written. Legacy format-one archives stay valid
// when their plans match the supported schema; unknown shapes are refused.
func Preflight(archive Archive, configuration config.Config) error {
	if archive.Snapshot != "" && archive.Snapshot != snapshotFormat {
		return fmt.Errorf("backup snapshot format is not supported")
	}
	if err := archive.problem(); err != nil {
		return err
	}
	if err := archive.Settings.Apply(configuration).Validate(); err != nil {
		return fmt.Errorf("backup settings do not fit this install: %w", err)
	}
	if len(rules.LoadCatalogue(archive.Catalogue).Problems) > 0 {
		return fmt.Errorf("backup catalogue cannot be loaded")
	}
	for _, record := range archive.Projects {
		if err := store.ValidateProjectOrganization(record.Project); err != nil {
			return fmt.Errorf("project %s has invalid organization metadata: %w", record.Project.Name, err)
		}
		if len(record.Generations) == 0 {
			return fmt.Errorf("project %s has no generations", record.Project.Name)
		}
		numbers := map[int]bool{}
		variables := map[string]bool{}
		for _, variable := range record.Variables {
			if variables[variable.Key] {
				return fmt.Errorf("project %s has duplicate variables", record.Project.Name)
			}
			variables[variable.Key] = true
		}
		for _, kept := range record.Generations {
			g := kept.Generation
			if g.Number < 1 || numbers[g.Number] {
				return fmt.Errorf("project %s has invalid version numbers", record.Project.Name)
			}
			numbers[g.Number] = true
			plan, err := ir.Decode([]byte(g.IRJSON))
			if err != nil {
				return fmt.Errorf("project %s version %d has an incompatible plan", record.Project.Name, g.Number)
			}
			if err = ir.ValidateSchema(plan); err != nil {
				return fmt.Errorf("project %s version %d has an incompatible plan", record.Project.Name, g.Number)
			}
			if _, err := composefile.Load(context.Background(), "compose.yaml", []byte(g.ComposeYAML), nil, false); err != nil {
				return fmt.Errorf("project %s version %d has invalid Compose YAML", record.Project.Name, g.Number)
			}
			var options pipeline.Options
			if err = json.Unmarshal([]byte(g.OptionsJSON), &options); err != nil {
				return fmt.Errorf("project %s has incompatible options", record.Project.Name)
			}
			var report policy.Report
			if err = json.Unmarshal([]byte(g.PolicyJSON), &report); err != nil {
				return fmt.Errorf("project %s has incompatible policy data", record.Project.Name)
			}
			declared := map[string]bool{}
			for _, file := range plan.ConfigFiles {
				declared[file.Path] = true
				if _, found := g.Files[file.Path]; !found {
					return fmt.Errorf("project %s is missing a bundled file", record.Project.Name)
				}
			}
			for name := range g.Files {
				if !filepath.IsLocal(name) || !declared[name] {
					return fmt.Errorf("project %s has an unsafe bundled path", record.Project.Name)
				}
			}
		}
	}
	return nil
}
