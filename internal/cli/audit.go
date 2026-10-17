package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/aiulian25/composewright/internal/store"
)

type auditVerification struct {
	Intact  bool `json:"intact"`
	Entries int  `json:"entries"`
}

func (application *app) auditCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "audit",
		Short: "Check the audit log",
		Long: `Every action is recorded in an audit log whose entries each carry the hash of
the one before, so editing, reordering or removing an entry other than the
newest breaks the chain. It does not stop someone who can write the database
from rebuilding the whole chain.`,
	}
	command.AddCommand(&cobra.Command{
		Use:   "verify",
		Short: "Check that no audit entry was edited, reordered or removed",
		Long: `Recompute the audit log's hash chain from its first entry and report how many
entries it holds. A broken chain names the first entry that does not match and
exits with a non-zero status. doctor runs the same check.`,
		Example: "  composewright audit verify\n  composewright audit verify --json",
		Args:    cobra.NoArgs,
		RunE:    func(command *cobra.Command, _ []string) error { return application.runAuditVerify(command.Context()) },
	})
	var filter store.AuditFilter
	var project string
	list := &cobra.Command{
		Use:   "list",
		Short: "List audit entries, newest first",
		Long: `List every audit entry, newest first: when, who, what and the details
recorded with it. Details may name a secret but never hold its value.
--project takes a project's name, or the ID of one that was deleted.`,
		Example: "  composewright audit list\n  composewright audit list --action reveal\n  composewright audit list --project miniflux --json",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return application.runAuditList(command.Context(), filter, project)
		},
	}
	list.Flags().StringVar(&filter.Action, "action", "", "only entries of this action, such as reveal or settings")
	list.Flags().StringVar(&project, "project", "", "only entries of this project")
	command.AddCommand(list)
	return command
}

func (application *app) runAuditList(ctx context.Context, filter store.AuditFilter, project string) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	if project != "" {
		found, err := workspace.Store.FindProject(ctx, project)
		switch {
		case err == nil:
			filter.ProjectID = found.ID
		case errors.Is(err, store.ErrNotFound):
			filter.ProjectID = project
		default:
			return err
		}
	}
	entries, err := workspace.Store.AuditPage(ctx, filter)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, entries)
	}
	projects, err := workspace.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, summary := range projects {
		names[summary.ID] = summary.Name
	}
	rows := [][]string{{"SEQ", "WHEN", "WHO", "ACTION", "PROJECT", "DETAILS"}}
	for _, entry := range entries {
		details, err := json.Marshal(entry.Details)
		if err != nil {
			return err
		}
		projectName := entry.ProjectID
		if name, known := names[entry.ProjectID]; known {
			projectName = name
		}
		rows = append(rows, []string{strconv.FormatInt(entry.Sequence, 10), entry.Time.Local().Format(timeLayout),
			sanitize(entry.Actor), entry.Action, sanitize(projectName), sanitize(string(details))})
	}
	return table(application.stdout, rows...)
}

func (application *app) runAuditVerify(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	count, err := workspace.Store.VerifyAudit(ctx)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, auditVerification{Intact: true, Entries: count})
	}
	noun := "entries"
	if count == 1 {
		noun = "entry"
	}
	fmt.Fprintf(application.stdout, "audit log intact: %d %s\n", count, noun)
	return nil
}
