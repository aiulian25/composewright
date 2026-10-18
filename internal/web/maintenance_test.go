package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/release"
	"github.com/aiulian25/composewright/internal/store"
)

// TestSettingsKeyRotation: Maintenance shows the master key; rotating it
// asks for the password, waits for runs to finish, and the running server
// seals and opens with the new key afterwards; purging deletes the old one.
func TestSettingsKeyRotation(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	ctx, opened := context.Background(), harness.server.workspace()
	if err := opened.Store.SetSecret(ctx, "llm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	oldID := opened.Key.ID
	response, page := harness.get(settingsURL(sectionMaintenance))
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, oldID) || !strings.Contains(page, "0 days old") || !strings.Contains(page, `action="/settings/audit/verify"`) {
		t.Fatal("Maintenance does not show the master key, its age and the audit chain check")
	}
	response, page = harness.post("/settings/maintenance/rotate", url.Values{currentPasswordField: {"wrong"}})
	status(t, response, http.StatusBadRequest, page)
	finish := make(chan struct{})
	running, err := harness.server.jobs.start(jobGenerate, "busy", "", func(ctx context.Context, _ *job) (string, *pipeline.Result, error) {
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return "", nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response, page = harness.post("/settings/maintenance/rotate", url.Values{currentPasswordField: {testPassword}})
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, "A run is going") || opened.Key.ID != oldID {
		t.Fatal("the key was rotated while a run was going")
	}
	close(finish)
	harness.waitForJob("/jobs/" + running.ID)
	response, page = harness.post("/settings/maintenance/rotate", url.Values{currentPasswordField: {testPassword}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusOK, page)
	newID := opened.Key.ID
	if newID == oldID || !strings.Contains(page, newID) || !strings.Contains(page, doneNotices[doneRotated]) {
		t.Fatalf("the page does not show the new key %s", newID)
	}
	if err := opened.Store.SetSecret(ctx, "github", "token"); err != nil {
		t.Fatal(err)
	}
	usage, err := opened.Store.KeyUsage(ctx)
	if err != nil || usage[newID] != 2 || usage[oldID] != 0 {
		t.Fatalf("values by key after rotating from the web: %v %v", usage, err)
	}
	if value, _, err := opened.Store.Secret(ctx, "llm"); err != nil || value != "sk-test" {
		t.Fatalf("a stored value no longer opens: %v", err)
	}
	harness.remember(page)
	response, page = harness.post("/settings/maintenance/purge", url.Values{currentPasswordField: {testPassword}})
	status(t, response, http.StatusSeeOther, page)
	if keys, err := opened.KeyStatus(ctx); err != nil || len(keys.Retired) != 0 {
		t.Fatalf("retired keys left: %+v %v", keys, err)
	}
	latest, err := opened.Store.AuditPage(ctx, store.AuditFilter{Limit: 2})
	if err != nil || latest[0].Action != store.ActionPurgeKeys || latest[1].Action != store.ActionRotate ||
		latest[0].Actor != webActorPrefix+"admin" || latest[1].Actor != webActorPrefix+"admin" {
		t.Fatalf("rotation and purge not audited as the web user: %+v %v", latest, err)
	}
}

// TestSettingsUpdateCheck: the update check asks the release server once,
// shows the latest tag with its link or that this is it, and is audited.
func TestSettingsUpdateCheck(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	tag := "v0.2.0"
	releases := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"tag_name":"`+tag+`","html_url":"https://example.test/releases/`+tag+`","published_at":"2026-10-12T08:00:00Z"}`)
	}))
	defer releases.Close()
	target := mustURL(releases.URL)
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Release: {target.Hostname()}}, PrivateTargets: []string{target.Host}})
	if err != nil {
		t.Fatal(err)
	}
	var lookupErr error
	harness.server.options.Latest = func(ctx context.Context, _ *netguard.Guard) (release.Release, error) {
		if lookupErr != nil || tag == "" {
			return release.Release{}, lookupErr
		}
		return release.Fetch(ctx, guard, releases.URL)
	}
	response, page := harness.post("/settings/maintenance/update-check", nil)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "v0.2.0 is the latest release, published 12 Oct 2026; this is test.") || !strings.Contains(page, `href="https://example.test/releases/v0.2.0"`) {
		t.Fatal("the update check does not show the newer release and its link")
	}
	tag = "test"
	response, page = harness.post("/settings/maintenance/update-check", nil)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "composewright test is the latest release.") {
		t.Fatal("the update check does not say this is the latest release")
	}
	latest, err := harness.server.workspace().Store.AuditPage(context.Background(), store.AuditFilter{Limit: 1})
	if err != nil || latest[0].Action != store.ActionUpdateCheck {
		t.Fatalf("the update check was not audited: %+v %v", latest, err)
	}
	tag = ""
	response, page = harness.post("/settings/maintenance/update-check", nil)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, noReleaseYet) {
		t.Fatal("the update check does not say no release is published")
	}
	lookupErr = errors.New("connection refused")
	response, page = harness.post("/settings/maintenance/update-check", nil)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "The update check did not answer: connection refused.") {
		t.Fatal("the update check does not explain a failed lookup")
	}
	latest, err = harness.server.workspace().Store.AuditPage(context.Background(), store.AuditFilter{Limit: 1})
	if err != nil || latest[0].Action != store.ActionUpdateCheck || latest[0].Details["ok"] != false {
		t.Fatalf("the failed update check was not audited as failed: %+v %v", latest, err)
	}
}

func advancedSent() url.Values {
	return url.Values{"sandbox_tier": {"runsc"}, "timeout": {"5m"}, "validate_memory": {"3g"}, "validate_cpus": {"1.5"}, "validate_pids": {"256"},
		"max_iterations": {"2"}, "docker_host": {"unix:///run/user/1000/docker.sock"}, "cli_path": {""},
		"allow_hosts": {"github.com\ncodeberg.org"}, "session_ttl": {"24h"}}
}

// TestSettingsAdvanced: the Advanced page writes the sandbox, Docker, code
// host and session keys, keeps what was typed when a value is wrong, and
// stores and removes a GitHub token sealed in the database.
func TestSettingsAdvanced(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.get(settingsURL(sectionAdvanced))
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `<option value="runsc">runsc</option>`) {
		t.Fatal("the sandbox tier does not offer runsc")
	}
	sent := advancedSent()
	sent.Set("timeout", "soon")
	response, page = harness.post("/settings/advanced", sent)
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, "Run timeout must be a length of time") || !strings.Contains(page, `value="3g"`) || harness.server.config().Docker.Validate.MemoryLimit == "3g" {
		t.Fatal("a wrong timeout was saved, not explained, or lost what was typed")
	}
	response, page = harness.post("/settings/advanced", advancedSent())
	status(t, response, http.StatusSeeOther, page)
	saved := harness.server.config()
	limits := saved.Docker.Validate
	if limits.Timeout != 5*time.Minute || limits.MemoryLimit != "3g" || limits.CPUs != 1.5 || limits.PIDsLimit != 256 || limits.MaxIterations != 2 ||
		saved.Docker.SandboxTier != config.SandboxTierRunsc || saved.Docker.Host != "unix:///run/user/1000/docker.sock" ||
		len(saved.SCM.AllowHosts) != 2 || saved.UI.SessionTTL != 24*time.Hour {
		t.Fatalf("the Advanced page did not write every key: %+v %+v %+v", saved.Docker, saved.SCM, saved.UI)
	}
	ctx, database := context.Background(), harness.server.workspace().Store
	response, page = harness.post("/settings/advanced/token", url.Values{tokenField: {""}})
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, "Paste the GitHub token first.") || harness.server.config().SCM.GitHubTokenRef != "" {
		t.Fatal("an empty token was not refused")
	}
	response, page = harness.post("/settings/advanced/token", url.Values{tokenField: {"github_pat_test"}})
	status(t, response, http.StatusSeeOther, page)
	if token, _, err := database.Secret(ctx, githubKeyName); err != nil || token != "github_pat_test" || harness.server.config().SCM.GitHubTokenRef != githubTokenRef {
		t.Fatalf("the token was not stored and named: %v", err)
	}
	response, page = harness.post("/settings/advanced/token", url.Values{clearField: {"1"}})
	status(t, response, http.StatusSeeOther, page)
	stored, err := database.HasSecret(ctx, githubKeyName)
	if err != nil || stored || harness.server.config().SCM.GitHubTokenRef != "" {
		t.Fatalf("the token was not removed: %v %v", stored, err)
	}
	latest, err := database.AuditPage(ctx, store.AuditFilter{Limit: 1})
	if err != nil || latest[0].Action != store.ActionDeleteSecret || latest[0].Details["name"] != githubKeyName {
		t.Fatalf("removing the token was not audited: %+v %v", latest, err)
	}
	// A token stored with config set-secret alone is not read until the
	// configuration names it, and the card says so.
	if err := database.SetSecret(ctx, githubKeyName, "github_pat_unread"); err != nil {
		t.Fatal(err)
	}
	response, page = harness.get(settingsURL(sectionAdvanced))
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "stored, not used") || strings.Contains(page, "stored, encrypted") {
		t.Fatal("a stored token nothing reads is shown as in use")
	}
}

// TestSettingsCatalogueOverrides: a backup carries the catalogue.d files,
// and restoring it on another install writes them back readable only by
// composewright. Its Advanced page then lists each file with what it adds,
// the one left out with its problem, and the folder, and its next
// generation takes the override's healthcheck, cited as the catalogue's.
func TestSettingsCatalogueOverrides(t *testing.T) {
	const passphrase = "a long backup passphrase"
	healthcheck := []string{"CMD-SHELL", "/usr/bin/miniflux -healthcheck auto || exit 1"}
	files := map[string]string{
		"miniflux.yaml": "apps:\n  - images: [miniflux/miniflux]\n    healthcheck: [\"CMD-SHELL\", \"/usr/bin/miniflux -healthcheck auto || exit 1\"]\n",
		"broken.yaml":   "apps:\n  - images: [ghcr.io/x/broken]\n    memory_limit: lots\n",
	}
	source := newHarness(t)
	source.signUp()
	if err := os.MkdirAll(source.server.paths().CatalogueDir(), config.DirMode); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(source.server.paths().CatalogueDir(), name), []byte(content), config.FileMode); err != nil {
			t.Fatal(err)
		}
	}
	source.setBackupPassphrase(passphrase)
	response, page := source.post("/settings/backup/now", nil)
	status(t, response, http.StatusSeeOther, page)
	download := url.Values{backupNameField: {source.storedBackups()[0].Name}, currentPasswordField: {testPassword}}
	response, content := source.post("/settings/backup/download", download)
	status(t, response, http.StatusOK, "")

	target := newHarness(t)
	target.signUp()
	response, page = target.restoreFrom(content, passphrase, testPassword, false)
	status(t, response, http.StatusSeeOther, page)
	directory := target.server.paths().CatalogueDir()
	for name, want := range files {
		written, err := os.ReadFile(filepath.Join(directory, name))
		info, statErr := os.Stat(filepath.Join(directory, name))
		if err != nil || statErr != nil || string(written) != want || info.Mode().Perm() != config.FileMode {
			t.Fatalf("%s was not written back as it was, readable only by composewright: %v %v", name, err, statErr)
		}
	}
	response, page = target.get(settingsURL(sectionAdvanced))
	status(t, response, http.StatusOK, page)
	for _, want := range []string{`<th scope="row" class="mono">miniflux.yaml</th><td>0</td><td>1</td>`, "broken.yaml: app ghcr.io/x/broken: memory_limit",
		"1 problem", directory} {
		if !strings.Contains(page, want) {
			t.Errorf("the Catalogue overrides card lacks %q", want)
		}
	}
	target.remember(page)
	response, page = target.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	target.waitForJob(response.Header.Get("Location"))
	stored, err := pipeline.Load(context.Background(), target.server.workspace(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	app, _ := stored.Plan.Service("miniflux")
	if app == nil || !slices.Equal(app.Healthcheck.Test, healthcheck) || app.Healthcheck.Provenance.Source != ir.SourceHealthchecks {
		t.Fatalf("the generation after the restore did not take the override: %+v", app)
	}
}

func (harness *harness) importProfile(content []byte) (*http.Response, string) {
	harness.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField(csrfField, harness.token)
	part, _ := writer.CreateFormFile(profileFileField, profileFile)
	_, _ = part.Write(content)
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, harness.http.URL+"/settings/profile/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := harness.client.Do(request)
	if err != nil {
		harness.t.Fatal(err)
	}
	defer response.Body.Close()
	page, _ := io.ReadAll(response.Body)
	return response, string(page)
}

// TestSettingsProfileRoundTrip: the exported profile.yaml is what profile
// export writes, and importing an edited one replaces the profile.
func TestSettingsProfileRoundTrip(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, exported := harness.get("/settings/profile/export")
	status(t, response, http.StatusOK, exported)
	want, err := config.RenderProfile(harness.server.config().Profile)
	if err != nil || exported != string(want) {
		t.Fatalf("the download differs from profile export: %v", err)
	}
	profile := harness.server.config().Profile
	profile.Defaults.TZ = "Europe/London"
	profile.Networks = []config.ProfileNetwork{{Name: "edge", Kind: "proxy", Provider: "traefik", Default: true}}
	edited, err := config.RenderProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	response, page := harness.importProfile(edited)
	status(t, response, http.StatusSeeOther, page)
	if _, exported = harness.get("/settings/profile/export"); exported != string(edited) {
		t.Fatalf("the imported profile does not export as it was imported:\n%s", exported)
	}
	response, page = harness.importProfile([]byte("defaults:\n  restart: sometimes\n"))
	status(t, response, http.StatusBadRequest, page)
	if harness.server.config().Profile.Defaults.TZ != "Europe/London" {
		t.Fatal("an invalid profile changed the saved one")
	}
}

// TestSettingsDockerChecks: Run checks shows doctor's checks on the page;
// a Docker CLI that does not run fails them.
func TestSettingsDockerChecks(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	if err := config.SetInFile(harness.server.paths().ConfigFile, "docker.cli_path", `"/nonexistent/docker"`); err != nil {
		t.Fatal(err)
	}
	if err := harness.server.reload(); err != nil {
		t.Fatal(err)
	}
	response, page := harness.post("/settings/maintenance/doctor", nil)
	status(t, response, http.StatusOK, page)
	for _, want := range []string{"data and key", "audit log", "docker daemon", `<span class="chip chip-danger">fail</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Docker checks lack %q", want)
		}
	}
}
