package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/export"
	"github.com/aiulian25/composewright/internal/ir"
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
	zipFolderMode   = 0o700
	// folderFiles and arcaneFiles are how many files a save writes besides
	// the secret files: compose.yaml, .env and DEPLOY.md, or compose.yaml
	// and .env.example for Arcane.
	folderFiles = 3
	arcaneFiles = 2
	plainText   = "text/plain; charset=utf-8"
	zipType     = "application/zip"
	webMask     = "<hidden: Reveal secrets shows it>"
	// retestField asks a side-by-side save to start a test run as well.
	retestField = "retest"
	// The Save form's fields, also read from the address once the folder
	// chooser picked a folder; changeParameter opens the chooser.
	fieldFile       = "file"
	fieldFolder     = "folder"
	fieldLocation   = "location"
	fieldCustom     = "custom"
	fieldOverwrite  = "overwrite"
	changeParameter = "change"
)

type filesView struct {
	Project    store.Project
	Generation store.Generation
	Passed     bool
	// Failed is a project whose last test failed; the page links to why.
	Failed    bool
	Tone      string
	Headline  string
	Summary   string
	Tab       string
	Revealed  bool
	Masked    bool
	Folder    string
	Location  string
	Locations []option
	Custom    string
	Target    string
	// TargetPath is the folder Save writes to as a person reads it; Arcane
	// gets compose.yaml and a .env.example without secrets. Choosing shows
	// the folder chooser, Replace asks before replacing files from an
	// earlier save, and Saved names the folder just written.
	TargetPath string
	Arcane     bool
	Choosing   bool
	Replace    bool
	Saved      string
	// SecretFiles are the secrets passed as files a folder save writes;
	// Saving counts every file it writes.
	SecretFiles  []string
	Saving       int
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
	choosing bool
	replace  bool
	saved    string
}

// filesPage shows the Result page; the folder chooser's choice arrives in
// the address, and one that names no usable folder reopens the chooser.
func (server *Server) filesPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	query := request.URL.Query()
	state := filesState{tab: query.Get(fieldFile), choosing: query.Has(changeParameter), folder: strings.TrimSpace(query.Get(fieldFolder)),
		location: query.Get(fieldLocation), custom: strings.TrimSpace(query.Get(fieldCustom))}
	if state.folder == "" {
		state.folder = stored.Project.Name
	}
	if _, err := server.target(state.folder, state.location, state.custom); err != nil {
		state.problem, state.status, state.choosing = capitalised(err.Error())+".", http.StatusBadRequest, true
	}
	return server.renderFiles(writer, request, current, stored, state)
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
		Location: state.location, Custom: state.custom, Passed: project.Status == store.StatusPassed, ComposeText: stored.Generation.ComposeYAML,
		Failed: project.Status == store.StatusFailed || project.Status == store.StatusNeedsHuman, Choosing: state.choosing,
		Replace: state.replace, Saved: state.saved}
	if view.Folder == "" {
		view.Folder = project.Name
	}
	if view.Location == "" {
		view.Location = locationData
	}
	view.Tone, view.Headline, view.Summary = filesBanner(stored)
	env, err := fileText(stored, fileEnv, state.revealed)
	if err != nil {
		return view, err
	}
	view.Masked = !state.revealed && strings.Contains(env, webMask)
	view.ComposeLines = server.composeView(stored)
	view.EnvLines, view.DeployLines = plainLines(env, languageEnv), plainLines(stored.Generation.DeployMD, languageMarkdown)
	view.Locations = server.locationOptions(view.Location)
	if target, err := server.target(view.Folder, view.Location, view.Custom); err == nil {
		view.Target = target
		view.TargetPath, view.Arcane = shownTarget(target, view.Folder)
	}
	view.SecretFiles = slices.Sorted(maps.Keys(stored.Plan.SecretFiles(nil)))
	view.Saving = folderFiles + len(view.SecretFiles)
	if view.Arcane {
		view.Saving = arcaneFiles
	}
	generations, err := server.workspace().Store.Generations(ctx, project.ID)
	if err != nil {
		return view, err
	}
	view.Versions = versionRows(generations, stored.Generation.ID)
	view.EnvText, view.NextVersion = env, stored.Generation.Number+1
	_, tested := pipeline.StoredVerdict(stored.Generation)
	view.EditedUntested = stored.Options.Edited && !tested
	marks := make([]provenanceMark, 0, len(view.ComposeLines))
	for _, line := range view.ComposeLines {
		marks = append(marks, provenanceMark{Badge: line.Badge, Tone: line.Tone})
	}
	encoded, err := json.Marshal(marks)
	view.ComposeProvenance = string(encoded)
	return view, err
}

// testedRun is the newest version that ran in the sandbox, or the one a
// link names. The plan may have changed since it ran.
func testedRun(generations []store.Generation, wanted string) (store.Generation, sandbox.Verdict, bool) {
	for _, generation := range generations {
		verdict, tested := pipeline.StoredVerdict(generation)
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
	for _, generation := range generations {
		row := versionRow{ID: generation.ID, Number: generation.Number, Engine: generation.Engine, When: generation.CreatedAt,
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
		verdict, tested := pipeline.StoredVerdict(generation)
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
	summary = countText(len(plan.Services), serviceSingular, servicePlural) + " · " + countText(len(plan.EnvFile), "variable", "variables") +
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
	verdict, found := pipeline.StoredVerdict(generation)
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
	if !store.ValidProjectName(folder) {
		return "", badRequest("A folder name uses lowercase letters, digits, - and _")
	}
	profile := server.config().Profile
	switch location {
	case locationProfile:
		if profile.Storage.BasePath == "" {
			return "", badRequest("Set a base path in Settings › Paths first")
		}
		return strings.NewReplacer(config.LayoutBase, profile.Storage.BasePath, config.LayoutProject, folder).Replace(profile.Storage.SaveDir), nil
	case locationArcane, locationCustom:
		if !filepath.IsAbs(custom) {
			return "", badRequest("Give an absolute folder path")
		}
		if location == locationArcane {
			return arcanePrefix + filepath.Clean(custom), nil
		}
		return filepath.Join(filepath.Clean(custom), folder), nil
	}
	return filepath.Join(server.paths().DataDir, exportsDir, folder), nil
}

// shownTarget is a save target as a person reads it: an Arcane target is
// the templates folder, which gets the project's folder.
func shownTarget(target, folder string) (string, bool) {
	templates, arcane := strings.CutPrefix(target, arcanePrefix)
	if arcane {
		return filepath.Join(templates, folder), true
	}
	return target, false
}

func (server *Server) save(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	state := filesState{folder: strings.TrimSpace(request.PostFormValue(fieldFolder)), location: request.PostFormValue(fieldLocation),
		custom: strings.TrimSpace(request.PostFormValue(fieldCustom)), tab: request.PostFormValue(fieldFile)}
	target, err := server.target(state.folder, state.location, state.custom)
	if err == nil {
		var written export.Written
		written, err = pipeline.Export(ctx, server.as(current.user.Username), stored, target, request.PostFormValue(fieldOverwrite) != "", viaWeb)
		state.saved = written.Directory
	}
	switch {
	case errors.Is(err, export.ErrExists):
		state.saved, state.replace, state.status = "", true, http.StatusConflict
	case err != nil:
		state.saved, state.problem, state.status = "", capitalised(err.Error())+".", http.StatusBadRequest
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

// zipEntry is one file of a download, by its path in the project folder.
type zipEntry struct {
	name    string
	content []byte
}

// copyShell hands the page's copy button the files as shell commands, the
// secrets in them, and records that in the audit log.
func (server *Server) copyShell(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	bundle, err := stored.Bundle()
	if err != nil {
		return err
	}
	details := map[string]any{"generation": stored.Generation.ID, "via": "clipboard"}
	if err := server.workspace().Store.Audit(ctx, webActorPrefix+current.user.Username, store.ActionExport, stored.Project.ID, details); err != nil {
		return err
	}
	writer.Header().Set(contentType, plainText)
	_, err = writer.Write(export.Shell(bundle)) //nolint:gosec // G705: served as text/plain with nosniff; the commands are exactly what was asked for
	return err
}

// download sends compose.yaml, .env, DEPLOY.md and the secret files as a
// zip; the .env and secret entries are marked 0600, their folder 0700.
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
	files := []zipEntry{{export.ComposeName, bundle.Compose}, {export.EnvName, bundle.Env}, {export.DeployName, bundle.Deploy}}
	if len(bundle.Secrets) > 0 {
		folder := &zip.FileHeader{Name: bundle.Project + "/" + ir.SecretsDirectory + "/", Modified: modified}
		folder.SetMode(fs.ModeDir | zipFolderMode)
		if _, err := zipped.CreateHeader(folder); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(bundle.Secrets)) {
		files = append(files, zipEntry{name, bundle.Secrets[name]})
	}
	for _, file := range files {
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
