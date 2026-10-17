package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/docker/go-units"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	uploadsDir     = "uploads"
	uploadPrefix   = "upload-"
	uploadDirMode  = 0o700
	uploadFileMode = 0o600
	maxUploadFiles = 50
	pastedCompose  = "compose.yaml"
	pastedEnv      = ".env"
	uploadedTitle  = "the files you gave"
	// mixedUploadProblem refuses an archive given with other files or a
	// pasted compose file: which of them is the repository is unclear.
	mixedUploadProblem = "Give one archive, or loose files, not both."
	archiveKindProblem = "%s cannot be unpacked: give the repository as one .zip or .tar.gz."
	actionApply        = "apply"
	actionValidate     = "validate"
	actionFiles        = "files"
	viaWeb             = "web"
	valuePrefix        = "value:"
	secretPrefix       = "secret:"
	trustField         = "trust"
	// trustAlwaysField trusts the namespace in config.yaml as well, for
	// every project from now on.
	trustAlwaysField = "trust_always"
	// benignRemoveField names the ignored log pattern to stop ignoring on
	// the Review page.
	benignRemoveField = "benign_remove"
	// optionalField holds the optional services ticked on the Review page.
	optionalField   = "optional"
	publicHostField = "public_host"
	// portField chooses a host port on the Review page, as app:8080=8081.
	portField = "port"
	// publishedPortHint and noAddressHint say what a blank public address
	// gives when the app joins no network.
	publishedPortHint = "the published port"
	noAddressHint     = "no public address"
	// composeFileField holds the upstream compose file chosen on the
	// Review page.
	composeFileField = "compose_file"
	// memoryLimitField is a finding's "Use 1GiB" button: service=limit.
	memoryLimitField     = "memory_limit"
	memoryLimitSeparator = "="
	lineField            = "line"
	emptyLineProblem     = "That log line has no text to ignore."
	// trustAlwaysProblem follows a revision that already went in, so it
	// says what did change.
	trustAlwaysProblem = "the changes were applied and %s is trusted for this project, but adding it to registries.allow failed: %w"
	storageVolumes     = "volumes"
	storageBind        = "bind"
	appliedNotice      = "Changes applied. The plan was gated and rendered again as a new version."
	editedNotice       = "Saved compose.yaml as a new version. The policy gate checked it; the test runs exactly this file."
	blockedNotice      = "Saved compose.yaml as a new version, but the policy gate blocks it: see the findings below."
	paneCompose        = "compose"
	paneEnv            = "env"
	paneDeploy         = "deploy"
	// durationPrecision is how finely a run's time is shown: 14.2s.
	durationPrecision = 100 * time.Millisecond
	generationQuery   = "generation"
	olderRunNotice    = "This run tested an earlier version; the plan has changed since. Run the test again to check the current one."
	// tailLines is how much of the failing service's log the Validate page
	// shows next to why it failed.
	tailLines = 12
)

// failureHints say in plain words what each kind of failed run usually
// means and where to look.
var failureHints = map[string]string{
	sandbox.ReasonCrash: "The container stopped by itself. Its last lines below usually say why: a missing or wrong variable, " +
		"a folder it cannot write, or a port it cannot use.",
	sandbox.ReasonUnhealthy: "It started, but its healthcheck kept failing. Check that the healthcheck tests the right port and path, " +
		"and the app's last lines below.",
	sandbox.ReasonTimeout: "It was not healthy before the time ran out: a slow first start, a service it needs that never came up, " +
		"or a healthcheck that cannot pass.",
	sandbox.ReasonLogs:  "Every container kept running, but a line in the logs reads like a fatal error.",
	sandbox.ReasonStart: "Docker could not start the stack: an image it cannot pull, a port already in use, or a setting it refuses. Its message is above.",
	sandbox.ReasonUnreachable: "Every service is up, but the app did not answer another container on the sandbox network, the way a proxy reaches it, as the plan expects. " +
		"It may listen on 127.0.0.1 inside its container, or on another port than the plan says; its last lines below often name the address. " +
		"When it answered with an error, the probe path above may not be the app's health endpoint.",
}

// archiveExtension are the archives people may drop; only those
// fetch.IsArchive accepts can be unpacked.
var archiveExtension = []string{".zip", ".tar", ".gz", ".tgz", ".xz", ".bz2", ".7z", ".rar"}

type option struct {
	Value    string
	Label    string
	Sub      string
	Selected bool
	Disabled bool
}

type newView struct {
	Source       string
	Ref          string
	Name         string
	Engines      []option
	Outbound     string
	ArchiveLimit string
	FilesOnly    bool
}

// skipTest is the New page's "Test in sandbox after generating" left
// unticked: skip the review and the sandbox and go straight to the files.
// The checkbox comes first in the form, so a ticked box reads as its own
// value; a form without the field at all keeps the review.
const skipTest = "0"

func (server *Server) newView(source, ref, name, engine string) newView {
	if engine == "" {
		engine = server.config().LLM.Engine
	}
	return newView{Source: source, Ref: ref, Name: name, Engines: server.engineOptions(engine), Outbound: outbound(server.config()),
		ArchiveLimit: units.BytesSize(float64(fetch.MaxArchiveBytes))}
}

// outbound says where this session may connect, in the words of the
// egress allowlist.
func outbound(configuration config.Config) string {
	parts := []string{strings.Join(configuration.SCM.AllowHosts, ", "), "registry checks for the allowlisted images"}
	if label, known := providerNames[configuration.LLM.Provider]; known {
		parts = append([]string{label + " at " + configuration.LLM.BaseURL}, parts...)
	}
	return strings.Join(parts, " · ")
}

func (server *Server) newPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	page := server.page(request, current, "New project").flow(stepSource)
	page.Draft, page.CLI = true, "composewright gen https://github.com/<owner>/<repo> --engine "+server.config().LLM.Engine
	page.Data = server.newView("", "", "", "")
	return server.render(writer, http.StatusOK, "new", page)
}

// startGeneration takes one source, a repository link or folder, or files
// and a pasted compose file, and starts the analysis.
// generationRequest is what someone asked to generate from: a repository
// link or a folder, or files they gave (uploaded, or a pasted compose file
// and .env).
type generationRequest struct {
	source    string
	ref       string
	name      string
	engine    string
	compose   string
	env       string
	files     []*multipart.FileHeader
	filesOnly bool
}

func (ask generationRequest) given() bool {
	return len(ask.files) > 0 || strings.TrimSpace(ask.compose) != ""
}

// problem says why a request cannot start, if it cannot.
func (ask generationRequest) problem() string {
	switch {
	case ask.given() && ask.source != "":
		return "Choose one source: files or a pasted compose file, or a repository link."
	case !ask.given() && ask.source == "":
		return "Give a repository link, a folder on this machine, or files."
	case ask.name != "" && !store.ValidProjectName(ask.name):
		return "A project name uses lowercase letters, digits, - and _ and starts with a letter or digit."
	case len(ask.files) > maxUploadFiles:
		return fmt.Sprintf("At most %d files at a time.", maxUploadFiles)
	}
	return ask.archiveProblem()
}

// archiveProblem says why the archives among the files cannot be used:
// one .zip or .tar.gz, given alone, is unpacked as the repository.
func (ask generationRequest) archiveProblem() string {
	archives := slices.DeleteFunc(slices.Clone(ask.files), func(header *multipart.FileHeader) bool { return !isArchive(header) })
	if len(archives) == 0 {
		return ""
	}
	for _, header := range archives {
		if !fetch.IsArchive(uploadName(header)) {
			return fmt.Sprintf(archiveKindProblem, uploadName(header))
		}
	}
	pasted := strings.TrimSpace(ask.compose) != "" || strings.TrimSpace(ask.env) != ""
	if len(ask.files) > 1 || pasted {
		return mixedUploadProblem
	}
	return ""
}

// archive is the one archive given as the repository, if it was.
func (ask generationRequest) archive() *multipart.FileHeader {
	if len(ask.files) != 1 || !isArchive(ask.files[0]) {
		return nil
	}
	return ask.files[0]
}

func isArchive(header *multipart.FileHeader) bool {
	name := strings.ToLower(header.Filename)
	return slices.ContainsFunc(archiveExtension, func(extension string) bool { return strings.HasSuffix(name, extension) })
}

// uploadName is an uploaded file's base name, without any folder it gave.
func uploadName(header *multipart.FileHeader) string {
	return filepath.Base(filepath.Clean("/" + header.Filename))
}

func uploadedFiles(request *http.Request) []*multipart.FileHeader {
	if request.MultipartForm == nil {
		return nil
	}
	return request.MultipartForm.File["files"]
}

// startGeneration starts from the New page's form.
func (server *Server) startGeneration(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ask := generationRequest{source: field(request, "source"), ref: field(request, "ref"), name: field(request, "name"),
		engine: request.PostFormValue("engine"), compose: request.PostFormValue("compose"), env: request.PostFormValue("env"),
		files: uploadedFiles(request), filesOnly: request.PostFormValue("test") == skipTest}
	if problem := ask.problem(); problem != "" {
		page := server.page(request, current, "New project").flow(stepSource)
		view := server.newView(ask.source, ask.ref, ask.name, ask.engine)
		view.FilesOnly = ask.filesOnly
		page.Error, page.Data = problem, view
		return server.render(writer, http.StatusBadRequest, "new", page)
	}
	return server.launch(writer, request, current, ask)
}

// launch keeps what was given for the run and starts the generation.
func (server *Server) launch(writer http.ResponseWriter, request *http.Request, current *visit, ask generationRequest) error {
	source, title, uploads := ask.source, ask.source, ""
	if ask.given() {
		var err error
		if uploads, err = server.saveUploads(ask.files, ask.compose, ask.env); err != nil {
			return err
		}
		source, title = uploads, uploadedTitle
		if archive := ask.archive(); archive != nil {
			title = uploadName(archive)
			source = filepath.Join(uploads, title)
		}
	}
	options := pipeline.Options{Source: source, Ref: ask.ref, Name: ask.name, Engine: ask.engine, Uploaded: ask.given()}
	username, filesOnly := current.user.Username, ask.filesOnly
	started, err := server.jobs.start(jobGenerate, title, ask.name, func(ctx context.Context, running *job) (string, *pipeline.Result, error) {
		if uploads != "" {
			defer server.removeUploads(uploads)
		}
		return server.generate(ctx, running, username, options, filesOnly)
	})
	if err != nil {
		if uploads != "" {
			_ = os.RemoveAll(uploads)
		}
		return err
	}
	return redirect(writer, request, "/jobs/"+started.ID)
}

// generate runs the analysis. Its next page is the review, or with
// filesOnly the files, unless the policy gate blocks the plan: then the
// review says why.
func (server *Server) generate(ctx context.Context, running *job, username string, options pipeline.Options, filesOnly bool) (string, *pipeline.Result, error) {
	connection, err := server.connect(ctx, username, options.Engine, false)
	if err != nil {
		return "", nil, err
	}
	if connection.Note != "" {
		running.notice(connection.Note)
	}
	options.Engine = connection.Engine
	dependencies := connection.Dependencies
	dependencies.Observer = running.observer()
	result, err := pipeline.Generate(ctx, dependencies, options)
	if err != nil {
		return "", nil, err
	}
	if filesOnly && !result.Report.Blocked() {
		return projectURL(result.Project.Name, pageFiles), result, nil
	}
	return projectURL(result.Project.Name, pageReview) + "?job=" + running.ID, result, nil
}

// connect builds a run's dependencies acting as username, so its audit
// entries name the person who started it.
func (server *Server) connect(ctx context.Context, username, engine string, validate bool) (pipeline.Connection, error) {
	return server.options.Connect(ctx, server.pipelineSetup(username, engine, validate))
}

func (server *Server) pipelineSetup(username, engine string, validate bool) pipeline.Setup {
	return pipeline.Setup{Config: server.config(), Paths: server.paths(), Workspace: server.as(username),
		Version: server.options.Version, Engine: engine, Validate: validate}
}

// uploadsRoot holds a private folder for each run's uploaded files. It is
// absolute, as the source a run reports is.
func (server *Server) uploadsRoot() string {
	root := filepath.Join(server.paths().DataDir, uploadsDir)
	if absolute, err := filepath.Abs(root); err == nil {
		return absolute
	}
	return root
}

// shownSource is a job's source as people see it: an upload is named by its
// title, not by the private folder it was saved in.
func (server *Server) shownSource(snapshot snapshot) *fetch.Source {
	uploaded := snapshot.Source != nil && strings.HasPrefix(snapshot.Source.URL, server.uploadsRoot()+string(filepath.Separator))
	if !uploaded {
		return snapshot.Source
	}
	shown := *snapshot.Source
	shown.URL = snapshot.Title
	return &shown
}

// saveUploads writes the given files into a private folder under the data
// directory: base names only, no overwriting one another. An archive is
// kept as it is; the analysis unpacks it.
func (server *Server) saveUploads(files []*multipart.FileHeader, compose, env string) (string, error) {
	root := server.uploadsRoot()
	if err := os.MkdirAll(root, uploadDirMode); err != nil {
		return "", err
	}
	directory, err := os.MkdirTemp(root, uploadPrefix)
	if err != nil {
		return "", err
	}
	written := func() error {
		for _, header := range files {
			if err := saveUpload(directory, header); err != nil {
				return err
			}
		}
		pasted := map[string]string{pastedCompose: compose, pastedEnv: env}
		for name, content := range pasted {
			if strings.TrimSpace(content) == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(directory, name), []byte(content), uploadFileMode); err != nil {
				return err
			}
		}
		return nil
	}()
	if written != nil {
		_ = os.RemoveAll(directory)
		return "", written
	}
	return directory, nil
}

func saveUpload(directory string, header *multipart.FileHeader) error {
	name := uploadName(header)
	if name == "/" || name == "." {
		return nil
	}
	source, err := header.Open()
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, uploadFileMode)
	if err != nil {
		return badRequest("Two files are called %s; give each file once.", name)
	}
	_, copyErr := io.Copy(target, source)
	return errors.Join(copyErr, target.Close())
}

// removeUploads deletes the uploaded files once the analysis is over.
func (server *Server) removeUploads(directory string) {
	if err := os.RemoveAll(directory); err != nil {
		server.options.Logger.Warn("uploaded files were not removed", "directory", directory, "error", err)
	}
}

// sweepUploads removes the upload folders a crash left behind, which may
// hold a .env. No job runs before the server starts, so each one is stale.
func (server *Server) sweepUploads() {
	root := server.uploadsRoot()
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		server.options.Logger.Warn("uploads left by a crash were not looked for", "directory", root, "error", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), uploadPrefix) {
			server.removeUploads(filepath.Join(root, entry.Name()))
		}
	}
}

func projectURL(name, page string) string {
	target := "/projects/" + url.PathEscape(name)
	if page != "" {
		target += "/" + page
	}
	return target
}

// jobPage shows a run: the Analyse step for a generation, which moves on
// to Review by itself, or the Validate step.
func (server *Server) jobPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	running, err := server.job(request)
	if err != nil {
		return err
	}
	snapshot := running.snapshot()
	if snapshot.Finished && snapshot.Failure == "" && snapshot.Next != "" {
		return redirect(writer, request, snapshot.Next)
	}
	if snapshot.Kind == jobGenerate {
		page := server.page(request, current, "Analysing").flow(stepAnalyse)
		page.Data = server.analyseView(snapshot)
		return server.render(writer, http.StatusOK, "analyse", page)
	}
	return server.renderValidate(writer, request, current, snapshot)
}

// validatePage shows a project's last run: the live one while it runs,
// otherwise the kept verdict and logs.
func (server *Server) validatePage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	name := request.PathValue("name")
	if running, busy := server.jobs.running(name); busy && running.Kind == jobValidate {
		return redirect(writer, request, "/jobs/"+running.ID)
	}
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), name)
	if err != nil {
		return err
	}
	generations, err := server.workspace().Store.Generations(ctx, stored.Project.ID)
	if err != nil {
		return err
	}
	tested, verdict, found := testedRun(generations, request.URL.Query().Get(generationQuery))
	if !found {
		return redirect(writer, request, projectURL(stored.Project.Name, pageReview))
	}
	lines, err := pipeline.KeptLogs(tested)
	if err != nil {
		return err
	}
	kept := snapshot{Kind: jobValidate, Project: stored.Project.Name, Finished: true, Lines: lines,
		Result: &pipeline.Result{Verdicts: []sandbox.Verdict{verdict}}}
	if tested.ID != stored.Generation.ID {
		kept.Notices = []string{olderRunNotice}
	}
	return server.renderValidate(writer, request, current, kept)
}

func (server *Server) renderValidate(writer http.ResponseWriter, request *http.Request, current *visit, run snapshot) error {
	view := server.validateView(run)
	page := server.page(request, current, "Validating "+run.Project).flow(stepValidate).of(run.Project)
	page.Steps[stepValidate-1].Failed = view.Failed
	page.Data, page.CLI = view, "composewright validate "+run.Project
	return server.render(writer, http.StatusOK, "validate", page)
}

type analyseView struct {
	Job       snapshot
	Heading   string
	Ref       string
	Elapsed   string
	Seconds   int
	Steps     []stepView
	Facts     []factRow
	FactChips []string
	// Suspicious are repository lines addressed to a language model.
	Suspicious []extract.Note
	Events     string
	// Live is a generation you can watch; otherwise the page shows a
	// stored project's analysis.
	Live      bool
	Project   string
	Files     []fileRead
	Rules     []ruleRow
	Filled    int
	Fields    int
	Questions int
}

func (server *Server) analyseView(snapshot snapshot) analyseView {
	rows, chips := factRows(snapshot.Facts)
	settings := server.config().LLM
	source := server.shownSource(snapshot)
	details := map[string]string{
		pipeline.StepFetch:   sourceText(source),
		pipeline.StepExtract: extractSummary(snapshot.Facts),
		pipeline.StepModel:   providerNames[settings.Provider] + " · " + settings.Model + " · structured JSON",
	}
	heading := "Analysing " + snapshot.Title
	if source != nil && source.URL != "" {
		heading = "Analysing " + strings.TrimPrefix(source.URL, githubPrefix)
	}
	end := snapshot.Ended
	if end.IsZero() {
		end = server.options.Now()
	}
	elapsed := end.Sub(snapshot.Started).Round(time.Second)
	view := analyseView{Job: snapshot, Heading: heading, Steps: steps(generationSteps, snapshot, details), Facts: rows, FactChips: chips,
		Events: "/jobs/" + snapshot.ID + "/events", Elapsed: elapsed.String(), Seconds: int(elapsed.Seconds()), Live: true}
	if source != nil && source.Commit != "" {
		view.Ref = sourceText(source)
	}
	if snapshot.Facts != nil {
		view.Files, view.Suspicious = filesRead(snapshot.Facts.Files, nil), snapshot.Facts.Suspicious
	}
	return view
}

// analysePage shows what the rules made of a stored project's repository:
// the fields each rule filled and the facts they read.
func (server *Server) analysePage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	rows, chips := factRows(stored.Facts)
	rows = append(rows, imageVersionRows(stored.Images)...)
	view := analyseView{Project: stored.Project.Name, Heading: stored.Project.Name, Ref: sourceText(storedSource(stored)), Facts: rows,
		FactChips: chips, Questions: questionCount(envRows(stored.Plan, stored.Values, false))}
	view.Rules, view.Filled, view.Fields = ruleRows(stored.Plan)
	if stored.Facts != nil {
		view.Files, view.Suspicious = filesRead(stored.Facts.Files, stored.Plan), stored.Facts.Suspicious
	}
	page := server.page(request, current, "Analysis of "+stored.Project.Name).flow(stepAnalyse).of(stored.Project.Name)
	page.Data, page.CLI = view, "composewright show "+stored.Project.Name
	return server.render(writer, http.StatusOK, "analyse", page)
}

type validateView struct {
	Job     snapshot
	Tone    string
	Project string
	Sandbox string
	Running bool
	Passed  bool
	Failed  bool
	Title   string
	Detail  string
	Attempt string
	Steps   []runStep
	Matches []string
	// Ignorable offers to keep each matched line as normal for the app.
	Ignorable bool
	// Repairable offers to ask the configured model for a repair of the
	// latest version's failed run.
	Repairable bool
	// Hint, Service, HealthSaid and Tail explain a failed run: what it
	// usually means, the service it points at, that service's last
	// healthcheck answer and its last lines.
	Hint       string
	Service    string
	HealthSaid string
	Tail       []lineView
	Limits     config.Validate
	Runtime    string
	Services   []stateView
	Lines      []lineView
	Events     string
}

func (server *Server) validateView(snapshot snapshot) validateView {
	view := validateView{Job: snapshot, Tone: toneInfo, Project: snapshot.Project, Running: !snapshot.Finished, Lines: lineViews(snapshot.Lines),
		Events: "/jobs/" + snapshot.ID + "/events", Limits: server.config().Docker.Validate, Runtime: sandbox.Runtime(server.config().Docker.SandboxTier)}
	verdict, found := lastVerdict(snapshot.Result)
	if found {
		view.Sandbox, view.Services, view.Matches = verdict.Project, stateViews(verdict), verdict.LogMatches
		view.Runtime = sandbox.Runtime(verdict.Sandbox.Tier)
	}
	switch {
	case view.Running:
		view.Attempt, view.Steps = snapshot.Steps[pipeline.StepValidate].Note, startingRun
		view.Title = "Starting " + snapshot.Project + " in a throw-away sandbox…"
		view.Detail = "Nothing to do here. When every service is healthy you go straight to your files; the sandbox is removed either way."
	case snapshot.Failure != "":
		view.Failed, view.Tone, view.Title, view.Detail = true, toneDanger, "Validation stopped", snapshot.Failure
	case found && verdict.Passed():
		view.Passed, view.Tone, view.Attempt, view.Steps = true, toneOk, attemptText(verdict), runSteps(verdict)
		view.Title = fmt.Sprintf("Every service is up in %s", verdict.Duration.Round(time.Second))
		view.Detail = "The sandbox has been removed. Your files are ready."
	case found:
		view.Failed, view.Tone, view.Attempt, view.Steps = true, toneDanger, attemptText(verdict), runSteps(verdict)
		failing, _ := failingState(verdict)
		view.Title, view.Hint, view.Service, view.HealthSaid = failureText(verdict), failureHints[verdict.Reason], failing.Service, failing.HealthOutput
		view.Detail, view.Tail = "The sandbox has been removed; its logs are kept with the project.", tailOf(view.Lines, failing.Service)
		view.Ignorable = verdict.Reason == sandbox.ReasonLogs
		view.Repairable = server.config().LLM.Provider != config.ProviderNone && !slices.Contains(snapshot.Notices, olderRunNotice)
	}
	return view
}

// failingState is the service a failed run points at, if any: the one
// that failed its step, or the one the probe could not reach.
func failingState(verdict sandbox.Verdict) (sandbox.ServiceState, bool) {
	unreachable := ""
	if verdict.Reason == sandbox.ReasonUnreachable && verdict.Reach != nil {
		unreachable = verdict.Reach.Service
	}
	for _, state := range verdict.Services {
		if serviceStep(state, verdict).State == pipeline.StateFailed || state.Service == unreachable {
			return state, true
		}
	}
	return sandbox.ServiceState{}, false
}

// tailOf is a service's last lines of output.
func tailOf(lines []lineView, service string) []lineView {
	var own []lineView
	for _, line := range lines {
		if line.Source == service {
			own = append(own, line)
		}
	}
	return own[max(0, len(own)-tailLines):]
}

func attemptText(verdict sandbox.Verdict) string {
	outcome := "failed"
	if verdict.Passed() {
		outcome = "passed"
	}
	return fmt.Sprintf("Attempt %d %s, %s", verdict.Iteration, outcome, verdict.Duration.Round(durationPrecision))
}

func lastVerdict(result *pipeline.Result) (sandbox.Verdict, bool) {
	if result == nil || len(result.Verdicts) == 0 {
		return sandbox.Verdict{}, false
	}
	return result.Verdicts[len(result.Verdicts)-1], true
}

type reviewView struct {
	Compose     []badgedLine
	ComposeText string
	EnvLines    []badgedLine
	DeployLines []badgedLine
	Edited      bool
	Variables   int
	Secrets     int
	Questions   int
	Pane        string
	// Draft is an edit that failed to load, shown again in the editor.
	Draft      string
	Project    store.Project
	Source     string
	Confidence string
	Tones      reviewTones
	PolicyText string
	Engine     string
	Blocked    bool
	Denied     int
	Needed     int
	Warnings   int
	Expose     []option
	Networks   []option
	// PublicHost is the address the person set; DerivedHost is the one a
	// blank field keeps, "" when the app joins no network, and BlankHint
	// says what a blank field gives.
	PublicHost  string
	DerivedHost string
	BlankHint   string
	Storage     []option
	Env         []envRow
	Optional    []ir.OptionalSetting
	Generated   int
	Needs       []needView
	Findings    []findingView
	Registries  []string
	Benign      []string
	TrustOffers []trustOffer
	Revealed    bool
	Running     string
	Suspicious  []string
	Notes       []noteView
	Notices     []string
	// LastFailure is the last test run when it failed; ChangedSince says
	// the plan was changed after it.
	LastFailure  string
	ChangedSince bool
	// OptionalServices are the services upstream starts only with a
	// Compose profile, each one to switch on.
	OptionalServices []option
	// ReadOnly says the source cannot be read again, so neither the
	// optional services nor the compose file can be switched here.
	ReadOnly bool
	// Variants are the upstream compose files to build the plan from, the
	// one it was built from selected; none when upstream ships one.
	Variants []option
}

func (server *Server) reviewView(ctx context.Context, stored *pipeline.Stored, revealed bool) (reviewView, error) {
	generations, err := server.workspace().Store.Generations(ctx, stored.Project.ID)
	if err != nil {
		return reviewView{}, err
	}
	observed, err := pipeline.Observed(ctx, stored, generations)
	if err != nil {
		return reviewView{}, err
	}
	// Appended, not merged: Merge would drop the measured peak of a service
	// whose limit already has a P10 finding, such as one from upstream.
	report, plan := policy.Report{Findings: append(slices.Clone(stored.Report.Findings), observed.Findings...)}, stored.Plan
	configuration := server.config()
	profile := configuration.Profile
	mode, networks := render.Exposure(profile, stored.Options.ExposeVia, stored.Options.Networks)
	derivedHost := render.Options{Profile: profile, Networks: networks}.PublicAddress(plan.App.Name)
	view := reviewView{Project: stored.Project, Source: sourceText(storedSource(stored)), Confidence: plan.Confidence,
		Engine: engineText(stored.Generation), Blocked: report.Blocked(), Denied: report.Count(policy.Deny), Needed: report.Count(policy.Need),
		Warnings: report.Count(policy.Warn), Expose: portOptions(mode), Networks: networkOptions(profile, networks),
		PublicHost: stored.Options.PublicHost, DerivedHost: derivedHost, BlankHint: blankHostHint(derivedHost, mode),
		Storage: server.storageOptions(stored.Options.Storage), Env: envRows(plan, stored.Values, revealed), Optional: render.OptionalSettings(plan),
		Needs: needViews(plan, stored.Options.AllowNeeds), Findings: findingViews(report, stored.Generation.ComposeYAML), Registries: stored.Options.AllowRegistries,
		Benign: stored.Options.BenignLogPatterns, OptionalServices: optionalServiceOptions(plan, stored.Rereadable()), ReadOnly: !stored.Rereadable(),
		Variants:    variantOptions(stored),
		TrustOffers: trustOffers(plan, report, configuration.Registries.Allow), Revealed: revealed, Suspicious: plan.SuspiciousInstructions}
	for _, note := range plan.PostInstallNotes {
		view.Notes = append(view.Notes, noteView{Text: note, Suspicious: repeatsSuspicious(note, plan.SuspiciousInstructions)})
	}
	for _, entry := range plan.EnvFile {
		if entry.Generate != "" {
			view.Generated++
		}
	}
	view.Tones, view.PolicyText = tonesFor(plan.Confidence, report), policyText(report)
	if running, busy := server.jobs.running(stored.Project.Name); busy {
		view.Running = "/jobs/" + running.ID
	}
	view.Variables, view.Edited, view.ComposeText = len(plan.EnvFile), stored.Options.Edited, stored.Generation.ComposeYAML
	for _, row := range view.Env {
		if row.Secret {
			view.Secrets++
		}
	}
	view.Questions = questionCount(view.Env)
	view.Compose, view.Pane = server.composeView(stored), paneCompose
	view.EnvLines = plainLines(server.envText(stored, revealed), languageEnv)
	view.DeployLines = plainLines(stored.Generation.DeployMD, languageMarkdown)
	if tested, verdict, found := testedRun(generations, ""); found && !verdict.Passed() {
		view.LastFailure, view.ChangedSince = failureText(verdict), tested.ID != stored.Generation.ID
	}
	return view, nil
}

// noteView is a note for deployment; Suspicious says it repeats a line
// addressed to a language model.
type noteView struct {
	Text       string
	Suspicious bool
}

// locatedInstruction is a suspicious instruction the rules found: its file
// and line, then its text. One a model reported may have neither.
var locatedInstruction = regexp.MustCompile(`^(\S+:\d+): (.+)$`)

// repeatsSuspicious says whether a note repeats a suspicious instruction:
// the rules cite its file and line, a model may quote its text.
func repeatsSuspicious(note string, instructions []string) bool {
	return slices.ContainsFunc(instructions, func(instruction string) bool {
		match := locatedInstruction.FindStringSubmatch(instruction)
		if match == nil {
			return instruction != "" && strings.Contains(note, instruction)
		}
		location, text := match[1], match[2]
		return strings.Contains(note, "("+location+")") || strings.Contains(note, text)
	})
}

// composeView badges compose.yaml; an edited version is compared with the
// plan's own rendering to mark the person's lines.
func (server *Server) composeView(stored *pipeline.Stored) []badgedLine {
	rendered := ""
	if stored.Options.Edited {
		plan, err := pipeline.PlanCompose(server.config(), server.options.Version, stored)
		if err != nil {
			server.options.Logger.Warn("rendering the plan to compare an edited compose.yaml", "project", stored.Project.Name, "error", err)
		}
		rendered = string(plan)
	}
	return composeLines(stored.Plan, stored.Generation.ComposeYAML, rendered, imageFacts(stored.Images))
}

// envText is the .env file with secrets masked unless revealed.
func (server *Server) envText(stored *pipeline.Stored, revealed bool) string {
	mask := webMask
	if revealed {
		mask = pipeline.Revealed
	}
	env, err := stored.EnvFile(mask)
	if err != nil {
		server.options.Logger.Warn("rendering .env for the context pane", "project", stored.Project.Name, "error", err)
	}
	return string(env)
}

func storedSource(stored *pipeline.Stored) *fetch.Source {
	return &fetch.Source{URL: stored.Project.SourceURL, Ref: stored.Project.SourceRef, Commit: stored.Project.SourceCommit}
}

func engineText(generation store.Generation) string {
	if generation.Model == "" {
		return generation.Engine
	}
	return generation.Engine + " · " + generation.Model
}

// portOptions are the ways the app's ports can be published.
func portOptions(current string) []option {
	options := []option{{Value: render.ExposeLocal, Label: "127.0.0.1", Sub: "Port on this machine only"},
		{Value: render.ExposeLAN, Label: "0.0.0.0", Sub: "Every device on your network"},
		{Value: render.ExposeNone, Label: "not published", Sub: "Only through the networks above"}}
	for index := range options {
		options[index].Selected = options[index].Value == current
	}
	return options
}

// networkOptions are the profile's networks; the app may join any of them.
func networkOptions(profile config.Profile, chosen []string) []option {
	options := make([]option, 0, len(profile.Networks))
	for _, network := range profile.Networks {
		sub := strings.TrimSpace(network.Provider + " " + network.Kind)
		if network.Domain != "" {
			sub += " · " + network.Domain
		}
		if network.Default {
			sub += " · joined by default"
		}
		options = append(options, option{Value: network.Name, Label: network.Name, Sub: sub, Selected: slices.Contains(chosen, network.Name)})
	}
	return options
}

func (server *Server) storageOptions(current string) []option {
	storage := server.config().Profile.Storage
	bindSub := "Folder per app under " + storage.BasePath
	if storage.BasePath == "" {
		bindSub = "Set a base path in Settings first"
	}
	options := []option{{Value: "", Label: "profile default", Sub: storage.Style}, {Value: storageVolumes, Label: "volume", Sub: "Named volume per app"},
		{Value: storageBind, Label: "bind", Sub: bindSub, Disabled: storage.BasePath == ""}}
	for index := range options {
		options[index].Selected = options[index].Value == current
	}
	return options
}

func (server *Server) reviewPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	query := request.URL.Query()
	notice := ""
	switch {
	case query.Has("applied"):
		notice = appliedNotice
	case query.Has("edited") && stored.Report.Blocked():
		notice = blockedNotice
	case query.Has("edited"):
		notice = editedNotice
	}
	view, err := server.reviewView(request.Context(), stored, false)
	if err != nil {
		return err
	}
	view.Pane = paneFor(query.Get("pane"))
	if query.Has("edit") {
		view.Draft = view.ComposeText
	}
	if finished, found := server.jobs.get(query.Get("job")); found {
		view.Notices = finished.snapshot().Notices
	}
	page := server.reviewPageFor(request, current, stored)
	page.Notice, page.Data = notice, view
	return server.render(writer, http.StatusOK, "review", page)
}

// paneFor picks the file the context pane shows; compose.yaml unless the
// link asks for another.
func paneFor(value string) string {
	if value == paneEnv || value == paneDeploy {
		return value
	}
	return paneCompose
}

// reviewPageFor is the Review page's frame: under the project, with the
// test command in the context pane.
func (server *Server) reviewPageFor(request *http.Request, current *visit, stored *pipeline.Stored) Page {
	page := server.page(request, current, "Review "+stored.Project.Name).flow(stepReview).of(stored.Project.Name)
	page.CLI = "composewright validate " + stored.Project.Name
	return page
}

// busy refuses a change to a project while one of its runs goes on, which
// would write a version beside it.
func (server *Server) busy(name string) error {
	if _, running := server.jobs.running(name); running {
		return conflict(runningConflict, name)
	}
	return nil
}

// editCompose saves compose.yaml as the person wrote it on the Review page.
// Both policy gates check it, and the test runs exactly this file.
func (server *Server) editCompose(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	name := request.PathValue("name")
	if err := server.busy(name); err != nil {
		return err
	}
	compose := request.PostFormValue("compose")
	if strings.TrimSpace(compose) == "" {
		return badRequest("compose.yaml is empty; there is nothing to save.")
	}
	connection, err := server.connect(ctx, current.user.Username, pipeline.EngineRules, false)
	if err != nil {
		return err
	}
	_, err = pipeline.EditCompose(ctx, connection.Dependencies, name, []byte(compose))
	if errors.Is(err, pipeline.ErrComposeInvalid) {
		return server.composeProblem(writer, request, current, name, compose, err)
	}
	if err != nil {
		return err
	}
	return redirect(writer, request, projectURL(name, pageReview)+"?edited")
}

// composeProblem shows the Review page with the editor still holding what
// the person typed, and why it was refused.
func (server *Server) composeProblem(writer http.ResponseWriter, request *http.Request, current *visit, name, compose string, problem error) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), name)
	if err != nil {
		return errors.Join(problem, err)
	}
	view, err := server.reviewView(request.Context(), stored, false)
	if err != nil {
		return errors.Join(problem, err)
	}
	view.Draft = compose
	page := server.reviewPageFor(request, current, stored)
	page.Error, page.Data = capitalised(problem.Error())+".", view
	return server.render(writer, http.StatusBadRequest, "review", page)
}

// planJSON shows the stored plan, the IR that rules and models write; it
// holds no secret value.
func (server *Server) planJSON(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	content, err := json.MarshalIndent(stored.Plan, "", "  ")
	if err != nil {
		return err
	}
	writer.Header().Set(contentType, jsonType)
	_, err = writer.Write(content)
	return err
}

func (server *Server) renderReview(writer http.ResponseWriter, request *http.Request, current *visit, stored *pipeline.Stored, revealed bool,
	notice, problem string, status int) error {
	view, err := server.reviewView(request.Context(), stored, revealed)
	if err != nil {
		return err
	}
	page := server.reviewPageFor(request, current, stored)
	page.Notice, page.Error, page.Data = notice, problem, view
	return server.render(writer, status, "review", page)
}

// review applies what changed on the page, then goes where the button
// said: stay, the files, or an approved sandbox run.
func (server *Server) review(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	name := request.PathValue("name")
	if err := server.busy(name); err != nil {
		return err
	}
	stored, err := pipeline.Load(ctx, server.workspace(), name)
	if err != nil {
		return err
	}
	revision := revisionFrom(request.PostForm, stored)
	action := request.PostFormValue("action")
	if key := request.PostFormValue("regenerate"); key != "" {
		revision.Regenerate, action = []string{key}, actionApply
	}
	if len(revision.MemoryLimits) > 0 {
		action = actionApply
	}
	alwaysPattern := request.PostFormValue(trustAlwaysField)
	for _, pattern := range []string{alwaysPattern, request.PostFormValue(trustField)} {
		if pattern != "" {
			revision.AllowRegistries, action = append(revision.AllowRegistries, pattern), actionApply
		}
	}
	if choice := request.PostFormValue(portField); choice != "" {
		key, host, err := pipeline.ParsePortChoice(choice)
		if err != nil {
			return server.reviewProblem(writer, request, current, name, err)
		}
		revision.Ports, action = map[string]int{key: host}, actionApply
	}
	if pattern := request.PostFormValue(benignRemoveField); pattern != "" {
		if err := server.removeBenign(ctx, current.user.Username, name, pattern); err != nil {
			return err
		}
	}
	enabled, switched, err := optionalSwitch(request.PostForm, stored)
	if err != nil {
		return server.reviewProblem(writer, request, current, name, err)
	}
	if changed(revision, stored, server.config().Profile) {
		if _, err := server.revise(ctx, current.user.Username, name, revision); err != nil {
			return server.reviewProblem(writer, request, current, name, err)
		}
	}
	// registries.allow is written only once the revision is in, so a
	// rejected page never leaves the namespace trusted everywhere.
	if err := server.trustAlways(request, current, alwaysPattern); err != nil {
		return server.reviewProblem(writer, request, current, name, fmt.Errorf(trustAlwaysProblem, alwaysPattern, err))
	}
	variant, variantSwitched := variantSwitch(request.PostForm, stored)
	if switched || variantSwitched {
		return server.regenerate(writer, request, current, name, enabled, variant)
	}
	switch action {
	case actionValidate:
		return server.startValidation(writer, request, current, name)
	case actionFiles:
		return redirect(writer, request, projectURL(name, pageFiles))
	}
	return redirect(writer, request, projectURL(name, pageReview)+"?applied")
}

// trustAlways adds an image namespace to registries.allow the way the
// Engine settings page saves it.
func (server *Server) trustAlways(request *http.Request, current *visit, pattern string) error {
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	allow := server.config().Registries.Allow
	if pattern == "" || slices.Contains(allow, pattern) {
		return nil
	}
	allow = append(slices.Clone(allow), pattern)
	return server.update(request, current, map[string]string{registriesAllowKey: jsonText(allow)})
}

// reviewProblem shows the Review page again with what went wrong; the
// revision errors are the person's to fix.
func (server *Server) reviewProblem(writer http.ResponseWriter, request *http.Request, current *visit, name string, err error) error {
	stored, loadErr := pipeline.Load(request.Context(), server.workspace(), name)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	return server.renderReview(writer, request, current, stored, false, "", capitalised(err.Error())+".", http.StatusBadRequest)
}

// regenerate generates the project again from its source, as its latest
// version was made, with the optional services enabled switched on, from
// the compose file composeFile when one is named. It runs as a job, as the
// New page's do, and lands on the Review.
func (server *Server) regenerate(writer http.ResponseWriter, request *http.Request, current *visit, name string, enabled []string, composeFile string) error {
	stored, err := pipeline.Load(request.Context(), server.workspace(), name)
	if err != nil {
		return err
	}
	options, err := stored.Regeneration(enabled, composeFile)
	if err != nil {
		return server.reviewProblem(writer, request, current, name, err)
	}
	username := current.user.Username
	started, err := server.jobs.start(jobGenerate, stored.Project.SourceURL, stored.Project.Name, func(ctx context.Context, running *job) (string, *pipeline.Result, error) {
		return server.generate(ctx, running, username, options, false)
	})
	if err != nil {
		return err
	}
	return redirect(writer, request, "/jobs/"+started.ID)
}

func (server *Server) removeBenign(ctx context.Context, username, name, pattern string) error {
	connection, err := server.connect(ctx, username, pipeline.EngineRules, false)
	if err != nil {
		return err
	}
	return pipeline.RemoveBenignPattern(ctx, connection.Dependencies, name, pattern)
}

func (server *Server) revise(ctx context.Context, username, name string, revision pipeline.Revision) (*pipeline.Result, error) {
	connection, err := server.connect(ctx, username, pipeline.EngineRules, false)
	if err != nil {
		return nil, err
	}
	return pipeline.Revise(ctx, connection.Dependencies, name, revision)
}

// blankHostHint is what a blank public address gives: the derived host,
// else the published port, else none.
func blankHostHint(derivedHost, mode string) string {
	if derivedHost != "" {
		return derivedHost
	}
	if mode == render.ExposeNone {
		return noAddressHint
	}
	return publishedPortHint
}

func revisionFrom(form url.Values, stored *pipeline.Stored) pipeline.Revision {
	revision := pipeline.Revision{ExposeVia: form.Get("expose"), Networks: form["network"], PublicHost: pipeline.TidyPublicHost(form.Get(publicHostField)),
		Storage: form.Get("storage"), AllowNeeds: form["need"], AllowRegistries: form["registry"], Values: map[string]string{}, Secrets: map[string]string{}}
	for _, entry := range stored.Plan.EnvFile {
		value, present := form[valuePrefix+entry.Key]
		// The page shows the rendered value, as a public URL follows the
		// exposure: sent back unchanged, it is no edit to pin.
		edited := present && value[0] != entry.Value && value[0] != stored.Values[entry.Key]
		if edited && pipeline.Editable(entry) {
			revision.Values[entry.Key] = value[0]
		}
		if typed := form.Get(secretPrefix + entry.Key); typed != "" && pipeline.Supplied(entry) {
			revision.Secrets[entry.Key] = typed
		}
	}
	if key := strings.TrimSpace(form.Get("add-key")); key != "" {
		revision.Add = []pipeline.Variable{{Key: key, Value: form.Get("add-value")}}
	}
	if service, limit, found := strings.Cut(form.Get(memoryLimitField), memoryLimitSeparator); found {
		revision.MemoryLimits = map[string]string{service: limit}
	}
	return revision
}

// changed compares the form with the project as it renders: the Review
// shows the profile's defaults already applied.
func changed(revision pipeline.Revision, stored *pipeline.Stored, profile config.Profile) bool {
	options := stored.Options
	mode, networks := render.Exposure(profile, options.ExposeVia, options.Networks)
	edits := len(revision.Values) + len(revision.Secrets) + len(revision.Regenerate) + len(revision.Add) + len(revision.MemoryLimits) + len(revision.Ports)
	return edits > 0 || revision.ExposeVia != mode || !sameSet(revision.Networks, networks) || revision.PublicHost != options.PublicHost ||
		revision.Storage != options.Storage ||
		!sameSet(revision.AllowNeeds, options.AllowNeeds) || !sameSet(revision.AllowRegistries, options.AllowRegistries)
}

// optionalSwitch are the optional services the form has ticked, with those
// they depend on, and whether they differ from those the plan runs: an
// optional service another ticked one depends on is switched on whether
// ticked or not. A source that cannot be read again shows them read-only,
// so its form never switches.
func optionalSwitch(form url.Values, stored *pipeline.Stored) (enabled []string, switched bool, err error) {
	if !stored.Rereadable() {
		return nil, false, nil
	}
	enabled, err = rules.Enabled(stored.Plan.OptionalServices, form[optionalField])
	return enabled, err == nil && !sameSet(enabled, rules.Running(stored.Plan)), err
}

// variantSwitch is the compose file the form chose, when the project can be
// generated again from it.
func variantSwitch(form url.Values, stored *pipeline.Stored) (string, bool) {
	chosen := form.Get(composeFileField)
	if !stored.CanBuildFrom(chosen) {
		return "", false
	}
	return chosen, true
}

func sameSet(left, right []string) bool {
	return len(left) == len(right) && !slices.ContainsFunc(left, func(value string) bool { return !slices.Contains(right, value) })
}

// reveal shows the secrets on the Review or Files page; every reveal is
// written to the audit log.
func (server *Server) reveal(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	view := request.PostFormValue("view")
	details := map[string]any{"generation": stored.Generation.ID, "via": viaWeb, "page": view, "keys": stored.MaskedKeys()}
	if err := server.workspace().Store.Audit(ctx, webActorPrefix+current.user.Username, store.ActionReveal, stored.Project.ID, details); err != nil {
		return err
	}
	if view == actionFiles {
		return server.renderFiles(writer, request, current, stored, filesState{revealed: true, tab: fileEnv})
	}
	return server.renderReview(writer, request, current, stored, true, "", "", http.StatusOK)
}

// markBenign keeps a log line that failed a run as normal for this app,
// matched as literal text without its container and time, and runs the
// test again.
func (server *Server) markBenign(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	name := request.PathValue("name")
	if err := server.busy(name); err != nil {
		return err
	}
	message := sandbox.LogMessage(request.PostFormValue(lineField))
	if message == "" {
		return badRequest(emptyLineProblem)
	}
	connection, err := server.connect(ctx, current.user.Username, pipeline.EngineRules, false)
	if err != nil {
		return err
	}
	if err := pipeline.AddBenignPattern(ctx, connection.Dependencies, name, regexp.QuoteMeta(message)); err != nil {
		return err
	}
	return server.startValidation(writer, request, current, name)
}

// validate approves and runs the latest generation from the project or
// files page.
func (server *Server) validate(writer http.ResponseWriter, request *http.Request, current *visit) error {
	return server.startValidation(writer, request, current, request.PathValue("name"))
}

func (server *Server) startValidation(writer http.ResponseWriter, request *http.Request, current *visit, name string) error {
	address, err := server.launchValidation(request.Context(), current, name)
	if err != nil {
		return err
	}
	return redirect(writer, request, address)
}

// launchValidation starts a test run of a project's latest version and
// returns the address of its live page.
func (server *Server) launchValidation(ctx context.Context, current *visit, name string) (string, error) {
	stored, err := pipeline.Load(ctx, server.workspace(), name)
	if err != nil {
		return "", err
	}
	if stored.Report.Blocked() {
		return "", conflict("The policy gate blocks %s. Resolve the findings on the Review page first.", name)
	}
	engine, username := stored.Generation.Engine, current.user.Username
	started, err := server.jobs.start(jobValidate, "Validating "+name, stored.Project.Name, func(ctx context.Context, running *job) (string, *pipeline.Result, error) {
		connection, err := server.connect(ctx, username, engine, true)
		if err != nil {
			return "", nil, err
		}
		if connection.Note != "" {
			running.notice(connection.Note)
		}
		dependencies := connection.Dependencies
		dependencies.Observer = running.observer()
		result, err := pipeline.ApproveAndValidate(ctx, dependencies, stored.Project.Name, viaWeb)
		verdict, found := lastVerdict(result)
		if found && verdict.Passed() {
			return projectURL(stored.Project.Name, pageFiles), result, err
		}
		return "", result, err
	})
	if err != nil {
		return "", err
	}
	return "/jobs/" + started.ID, nil
}

// reviewTones colour the Review page's summary chips.
type reviewTones struct {
	Confidence string
	Policy     string
	Warnings   string
}

var confidenceTones = map[string]string{ir.ConfidenceHigh: toneOk, ir.ConfidenceMedium: toneWarn, ir.ConfidenceLow: toneDanger}

func tonesFor(confidence string, report policy.Report) reviewTones {
	tones := reviewTones{Confidence: confidenceTones[confidence], Policy: toneOk, Warnings: toneNeutral}
	if tones.Confidence == "" {
		tones.Confidence = toneNeutral
	}
	if report.Blocked() {
		tones.Policy = toneDanger
	}
	if report.Count(policy.Warn) > 0 {
		tones.Warnings = toneWarn
	}
	return tones
}

func policyText(report policy.Report) string {
	blocking := report.Count(policy.Deny) + report.Count(policy.Need)
	if blocking > 0 {
		return "Policy gate: " + countText(blocking, "finding blocks", "findings block") + " the plan"
	}
	return "Policy gate: passed"
}
