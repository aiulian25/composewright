package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

const timeLayout = "2006-01-02 15:04"

func (application *app) listCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List projects with their status and images",
		Long: `List every stored project: its status (generated, blocked, passed, failed,
needs_human), how many generations it has, the engine of the latest one and
its images.`,
		Example: "  composewright list\n  composewright list --json",
		Args:    cobra.NoArgs,
		RunE:    func(command *cobra.Command, _ []string) error { return application.runList(command.Context()) },
	}
}

func (application *app) runList(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	projects, err := workspace.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, projects)
	}
	if len(projects) == 0 {
		fmt.Fprintln(application.stdout, "No projects yet. Try: composewright gen https://github.com/miniflux/v2")
		return nil
	}
	rows := [][]string{{"NAME", "STATUS", "GENERATIONS", "ENGINE", "SOURCE", "UPDATED"}}
	for _, project := range projects {
		rows = append(rows, []string{
			sanitize(project.Name), project.Status, strconv.Itoa(project.Generations), project.Engine,
			sanitize(project.SourceURL), project.UpdatedAt.Local().Format(timeLayout),
		})
	}
	return table(application.stdout, rows...)
}
