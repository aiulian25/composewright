package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/aiulian25/composewright/internal/store"
)

const (
	sectionAudit    = "audit"
	auditPageSize   = 50
	auditActionKey  = "action"
	auditProjectKey = "project"
	auditBeforeKey  = "before"
	auditExportName = "composewright-audit.jsonl"
	jsonLinesType   = "application/jsonl; charset=utf-8"
	auditVia        = "audit"
	detailsIndent   = "  "
	auditIntact     = "Hash chain intact, %s."
	auditCommand    = "composewright audit list"
)

// auditView is the Audit log page: one page of entries, newest first, the
// filters that picked them and where the next page starts.
type auditView struct {
	Entries  []auditRow
	Actions  []option
	Projects []option
	Action   string
	Project  string
	// Next is the sequence number older entries start before; zero when
	// this page holds the oldest.
	Next int64
}

type auditRow struct {
	Sequence int64
	Time     time.Time
	Actor    string
	Action   string
	Project  string
	// Details is the entry's details as indented JSON. They may name a
	// secret but never hold its value.
	Details string
}

// auditFilter reads the filters from the page's query, or from the export
// form that repeats them.
func auditFilter(request *http.Request) store.AuditFilter {
	before, _ := strconv.ParseInt(request.FormValue(auditBeforeKey), 10, 64)
	return store.AuditFilter{Action: request.FormValue(auditActionKey), ProjectID: request.FormValue(auditProjectKey), Before: before}
}

func (server *Server) auditView(ctx context.Context, filter store.AuditFilter) (auditView, error) {
	database := server.workspace().Store
	view := auditView{Action: filter.Action, Project: filter.ProjectID}
	// One more than a page says whether older entries are left.
	filter.Limit = auditPageSize + 1
	entries, err := database.AuditPage(ctx, filter)
	if err != nil {
		return view, err
	}
	if len(entries) > auditPageSize {
		entries = entries[:auditPageSize]
		view.Next = entries[auditPageSize-1].Sequence
	}
	actions, err := database.AuditActions(ctx)
	if err != nil {
		return view, err
	}
	for _, action := range actions {
		view.Actions = append(view.Actions, option{Value: action, Label: action, Selected: action == filter.Action})
	}
	names, err := server.auditProjects(ctx, &view)
	if err != nil {
		return view, err
	}
	for _, entry := range entries {
		row, err := auditRowFor(entry, names)
		if err != nil {
			return view, err
		}
		view.Entries = append(view.Entries, row)
	}
	return view, nil
}

// auditProjects offers every project the log names in the Project filter:
// those that exist by name, those deleted since by ID. It returns the names.
func (server *Server) auditProjects(ctx context.Context, view *auditView) (map[string]string, error) {
	database := server.workspace().Store
	projects, err := database.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, project := range projects {
		names[project.ID] = project.Name
		view.Projects = append(view.Projects, option{Value: project.ID, Label: project.Name, Selected: project.ID == view.Project})
	}
	logged, err := database.AuditProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, identifier := range logged {
		if _, exists := names[identifier]; exists {
			continue
		}
		view.Projects = append(view.Projects, option{Value: identifier, Label: identifier, Selected: identifier == view.Project})
	}
	return names, nil
}

// auditRowFor names the entry's project, or keeps the ID of one that was
// deleted since.
func auditRowFor(entry store.AuditEntry, names map[string]string) (auditRow, error) {
	row := auditRow{Sequence: entry.Sequence, Time: entry.Time, Actor: entry.Actor, Action: entry.Action, Project: entry.ProjectID}
	if name, known := names[entry.ProjectID]; known {
		row.Project = name
	}
	if len(entry.Details) == 0 {
		return row, nil
	}
	details, err := json.MarshalIndent(entry.Details, "", detailsIndent)
	row.Details = string(details)
	return row, err
}

// verifyAudit recomputes the audit log's hash chain and says whether it
// holds.
func (server *Server) verifyAudit(writer http.ResponseWriter, request *http.Request, current *visit) error {
	state := settingsState{shown: sectionAudit, status: http.StatusOK, configuration: server.config()}
	count, err := server.workspace().Store.VerifyAudit(request.Context())
	if err != nil {
		state.problem = capitalised(err.Error()) + "."
		return server.renderSettings(writer, request, current, state)
	}
	state.notice = fmt.Sprintf(auditIntact, countText(count, "entry", "entries"))
	return server.renderSettings(writer, request, current, state)
}

// exportAudit downloads the entries the filters pick as JSON Lines, one
// entry per line, and audits the download.
func (server *Server) exportAudit(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ctx, database := request.Context(), server.workspace().Store
	filter := auditFilter(request)
	filter.Before = 0
	entries, err := database.AuditPage(ctx, filter)
	if err != nil {
		return err
	}
	details := map[string]any{"via": auditVia, "entries": len(entries)}
	if err := database.Audit(ctx, webActorPrefix+current.user.Username, store.ActionExport, "", details); err != nil {
		return err
	}
	writer.Header().Set(contentType, jsonLinesType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+auditExportName+`"`)
	encoder := json.NewEncoder(writer)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	return nil
}
