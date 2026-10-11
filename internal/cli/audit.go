package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
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
	return command
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
