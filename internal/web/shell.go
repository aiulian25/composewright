package web

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	iconPassed  = "ok"
	iconDraft   = "draft"
	iconBlocked = "blocked"
	iconFailed  = "failed"
	homePrefix  = "~"
	hostsJoiner = ", "
)

// Crumb is one step of the breadcrumb in the top bar.
type Crumb struct {
	Text string
	Href string
}

// ProjectLink is a project in the sidebar, with an icon for its state. It
// opens the step the project is at.
type ProjectLink struct {
	Name     string
	Subtitle string
	Icon     string
	Href     string
	// Badge is what a project still needs, for the Home page's recent list.
	Badge   projectBadge
	Current bool
}

type projectBadge struct {
	Text string
	Tone string
}

// EngineMenu is the top bar's switch between rules only and a model.
type EngineMenu struct {
	Label   string
	Options []option
}

// StatusBar is the line along the bottom of every signed-in page.
type StatusBar struct {
	Docker     string
	DockerTone string
	Sandbox    string
	Engine     string
	Egress     string
	DataDir    string
	Version    string
}

var (
	projectIcons = map[string]string{store.StatusPassed: iconPassed, store.StatusGenerated: iconDraft, store.StatusBlocked: iconBlocked,
		store.StatusFailed: iconFailed, store.StatusNeedsHuman: iconFailed}
	engineLabels = map[string]string{config.EngineRules: "rules only", config.EngineAssisted: "rules + model", config.EngineModel: "model drafts"}
	// projectSteps is the page a project's state leads to: a plan to
	// review, a failed run to read, or files that are ready.
	projectSteps = map[string]string{store.StatusPassed: pageFiles, store.StatusGenerated: pageReview, store.StatusBlocked: pageReview,
		store.StatusFailed: pageValidate, store.StatusNeedsHuman: pageValidate}
	projectBadges = map[string]projectBadge{store.StatusGenerated: {"needs a test", toneAccent}, store.StatusBlocked: {"blocked", toneDanger},
		store.StatusFailed: {"test failed", toneDanger}, store.StatusNeedsHuman: {"test failed", toneDanger}}
)

// projectLinks lists every project for the sidebar, most recent first.
func (server *Server) projectLinks(summaries []store.ProjectSummary, listingError error) []ProjectLink {
	if listingError != nil {
		server.options.Logger.Warn("listing projects for the sidebar", "error", listingError)
		return nil
	}
	links := make([]ProjectLink, 0, len(summaries))
	for _, summary := range summaries {
		links = append(links, ProjectLink{Name: summary.Name, Subtitle: projectSubtitle(summary), Icon: projectIcons[summary.Status],
			Href: projectURL(summary.Name, projectSteps[summary.Status]), Badge: projectBadges[summary.Status]})
	}
	return links
}

func projectSubtitle(summary store.ProjectSummary) string {
	services := countText(len(strings.Fields(summary.Images)), "service", "services")
	switch summary.Status {
	case store.StatusGenerated:
		return "draft · " + services
	case store.StatusBlocked:
		return "blocked by the policy gate"
	case store.StatusFailed, store.StatusNeedsHuman:
		return "did not pass · " + services
	}
	return services
}

// engineOptions are the ways a plan can be written; the model ones need a
// configured provider.
func (server *Server) engineOptions(selected string) []option {
	settings := server.config().LLM
	label, hasModel := providerNames[settings.Provider]
	model := label + " · " + settings.Model
	if name := savedName(settings); name != "" {
		model = name + " (" + model + ")"
	}
	if !hasModel {
		model = "a model (none configured)"
	}
	options := []option{
		{Value: config.EngineRules, Label: "Rules only (no model)", Sub: "The rules read the repository; nothing is sent to a model."},
		{Value: config.EngineAssisted, Label: "Rules, then " + model + " for what they leave open", Disabled: !hasModel,
			Sub: "The model sees only the fields below the confidence threshold, with secrets redacted."},
		{Value: config.EngineModel, Label: model + " drafts the whole plan", Disabled: !hasModel,
			Sub: "The model writes the plan from the facts; the policy gate still decides what may run."},
	}
	for index := range options {
		options[index].Selected = options[index].Value == selected
	}
	return options
}

func (server *Server) engineMenu() EngineMenu {
	current := server.config().LLM.Engine
	return EngineMenu{Label: engineLabels[current], Options: server.engineOptions(current)}
}

func (server *Server) statusBar(ctx context.Context) StatusBar {
	configuration := server.config()
	docker := server.status.docker(ctx)
	sandbox := "Sandbox idle"
	if project, busy := server.jobs.validating(); busy {
		sandbox = "Sandbox: " + project + " running"
	}
	return StatusBar{Docker: docker.Text, DockerTone: docker.Tone, Sandbox: sandbox, Engine: "Engine: " + engineLabels[configuration.LLM.Engine],
		Egress: "Egress: " + strings.Join(egressHosts(configuration, server.options.Version), hostsJoiner), DataDir: homeRelative(server.paths().DataDir),
		Version: server.options.Version}
}

// egressHosts are the hosts the guard lets a web run contact: the code
// hosts, every host behind a trusted registry and, when a model is set up,
// its server, even if the default engine is rules only, since a run may pick
// a model. The update check's host is the CLI's alone.
func egressHosts(configuration config.Config, version string) []string {
	policy, err := netguard.PolicyFromConfig(configuration, nil, version)
	if err != nil {
		// No run can build its guard either, so none contacts anything.
		return nil
	}
	hosts := slices.Concat(policy.Hosts[netguard.SCM], policy.Hosts[netguard.Registry], policy.Hosts[netguard.LLM])
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return homePrefix + strings.TrimPrefix(path, home)
}
