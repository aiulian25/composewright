package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

// Bundle is the stored project as files: compose.yaml, the .env with its
// secrets, a secret-free .env.example and DEPLOY.md.
func (stored *Stored) Bundle() (export.Bundle, error) {
	env, err := stored.EnvFile(Revealed)
	if err != nil {
		return export.Bundle{}, err
	}
	example, err := stored.EnvExample()
	if err != nil {
		return export.Bundle{}, err
	}
	return export.Bundle{Project: stored.Project.Name, Compose: []byte(stored.Generation.ComposeYAML), Env: env,
		EnvExample: example, Deploy: []byte(stored.Generation.DeployMD)}, nil
}

// Export writes a project's files to target (a folder, or arcane:DIR) and
// audits where they went and how.
func Export(ctx context.Context, opened *workspace.Workspace, stored *Stored, target string, overwrite bool, via string) (export.Written, error) {
	bundle, err := stored.Bundle()
	if err != nil {
		return export.Written{}, err
	}
	written, err := export.Write(target, opened.Config.Profile, bundle, overwrite)
	if err != nil {
		return written, err
	}
	details := map[string]any{"destination": written.Directory, "contains_secrets": written.ContainsSecret, "generation": stored.Generation.ID, "via": via}
	return written, opened.Store.Audit(ctx, opened.Actor, store.ActionExport, stored.Project.ID, details)
}

// Remove deletes a project, its generations and every sealed secret, and
// its working files under the data directory; exported copies stay.
func Remove(ctx context.Context, opened *workspace.Workspace, name string, details map[string]any) (store.Project, error) {
	project, err := opened.Store.FindProject(ctx, name)
	if err != nil {
		return store.Project{}, err
	}
	if err := opened.Store.DeleteProject(ctx, project.ID); err != nil {
		return project, err
	}
	if err := os.RemoveAll(filepath.Join(opened.Paths.ProjectsDir(), project.Slug)); err != nil {
		return project, err
	}
	audited := map[string]any{"name": project.Name}
	for key, value := range details {
		audited[key] = value
	}
	return project, opened.Store.Audit(ctx, opened.Actor, store.ActionDelete, project.ID, audited)
}
