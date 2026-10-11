package rules

import (
	"fmt"
	"path"
	"slices"

	"github.com/aiulian25/composewright/internal/ir"
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
	}
	for _, note := range builder.facts.Notes {
		plan.PostInstallNotes = append(plan.PostInstallNotes, fmt.Sprintf("%s (%s)", note.Text, note.Location))
	}
	for _, entry := range plan.EnvFile {
		if entry.Generate != "" {
			plan.PostInstallNotes = append(plan.PostInstallNotes, fmt.Sprintf("%s was generated for you; it is in .env", entry.Key))
		}
	}
	for _, note := range builder.facts.Suspicious {
		plan.SuspiciousInstructions = append(plan.SuspiciousInstructions, fmt.Sprintf("%s: %s", note.Location, note.Text))
	}
	plan.Confidence = rollUp(plan)
	plan.Unfilled = Gaps(plan)
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
		gaps = append(gaps, "app.summary")
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
			gaps = append(gaps, "env_file."+entry.Key+".description")
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
