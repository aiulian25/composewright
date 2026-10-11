package web

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/backup"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	noSuchSection        = "There is no such settings section."
	currentPasswordField = "current"
	passphraseField      = "passphrase"
	confirmField         = "confirm"
	backupFileField      = "backup"
	replaceField         = "replace"
	restoredQuery        = "restored"
	keptQuery            = "kept"
	backupNameLayout     = "20060102-150405"
	backupNamePrefix     = "composewright-"
	backupType           = "application/octet-stream"
	namesJoiner          = ", "
	queryNamesJoiner     = ","
)

var errPassphrasesDiffer = errors.New("the two passphrases differ")

// downloadBackup sends the Settings choices, the model keys they use and
// every project in one file encrypted with the passphrase given, once the
// account password is confirmed again.
func (server *Server) downloadBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	content, err := server.createBackup(request, current)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	name := backupNamePrefix + server.options.Now().UTC().Format(backupNameLayout) + backup.FileExtension
	writer.Header().Set(contentType, backupType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, err = writer.Write(content) //nolint:gosec // G705: an encrypted attachment served as application/octet-stream with nosniff
	return err
}

func (server *Server) createBackup(request *http.Request, current *visit) ([]byte, error) {
	passphrase := request.PostFormValue(passphraseField)
	if passphrase != request.PostFormValue(confirmField) {
		return nil, errPassphrasesDiffer
	}
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, database := request.Context(), server.workspace().Store
	archive, err := backup.Collect(ctx, database, server.config(), server.options.Version, server.options.Now())
	if err != nil {
		return nil, err
	}
	content, err := backup.Seal(archive, passphrase)
	if err != nil {
		return nil, err
	}
	details := map[string]any{"projects": len(archive.Projects), "keys": sortedKeys(archive.Keys), "contains_secrets": true}
	return content, database.Audit(ctx, webActorPrefix+current.user.Username, store.ActionBackup, "", details)
}

// restoreBackup brings back a backup's Settings choices, model keys and
// projects, once the account password is confirmed again. A project that
// exists here is kept unless replacing it was asked for.
func (server *Server) restoreBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	restored, kept, err := server.restore(request, current)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	query := url.Values{sectionQuery: {sectionBackup}, restoredQuery: {strconv.Itoa(len(restored))}, keptQuery: {strings.Join(kept, queryNamesJoiner)}}
	return redirect(writer, request, settingsPath+"?"+query.Encode())
}

func (server *Server) restore(request *http.Request, current *visit) (restored, kept []string, err error) {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return nil, nil, err
	}
	content, err := uploadedBackup(request)
	if err != nil {
		release()
		return nil, nil, err
	}
	archive, err := backup.Open(content, request.PostFormValue(passphraseField))
	release()
	if err != nil {
		return nil, nil, err
	}
	replace := request.PostFormValue(replaceField) != ""
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if err := server.busyProjects(archive, replace); err != nil {
		return nil, nil, err
	}
	configuration := archive.Settings.Apply(server.config())
	if err := configuration.Validate(); err != nil {
		return nil, nil, fmt.Errorf("the backup's settings do not fit this install: %w", err)
	}
	ctx, database := request.Context(), server.workspace().Store
	for name, value := range archive.Keys {
		if err := database.SetSecret(ctx, name, value); err != nil {
			return nil, nil, err
		}
	}
	if err := server.update(request, current, settingsValues(configuration)); err != nil {
		return nil, nil, err
	}
	details := map[string]any{"replace": replace, "keys": sortedKeys(archive.Keys), "backup_created": archive.Created, "backup_version": archive.Version}
	var failure error
	for _, record := range archive.Projects {
		written, err := database.RestoreProject(ctx, record, replace)
		if err != nil {
			failure, details["failed"] = fmt.Errorf("project %s: %w", record.Project.Name, err), record.Project.Name
			break
		}
		if written {
			restored = append(restored, record.Project.Name)
			continue
		}
		kept = append(kept, record.Project.Name)
	}
	// What was written is audited even when a project failed part way.
	details["restored"], details["kept"] = restored, kept
	audited := database.Audit(ctx, webActorPrefix+current.user.Username, store.ActionRestore, "", details)
	return restored, kept, errors.Join(failure, audited)
}

// busyProjects refuses to replace a project while a run of it is going on.
func (server *Server) busyProjects(archive backup.Archive, replace bool) error {
	if !replace {
		return nil
	}
	for _, record := range archive.Projects {
		if _, busy := server.jobs.running(record.Project.Name); busy {
			return errors.New(strings.TrimSuffix(fmt.Sprintf(runningConflict, record.Project.Name), "."))
		}
	}
	return nil
}

func uploadedBackup(request *http.Request) ([]byte, error) {
	file, _, err := request.FormFile(backupFileField)
	if err != nil {
		return nil, errors.New("choose the backup file to restore")
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

// settingsValues are every key the Settings page writes, so a restore
// writes them in one save that is validated as a whole.
func settingsValues(configuration config.Config) map[string]string {
	values := profileValues(configuration.Profile)
	maps.Copy(values, engineValues(configuration))
	maps.Copy(values, modelsValues(configuration.LLM))
	values[themeKey] = quote(configuration.UI.Theme)
	return values
}

// backupProblem shows the Backup page again with what went wrong.
func (server *Server) backupProblem(err error) settingsState {
	return settingsState{shown: sectionBackup, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: server.config()}
}

// restoredNotice says what the last restore did: how many projects came
// back, and which ones were kept as they were here.
func restoredNotice(query url.Values) string {
	count, _ := strconv.Atoi(query.Get(restoredQuery))
	notice := "Restored your settings and " + countText(count, "project", "projects") + "."
	var kept []string
	for _, name := range strings.Split(query.Get(keptQuery), queryNamesJoiner) {
		if store.ValidProjectName(name) {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 {
		return notice
	}
	return notice + " Kept as they were here: " + strings.Join(kept, namesJoiner) + "."
}

func sortedKeys(values map[string]string) []string {
	return slices.Sorted(maps.Keys(values))
}
