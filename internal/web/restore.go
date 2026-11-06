package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/backup"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

type restoreJournal struct {
	Archive       backup.Archive
	Before        backup.Archive
	Source        string
	Replace       bool
	SettingsDone  bool
	CatalogueDone bool
	Completed     bool
}

func (server *Server) restoreJournal(ctx context.Context) (restoreJournal, bool, error) {
	text, found, err := server.workspace().Store.Secret(ctx, backup.RestoreJournalName)
	var journal restoreJournal
	if err == nil && found {
		err = json.Unmarshal([]byte(text), &journal)
	}
	return journal, found, err
}
func (server *Server) writeRestoreJournal(ctx context.Context, journal restoreJournal) error {
	content, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return server.workspace().Store.SetSecret(ctx, backup.RestoreJournalName, string(content))
}

// beginRestore excludes jobs throughout recovery. It is intentionally stricter
// than per-project busy checks because restored provider/settings affect jobs.
func (registry *jobs) beginRestore() error {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	for _, job := range registry.byID {
		if !job.isFinished() {
			return conflict("Wait for all runs to finish before restoring a backup.")
		}
	}
	registry.restoring = true
	return nil
}
func (registry *jobs) finishRestore() {
	registry.mutex.Lock()
	registry.restoring = false
	registry.mutex.Unlock()
}
func (registry *jobs) restorePending() bool {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	return registry.restoring
}

// prepareRestore validates everything before recording a durable, encrypted
// intent and recovery snapshot. Callers hold settingsMutex.
func (server *Server) prepareRestore(ctx context.Context, archive backup.Archive, source string, replace bool) error {
	old, found, err := server.restoreJournal(ctx)
	if err != nil {
		return err
	}
	if found && !old.Completed {
		return conflict("A restore is unfinished. Resume or cancel it from Backups first.")
	}
	if err = backup.Preflight(archive, server.config()); err != nil {
		return err
	}
	if err = server.jobs.beginRestore(); err != nil {
		return err
	}
	ready := false
	defer func() {
		if !ready {
			check, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pending, exists, failure := server.restoreJournal(check)
			// A commit error can leave its durable intent visible. Fail closed
			// until that intent is resumed or a clean restart rechecks it.
			if failure == nil && (!exists || pending.Completed) {
				server.jobs.finishRestore()
			}
		}
	}()
	before, err := backup.Collect(ctx, server.workspace().Store, server.config(), server.paths().CatalogueDir(), server.options.Version, server.options.Now())
	if err != nil {
		return err
	}
	journal := restoreJournal{Archive: archive, Before: before, Source: source, Replace: replace}
	encoded, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err = server.workspace().Store.PrepareRestore(ctx, backup.RestoreJournalName, backup.RestoreCheckpointName, string(encoded)); err != nil {
		return err
	}
	ready = true
	return nil
}

// resumeRestore replays filesystem steps idempotently. The database batch and
// checkpoint commit together, so a crash never repeats committed project work.
func (server *Server) resumeRestore(request *http.Request, current *visit) (restored, kept []string, err error) {
	ctx := request.Context()
	journal, found, err := server.restoreJournal(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, badRequest("There is no unfinished restore.")
	}
	if journal.Completed {
		var done store.RestoreProgress
		text, exists, failure := server.workspace().Store.Secret(ctx, backup.RestoreCheckpointName)
		if failure != nil {
			return nil, nil, failure
		}
		if exists {
			if failure = json.Unmarshal([]byte(text), &done); failure != nil {
				return nil, nil, failure
			}
		}
		server.jobs.finishRestore()
		return done.Restored, done.Kept, nil
	}
	if err = backup.Preflight(journal.Archive, server.config()); err != nil {
		return nil, nil, err
	}
	if err = server.jobs.beginRestore(); err != nil {
		return nil, nil, err
	}
	var progress store.RestoreProgress
	checkpoint, committed, err := server.workspace().Store.Secret(ctx, backup.RestoreCheckpointName)
	if err != nil {
		return nil, nil, err
	}
	if committed {
		err = json.Unmarshal([]byte(checkpoint), &progress)
	} else {
		progress, err = server.workspace().Store.RestoreRecords(ctx, journal.Archive.Projects, journal.Before.Projects, journal.Archive.Keys, journal.Replace, backup.RestoreCheckpointName)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("restore unfinished; resume from Backups to inspect the recorded database progress: %w", err)
	}
	restored, kept = progress.Restored, progress.Kept
	failure := func(err error) ([]string, []string, error) {
		return restored, kept, fmt.Errorf("restore unfinished: database committed (%d restored, %d kept); settings/catalogue may be partial. Resume from Backups: %w", len(restored), len(kept), err)
	}
	configuration := journal.Archive.Settings.Apply(server.config())
	if err = server.update(request, current, settingsValues(configuration)); err != nil {
		return failure(err)
	}
	journal.SettingsDone = true
	if err = server.writeRestoreJournal(ctx, journal); err != nil {
		return failure(err)
	}
	if err = workspace.WriteCatalogueFiles(server.paths().CatalogueDir(), journal.Archive.Catalogue); err != nil {
		return failure(err)
	}
	server.catalogue.Store(workspace.LoadCatalogue(server.paths()))
	journal.CatalogueDone = true
	if err = server.writeRestoreJournal(ctx, journal); err != nil {
		return failure(err)
	}
	details := map[string]any{"from": journal.Source, "replace": journal.Replace, "restored": restored, "kept": kept, "recovery_snapshot": true}
	if err = server.workspace().Store.Audit(ctx, webActorPrefix+current.user.Username, store.ActionRestore, "", details); err != nil {
		return failure(err)
	}
	journal.Completed = true
	if err = server.writeRestoreJournal(ctx, journal); err != nil {
		return failure(err)
	}
	server.jobs.finishRestore()
	return restored, kept, nil
}

func (server *Server) resumeBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return err
	}
	release()
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	restored, kept, err := server.resumeRestore(request, current)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	return redirect(writer, request, backupURL(url.Values{restoredQuery: {strconv.Itoa(len(restored))}, keptQuery: {strings.Join(kept, queryNamesJoiner)}}))
}
func (server *Server) cancelBackupRestore(writer http.ResponseWriter, request *http.Request, current *visit) error {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return err
	}
	defer release()
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if committed, err := server.workspace().Store.HasSecret(request.Context(), backup.RestoreCheckpointName); err != nil {
		return err
	} else if committed {
		return conflict("Database changes committed; resume the restore or download the original recovery snapshot.")
	}
	journal, found, err := server.restoreJournal(request.Context())
	if err != nil {
		return err
	}
	if !found {
		return badRequest("No restore to cancel.")
	}
	journal.Completed = true
	if err = server.writeRestoreJournal(request.Context(), journal); err != nil {
		return err
	}
	server.jobs.finishRestore()
	return redirect(writer, request, backupURL(url.Values{}))
}
func (server *Server) downloadRestoreRecovery(writer http.ResponseWriter, request *http.Request, current *visit) error {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return err
	}
	defer release()
	if request.PostFormValue(passphraseField) != request.PostFormValue(confirmField) {
		return badRequest("The two passphrases differ.")
	}
	server.settingsMutex.Lock()
	journal, found, err := server.restoreJournal(request.Context())
	server.settingsMutex.Unlock()
	if err != nil {
		return err
	}
	if !found {
		return badRequest("There is no recovery snapshot.")
	}
	content, err := backup.Seal(journal.Before, request.PostFormValue(passphraseField))
	if err != nil {
		return err
	}
	writer.Header().Set(contentType, backupType)
	writer.Header().Set("Content-Disposition", `attachment; filename="before-restore.cwbackup"`)
	_, err = writer.Write(content) //nolint:gosec // G705: an encrypted attachment served as application/octet-stream with nosniff
	return err
}
