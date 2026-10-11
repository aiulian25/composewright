package web

import (
	"net/http"
	"strings"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	webActorPrefix = "web:"
	auditShown     = 25
)

var statusTones = map[string]string{
	store.StatusPassed:     toneOk,
	store.StatusGenerated:  toneNeutral,
	store.StatusBlocked:    toneDanger,
	store.StatusFailed:     toneDanger,
	store.StatusNeedsHuman: toneWarn,
}

// as is the workspace one signed-in user acts through: audit entries name
// them and the configuration is the current one.
func (server *Server) as(username string) *workspace.Workspace {
	view := *server.options.Workspace
	view.Actor = webActorPrefix + username
	view.Config = server.config()
	return &view
}

type projectRow struct {
	Name    string
	Status  string
	Tone    string
	Source  string
	Updated string
	Engine  string
	Images  []string
	Running string
}

func (server *Server) projects(writer http.ResponseWriter, request *http.Request, current *visit) error {
	summaries, err := server.workspace().Store.ListProjects(request.Context())
	if err != nil {
		return err
	}
	rows := make([]projectRow, 0, len(summaries))
	for _, summary := range summaries {
		row := projectRow{Name: summary.Name, Status: summary.Status, Tone: statusTones[summary.Status], Engine: summary.Engine,
			Source: sourceText(&fetch.Source{URL: summary.SourceURL, Commit: summary.SourceCommit}), Updated: summary.UpdatedAt.Local().Format(timeLayout),
			Images: strings.Fields(summary.Images)}
		if running, busy := server.jobs.running(summary.Name); busy {
			row.Running = "/jobs/" + running.ID
		}
		rows = append(rows, row)
	}
	page := server.page(request, current, "Projects")
	page.Nav, page.Data, page.Crumbs = navProjects, rows, []Crumb{{Text: "Projects"}}
	return server.render(writer, http.StatusOK, "projects", page)
}

type generationRow struct {
	ID         string
	When       string
	Engine     string
	Iteration  int
	Validation string
	Tone       string
	// Tested versions link to their run; Detail is why a failed one failed.
	Tested bool
	Detail string
}

type projectView struct {
	Project     store.Project
	Tone        string
	Source      string
	Blocked     bool
	Generations []generationRow
	Audit       []store.AuditEntry
	Running     string
}

func (server *Server) projectPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx := request.Context()
	stored, err := pipeline.Load(ctx, server.workspace(), request.PathValue("name"))
	if err != nil {
		return err
	}
	database := server.workspace().Store
	generations, err := database.Generations(ctx, stored.Project.ID)
	if err != nil {
		return err
	}
	audit, err := database.AuditEntries(ctx, stored.Project.ID, auditShown)
	if err != nil {
		return err
	}
	view := projectView{Project: stored.Project, Tone: statusTones[stored.Project.Status], Source: sourceText(storedSource(stored)),
		Blocked: stored.Report.Blocked(), Audit: audit}
	for _, generation := range generations {
		view.Generations = append(view.Generations, generationRowFor(generation))
	}
	if running, busy := server.jobs.running(stored.Project.Name); busy {
		view.Running = "/jobs/" + running.ID
	}
	page := server.page(request, current, stored.Project.Name).of(stored.Project.Name)
	// This is the project's own page, so its crumb is not a link.
	page.Crumbs[len(page.Crumbs)-1].Href = ""
	page.Nav, page.Data = navProjects, view
	return server.render(writer, http.StatusOK, "project", page)
}

func generationRowFor(generation store.Generation) generationRow {
	row := generationRow{ID: generation.ID, When: generation.CreatedAt.Local().Format(timeLayout), Engine: engineText(generation),
		Iteration: generation.Iteration, Validation: "not run", Tone: toneNeutral}
	verdict, tested := storedVerdict(generation)
	if !tested {
		return row
	}
	row.Validation, row.Tone, row.Tested = verdict.Result, toneDanger, true
	if verdict.Passed() {
		row.Tone = toneOk
	}
	if !verdict.Passed() {
		row.Detail = verdict.Detail
	}
	if verdict.Reason != "" {
		row.Validation += " (" + verdict.Reason + ")"
	}
	return row
}

// deleteView is the delete confirmation; keeping the project goes back to
// the step it is at.
type deleteView struct {
	Name string
	Back string
}

func (server *Server) deletePage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	project, err := server.workspace().Store.FindProject(request.Context(), request.PathValue("name"))
	if err != nil {
		return err
	}
	page := server.page(request, current, "Delete "+project.Name).of(project.Name)
	page.Nav, page.Data = navProjects, deleteView{Name: project.Name, Back: projectURL(project.Name, projectSteps[project.Status])}
	return server.render(writer, http.StatusOK, "delete", page)
}

func (server *Server) delete(writer http.ResponseWriter, request *http.Request, current *visit) error {
	name := request.PathValue("name")
	if _, busy := server.jobs.running(name); busy {
		return conflict("%s is running. Stop the run before deleting the project.", name)
	}
	if _, err := pipeline.Remove(request.Context(), server.as(current.user.Username), name, map[string]any{"via": viaWeb}); err != nil {
		return err
	}
	return redirect(writer, request, "/projects")
}
