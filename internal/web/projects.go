package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
// them and the configuration and the rules catalogue are the current ones.
// It leaves out the master key, which a rotation swaps while requests run.
func (server *Server) as(username string) *workspace.Workspace {
	shared := server.workspace()
	return &workspace.Workspace{Config: server.config(), Paths: shared.Paths, Store: shared.Store, Actor: webActorPrefix + username,
		Catalogue: server.catalogue.Load()}
}

type projectRow struct {
	ID       string
	Labels   string
	PinOrder int
	Revision int64
	Name     string
	Status   string
	Tone     string
	Source   string
	Updated  string
	Engine   string
	Images   []string
	Running  string
}

type projectFilter struct{ Query, Status, Label string }
type projectGroup struct {
	Label string
	Rows  []projectRow
}
type projectsView struct {
	Groups   []projectGroup
	Pinned   []projectRow
	Other    []projectRow
	Total    int
	Matches  int
	Filter   projectFilter
	Labels   []option
	Statuses []option
	Next     string
}

func readProjectFilter(request *http.Request) projectFilter {
	query := request.URL.Query()
	return projectFilter{Query: strings.TrimSpace(query.Get("q")), Status: query.Get("status"), Label: strings.ToLower(query.Get("label"))}
}

func labelTokens(labels []string) string {
	tokens := make([]string, 0, len(labels))
	for _, label := range labels {
		tokens = append(tokens, strings.ToLower(label))
	}
	return strings.Join(tokens, ",")
}

func labelOptions(labels [][]string, selected string) []option {
	names := map[string]string{}
	for _, group := range labels {
		for _, label := range group {
			key := strings.ToLower(label)
			if _, found := names[key]; !found {
				names[key] = label
			}
		}
	}
	if selected != "" {
		if _, found := names[selected]; !found {
			names[selected] = selected
		}
	}
	var keys []string
	for key := range names {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var options []option
	for _, key := range keys {
		options = append(options, option{Value: key, Label: names[key], Selected: key == selected})
	}
	return options
}
func projectStatusOptions(selected string) []option {
	var options []option
	for _, status := range []string{store.StatusGenerated, store.StatusPassed, store.StatusBlocked, store.StatusFailed, store.StatusNeedsHuman} {
		options = append(options, option{Value: status, Label: strings.ReplaceAll(status, "_", " "), Selected: status == selected})
	}
	if selected != "" && !slices.ContainsFunc(options, func(item option) bool { return item.Value == selected }) {
		options = append(options, option{Value: selected, Label: selected, Selected: true})
	}
	return options
}

func projectMatches(summary store.ProjectSummary, filter projectFilter) bool {
	if !strings.Contains(strings.ToLower(summary.Name), strings.ToLower(filter.Query)) || filter.Status != "" && summary.Status != filter.Status {
		return false
	}
	return filter.Label == "" || slices.Contains(strings.Split(labelTokens(summary.Labels), ","), filter.Label)
}

func (server *Server) projects(writer http.ResponseWriter, request *http.Request, current *visit) error {
	page := server.page(request, current, "Projects")
	if page.listingError != nil {
		return page.listingError
	}
	view := projectsViewFor(page.summaries, readProjectFilter(request))
	view.Next = "/projects"
	if query := request.URL.RawQuery; query != "" {
		view.Next += "?" + query
	}
	for _, group := range [][]projectRow{view.Pinned, view.Other} {
		for index := range group {
			if running, busy := server.jobs.running(group[index].Name); busy {
				group[index].Running = "/jobs/" + running.ID
			}
		}
	}
	page.Nav, page.Data, page.Crumbs = navProjects, view, []Crumb{{Text: "Projects"}}
	return server.render(writer, http.StatusOK, "projects", page)
}

func projectsViewFor(summaries []store.ProjectSummary, filter projectFilter) projectsView {
	view := projectsView{Total: len(summaries), Filter: filter, Statuses: projectStatusOptions(filter.Status)}
	var labels [][]string
	for _, summary := range summaries {
		labels = append(labels, summary.Labels)
		if !projectMatches(summary, filter) {
			continue
		}
		row := projectRow{ID: summary.ID, Name: summary.Name, Labels: strings.Join(summary.Labels, ", "), PinOrder: summary.PinOrder, Revision: summary.OrganizationRevision, Status: summary.Status, Tone: statusTones[summary.Status], Engine: summary.Engine,
			Source: sourceText(&fetch.Source{URL: summary.SourceURL, Commit: summary.SourceCommit}), Updated: summary.UpdatedAt.Local().Format(timeLayout), Images: strings.Fields(summary.Images)}
		view.Matches++
		if row.PinOrder > 0 {
			view.Pinned = append(view.Pinned, row)
		} else {
			view.Other = append(view.Other, row)
		}
	}
	slices.SortFunc(view.Pinned, func(a, b projectRow) int {
		if a.PinOrder < b.PinOrder {
			return -1
		}
		if a.PinOrder > b.PinOrder {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(view.Pinned) > 0 {
		view.Groups = append(view.Groups, projectGroup{Label: "Pinned", Rows: view.Pinned})
	}
	if len(view.Other) > 0 {
		label := ""
		if len(view.Pinned) > 0 {
			label = "Other projects"
		}
		view.Groups = append(view.Groups, projectGroup{Label: label, Rows: view.Other})
	}
	view.Labels = labelOptions(labels, filter.Label)
	return view
}

// Organization is independent of deployments, but cannot race a restore's
// captured recovery snapshot. Authentication and CSRF come from authed.
func (server *Server) organizeProject(writer http.ResponseWriter, request *http.Request, current *visit) error {
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if server.jobs.restorePending() {
		return conflict("Finish the pending restore before organizing projects.")
	}
	project, err := server.workspace().Store.FindProject(request.Context(), request.PathValue("name"))
	if err != nil {
		return err
	}
	if request.PostFormValue("project_id") != project.ID {
		return conflict("This project was replaced. Reload Projects before changing its organization.")
	}
	revision, err := strconv.ParseInt(request.PostFormValue("revision"), 10, 64)
	if err != nil || revision < 0 {
		return badRequest("Reload Projects before changing its organization.")
	}
	err = server.workspace().Store.OrganizeProject(request.Context(), project.ID, revision, strings.Split(request.PostFormValue("labels"), ","), request.PostFormValue("action"))
	if errors.Is(err, store.ErrOrganizationStale) {
		return conflict("%s", err)
	}
	if errors.Is(err, store.ErrOrganizationInvalid) {
		return badRequest("Organization was not changed: %s", err)
	}
	if err != nil {
		return err
	}
	next := request.PostFormValue("next")
	parsed, parseErr := url.Parse(next)
	if parseErr != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || strings.HasPrefix(next, "//") || parsed.Path != "/projects" || parsed.Fragment != "" {
		next = "/projects"
	}
	return redirect(writer, request, next)
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
	// Checked says Images holds what a check of the images just found.
	Checked bool
	Images  []imageRow
}

func (server *Server) projectPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	return server.renderProject(writer, request, current, nil)
}

// renderProject shows the project page; checked, when not nil, is what a
// check of its images found.
func (server *Server) renderProject(writer http.ResponseWriter, request *http.Request, current *visit, checked []pipeline.ImageStatus) error {
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
	audit, err := database.AuditPage(ctx, store.AuditFilter{ProjectID: stored.Project.ID, Limit: auditShown})
	if err != nil {
		return err
	}
	view := projectView{Project: stored.Project, Tone: statusTones[stored.Project.Status], Source: sourceText(storedSource(stored)),
		Blocked: stored.Report.Blocked(), Audit: audit, Checked: checked != nil,
		Images: imageRows(checked, stored.Options.Edited)}
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
	verdict, tested := pipeline.StoredVerdict(generation)
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
