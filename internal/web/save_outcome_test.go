package web

import (
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

	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
)

func TestSavedEditSurvivesFullValidationQueue(t *testing.T) {
	server := intakeServer(t)
	server.options.Connect = func(ctx context.Context, setup pipeline.Setup) (pipeline.Connection, error) {
		if setup.Validate {
			<-ctx.Done()
			return pipeline.Connection{}, ctx.Err()
		}
		return offline(ctx, setup)
	}
	server.jobs = newJobs(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { server.jobs.shutdown(time.Second) })
	current := &visit{user: store.User{Username: "test"}}
	ctx := context.Background()
	connection, err := server.connect(ctx, "test", pipeline.EngineRules, false)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "compose.yaml"), []byte("services:\n  app:\n    image: docker.io/library/nginx:1.27-alpine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	generated, err := pipeline.Generate(ctx, connection.Dependencies, pipeline.Options{Source: source, Name: "saved"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := pipeline.Load(ctx, server.workspace(), "saved")
	if err != nil {
		t.Fatal(err)
	}
	shown, err := env.EnvFile(webMask)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []*job
	for _, name := range []string{"other-one", "other-two"} {
		started, err := server.jobs.start(jobValidate, name, name, func(ctx context.Context, _ *job) (string, *pipeline.Result, error) {
			<-ctx.Done()
			return "", nil, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		blocked = append(blocked, started)
	}
	submit := func(compose, base string, retest bool) *httptest.ResponseRecorder {
		form := url.Values{fileCompose: {compose}, fileEnv: {string(shown)}, baseField: {base}, retestField: {""}}
		if retest {
			form.Set(retestField, "1")
		}
		request := httptest.NewRequest(http.MethodPost, "/projects/saved/files/save", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.SetPathValue("name", "saved")
		response := httptest.NewRecorder()
		if err := server.saveFiles(response, request, current); err != nil {
			t.Fatalf("save returned failure after persistence: %v", err)
		}
		return response
	}
	edited := generated.Generation.ComposeYAML + "\n# saved under a full test queue\n"
	response := submit(edited, generated.Generation.ID, true)
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var answer saveFilesResult
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Saved.ID == "" || answer.Saved.Number != generated.Generation.Number+1 || answer.Validation.Started || !answer.Validation.Requested || answer.Validation.Retry != "/projects/saved/validate" {
		t.Fatalf("outcome: %+v", answer)
	}
	saved, err := pipeline.Load(ctx, server.workspace(), "saved")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Generation.ID != answer.Saved.ID || saved.Generation.ComposeYAML != edited {
		t.Fatal("response is not the committed version")
	}
	normal := submit(edited, saved.Generation.ID, false)
	var normalAnswer saveFilesResult
	if err := json.Unmarshal(normal.Body.Bytes(), &normalAnswer); err != nil {
		t.Fatal(err)
	}
	if normal.Code != 200 || normalAnswer.Next != "/projects/saved/files" || normalAnswer.Validation.Requested || normalAnswer.Saved.ID != saved.Generation.ID {
		t.Fatalf("ordinary save: %+v", normalAnswer)
	}
	stale := submit(edited, generated.Generation.ID, true)
	if stale.Code != 409 {
		t.Fatalf("stale status: %d", stale.Code)
	}
	if len(server.jobs.byID) != 2 {
		t.Fatal("a refused or already saved edit started an extra job")
	}
	// Refused persistence must not create a generation or start another job.
	refused := submit("services: [", saved.Generation.ID, true)
	if refused.Code != 422 {
		t.Fatalf("refused status: %d", refused.Code)
	}
	latest, err := pipeline.Load(ctx, server.workspace(), "saved")
	if err != nil || latest.Generation.ID != saved.Generation.ID {
		t.Fatal("refused edit persisted")
	}
	// The refreshed page names the version and offers POST validation only.
	request := httptest.NewRequest(http.MethodGet, answer.Next, nil)
	request.SetPathValue("name", "saved")
	refreshed := httptest.NewRecorder()
	if err := server.filesPage(refreshed, request, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refreshed.Body.String(), "Retry test of saved v") {
		t.Fatal("no retry action on refreshed saved files")
	}
	for _, running := range blocked {
		running.cancel()
	}
	server.jobs.wait.Wait()
	request = httptest.NewRequest(http.MethodPost, answer.Validation.Retry, nil)
	request.SetPathValue("name", "saved")
	retry := httptest.NewRecorder()
	if err := server.validate(retry, request, current); err != nil {
		t.Fatal(err)
	}
	if retry.Code != 303 {
		t.Fatalf("retry status %d", retry.Code)
	}
	latest, err = pipeline.Load(ctx, server.workspace(), "saved")
	if err != nil || latest.Generation.ID != saved.Generation.ID {
		t.Fatal("retry created an edit version")
	}
}
