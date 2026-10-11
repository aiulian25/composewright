package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/auth"
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
	backupNameField      = "name"
	replaceField         = "replace"
	scheduleField        = "schedule"
	everyDaysField       = "every_days"
	keepField            = "keep"
	restoredQuery        = "restored"
	keptQuery            = "kept"
	createdQuery         = "created"
	deletedQuery         = "deleted"
	passphraseQuery      = "passphrase"
	notResealedQuery     = "not_resealed"
	uploadedSource       = "upload"
	backupType           = "application/octet-stream"
	namesJoiner          = ", "
	queryNamesJoiner     = ","
	// backupCheckEvery is how often the scheduler looks whether an automatic
	// backup is due, so the first one comes within this time of turning
	// the schedule on.
	backupCheckEvery = time.Hour
	scheduleActor    = "schedule:backups"
)

var (
	errPassphrasesDiffer = errors.New("the two passphrases differ")
	errNoPassphrase      = errors.New("set a backup passphrase first")
	kindLabels           = map[string]string{backup.KindAutomatic: "automatic", backup.KindManual: "manual"}
)

// backupView is the Backups page: whether a passphrase is set, the schedule
// with when the next automatic backup comes, and the backups kept here.
type backupView struct {
	PassphraseSet bool
	Schedule      config.Backup
	Schedules     []string
	MaxDays       int
	MaxKept       int
	Next          string
	Folder        string
	Stored        []storedRow
}

type storedRow struct {
	Name string
	Kind string
	When string
	Size string
}

func (server *Server) backupView(ctx context.Context, schedule config.Backup) (backupView, error) {
	folder := server.backupFolder()
	stored, err := folder.List()
	if err != nil {
		return backupView{}, err
	}
	passphraseSet, err := server.workspace().Store.HasSecret(ctx, backup.PassphraseName)
	view := backupView{PassphraseSet: passphraseSet, Schedule: schedule, Schedules: config.BackupSchedules(), MaxDays: config.MaxBackupDays,
		MaxKept: config.MaxBackupsKept, Folder: string(folder),
		Next: nextBackupText(schedule, backup.LastAutomatic(stored), server.options.Now(), passphraseSet)}
	for _, item := range stored {
		view.Stored = append(view.Stored, storedRow{Name: item.Name, Kind: kindLabels[item.Kind], When: item.Created.Local().Format(timeLayout),
			Size: units.HumanSize(float64(item.Size))})
	}
	return view, err
}

// nextBackupText says when the schedule makes its next backup.
func nextBackupText(schedule config.Backup, last, now time.Time, passphraseSet bool) string {
	next := backup.Next(schedule, last, now)
	switch {
	case next.IsZero():
		return "Automatic backups are off."
	case !passphraseSet:
		return "Automatic backups start once a backup passphrase is set."
	case !now.Before(next):
		return "The next automatic backup is due and is made within the hour."
	}
	return "The next automatic backup is due " + next.Local().Format(timeLayout) + "."
}

func (server *Server) backupFolder() backup.Folder {
	return backup.Folder(server.paths().BackupsDir())
}

// scheduleBackups makes an automatic backup whenever one is due, looking
// every backupCheckEvery until the server closes.
func (server *Server) scheduleBackups(ctx context.Context) {
	defer close(server.backupsDone)
	ticker := time.NewTicker(backupCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.backupIfDue(ctx)
		}
	}
}

// backupIfDue makes an automatic backup when the schedule says one is due.
// A failure is logged, and the next look tries again.
func (server *Server) backupIfDue(ctx context.Context) {
	stored, err := server.backupFolder().List()
	if err != nil {
		server.options.Logger.Warn("listing the stored backups", "error", err)
		return
	}
	if !backup.Due(server.config().Backup, backup.LastAutomatic(stored), server.options.Now()) {
		return
	}
	_, err = server.storeBackup(ctx, backup.KindAutomatic, scheduleActor)
	if err != nil && !errors.Is(err, errNoPassphrase) {
		server.options.Logger.Warn("automatic backup", "error", err)
	}
}

// storeBackup seals a backup with the backup passphrase and keeps it, then
// drops the oldest automatic ones beyond what the schedule keeps. It takes
// the Settings lock, so it never captures a restore half done.
func (server *Server) storeBackup(ctx context.Context, kind, actor string) (backup.Stored, error) {
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	database := server.workspace().Store
	passphrase, found, err := database.Secret(ctx, backup.PassphraseName)
	if err != nil {
		return backup.Stored{}, err
	}
	if !found {
		return backup.Stored{}, errNoPassphrase
	}
	release, free := server.hashSlot()
	if !free {
		return backup.Stored{}, errors.New(strings.TrimSuffix(tooManyPasswordChecks, "."))
	}
	defer release()
	now, configuration := server.options.Now(), server.config()
	archive, err := backup.Collect(ctx, database, configuration, server.options.Version, now)
	if err != nil {
		return backup.Stored{}, err
	}
	content, err := backup.Seal(archive, passphrase)
	if err != nil {
		return backup.Stored{}, err
	}
	folder := server.backupFolder()
	stored, err := folder.Write(kind, now, content)
	if err != nil {
		return backup.Stored{}, err
	}
	details := map[string]any{"backup": stored.Name, "kind": kind, "projects": len(archive.Projects), "keys": sortedKeys(archive.Keys)}
	var pruneErr error
	if kind == backup.KindAutomatic {
		details["pruned"], pruneErr = folder.Prune(configuration.Backup.Keep)
	}
	return stored, errors.Join(pruneErr, database.Audit(ctx, actor, store.ActionBackup, "", details))
}

// backUpNow keeps a manual backup made now.
func (server *Server) backUpNow(writer http.ResponseWriter, request *http.Request, current *visit) error {
	stored, err := server.storeBackup(request.Context(), backup.KindManual, webActorPrefix+current.user.Username)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	return redirect(writer, request, backupURL(url.Values{createdQuery: {stored.Name}}))
}

// downloadBackup sends a stored backup, once the account password is
// confirmed again. It opens with the backup passphrase on any install.
func (server *Server) downloadBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	name := request.PostFormValue(backupNameField)
	content, err := server.storedForDownload(request, current, name)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	writer.Header().Set(contentType, backupType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, err = writer.Write(content) //nolint:gosec // G705: an encrypted attachment served as application/octet-stream with nosniff
	return err
}

func (server *Server) storedForDownload(request *http.Request, current *visit, name string) ([]byte, error) {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return nil, err
	}
	release()
	content, err := server.backupFolder().Read(name)
	if err != nil {
		return nil, err
	}
	details := map[string]any{"backup": name, "via": "download", "contains_secrets": true}
	return content, server.workspace().Store.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionExport, "", details)
}

// deleteBackup removes a stored backup.
func (server *Server) deleteBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	name := request.PostFormValue(backupNameField)
	if err := server.backupFolder().Delete(name); err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	details := map[string]any{"backup": name}
	if err := server.workspace().Store.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionDelete, "", details); err != nil {
		return err
	}
	return redirect(writer, request, backupURL(url.Values{deletedQuery: {name}}))
}

// setBackupPassphrase sets the passphrase every backup is encrypted with,
// once the account password is confirmed again.
func (server *Server) setBackupPassphrase(writer http.ResponseWriter, request *http.Request, current *visit) error {
	resealed, notResealed, err := server.changeBackupPassphrase(request, current)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	query := url.Values{passphraseQuery: {strconv.Itoa(resealed)}}
	if len(notResealed) > 0 {
		query.Set(notResealedQuery, strings.Join(notResealed, queryNamesJoiner))
	}
	return redirect(writer, request, backupURL(query))
}

// changeBackupPassphrase stores a new backup passphrase. The stored backups
// are re-encrypted with it, so each still opens with the passphrase in use;
// one that cannot be is named and keeps the old passphrase.
func (server *Server) changeBackupPassphrase(request *http.Request, current *visit) (int, []string, error) {
	passphrase := request.PostFormValue(passphraseField)
	if passphrase != request.PostFormValue(confirmField) {
		return 0, nil, errPassphrasesDiffer
	}
	if err := auth.CheckPassword(passphrase); err != nil {
		return 0, nil, err
	}
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return 0, nil, err
	}
	defer release()
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	ctx, database := request.Context(), server.workspace().Store
	old, found, err := database.Secret(ctx, backup.PassphraseName)
	if err != nil {
		return 0, nil, err
	}
	resealed, notResealed := 0, []string(nil)
	if found {
		if resealed, notResealed, err = server.resealBackups(old, passphrase); err != nil {
			return 0, nil, err
		}
	}
	if err := database.SetSecret(ctx, backup.PassphraseName, passphrase); err != nil {
		return 0, nil, err
	}
	details := map[string]any{"name": backup.PassphraseName, "resealed": resealed, "not_resealed": notResealed}
	return resealed, notResealed, database.Audit(ctx, webActorPrefix+current.user.Username, store.ActionSetSecret, "", details)
}

// resealBackups re-encrypts every stored backup from one passphrase to
// another and names the ones it could not.
func (server *Server) resealBackups(from, to string) (int, []string, error) {
	folder := server.backupFolder()
	stored, err := folder.List()
	if err != nil {
		return 0, nil, err
	}
	resealed, notResealed := 0, []string(nil)
	for _, item := range stored {
		if err := resealBackup(folder, item.Name, from, to); err != nil {
			server.options.Logger.Warn("re-encrypting a backup", "backup", item.Name, "error", err)
			notResealed = append(notResealed, item.Name)
			continue
		}
		resealed++
	}
	return resealed, notResealed, nil
}

func resealBackup(folder backup.Folder, name, from, to string) error {
	content, err := folder.Read(name)
	if err != nil {
		return err
	}
	resealed, err := backup.Reseal(content, from, to)
	if err != nil {
		return err
	}
	return folder.Rewrite(name, resealed)
}

// restoreBackup brings back a backup's Settings choices, model keys and
// projects, once the account password is confirmed again: a stored one, or
// an uploaded file with its passphrase. A project that exists here is kept
// unless replacing it was asked for.
func (server *Server) restoreBackup(writer http.ResponseWriter, request *http.Request, current *visit) error {
	restored, kept, err := server.restore(request, current)
	if err != nil {
		return server.renderSettings(writer, request, current, server.backupProblem(err))
	}
	return redirect(writer, request, backupURL(url.Values{restoredQuery: {strconv.Itoa(len(restored))}, keptQuery: {strings.Join(kept, queryNamesJoiner)}}))
}

func (server *Server) restore(request *http.Request, current *visit) (restored, kept []string, err error) {
	release, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return nil, nil, err
	}
	archive, source, err := server.restoreSource(request)
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
	details := map[string]any{"from": source, "replace": replace, "keys": sortedKeys(archive.Keys), "backup_created": archive.Created,
		"backup_version": archive.Version}
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

// restoreSource opens the backup a restore names: a stored one, with the
// backup passphrase, or an uploaded file, with the passphrase given. It
// returns where the backup came from, for the audit log.
func (server *Server) restoreSource(request *http.Request) (backup.Archive, string, error) {
	name := request.PostFormValue(backupNameField)
	if name == "" {
		content, err := uploadedBackup(request)
		if err != nil {
			return backup.Archive{}, "", err
		}
		archive, err := backup.Open(content, request.PostFormValue(passphraseField))
		return archive, uploadedSource, err
	}
	content, err := server.backupFolder().Read(name)
	if err != nil {
		return backup.Archive{}, "", err
	}
	passphrase, found, err := server.workspace().Store.Secret(request.Context(), backup.PassphraseName)
	if err != nil {
		return backup.Archive{}, "", err
	}
	if !found {
		return backup.Archive{}, "", errNoPassphrase
	}
	archive, err := backup.Open(content, passphrase)
	return archive, name, err
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

// backupValues are the keys the Backups page's schedule writes.
func backupValues(schedule config.Backup) map[string]string {
	return map[string]string{"backup.schedule": quote(schedule.Schedule), "backup.every_days": strconv.Itoa(schedule.EveryDays),
		"backup.keep": strconv.Itoa(schedule.Keep)}
}

// backupForm reads the Backups page's schedule.
func backupForm(request *http.Request, saved config.Backup) (config.Backup, error) {
	reader := &numbers{request: request}
	schedule := config.Backup{Schedule: field(request, scheduleField), EveryDays: reader.whole(everyDaysField, "Every how many days", saved.EveryDays),
		Keep: reader.whole(keepField, "Automatic backups kept", saved.Keep)}
	return schedule, errors.Join(reader.problems...)
}

// backupProblem shows the Backups page again with what went wrong.
func (server *Server) backupProblem(err error) settingsState {
	return settingsState{shown: sectionBackup, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: server.config()}
}

func backupURL(query url.Values) string {
	query.Set(sectionQuery, sectionBackup)
	return settingsPath + "?" + query.Encode()
}

// backupNotice says what the last action on the Backups page did.
func backupNotice(query url.Values) string {
	switch {
	case query.Has(restoredQuery):
		return restoredNotice(query)
	case query.Has(passphraseQuery):
		return passphraseNotice(query)
	case backup.IsStoredName(query.Get(createdQuery)):
		return "Backed up as " + query.Get(createdQuery) + "."
	case backup.IsStoredName(query.Get(deletedQuery)):
		return "Deleted " + query.Get(deletedQuery) + "."
	}
	return ""
}

// restoredNotice says how many projects came back, and which ones were
// kept as they were here.
func restoredNotice(query url.Values) string {
	count, _ := strconv.Atoi(query.Get(restoredQuery))
	notice := "Restored your settings and " + countText(count, "project", "projects") + "."
	kept := slices.DeleteFunc(strings.Split(query.Get(keptQuery), queryNamesJoiner), func(name string) bool { return !store.ValidProjectName(name) })
	if len(kept) == 0 {
		return notice
	}
	return notice + " Kept as they were here: " + strings.Join(kept, namesJoiner) + "."
}

// passphraseNotice says the passphrase is set, how many stored backups were
// re-encrypted with it, and which still need the old one.
func passphraseNotice(query url.Values) string {
	notice := "The backup passphrase is set."
	if count, _ := strconv.Atoi(query.Get(passphraseQuery)); count > 0 {
		notice += " " + countText(count, "stored backup was", "stored backups were") + " re-encrypted with it."
	}
	left := slices.DeleteFunc(strings.Split(query.Get(notResealedQuery), queryNamesJoiner), func(name string) bool { return !backup.IsStoredName(name) })
	if len(left) == 0 {
		return notice
	}
	return notice + " These still need the old passphrase: " + strings.Join(left, namesJoiner) + "."
}

func sortedKeys(values map[string]string) []string {
	return slices.Sorted(maps.Keys(values))
}
