package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/doctor"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/release"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

// The Maintenance page holds what the CLI's keys, version --check and
// doctor commands do; the Environment profile page imports and exports
// profile.yaml as profile import and export do.

const (
	sectionMaintenance = "maintenance"
	keysCommand        = "composewright keys status"
	doneQuery          = "done"
	doneRotated        = "rotated"
	donePurged         = "purged"
	doneImported       = "imported"
	profileFileField   = "profile"
	yamlType           = "application/yaml; charset=utf-8"
	versionPrefix      = "v"
	releaseDateLayout  = "2 Jan 2006"
	upToDate           = "composewright %s is the latest release."
	newerRelease       = "%s is the latest release, published %s; this is %s."
	noReleaseYet       = "No release is published yet."
	updateCheckFailed  = "The update check did not answer: %s."
	// doctorTimeout bounds the Docker checks, which run one docker command
	// after another.
	doctorTimeout = 30 * time.Second
	day           = 24 * time.Hour
)

var (
	errRunGoing = errors.New("a run is going; try again once it has finished")
	doneNotices = map[string]string{
		doneRotated:       "The master key is rotated: every stored value is encrypted with the new key.",
		donePurged:        "The retired keys are deleted.",
		doneImported:      "The profile is imported.",
		doneGitHubStored:  "The GitHub token is stored.",
		doneGitHubRemoved: "The GitHub token is removed.",
	}
	checkTones = map[string]string{doctor.StatusOK: toneOk, doctor.StatusWarn: toneWarn, doctor.StatusFail: toneDanger}
)

// maintenanceView is the Maintenance page: the master key, the version and
// what the last update check or Docker check said.
type maintenanceView struct {
	KeyID      string
	KeyCreated time.Time
	KeyAge     string
	Sealed     int
	Retired    []retiredKey
	Pending    bool
	Version    string
	Update     *updateCheck
	Checks     []checkRow
}

type retiredKey struct {
	ID     string
	Sealed int
}

type updateCheck struct {
	Text string
	URL  string
}

type checkRow struct {
	doctor.Check
	Tone string
}

func (server *Server) maintenanceView(ctx context.Context, state settingsState) (maintenanceView, error) {
	status, err := server.keyStatus(ctx)
	if err != nil {
		return maintenanceView{}, err
	}
	view := maintenanceView{KeyID: status.CurrentKeyID, Sealed: status.Usage[status.CurrentKeyID], Pending: status.Pending,
		Version: server.options.Version, Update: state.update}
	if !status.Created.IsZero() {
		view.KeyCreated = status.Created
		view.KeyAge = countText(max(0, int(server.options.Now().Sub(status.Created)/day)), "day", "days")
	}
	for _, keyID := range status.Retired {
		view.Retired = append(view.Retired, retiredKey{ID: keyID, Sealed: status.Usage[keyID]})
	}
	for _, check := range state.checks {
		view.Checks = append(view.Checks, checkRow{Check: check, Tone: checkTones[check.Status]})
	}
	return view, nil
}

// keyStatus reads the key status while no rotation runs, since a rotation
// changes the workspace's key.
func (server *Server) keyStatus(ctx context.Context) (workspace.KeyStatus, error) {
	server.keyMutex.RLock()
	defer server.keyMutex.RUnlock()
	return server.workspace().KeyStatus(ctx)
}

func (server *Server) rotateKey(writer http.ResponseWriter, request *http.Request, current *visit) error {
	err := server.keyStep(request, current, func(ctx context.Context, opened *workspace.Workspace, actor string) error {
		_, err := opened.RotateKey(ctx, actor)
		return err
	})
	return server.afterKeyStep(writer, request, current, err, doneRotated)
}

func (server *Server) purgeKeys(writer http.ResponseWriter, request *http.Request, current *visit) error {
	err := server.keyStep(request, current, func(ctx context.Context, opened *workspace.Workspace, actor string) error {
		_, err := opened.PurgeRetiredKeys(ctx, actor)
		return err
	})
	return server.afterKeyStep(writer, request, current, err, donePurged)
}

// keyStep runs a master key step once the account password is confirmed,
// while no Settings save and no run is going. The running server keeps
// using the workspace it changes, so the new key applies at once. A step
// that has started finishes, and is audited, even if the browser leaves.
func (server *Server) keyStep(request *http.Request, current *visit, step func(context.Context, *workspace.Workspace, string) error) error {
	freeHashSlot, err := server.confirmPassword(current, request.PostFormValue(currentPasswordField))
	if err != nil {
		return err
	}
	freeHashSlot()
	server.keyMutex.Lock()
	defer server.keyMutex.Unlock()
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if server.jobs.busy() {
		return errRunGoing
	}
	return step(context.WithoutCancel(request.Context()), server.workspace(), webActorPrefix+current.user.Username)
}

func (server *Server) afterKeyStep(writer http.ResponseWriter, request *http.Request, current *visit, err error, done string) error {
	if err != nil {
		state := settingsState{shown: sectionMaintenance, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: server.config()}
		return server.renderSettings(writer, request, current, state)
	}
	return redirect(writer, request, doneURL(sectionMaintenance, done))
}

// checkForUpdate asks GitHub Releases once for the latest release, through
// the egress guard, and says how it compares with this version.
func (server *Server) checkForUpdate(writer http.ResponseWriter, request *http.Request, current *visit) error {
	state := settingsState{shown: sectionMaintenance, status: http.StatusOK, configuration: server.config()}
	latest, err := server.latestRelease(request.Context())
	details := map[string]any{"ok": err == nil, "latest": latest.Tag, "version": server.options.Version}
	if auditErr := server.workspace().Store.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionUpdateCheck, "", details); auditErr != nil {
		return auditErr
	}
	if err != nil {
		state.problem = fmt.Sprintf(updateCheckFailed, strings.TrimSuffix(err.Error(), "."))
		return server.renderSettings(writer, request, current, state)
	}
	result := compareRelease(latest, server.options.Version)
	state.update = &result
	return server.renderSettings(writer, request, current, state)
}

func (server *Server) latestRelease(ctx context.Context) (release.Release, error) {
	policy, err := netguard.PolicyFromConfig(server.config(), nil, server.options.Version)
	if err != nil {
		return release.Release{}, err
	}
	guard, err := netguard.New(policy)
	if err != nil {
		return release.Release{}, err
	}
	return server.options.Latest(ctx, guard)
}

func compareRelease(latest release.Release, version string) updateCheck {
	if latest.Tag == "" {
		return updateCheck{Text: noReleaseYet}
	}
	if strings.TrimPrefix(latest.Tag, versionPrefix) == strings.TrimPrefix(version, versionPrefix) {
		return updateCheck{Text: fmt.Sprintf(upToDate, version)}
	}
	return updateCheck{Text: fmt.Sprintf(newerRelease, latest.Tag, latest.Published.Format(releaseDateLayout), version), URL: latest.URL}
}

// runDoctor runs the checks `composewright doctor` runs, without
// contacting the model.
func (server *Server) runDoctor(writer http.ResponseWriter, request *http.Request, current *visit) error {
	loaded := server.loadedConfig()
	state := settingsState{shown: sectionMaintenance, status: http.StatusOK, configuration: loaded.Config}
	state.checks = server.doctorChecks(request.Context(), loaded)
	return server.renderSettings(writer, request, current, state)
}

// doctorChecks keeps rotations out because the checks open the workspace,
// which finishes a key swap a rotation may be halfway through.
func (server *Server) doctorChecks(ctx context.Context, loaded *config.Loaded) []doctor.Check {
	docker, dockerErr := sandbox.FromConfig(loaded.Config, loaded.Paths.ProjectsDir())
	checkCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	server.keyMutex.RLock()
	defer server.keyMutex.RUnlock()
	return doctor.Run(checkCtx, doctor.Inputs{Loaded: loaded, Sandbox: docker, Docker: dockerErr})
}

// exportProfile downloads the profile as `composewright profile export`
// writes it.
func (server *Server) exportProfile(writer http.ResponseWriter, _ *http.Request, _ *visit) error {
	rendered, err := config.RenderProfile(server.config().Profile)
	if err != nil {
		return err
	}
	writer.Header().Set(contentType, yamlType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+profileFile+`"`)
	_, err = writer.Write(rendered)
	return err
}

// importProfile replaces the profile with an uploaded profile.yaml, checked
// as `composewright profile import` checks it.
func (server *Server) importProfile(writer http.ResponseWriter, request *http.Request, current *visit) error {
	if err := server.replaceProfile(request, current); err != nil {
		state := settingsState{shown: sectionProfile, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: server.config()}
		return server.renderSettings(writer, request, current, state)
	}
	return redirect(writer, request, doneURL(sectionProfile, doneImported))
}

func (server *Server) replaceProfile(request *http.Request, current *visit) error {
	file, _, err := request.FormFile(profileFileField)
	if err != nil {
		return errors.New("choose the profile.yaml to import")
	}
	content, err := io.ReadAll(file)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	profile, err := config.DecodeProfile(content)
	if err != nil {
		return err
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	return server.update(request, current, profileValues(profile))
}

func doneURL(section, done string) string {
	return settingsURL(section) + "&" + doneQuery + "=" + url.QueryEscape(done)
}
