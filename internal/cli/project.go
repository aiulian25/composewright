package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	cliMask     = "<hidden: composewright show --reveal>"
	fileCompose = "compose"
	fileEnv     = "env"
	fileDeploy  = "deploy"
	filePlan    = "plan"
)

func (application *app) showCommand() *cobra.Command {
	var reveal bool
	var file string
	command := &cobra.Command{
		Use:   "show <project>",
		Short: "Print a project's compose.yaml, .env, DEPLOY.md or plan",
		Long: `Print the files of a project's latest generation. Secret values in .env are
hidden unless you pass --reveal; every reveal is written to the audit log.`,
		Example: `  composewright show miniflux
  composewright show miniflux --file plan
  composewright show miniflux --file env --reveal`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runShow(command.Context(), arguments[0], file, reveal)
		},
	}
	command.Flags().BoolVar(&reveal, "reveal", false, "print secret values (audited)")
	command.Flags().StringVar(&file, "file", "", "only this file: compose, env, deploy or plan")
	return command
}

func (application *app) runShow(ctx context.Context, name, file string, reveal bool) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	stored, err := pipeline.Load(ctx, workspace, name)
	if err != nil {
		return err
	}
	if reveal {
		if err := workspace.Store.Audit(ctx, workspace.Actor, store.ActionReveal, stored.Project.ID, map[string]any{"generation": stored.Generation.ID}); err != nil {
			return err
		}
	}
	mask := cliMask
	if reveal {
		mask = pipeline.Revealed
	}
	env, err := stored.EnvFile(mask)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"project": stored.Project, "generation": stored.Generation.ID, "plan": stored.Plan,
			"engine": stored.Generation.Engine, "provider": stored.Generation.Provider, "model": stored.Generation.Model,
			"policy": stored.Report, "compose": stored.Generation.ComposeYAML, "env": string(env), "deploy": stored.Generation.DeployMD})
	}
	switch file {
	case fileCompose:
		_, err = fmt.Fprint(application.stdout, stored.Generation.ComposeYAML)
	case fileEnv:
		_, err = application.stdout.Write(env)
	case fileDeploy:
		_, err = fmt.Fprint(application.stdout, stored.Generation.DeployMD)
	case filePlan:
		err = errors.Join(application.printPlan(application.stdout, stored.Plan), application.printPolicy(application.stdout, stored.Report))
	case "":
		fmt.Fprintf(application.stdout, "%s · status %s · generation %s · engine %s\n\n", stored.Project.Name, stored.Project.Status,
			short(stored.Generation.ID), sanitize(strings.TrimSpace(stored.Generation.Engine+" "+stored.Generation.Model)))
		fmt.Fprintln(application.stdout, application.colours.muted("── compose.yaml"))
		fmt.Fprint(application.stdout, stored.Generation.ComposeYAML)
		fmt.Fprintln(application.stdout, application.colours.muted("── .env"))
		_, err = application.stdout.Write(env)
	default:
		err = fmt.Errorf("--file %q must be compose, env, deploy or plan", file)
	}
	return err
}

func (application *app) rmCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <project>",
		Short: "Delete a project and wipe its secrets",
		Long: `Delete a project, all its generations and every encrypted secret, and remove
its working files under the data directory. Files you exported elsewhere are
not touched. The deletion is recorded in the audit log.`,
		Example: "  composewright rm miniflux\n  composewright rm miniflux --yes",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runRemove(command.Context(), arguments[0])
		},
	}
}

func (application *app) runRemove(ctx context.Context, name string) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	project, err := workspace.Store.FindProject(ctx, name)
	if err != nil {
		return err
	}
	confirmed, err := application.confirm(fmt.Sprintf("Delete project %s and its encrypted secrets?", project.Name))
	if err != nil {
		return err
	}
	if !confirmed {
		return errors.New("nothing deleted")
	}
	if _, err := pipeline.Remove(ctx, workspace, project.Name, map[string]any{"confirmed_with_yes_flag": application.global.yes}); err != nil {
		return err
	}
	if !application.global.json {
		fmt.Fprintf(application.stdout, "deleted %s\n", project.Name)
	}
	return nil
}

func (application *app) exportCommand() *cobra.Command {
	var target string
	var overwrite bool
	command := &cobra.Command{
		Use:   "export <project>",
		Short: "Write compose.yaml, .env and DEPLOY.md to a folder or Arcane",
		Long: `Write the latest generation to disk.

  --to DIR          compose.yaml, .env (mode 0600, contains secrets) and DEPLOY.md
  --to arcane:DIR   an Arcane template: DIR/<project>/compose.yaml and a
                    .env.example without secrets (DIR is Arcane's templates folder)

Without --to, the folder is profile.storage.save_dir (default {base}/{project}).
For an Arcane project, export to <PROJECTS_DIRECTORY>/<project> as a plain folder.
Existing files are kept unless you pass --overwrite. Every export is audited.`,
		Example: `  composewright export miniflux --to ./miniflux
  composewright export miniflux --to arcane:/srv/arcane/data/templates
  composewright export miniflux --overwrite`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return application.runExport(command.Context(), arguments[0], target, overwrite)
		},
	}
	command.Flags().StringVar(&target, "to", "", "destination folder, or arcane:<templates folder>")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that already exist")
	return command
}

func (application *app) runExport(ctx context.Context, name, target string, overwrite bool) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	stored, err := pipeline.Load(ctx, workspace, name)
	if err != nil {
		return err
	}
	written, err := pipeline.Export(ctx, workspace, stored, target, overwrite, "cli")
	if err != nil {
		return err
	}
	return application.reportExport(written)
}

func (application *app) reportExport(written export.Written) error {
	if application.global.json {
		return writeJSON(application.stdout, written)
	}
	for _, file := range written.Files {
		fmt.Fprintln(application.stdout, "wrote", file)
	}
	if written.ContainsSecret {
		fmt.Fprintln(application.stdout, application.colours.warn(".env holds secrets in plain text (mode 0600); keep it out of version control"))
	}
	return nil
}
