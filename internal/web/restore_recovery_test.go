package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/backup"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

func TestInterruptedRestoreResumesWithoutRepeatingProjects(t *testing.T) {
	server := intakeServer(t)
	server.options.Now = time.Now
	server.options.Flags = config.Flags{ConfigFile: server.paths().ConfigFile, DataDir: server.paths().DataDir}
	server.status = newStatusCache(server)
	server.jobs = newJobs(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.options.Connect = offline
	ctx := context.Background()
	source := t.TempDir()
	for name, content := range map[string]string{"compose.yaml": "services:\n  app:\n    image: docker.io/library/nginx:1.27-alpine\n    environment: {APP_PASSWORD: '${APP_PASSWORD}'}\n", ".env": "APP_PASSWORD=original-dummy-secret\n"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	connection, err := server.connect(ctx, "test", pipeline.EngineRules, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pipeline.Generate(ctx, connection.Dependencies, pipeline.Options{Source: source, Name: "restore"}); err != nil {
		t.Fatal(err)
	}
	archive, err := backup.Collect(ctx, server.workspace().Store, server.config(), server.paths().CatalogueDir(), "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	archive.Settings.Theme = config.ThemeLight
	archive.Projects[0].Variables[0].Value = "restored-dummy-secret"
	if err = server.prepareRestore(ctx, archive, "synthetic", true); err != nil {
		t.Fatal(err)
	}
	path := server.paths().ConfigFile
	if err = os.Rename(path, path+".before"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/settings/backup/resume", nil)
	current := &visit{user: store.User{Username: "test"}}
	if _, _, err = server.resumeRestore(request, current); err == nil {
		t.Fatal("filesystem failure not reported")
	}
	journal, found, err := server.restoreJournal(ctx)
	if err != nil || !found || journal.Completed {
		t.Fatal("lost pending journal")
	}
	if journal.Before.Projects[0].Variables[0].Value != "original-dummy-secret" {
		t.Fatal("original credential lost")
	}
	records, err := server.workspace().Store.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := records[0].Project.ID
	if records[0].Variables[0].Value != "restored-dummy-secret" {
		t.Fatal("database stage not committed")
	}
	view, err := server.backupView(ctx, server.config().Backup)
	if err != nil || !view.RestorePending || view.CanCancelRestore {
		t.Fatalf("incorrect recovery UI: %+v %v", view, err)
	}
	// Repair the injected filesystem fault and simulate a restarted job registry.
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".before", path); err != nil {
		t.Fatal(err)
	}
	if err = server.workspace().Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := workspace.Open(ctx, server.loaded)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{Loaded: server.loaded, Flags: server.options.Flags, Workspace: opened, Connect: offline, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close(); opened.Close() })
	server = restarted
	if !server.jobs.restorePending() {
		t.Fatal("restart forgot pending restore")
	}
	if _, err = server.jobs.start(jobGenerate, "refused", "new", func(context.Context, *job) (string, *pipeline.Result, error) { panic("must not run") }); err == nil {
		t.Fatal("pending restore admitted work")
	}

	restored, _, err := server.resumeRestore(request, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || server.jobs.restorePending() {
		t.Fatal("resume did not finish")
	}
	records, err = server.workspace().Store.ProjectRecords(ctx)
	if err != nil || records[0].Project.ID != id || len(records[0].Generations) != 1 {
		t.Fatal("committed project was repeated")
	}
	journal, _, err = server.restoreJournal(ctx)
	if err != nil || !journal.Completed {
		t.Fatal("completion not durable")
	}
	current.user.PasswordHash, err = auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{currentPasswordField: {testPassword}, passphraseField: {"long synthetic recovery passphrase"}, confirmField: {"long synthetic recovery passphrase"}}
	recoveryRequest := httptest.NewRequest(http.MethodPost, "/settings/backup/recovery", strings.NewReader(form.Encode()))
	recoveryRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	downloaded := httptest.NewRecorder()
	if err = server.downloadRestoreRecovery(downloaded, recoveryRequest, current); err != nil {
		t.Fatal(err)
	}
	recovered, err := backup.Open(downloaded.Body.Bytes(), "long synthetic recovery passphrase")
	if err != nil || recovered.Projects[0].Variables[0].Value != "original-dummy-secret" {
		t.Fatal("recovery export lost original")
	}

	raw, _ := os.ReadFile(server.paths().Database())
	if bytes.Contains(raw, []byte("original-dummy-secret")) || bytes.Contains(raw, []byte("restored-dummy-secret")) {
		t.Fatal("journal or project plaintext in database")
	}
	// Corrupt future imports must not replace the completed recovery journal.
	before, _ := json.Marshal(journal)
	beforeConfig, _ := os.ReadFile(server.paths().ConfigFile)
	bad := archive
	bad.Projects = append([]store.ProjectRecord(nil), archive.Projects...)
	bad.Projects[0].Generations = append([]store.GenerationRecord(nil), archive.Projects[0].Generations...)
	bad.Projects[0].Generations[0].Generation.IRJSON = "{}"
	if err = server.prepareRestore(ctx, bad, "bad", true); err == nil {
		t.Fatal("incompatible archive accepted")
	}
	after, _, _ := server.restoreJournal(ctx)
	encoded, _ := json.Marshal(after)
	if !bytes.Equal(before, encoded) {
		t.Fatal("invalid import mutated recovery state")
	}
	afterConfig, _ := os.ReadFile(server.paths().ConfigFile)
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("invalid import changed settings")
	}
	// A prepared operation can be cancelled before any restored DB writes.
	if err = server.prepareRestore(ctx, archive, "cancelled", true); err != nil {
		t.Fatal(err)
	}
	cancelRequest := httptest.NewRequest(http.MethodPost, "/settings/backup/cancel", strings.NewReader(url.Values{currentPasswordField: {testPassword}}.Encode()))
	cancelRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cancelled := httptest.NewRecorder()
	if err = server.cancelBackupRestore(cancelled, cancelRequest, current); err != nil {
		t.Fatal(err)
	}
	if server.jobs.restorePending() || cancelled.Code != 303 {
		t.Fatal("cancel did not release prepared restore")
	}
	final, err := server.workspace().Store.ProjectRecords(ctx)
	if err != nil || final[0].Project.ID != id {
		t.Fatal("cancel changed a project")
	}

}
