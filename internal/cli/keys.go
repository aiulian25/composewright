package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
)

func (application *app) keysCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "keys",
		Short: "Inspect and rotate the master key",
		Long: `The master key (key-encryption key) encrypts every stored secret with
XChaCha20-Poly1305. Each sealed value is bound to its project and variable
name, so values cannot be swapped between rows.`,
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "status",
			Short: "Show the current key, retired keys and what each protects",
			Long: `Show the id of the current master key, the retired keys still on disk, and how
many stored values each key protects. A pending rotation (a leftover
master.key.next) is reported so it can be finished with 'keys rotate'.`,
			Example: "  composewright keys status\n  composewright keys status --json",
			Args:    cobra.NoArgs,
			RunE:    func(command *cobra.Command, _ []string) error { return application.runKeysStatus(command.Context()) },
		},
		&cobra.Command{
			Use:   "rotate",
			Short: "Generate a new master key and re-encrypt every secret",
			Long: `Generate a new master key, re-encrypt every stored secret with it in a single
database transaction, then move the old key to keys/retired/. Nothing changes if
any value fails to re-encrypt. The old key stays on disk so an older backup of
the database can still be restored; delete it with 'keys purge' once backups
taken after the rotation exist.`,
			Example: "  composewright keys rotate",
			Args:    cobra.NoArgs,
			RunE:    func(command *cobra.Command, _ []string) error { return application.runKeysRotate(command.Context()) },
		},
		&cobra.Command{
			Use:   "purge",
			Short: "Delete retired keys that no stored value needs",
			Long: `Delete the files under keys/retired/. Refuses while any stored value is still
sealed with a retired key. Database backups taken before the last rotation can
no longer be decrypted afterwards.`,
			Example: "  composewright keys purge\n  composewright keys purge --yes",
			Args:    cobra.NoArgs,
			RunE:    func(command *cobra.Command, _ []string) error { return application.runKeysPurge(command.Context()) },
		},
	)
	return command
}

func (application *app) runKeysStatus(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	status, err := workspace.KeyStatus(ctx)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, status)
	}
	rows := [][]string{{"  current", status.CurrentKeyID, strconv.Itoa(status.Usage[status.CurrentKeyID]) + " sealed values", status.KeyFile}}
	for _, keyID := range status.Retired {
		rows = append(rows, []string{"  retired", keyID, strconv.Itoa(status.Usage[keyID]) + " sealed values", ""})
	}
	if err := table(application.stdout, rows...); err != nil {
		return err
	}
	orphans := unknownKeys(status.Usage, append([]string{status.CurrentKeyID}, status.Retired...))
	if len(orphans) > 0 {
		fmt.Fprintln(application.stdout, application.colours.danger("values sealed with keys that are not on disk: "+fmt.Sprint(orphans)))
	}
	if status.Pending {
		fmt.Fprintln(application.stdout, application.colours.warn("a rotation was interrupted; run `composewright keys rotate` again"))
	}
	return nil
}

func unknownKeys(usage map[string]int, known []string) []string {
	present := map[string]bool{}
	for _, keyID := range known {
		present[keyID] = true
	}
	var unknown []string
	for keyID := range usage {
		if !present[keyID] {
			unknown = append(unknown, keyID)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func (application *app) runKeysRotate(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	result, err := workspace.RotateKey(ctx)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, result)
	}
	fmt.Fprintf(application.stdout, "rotated %s -> %s, %d values re-encrypted; old key kept at %s\n",
		result.OldKeyID, result.NewKeyID, result.Resealed, result.Retired)
	return nil
}

func (application *app) runKeysPurge(ctx context.Context) error {
	workspace, err := application.openWorkspace(ctx)
	if err != nil {
		return err
	}
	defer workspace.Close()
	confirmed, err := application.confirm("Delete retired keys? Backups taken before the last rotation become unreadable.")
	if err != nil {
		return err
	}
	if !confirmed {
		return errors.New("purge cancelled")
	}
	purged, err := workspace.PurgeRetiredKeys(ctx)
	if err != nil {
		return err
	}
	if application.global.json {
		return writeJSON(application.stdout, map[string]any{"purged": purged})
	}
	fmt.Fprintf(application.stdout, "%d retired keys deleted\n", len(purged))
	return nil
}
