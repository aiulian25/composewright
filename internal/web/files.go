package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	fileCompose = "compose"
	fileEnv     = "env"
	fileDeploy  = "deploy"

	locationProfile = "profile"
	locationData    = "data"
	locationArcane  = "arcane"
	locationCustom  = "custom"
	exportsDir      = "exports"
	arcanePrefix    = "arcane:"
	zipFileMode     = 0o600
	plainText       = "text/plain; charset=utf-8"
	zipType         = "application/zip"
	webMask         = "<hidden: Reveal secrets shows it>"
	// retestField asks a side-by-side save to start a test run as well.
	retestField = "retest"
)

type filesView struct {
	Project    store.Project
	Generation store.Generation
	Passed     bool
	// Failed is a project whose last test failed; the page links to why.
	Failed       bool
	Tone         string
	Headline     string
	Summary      string
	Tab          string
	Revealed     bool
	Masked       bool
	Folder       string
	Locations    []option
	Custom       string
	Preview      string
	Target       string
	ComposeLines []badgedLine
	EnvLines     []badgedLine
	DeployLines  []badgedLine
	ComposeText  string
	Versions     []versionRow
	// The side-by-side editor: the .env as shown, each compose.yaml line's
	// provenance badge, the number the next version gets, and whether the
	// latest version is an edit not tested yet.
	EnvText           string
	ComposeProvenance string
	NextVersion       int
	EditedUntested    bool
}

// provenanceMark is a compose.yaml line's badge in the side-by-side editor.
type provenanceMark struct {
	Badge string `json:"badge"`
	Tone  string `json:"tone"`
}

// versionRow is one version of a project, newest first.
type versionRow struct {
	ID      string
	Number  int
	Result  string
	Tone    string
	Change  string
	Engine  string
	When    time.Time
	Current bool
	// Tested versions link to their run; Detail is why a failed one failed.
	Tested bool
	Detail string
}

type filesState struct {
	revealed bool
	tab      string
	notice   string
	problem  string
	status   int
	folder   string
	location string
	custom   string
}

func (server *Server) filesPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	return server.renderFiles(writer, request, current, stored, filesState{tab: request.URL.Query().Get("file")})
}

func (server *Server) renderFiles(writer http.ResponseWriter, request *http.Request, current *visit, stored *pipeline.Stored, state filesState) error {
	view, err := server.filesView(request.Context(), stored, state)
	if err != nil {
		return err
	}
	page := server.page(request, current, stored.Project.Name+" files").flow(stepDone).of(stored.Project.Name)
	page.CLI = "composewright export " + stored.Project.Name + " --to " + view.Target
	page.Notice, page.Error, page.Data = state.notice, state.problem, view
	status := state.status
	if status == 0 {
		status = http.StatusOK
	}
	return server.render(writer, status, "files", page)
}

func (server *Server) filesView(ctx context.Context, stored *pipeline.Stored, state filesState) (filesView, error) {
	tab := state.tab
	if tab != fileEnv && tab != fileDeploy {
		tab = fileCompose
	}
	project := stored.Project
	view := filesView{Project: project, Generation: stored.Generation, Tab: tab, Revealed: state.revealed, Folder: state.folder,
		Custom: state.custom, Passed: project.Status == store.StatusPassed, ComposeText: stored.Generation.ComposeYAML,
		Failed: project.Status == store.StatusFailed || project.Status == store.StatusNeedsHuman}
	if view.Folder == "" {
		view.Folder = project.Name
	}
	view.Tone, view.Headline, view.Summary = filesBanner(stored)
	env, err := fileText(stored, fileEnv, state.revealed)
	if err != nil {
		return view, err
	}
	view.Masked = !state.revealed && strings.Contains(env, webMask)
	view.ComposeLines = server.composeView(stored)
	view.EnvLines, view.DeployLines = plainLines(env, languageEnv), plainLines(stored.Generation.DeployMD, languageMarkdown)
	view.Locations = server.locationOptions(state.location)
	view.Preview = server.savePreview(view.Folder, state.location, state.custom)
	if target, err := server.target(view.Folder, state.location, state.custom); err == nil {
		view.Target = target
	}
	generations, err := server.workspace().Store.Generations(ctx, project.ID)
	if err != nil {
		return view, err
	}
	view.Versions = versionRows(generations, stored.Generation.ID)
	view.EnvText, view.NextVersion = env, len(generations)+1
	_, tested := storedVerdict(stored.Generation)
	view.EditedUntested = stored.Options.Edited && !tested
	marks := make([]provenanceMark, 0, len(view.ComposeLines))
	for _, line := range view.ComposeLines {
		marks = append(marks, provenanceMark{Badge: line.Badge, Tone: line.Tone})
	}
	encoded, err := json.Marshal(marks)
	view.ComposeProvenance = string(encoded)
	return view, err
}

// storedVerdict reads the sandbox verdict kept with a version, if it was
// tested.
func storedVerdict(generation store.Generation) (sandbox.Verdict, bool) {
	var verdict sandbox.Verdict
	if generation.ValidationJSON == "" || json.Unmarshal([]byte(generation.ValidationJSON), &verdict) != nil {
		return verdict, false
	}
	return verdict, verdict.Result != ""
}

// testedRun is the newest version that ran in the sandbox, or the one a
// link names. The plan may have changed since it ran.
func testedRun(generations []store.Generation, wanted string) (store.Generation, sandbox.Verdict, bool) {
	for _, generation := range generations {
		verdict, tested := storedVerdict(generation)
		if tested && (wanted == "" || generation.ID == wanted) {
			return generation, verdict, true
		}
	}
	return store.Generation{}, sandbox.Verdict{}, false
}

// failureText names a failed run the way the sandbox judged it.
func failureText(verdict sandbox.Verdict) string {
	return fmt.Sprintf("FAIL (%s): %s", verdict.Reason, verdict.Detail)
}

// versionRows lists a project's versions with what each one changed: the
// first plan, a Review change, an edit of compose.yaml or a repair.
func versionRows(generations []store.Generation, current string) []versionRow {
	rows := make([]versionRow, 0, len(generations))
	for index, generation := range generations {
		row := versionRow{ID: generation.ID, Number: len(generations) - index, Engine: generation.Engine, When: generation.CreatedAt,
			Current: generation.ID == current, Result: "not tested", Tone: toneNeutral, Change: "Review changes"}
		var options pipeline.Options
		_ = json.Unmarshal([]byte(generation.OptionsJSON), &options)
		switch {
		case options.Edited:
			row.Change = "Edited by you"
		case generation.Iteration > 1:
			row.Change = fmt.Sprintf("Repair, attempt %d", generation.Iteration)
		case row.Number == 1:
			row.Change = "Initial plan"
		}
		verdict, tested := storedVerdict(generation)
		row.Tested = tested
		if tested && verdict.Passed() {
			row.Result, row.Tone = "passed", toneOk
		}
		if tested && !verdict.Passed() {
			row.Result, row.Tone, row.Detail = "failed", toneDanger, verdict.Detail
		}
		rows = append(rows, row)
	}
	return rows
}

// checkFiles answers the side-by-side editor's live check.
func (server *Server) checkFiles(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	connection, err := server.connect(ctx, current.user.Username, pipeline.EngineRules, false)
	if err != nil {
		return err
	}
	check, err := pipeline.CheckEdit(ctx, connection.Dependencies, stored, editOf(request))
	if err != nil {
		return err
	}
	return writeJSON(writer, http.StatusOK, check)
}

// saveFiles stores a side-by-side edit as a new version, and with retest
// starts its test run. A refused edit answers 422 with the reasons, checked
// again here rather than trusted from the page.
func (server *Server) saveFiles(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	name := request.PathValue("name")
	if err := server.busy(name); err != nil {
		return err
	}
	connection, err := server.connect(ctx, current.user.Username, pipeline.EngineRules, false)
	if err != nil {
		return err
	}
	_, check, err := pipeline.SaveEdit(ctx, connection.Dependencies, name, editOf(request))
	if errors.Is(err, pipeline.ErrEditRefused) {
		return writeJSON(writer, http.StatusUnprocessableEntity, check)
	}
	if err != nil {
		return err
	}
	next := projectURL(name, pageFiles)
	if request.PostFormValue(retestField) != "" {
		next, err = server.launchValidation(ctx, current, name)
	}
	if err != nil {
		return err
	}
	return writeJSON(writer, http.StatusOK, map[string]string{"next": next})
}

func editOf(request *http.Request) pipeline.Edit {
	return pipeline.Edit{Compose: []byte(request.PostFormValue(fileCompose)), Env: request.PostFormValue(fileEnv), Mask: webMask}
}

func writeJSON(writer http.ResponseWriter, status int, value any) error {
	content, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writer.Header().Set(contentType, jsonType)
	writer.WriteHeader(status)
	_, err = writer.Write(content)
	return err
}

func fileText(stored *pipeline.Stored, key string, revealed bool) (string, error) {
	switch key {
	case fileEnv:
		mask := webMask
		if revealed {
			mask = pipeline.Revealed
		}
		env, err := stored.EnvFile(mask)
		return string(env), err
	case fileDeploy:
		return stored.Generation.DeployMD, nil
	}
	return stored.Generation.ComposeYAML, nil
}

func filesBanner(stored *pipeline.Stored) (tone, headline, summary string) {
	project, plan := stored.Project, stored.Plan
	secrets := 0
	for _, entry := range plan.EnvFile {
		if entry.Secret {
			secrets++
		}
	}
	summary = countText(len(plan.Services), "service", "services") + " · " + countText(len(plan.EnvFile), "variable", "variables") +
		" (" + countText(secrets, "secret", "secrets") + ") · stored encrypted"
	switch project.Status {
	case store.StatusPassed:
		return toneOk, passedText(stored.Generation), "Tested in a sandbox, which has been removed · " + summary
	case store.StatusBlocked:
		return toneDanger, "Blocked by the policy gate", "Resolve the findings on the Review page · " + summary
	case store.StatusFailed, store.StatusNeedsHuman:
		return toneDanger, "Did not pass validation", summary
	}
	return toneInfo, "Not tested", "You can test it from the Review page any time · " + summary
}

func passedText(generation store.Generation) string {
	verdict, found := storedVerdict(generation)
	if !found {
		return "Passed"
	}
	return fmt.Sprintf("Passed on attempt %d, %s", verdict.Iteration, verdict.Duration.Round(durationPrecision))
}

// locationOptions are where Save may write: the profile's save folder,
// the data directory, an Arcane templates folder, or any folder.
func (server *Server) locationOptions(current string) []option {
	profile := server.config().Profile
	saveDir := strings.ReplaceAll(profile.Storage.SaveDir, config.LayoutProject, "<folder>")
	saveDir = strings.ReplaceAll(saveDir, config.LayoutBase, profile.Storage.BasePath)
	profileLabel := "Profile save folder: " + saveDir
	if profile.Storage.BasePath == "" {
		profileLabel = "Profile save folder (set a base path in Settings › Paths)"
	}
	options := []option{
		{Value: locationData, Label: filepath.Join(server.paths().DataDir, exportsDir) + "/"},
		{Value: locationProfile, Label: profileLabel, Disabled: profile.Storage.BasePath == ""},
		{Value: locationArcane, Label: "Arcane templates folder (secrets left out)"},
		{Value: locationCustom, Label: "Another folder"},
	}
	if current == "" {
		current = locationData
	}
	for index := range options {
		options[index].Selected = options[index].Value == current
	}
	return options
}

// target is the export destination a Save form names.
func (server *Server) target(folder, location, custom string) (string, error) {
	if !projectName.MatchString(folder) {
		return "", badRequest("A folder name uses lowercase letters, digits, - and _.")
	}
	profile := server.config().Profile
	switch location {
	case locationProfile:
		if profile.Storage.BasePath == "" {
			return "", badRequest("Set a base path in Settings › Paths first.")
		}
		return strings.NewReplacer(config.LayoutBase, profile.Storage.BasePath, config.LayoutProject, folder).Replace(profile.Storage.SaveDir), nil
	case locationArcane, locationCustom:
		if !filepath.IsAbs(custom) {
			return "", badRequest("Give an absolute folder path.")
		}
		if location == locationArcane {
			return arcanePrefix + filepath.Clean(custom), nil
		}
		return filepath.Join(filepath.Clean(custom), folder), nil
	}
	return filepath.Join(server.paths().DataDir, exportsDir, folder), nil
}

func (server *Server) savePreview(folder, location, custom string) string {
	target, err := server.target(folder, location, custom)
	if err != nil {
		return ""
	}
	if templates, arcane := strings.CutPrefix(target, arcanePrefix); arcane {
		return filepath.Join(templates, folder) + "/{compose.yaml, .env.example}"
	}
	return target + "/{compose.yaml, .env (0600), DEPLOY.md}"
}

func (server *Server) save(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	state := filesState{folder: strings.TrimSpace(request.PostFormValue("folder")), location: request.PostFormValue("location"),
		custom: strings.TrimSpace(request.PostFormValue("custom")), tab: request.PostFormValue("file")}
	target, err := server.target(state.folder, state.location, state.custom)
	if err == nil {
		var written export.Written
		written, err = pipeline.Export(ctx, server.as(current.user.Username), stored, target, request.PostFormValue("overwrite") != "", viaWeb)
		state.notice = "Saved to " + written.Directory + "."
	}
	if err != nil {
		state.notice, state.problem, state.status = "", capitalised(err.Error())+".", http.StatusBadRequest
	}
	return server.renderFiles(writer, request, current, stored, state)
}

// copyEnv hands the .env with its secrets to the page's copy button and
// records that in the audit log.
func (server *Server) copyEnv(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	env, err := stored.EnvFile(pipeline.Revealed)
	if err != nil {
		return err
	}
	details := map[string]any{"generation": stored.Generation.ID, "via": "clipboard"}
	if err := server.workspace().Store.Audit(ctx, webActorPrefix+current.user.Username, store.ActionExport, stored.Project.ID, details); err != nil {
		return err
	}
	writer.Header().Set(contentType, plainText)
	_, err = writer.Write(env) //nolint:gosec // G705: served as text/plain with nosniff; the .env is exactly what was asked for
	return err
}

// download sends compose.yaml, .env and DEPLOY.md as a zip; the .env
// entry is marked 0600.
func (server *Server) download(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	bundle, err := stored.Bundle()
	if err != nil {
		return err
	}
	var archive bytes.Buffer
	zipped := zip.NewWriter(&archive)
	modified := stored.Generation.CreatedAt
	if modified.IsZero() {
		modified = time.Now()
	}
	for _, file := range []struct {
		name    string
		content []byte
	}{{export.ComposeName, bundle.Compose}, {export.EnvName, bundle.Env}, {export.DeployName, bundle.Deploy}} {
		header := &zip.FileHeader{Name: bundle.Project + "/" + file.name, Method: zip.Deflate, Modified: modified}
		header.SetMode(zipFileMode)
		entry, err := zipped.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := entry.Write(file.content); err != nil {
			return err
		}
	}
	if err := zipped.Close(); err != nil {
		return err
	}
	details := map[string]any{"generation": stored.Generation.ID, "via": "download", "contains_secrets": true}
	if err := server.workspace().Store.Audit(ctx, webActorPrefix+current.user.Username, store.ActionExport, stored.Project.ID, details); err != nil {
		return err
	}
	writer.Header().Set(contentType, zipType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+bundle.Project+`.zip"`)
	_, err = archive.WriteTo(writer)
	return err
}
