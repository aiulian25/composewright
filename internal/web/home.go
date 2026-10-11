package web

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
)

const (
	navHome = "home"
	// recentShown is how many projects the Home page lists as recent.
	recentShown = 5
	lineBreak   = "\n"
)

// pick is an app the Home page offers to start from.
type pick struct {
	Name     string
	Link     string
	Initials string
	Tone     string
}

// picks are the repositories of composewright's own test set
// (testdata/corpus), so each is known to go through.
var picks = []pick{
	{Name: "Uptime Kuma", Link: "github.com/louislam/uptime-kuma", Initials: "UK", Tone: toneAccent},
	{Name: "Vaultwarden", Link: "github.com/dani-garcia/vaultwarden", Initials: "V", Tone: toneInfo},
	{Name: "Paperless-ngx", Link: "github.com/paperless-ngx/paperless-ngx", Initials: "PN", Tone: toneOk},
	{Name: "Linkding", Link: "github.com/sissbruecker/linkding", Initials: "L", Tone: toneNeutral},
	{Name: "Miniflux", Link: "github.com/miniflux/v2", Initials: "M", Tone: toneAccent},
	{Name: "FreshRSS", Link: "github.com/FreshRSS/FreshRSS", Initials: "F", Tone: toneInfo},
	{Name: "Navidrome", Link: "github.com/navidrome/navidrome", Initials: "N", Tone: toneOk},
}

// pastedEnvLine is a line a .env file may hold at the top level: an assignment,
// a comment or nothing. A compose file never has a KEY=value line there.
var pastedEnvLine = regexp.MustCompile(`^(\s*|#.*|(export\s+)?[A-Za-z_][A-Za-z0-9_]*=.*)$`)

type homeView struct {
	Ask     string
	Engines []option
	Picks   []pick
	Recent  []ProjectLink
}

// home is the landing page: one box to start from a link, files or a
// pasted compose file, a few apps to try, and the latest projects.
func (server *Server) home(writer http.ResponseWriter, request *http.Request, current *visit) error {
	return server.renderHome(writer, request, current, "", "", http.StatusOK)
}

func (server *Server) renderHome(writer http.ResponseWriter, request *http.Request, current *visit, ask, problem string, status int) error {
	page := server.page(request, current, "Home")
	page.Nav, page.Crumbs, page.Error = navHome, []Crumb{{Text: "Home"}}, problem
	options := server.engineOptions(server.config().LLM.Engine)
	for index := range options {
		options[index].Label = engineLabels[options[index].Value]
	}
	page.Data = homeView{Ask: ask, Engines: options, Picks: picks, Recent: page.Projects[:min(len(page.Projects), recentShown)]}
	return server.render(writer, status, "home", page)
}

// start begins a generation from the Home page's box; a refused request
// comes back to Home with what was typed.
func (server *Server) start(writer http.ResponseWriter, request *http.Request, current *visit) error {
	ask := askFrom(request)
	if problem := ask.problem(); problem != "" {
		return server.renderHome(writer, request, current, request.PostFormValue("ask"), problem, http.StatusBadRequest)
	}
	return server.launch(writer, request, current, ask)
}

// askFrom reads the Home page's box. One line is a link or a folder;
// several lines are a pasted compose file, with any .env lines before or
// after it. A suggested app counts as its link.
func askFrom(request *http.Request) generationRequest {
	ask := generationRequest{engine: request.PostFormValue("engine"), files: uploadedFiles(request)}
	text := field(request, "ask")
	if chosen := field(request, "pick"); chosen != "" {
		text = chosen
	}
	if !strings.Contains(text, lineBreak) {
		ask.source = text
		return ask
	}
	ask.compose, ask.env = splitPaste(text)
	return ask
}

// splitPaste takes the .env lines pasted before or after a compose file
// out of it.
func splitPaste(text string) (compose, env string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", lineBreak), lineBreak)
	start, end := 0, len(lines)
	for start < end && pastedEnvLine.MatchString(lines[start]) {
		start++
	}
	for end > start && pastedEnvLine.MatchString(lines[end-1]) {
		end--
	}
	rest := append(slices.Clone(lines[:start]), lines[end:]...)
	return strings.Join(lines[start:end], lineBreak), strings.TrimSpace(strings.Join(rest, lineBreak))
}
