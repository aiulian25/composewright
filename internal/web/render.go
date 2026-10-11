package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/config"
)

const (
	navNew      = "new"
	navProjects = "projects"
	navSettings = "settings"

	stepSource   = 1
	stepAnalyse  = 2
	stepReview   = 3
	stepValidate = 4
	stepDone     = 5

	toneOk      = "ok"
	toneWarn    = "warn"
	toneDanger  = "danger"
	toneNeutral = "neutral"
	toneAccent  = "accent"
	toneInfo    = "info"

	timeLayout  = "2 Jan 2006 15:04"
	shortID     = 12
	contentType = "Content-Type"
	htmlType    = "text/html; charset=utf-8"
	jsonType    = "application/json; charset=utf-8"
)

var stepNames = []string{"Source", "Analyse", "Review", "Validate", "Result"}

const (
	pageAnalyse  = "analyse"
	pageReview   = "review"
	pageValidate = "validate"
	pageFiles    = "files"
)

// stepPages are the project pages behind the steps; Source has none once a
// project exists.
var stepPages = []string{"", pageAnalyse, pageReview, pageValidate, pageFiles}

// Page is what the layout needs; Data is the page's own view.
type Page struct {
	Title   string
	Theme   string
	Nav     string
	Steps   []StepLink
	CSRF    string
	User    string
	Notice  string
	Error   string
	Version string
	// Address is the page's own address with its query, for forms that
	// come back to it.
	Address  string
	Crumbs   []Crumb
	Projects []ProjectLink
	Engine   EngineMenu
	Status   StatusBar
	// Context pages have the pane on the right; CLI is the command its
	// footer shows for the same thing. Draft marks the New project page.
	Context bool
	CLI     string
	Draft   bool
	Data    any
}

type StepLink struct {
	Number int
	Name   string
	State  string
	Href   string
	Failed bool
}

func (server *Server) page(request *http.Request, current *visit, title string) Page {
	page := Page{Title: title, Theme: theme(server.config().UI.Theme), Version: server.assets, Address: request.URL.RequestURI()}
	if current == nil {
		return page
	}
	page.User, page.CSRF = current.user.Username, current.session.CSRF
	page.Crumbs = []Crumb{{Text: "Projects", Href: "/projects"}, {Text: title}}
	page.Projects = server.projectLinks(request.Context(), "")
	page.Engine, page.Status = server.engineMenu(), server.statusBar(request.Context())
	return page
}

// flow marks the page as a step of the New flow, with the context pane.
func (page Page) flow(step int) Page {
	page.Nav, page.Context = navNew, true
	for index, name := range stepNames {
		state := "todo"
		switch {
		case index+1 < step:
			state = "done"
		case index+1 == step:
			state = "current"
		}
		page.Steps = append(page.Steps, StepLink{Number: index + 1, Name: name, State: state})
	}
	return page
}

// of puts the page under a project: the breadcrumb names it, the sidebar
// marks it and the steps link to its pages.
func (page Page) of(project string) Page {
	page.Crumbs = []Crumb{{Text: "Projects", Href: "/projects"}, {Text: project, Href: projectURL(project, "")}}
	for index := range page.Projects {
		page.Projects[index].Current = page.Projects[index].Name == project
	}
	for index := range page.Steps {
		if stepPages[index] != "" {
			page.Steps[index].Href = projectURL(project, stepPages[index])
		}
	}
	return page
}

func theme(configured string) string {
	switch configured {
	case config.ThemeLight, config.ThemeSystem:
		return configured
	}
	return config.ThemeDark
}

// render executes a page into a buffer first, so a template error never
// sends half a page.
func (server *Server) render(writer http.ResponseWriter, status int, name string, page Page) error {
	pageTemplate, found := server.pages[name]
	if !found {
		return fmt.Errorf("no template for page %q", name)
	}
	var buffer bytes.Buffer
	if err := pageTemplate.Execute(&buffer, page); err != nil {
		return err
	}
	writer.Header().Set(contentType, htmlType)
	writer.WriteHeader(status)
	_, err := buffer.WriteTo(writer)
	return err
}

func functions() template.FuncMap {
	return template.FuncMap{
		"short": func(identifier string) string { return identifier[:min(len(identifier), shortID)] },
		"when": func(moment time.Time) string {
			if moment.IsZero() {
				return "never"
			}
			return moment.Local().Format(timeLayout)
		},
		"duration": func(value time.Duration) string { return value.Round(time.Second).String() },
		"plural": func(count int, one, many string) string {
			if count == 1 {
				return "1 " + one
			}
			return strconv.Itoa(count) + " " + many
		},
		"join":  strings.Join,
		"upper": strings.ToUpper,
	}
}

func redirect(writer http.ResponseWriter, request *http.Request, target string) error {
	http.Redirect(writer, request, target, http.StatusSeeOther) //nolint:gosec // G710: targets are built here or filtered by localPath
	return nil
}
