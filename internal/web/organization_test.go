package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/store"
)

func organizationServer(t *testing.T) *Server {
	t.Helper()
	server := intakeServer(t)
	server.options.Now = time.Now
	server.options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server.jobs = newJobs(time.Now, server.options.Logger)
	server.status = &statusCache{now: time.Now, check: func(context.Context) dockerState { return dockerState{Text: "not checked", Tone: toneNeutral} }}
	server.mux = http.NewServeMux()
	server.routes()
	return server
}

func TestProjectOrganizationAuthenticatedFormsAndFilters(t *testing.T) {
	ctx := context.Background()
	server := organizationServer(t)
	db := server.workspace().Store
	project, err := db.SaveProject(ctx, store.Project{Name: "media-app", Slug: "media-app", Status: store.StatusPassed, SourceKind: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveProject(ctx, store.Project{Name: "other", Slug: "other", Status: store.StatusFailed, SourceKind: "local"}); err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateFirstUser(ctx, "admin", "dummy-hash")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session := store.Session{IDHash: auth.TokenHash("dummy-session"), UserID: user.ID, CSRF: "dummy-csrf", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {session.CSRF}, "project_id": {project.ID}, "revision": {"0"}, "labels": {"Media, Home"}, "action": {"pin"}, "next": {"/projects?q=media&status=passed&label=media"}}
	send := func(cookie, csrf bool) *httptest.ResponseRecorder {
		submitted := url.Values{}
		for key, values := range form {
			submitted[key] = append([]string{}, values...)
		}
		if !csrf {
			submitted.Del("csrf")
		}
		request := httptest.NewRequest(http.MethodPost, "/projects/media-app/organization", strings.NewReader(submitted.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie {
			request.AddCookie(&http.Cookie{Name: plainSessionCookie, Value: "dummy-session"})
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	if response := send(false, true); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated organization allowed: %d", response.Code)
	}
	if response := send(true, false); response.Code != http.StatusForbidden {
		t.Fatalf("CSRF check bypassed: %d", response.Code)
	}
	unchanged, _ := db.FindProject(ctx, project.ID)
	if unchanged.PinOrder != 0 || len(unchanged.Labels) != 0 {
		t.Fatal("rejected writes changed metadata")
	}
	response := send(true, true)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != form.Get("next") {
		t.Fatalf("filter context lost: %d %s", response.Code, response.Body.String())
	}
	saved, _ := db.FindProject(ctx, project.ID)
	if saved.PinOrder != 1 || !reflect.DeepEqual(saved.Labels, []string{"Home", "Media"}) {
		t.Fatalf("organization not saved: %+v", saved)
	}
	if response := send(true, true); response.Code != http.StatusConflict {
		t.Fatalf("stale form accepted: %d", response.Code)
	}
	request := httptest.NewRequest(http.MethodGet, form.Get("next"), nil)
	request.AddCookie(&http.Cookie{Name: plainSessionCookie, Value: "dummy-session"})
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("projects did not render: %s", response.Body.String())
	}
	body := response.Body.String()
	for _, text := range []string{"1 of 2 projects", "Pinned", "Home, Media", "never passwords or tokens", `name="revision" value="` + strconv.FormatInt(saved.OrganizationRevision, 10) + `"`} {
		if !strings.Contains(body, text) {
			t.Errorf("missing organization UI %q", text)
		}
	}
	if strings.Contains(body, `aria-label="Organize other"`) {
		t.Fatal("combined filters did not narrow main list")
	}
	// A forged return URL never redirects outside the project list.
	form.Set("revision", strconv.FormatInt(saved.OrganizationRevision, 10))
	form.Set("next", "https://unrelated.example/")
	if response := send(true, true); response.Header().Get("Location") != "/projects" {
		t.Fatal("external redirect accepted")
	}
	saved, _ = db.FindProject(ctx, project.ID)
	form.Set("revision", strconv.FormatInt(saved.OrganizationRevision, 10))
	form.Set("project_id", "replaced-project-id")
	if response := send(true, true); response.Code != http.StatusConflict {
		t.Fatal("replaced project identity accepted")
	}
}

func TestProjectOrganizationFilteringAndCurrentNavigation(t *testing.T) {
	summaries := []store.ProjectSummary{
		{Project: store.Project{Name: "alpha", Status: store.StatusPassed, Labels: []string{"Media"}, PinOrder: 2}},
		{Project: store.Project{Name: "beta", Status: store.StatusFailed, Labels: []string{"Media"}, PinOrder: 1}},
		{Project: store.Project{Name: "gamma", Status: store.StatusPassed, Labels: []string{"Home"}}},
	}
	view := projectsViewFor(summaries, projectFilter{Query: "AL", Status: store.StatusPassed, Label: "media"})
	if view.Matches != 1 || len(view.Pinned) != 1 || view.Pinned[0].Name != "alpha" || view.Total != 3 {
		t.Fatalf("filters not combined: %+v", view)
	}
	if empty := projectsViewFor(summaries, projectFilter{Label: "absent"}); empty.Matches != 0 {
		t.Fatal("unknown label matched projects")
	}
	server := organizationServer(t)
	page := Page{User: "admin", Projects: server.projectLinks(summaries, nil), Data: projectsViewFor(summaries, projectFilter{})}.of("alpha")
	if pins := page.PinnedProjects(); len(pins) != 2 || pins[0].Name != "beta" || !pins[1].Current || pins[1].Href != projectURL("alpha", pageFiles) {
		t.Fatalf("pinned projection changed navigation: %+v", pins)
	}
	if page.Projects[0].Name != "alpha" {
		t.Fatal("sidebar sorting changed shared/Home order")
	}
	response := httptest.NewRecorder()
	if err := server.render(response, http.StatusOK, "projects", page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Body.String(), `data-project-current="true"`) || !strings.Contains(response.Body.String(), `aria-current="page"`) {
		t.Fatal("sidebar lost active navigation")
	}
}
