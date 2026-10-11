package web

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/redact"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	maxEnvFacts    = 12
	commitLength   = 7
	stateDone      = "done"
	statePending   = "pending"
	kindPlain      = "plain"
	kindGenerated  = "generated"
	kindSupplied   = "supplied"
	kindReference  = "reference"
	lineTimeLayout = "15:04:05.000"
	githubPrefix   = "https://"
)

type stepInfo struct {
	Name  string
	Title string
	About string
}

var generationSteps = []stepInfo{
	{pipeline.StepFetch, "Fetch", "GitHub tarball through the egress guard, or the files you gave; no git clone, no hooks"},
	{pipeline.StepExtract, "Extract facts (no model)", "compose files, Dockerfiles, env samples and docs, each with its file and line"},
	{pipeline.StepPlan, "Build the plan", "deterministic rules R1 to R6 with provenance and confidence"},
	{pipeline.StepImages, "Verify images", "registry allowlist · manifest HEAD · pin digest · source label against the repository"},
	{pipeline.StepModel, "Ask the model", "only about fields the rules left open; repository content sent as data, secrets redacted"},
	{pipeline.StepPolicy, "Policy gate", "enforced in code, independent of the model"},
	{pipeline.StepRender, "Render and store", "compose.yaml, .env and DEPLOY.md, read back with compose-go and gated again"},
}

type stepView struct {
	Number  int
	Name    string
	Title   string
	Detail  string
	State   string
	Elapsed string
}

func steps(catalogue []stepInfo, job snapshot, details map[string]string) []stepView {
	views := make([]stepView, 0, len(catalogue))
	for index, info := range catalogue {
		view := stepView{Number: index + 1, Name: info.Name, Title: info.Title, Detail: info.About, State: statePending}
		step, seen := job.Steps[info.Name]
		if seen {
			view.State = step.State
		}
		if detail := details[info.Name]; detail != "" && view.State == stateDone {
			view.Detail = detail
		}
		if step.Note != "" {
			view.Detail = step.Note
		}
		if record := job.Timings[info.Name]; !record.started.IsZero() && !record.ended.IsZero() {
			view.Elapsed = record.ended.Sub(record.started).Round(100 * time.Millisecond).String()
		}
		views = append(views, view)
	}
	return views
}

// sourceText is a source the way a person names it: owner/repository at a
// short commit, or the folder.
func sourceText(source *fetch.Source) string {
	if source == nil {
		return ""
	}
	text := strings.TrimPrefix(source.URL, githubPrefix)
	if source.Ref != "" {
		text += " " + source.Ref
	}
	if source.Commit != "" {
		text += " @ " + source.Commit[:min(len(source.Commit), commitLength)]
	}
	return text
}

type factRow struct {
	Kind   string
	Value  string
	Source string
	Secret bool
}

// factRows lists what extraction found, each with where it was found:
// images, ports, volumes, healthchecks and users in full, the first
// variables, and chips that count the rest.
func factRows(facts *extract.Facts) ([]factRow, []string) {
	if facts == nil {
		return nil, nil
	}
	var rows []factRow
	seen := map[string]bool{}
	add := func(row factRow) {
		key := row.Kind + "\x00" + row.Value
		if row.Value == "" || seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, row)
	}
	for _, file := range facts.ComposeFiles {
		for _, service := range file.Services {
			add(factRow{Kind: "image", Value: service.Image, Source: service.ImageAt.String()})
			for _, port := range service.Ports {
				add(factRow{Kind: "port", Value: fmt.Sprintf("%d/%s", port.Target, protocol(port.Protocol)), Source: port.Location.String()})
			}
			for _, volume := range service.Volumes {
				add(factRow{Kind: "volume", Value: volume.Target, Source: volume.Location.String()})
			}
			if service.Healthcheck != nil && len(service.Healthcheck.Test) > 0 {
				add(factRow{Kind: "healthcheck", Value: strings.Join(service.Healthcheck.Test, " "), Source: service.Healthcheck.Location.String()})
			}
		}
	}
	for _, dockerfile := range facts.Dockerfiles {
		for _, port := range dockerfile.Exposed {
			add(factRow{Kind: "port", Value: fmt.Sprintf("%d/%s", port.Port, protocol(port.Protocol)), Source: port.Location.String()})
		}
		for _, volume := range dockerfile.Volumes {
			add(factRow{Kind: "volume", Value: volume.Path, Source: volume.Location.String()})
		}
		if dockerfile.User != "" {
			add(factRow{Kind: "user", Value: "USER " + dockerfile.User, Source: dockerfile.UserAt.String()})
		}
	}
	for _, image := range facts.Images {
		add(factRow{Kind: "image", Value: image.Reference, Source: image.Location.String()})
	}
	variables := 0
	for _, variable := range facts.Variables {
		if variables == maxEnvFacts {
			break
		}
		where := ""
		if len(variable.Mentions) > 0 {
			where = variable.Mentions[0].Location.String()
		}
		add(factRow{Kind: "env", Value: variable.Name, Source: where, Secret: redact.IsSecretName(variable.Name)})
		variables++
	}
	var chips []string
	if extra := len(facts.Variables) - variables; extra > 0 {
		chips = append(chips, fmt.Sprintf("+%d more variables", extra))
	}
	chips = append(chips, countText(len(facts.ComposeFiles), "upstream compose file", "upstream compose files")+" (read as hints)")
	chips = append(chips, countText(len(facts.Suspicious), "suspicious instruction", "suspicious instructions")+" in the documentation")
	return rows, chips
}

func extractSummary(facts *extract.Facts) string {
	if facts == nil {
		return ""
	}
	images := map[string]bool{}
	for _, file := range facts.ComposeFiles {
		for _, service := range file.Services {
			images[service.Image] = service.Image != ""
		}
	}
	return fmt.Sprintf("%s · %s · %s · %s", countText(len(facts.Variables), "variable", "variables"),
		countText(len(images), "image", "images"), countText(len(facts.ComposeFiles), "compose file", "compose files"),
		countText(len(facts.Files), "file read", "files read"))
}

func countText(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return strconv.Itoa(count) + " " + many
}

func protocol(value string) string {
	if value == "" {
		return ir.ProtocolTCP
	}
	return value
}

type envRow struct {
	Key         string
	Value       string
	Stored      bool
	Kind        string
	Generator   string
	Source      string
	Description string
	Required    bool
	Secret      bool
	Badge       string
	BadgeTone   string
	SourceShort string
	Confidence  string
	// Question marks a value the rules were unsure of that the person can
	// change: the Review page highlights it.
	Question bool
}

// envRows shows every variable: plain values and connection strings as
// editable text, generated secrets masked unless revealed, and secrets a
// person must supply as empty password fields.
func envRows(plan *ir.IR, values map[string]string, revealed bool) []envRow {
	rows := make([]envRow, 0, len(plan.EnvFile))
	for _, entry := range plan.EnvFile {
		row := envRow{Key: entry.Key, Value: values[entry.Key], Stored: values[entry.Key] != "", Generator: entry.Generate,
			Description: entry.Description, Required: entry.Required, Secret: entry.Secret, Source: entry.Provenance.SourceRef}
		if row.Source == "" {
			row.Source = entry.Provenance.Source
		}
		switch {
		case entry.Generate != "":
			row.Kind = kindGenerated
		case pipeline.Supplied(entry):
			row.Kind = kindSupplied
		case entry.Secret:
			row.Kind = kindReference
		default:
			row.Kind = kindPlain
		}
		hidden := entry.Secret && !revealed && row.Kind != kindReference
		if hidden {
			row.Value = ""
		}
		row.Badge, row.Confidence, row.SourceShort = sourceBadges[entry.Provenance.Source], entry.Provenance.Confidence, path.Base(row.Source)
		row.BadgeTone = badgeTones[row.Badge]
		row.Question = row.Kind == kindPlain && row.Confidence != ir.ConfidenceHigh
		rows = append(rows, row)
	}
	return rows
}

type needView struct {
	Capability string
	Label      string
	Service    string
	Reason     string
	Requested  bool
	Granted    bool
}

var capabilityLabels = map[string]string{ir.NeedHostNetwork: "Host network", ir.NeedDevices: "Devices", ir.NeedPrivileged: "Privileged",
	ir.NeedBindMount: "Host folder", ir.NeedLowPort: "Port below 1024", ir.NeedCapAdd: "Capabilities"}

func needViews(plan *ir.IR, granted []string) []needView {
	requested := map[string]ir.Need{}
	for _, need := range plan.Needs {
		requested[need.Capability] = need
	}
	views := make([]needView, 0, len(ir.Capabilities))
	for _, capability := range ir.Capabilities {
		need, asked := requested[capability]
		views = append(views, needView{Capability: capability, Label: capabilityLabels[capability], Service: need.Service, Reason: need.Reason,
			Requested: asked, Granted: slices.Contains(granted, capability)})
	}
	return views
}

// findingView is a policy finding on the Review page; Line is the line of
// compose.yaml to change, and Code its text.
type findingView struct {
	Rule    string
	Service string
	Message string
	Tone    string
	Label   string
	Line    int
	Code    string
}

var severityViews = map[policy.Severity][2]string{
	policy.Deny: {toneDanger, "blocks the plan"},
	policy.Need: {toneDanger, "needs your grant"},
	policy.Warn: {toneWarn, "warning"},
}

func findingViews(report policy.Report, compose string) []findingView {
	lines, texts := composefile.KeyLines(compose), strings.Split(compose, "\n")
	var views []findingView
	for _, severity := range []policy.Severity{policy.Deny, policy.Need, policy.Warn} {
		for _, finding := range report.Findings {
			if finding.Severity != severity {
				continue
			}
			style, line := severityViews[severity], finding.Line(lines)
			views = append(views, findingView{Rule: finding.Rule, Service: finding.Service, Message: finding.Message, Tone: style[0], Label: style[1],
				Line: line, Code: lineText(texts, line)})
		}
	}
	return views
}

// trustOffers are the image namespaces a person could trust, one per image:
// to lift a registry-allowlist block, or to vouch for a namespace trusted
// only by the owner's name.
func trustOffers(plan *ir.IR, report policy.Report) []string {
	var offers []string
	for _, finding := range report.Findings {
		settledByTrust := strings.Contains(finding.Message, policy.OutsideAllowlist) || strings.Contains(finding.Message, policy.NotGitHubAccounts)
		if finding.Rule != "P1" || !settledByTrust {
			continue
		}
		service, found := plan.Service(finding.Service)
		if !found {
			continue
		}
		namespace, _, _ := strings.Cut(service.Image.Repository, "/")
		pattern := service.Image.Registry + "/" + namespace + "/*"
		if !slices.Contains(offers, pattern) {
			offers = append(offers, pattern)
		}
	}
	return offers
}

type lineView struct {
	Time   string
	Source string
	Text   string
	Tone   string
	Error  bool
}

// errorLine marks a log line worth reading when a run fails; the Logs
// pane can show only those. app.js uses the same pattern for live lines.
var errorLine = regexp.MustCompile(`(?i)\b(error|fatal|panic|denied|unhealthy|exited with code [1-9])`)

func lineViews(lines []sandbox.Line) []lineView {
	views := make([]lineView, 0, len(lines))
	for _, line := range lines {
		view := lineView{Source: line.Source, Text: line.Text, Tone: lineTone(line.Source), Error: errorLine.MatchString(line.Text)}
		if !line.Time.IsZero() {
			view.Time = line.Time.Local().Format(lineTimeLayout)
		}
		views = append(views, view)
	}
	return views
}

// questionCount is how many values the Review page asks about.
func questionCount(rows []envRow) int {
	count := 0
	for _, row := range rows {
		if row.Question {
			count++
		}
	}
	return count
}

// runStep is one row of the Validate page's Run card.
type runStep struct {
	Name   string
	Detail string
	State  string
}

const (
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
	healthStarting  = "starting"
	containerExited = "exited"
	containerDead   = "dead"
	tearDown        = "down -v --remove-orphans"
)

// startingRun is a run in progress: only the order of its steps is known;
// the live output carries the rest.
var startingRun = []runStep{{Name: "Policy gate on rendered YAML", Detail: "passed", State: stateDone},
	{Name: "Pull images and start", Detail: "live output on the right", State: pipeline.StateRunning},
	{Name: "Every service healthy", State: statePending}, {Name: "Scan the logs", State: statePending},
	{Name: "Reach the app from the network", State: statePending},
	{Name: "Tear down", Detail: tearDown, State: statePending}}

// runSteps lays a finished run out: the gate, starting, each service, the
// log scan, the reachability probe and the teardown.
func runSteps(verdict sandbox.Verdict) []runStep {
	steps := []runStep{{Name: "Policy gate on rendered YAML", Detail: "passed", State: stateDone}}
	startState, startDetail := stateDone, countText(len(verdict.Services), "service", "services")
	if verdict.Reason == sandbox.ReasonStart {
		startState, startDetail = pipeline.StateFailed, verdict.Detail
	}
	steps = append(steps, runStep{Name: "Pull images and start", Detail: startDetail, State: startState})
	for _, state := range verdict.Services {
		steps = append(steps, serviceStep(state, verdict))
	}
	// The sandbox scans the logs only when nothing failed before.
	scan := runStep{Name: "Scan the logs", Detail: "no fatal line", State: stateDone}
	switch {
	case verdict.Reason == sandbox.ReasonLogs && len(verdict.LogMatches) > 0:
		scan.Detail, scan.State = verdict.LogMatches[0], pipeline.StateFailed
	case !verdict.Passed() && verdict.Reason != sandbox.ReasonUnreachable:
		scan.Detail, scan.State = pipeline.StateSkipped, pipeline.StateSkipped
	}
	steps = append(steps, scan)
	if verdict.Reach != nil {
		steps = append(steps, reachStep(verdict))
	}
	return append(steps, runStep{Name: "Tear down", Detail: tearDown, State: stateDone})
}

// reachStep is the reachability probe: failed when nothing answered,
// skipped when it did not run.
func reachStep(verdict sandbox.Verdict) runStep {
	step := runStep{Name: "Reach " + verdict.Reach.Service + " from the network", Detail: verdict.Reach.Said, State: stateDone}
	switch {
	case verdict.Reason == sandbox.ReasonUnreachable:
		step.State = pipeline.StateFailed
	case !verdict.Reach.Checked:
		step.State = pipeline.StateSkipped
	}
	return step
}

// serviceStep follows the sandbox's own verdict rules: a stopped container
// fails unless it is an init service that finished, then health decides.
func serviceStep(state sandbox.ServiceState, verdict sandbox.Verdict) runStep {
	stopped := state.State == containerExited || state.State == containerDead
	switch {
	case stopped && state.ExitCode == 0 && verdict.Passed():
		return runStep{Name: state.Service + " finished", Detail: "exit code 0", State: stateDone}
	case stopped:
		return runStep{Name: fmt.Sprintf("%s exited with code %d", state.Service, state.ExitCode), Detail: `restart: "no"`, State: pipeline.StateFailed}
	case state.Health == healthUnhealthy:
		return runStep{Name: state.Service + " unhealthy", Detail: "healthcheck failed", State: pipeline.StateFailed}
	case state.Health == healthHealthy:
		return runStep{Name: state.Service + " healthy", Detail: "healthcheck passed", State: stateDone}
	case verdict.Reason == sandbox.ReasonTimeout && state.Health == healthStarting:
		return runStep{Name: state.Service + " not healthy in time", Detail: healthStarting, State: pipeline.StateFailed}
	}
	return runStep{Name: state.Service + " " + state.State, Detail: state.Health, State: stateDone}
}

func lineTone(source string) string {
	switch source {
	case sandbox.SourceSandbox:
		return "sandbox"
	case sandbox.SourceCompose:
		return "compose"
	}
	return "service"
}

type stateView struct {
	Service string
	State   string
	Detail  string
	Tone    string
}

func stateViews(verdict sandbox.Verdict) []stateView {
	views := make([]stateView, 0, len(verdict.Services))
	for _, state := range verdict.Services {
		view := stateView{Service: state.Service, State: state.State, Tone: toneOk}
		if state.Health != "" {
			view.State = state.Health
		}
		switch {
		case state.Health == healthUnhealthy || state.State == containerExited || state.State == containerDead:
			view.Tone = toneDanger
			view.Detail = fmt.Sprintf("exit code %d", state.ExitCode)
		case state.Health == healthStarting:
			view.Tone = toneAccent
		}
		views = append(views, view)
	}
	return views
}
