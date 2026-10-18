package rules

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
)

const (
	httpPort = "80"
	// hostnameVariable is the shell variable some healthchecks name their
	// own container by, written $HOSTNAME or $$HOSTNAME in compose.
	hostnameVariable = "HOSTNAME"
	variableMarks    = "${}"
)

// probeAccepted are the answers that show a documented path works: success
// or a redirect, as the explicit list the plan schema allows.
var probeAccepted = []int{200, 201, 202, 203, 204, 301, 302, 303, 307, 308}

var (
	// failsOnHTTPError is a healthcheck that turns an HTTP error into a
	// failure: wget does, curl only with -f, --fail or --fail-with-body.
	// ponytail: a -f anywhere in the command counts, even one given to test.
	failsOnHTTPError = regexp.MustCompile(`\bwget\b|(?:^|\s)(?:-[a-zA-Z]*f[a-zA-Z]*|--fail(?:-with-body)?)(?:\s|$)`)
	// sendsCredentials is a healthcheck that logs in, which the probe does not.
	sendsCredentials = regexp.MustCompile(`(?i)(?:^|\s)(?:-u|--user|--http-user|--password|--http-password)[\s=]|authorization:`)
)

// finish names the project, writes the summary, probe, notes and suspicious
// text, rolls confidence up and lists what remains unfilled.
func (builder *builder) finish() {
	plan := builder.plan
	app := plan.AppService()
	plan.App = ir.App{Name: builder.projectName(app), UpstreamURL: builder.upstreamURL()}
	plan.App.Summary = truncate(builder.facts.Summary.Text, maxSummaryLength)
	if port, found := probePort(app); found {
		plan.Probe = ir.Probe{Service: app.Name, Port: port, Path: defaultProbePath}
		builder.probePath(app, &plan.Probe)
	}
	for _, note := range builder.facts.Notes {
		plan.PostInstallNotes = append(plan.PostInstallNotes, fmt.Sprintf("%s (%s)", note.Text, note.Location))
	}
	for _, entry := range plan.EnvFile {
		if entry.Generate != "" {
			plan.PostInstallNotes = append(plan.PostInstallNotes, fmt.Sprintf("%s was generated for you; it is in %s", entry.Key, kept(entry)))
		}
	}
	for _, note := range builder.facts.Suspicious {
		plan.SuspiciousInstructions = append(plan.SuspiciousInstructions, fmt.Sprintf("%s: %s", note.Location, note.Text))
	}
	plan.Confidence = rollUp(plan)
	plan.Unfilled = Gaps(plan)
}

// kept is the file beside compose.yaml that holds an entry's value.
func kept(entry ir.EnvFileEntry) string {
	if entry.AsFile() {
		return entry.SecretPath()
	}
	return ownEnvFile
}

// probePort is the app's first published TCP port, else its first TCP
// port: the probe asks over HTTP, which a UDP port cannot answer.
func probePort(app *ir.Service) (int, bool) {
	if app == nil {
		return 0, false
	}
	var tcp []ir.Port
	for _, port := range app.Ports {
		if port.Protocol == ir.ProtocolTCP {
			tcp = append(tcp, port)
		}
	}
	if len(tcp) == 0 {
		return 0, false
	}
	published := slices.IndexFunc(tcp, func(port ir.Port) bool { return port.Publish })
	return tcp[max(published, 0)].Container, true
}

// probePath asks the probe for the path the evidence says answers: the URL
// the app's healthcheck asks on the probe port, else the path the catalogue
// knows, else the health endpoint the README documents. Without one the
// probe asks / and takes any answer.
func (builder *builder) probePath(app *ir.Service, probe *ir.Probe) {
	if healthcheckProbe(app, probe) {
		return
	}
	documented := known.appProbePath(app.Image)
	if documented == "" && len(builder.facts.HealthPaths) > 0 {
		documented = builder.facts.HealthPaths[0].Text
	}
	if documented != "" {
		probe.Path, probe.Expect = documented, slices.Clone(probeAccepted)
	}
}

// healthcheckProbe points the probe at the URL the service's healthcheck
// asks, when that is the container itself on the probe port, and expects
// success only when the healthcheck itself fails on an HTTP error.
func healthcheckProbe(service *ir.Service, probe *ir.Probe) bool {
	asked, found := ir.HealthcheckURL(service.Healthcheck.Test)
	if !found || strings.ContainsAny(asked.Path, variableMarks) {
		return false
	}
	host := strings.ToLower(asked.Hostname())
	itself := slices.Contains(ir.LoopbackHosts, host) || host == service.Name ||
		strings.Trim(host, variableMarks) == strings.ToLower(hostnameVariable)
	port := cmp.Or(asked.Port(), httpPort)
	if !itself || port != strconv.Itoa(probe.Port) {
		return false
	}
	probe.Path, probe.Expect = cmp.Or(asked.EscapedPath(), defaultProbePath), nil
	command := strings.Join(service.Healthcheck.Test, " ")
	if failsOnHTTPError.MatchString(command) && !sendsCredentials.MatchString(command) {
		probe.Expect = slices.Clone(probeAccepted)
	}
	return true
}

func (builder *builder) projectName(app *ir.Service) string {
	if builder.input.Name != "" {
		return slug(builder.input.Name)
	}
	if app != nil && !slices.Contains(genericServiceNames, app.Name) {
		return slug(app.Name)
	}
	return slug(firstNonEmpty(builder.repositoryName(), imageName(builder.plan, app), "app"))
}

// imageName is the last part of an image repository, such as nginx for
// docker.io/library/nginx: the app's, or the first service's when no
// service has the app role.
func imageName(plan *ir.IR, app *ir.Service) string {
	if app == nil && len(plan.Services) > 0 {
		app = &plan.Services[0]
	}
	if app == nil || app.Image.Repository == "" {
		return ""
	}
	return path.Base(app.Image.Repository)
}

func (builder *builder) upstreamURL() string {
	source := builder.input.Source
	if source.Owner == "" {
		return ""
	}
	return source.URL
}

func rollUp(plan *ir.IR) string {
	app := plan.AppService()
	switch {
	case app == nil || app.Image.Registry == "":
		return ir.ConfidenceLow
	case app.Image.Provenance.Confidence == ir.ConfidenceHigh:
		return ir.ConfidenceHigh
	}
	return ir.ConfidenceMedium
}

// Gaps lists the plan fields the rules could not fill or filled with low
// confidence; the review asks about them and a model may propose them.
func Gaps(plan *ir.IR) []string {
	var gaps []string
	if plan.App.Summary == "" {
		gaps = append(gaps, ir.SummaryField)
	}
	for index, service := range plan.Services {
		field := fmt.Sprintf("services.%d", index)
		if service.Image.Registry == "" || service.Image.Repository == "" {
			gaps = append(gaps, field+".image")
		}
		needsHealthcheck := service.Role == ir.RoleApp || service.Role == ir.RoleDB
		if needsHealthcheck && !service.Healthcheck.Defined() && service.Healthcheck.OmittedReason == "" {
			gaps = append(gaps, field+".healthcheck")
		}
		if service.Role == ir.RoleApp && len(service.Ports) == 0 {
			gaps = append(gaps, field+".ports")
		}
	}
	for _, entry := range plan.EnvFile {
		if entry.Description == "" {
			gaps = append(gaps, ir.DescriptionField(entry.Key))
		}
	}
	return gaps
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}
