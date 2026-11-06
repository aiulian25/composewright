package web

import (
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
)

func duplicateIntakeFiles(files []*multipart.FileHeader, compose, env string) string {
	seen := map[string]bool{}
	for _, file := range files {
		name := uploadName(file)
		if seen[name] {
			return fmt.Sprintf("Two files are called %s; give each file once.", name)
		}
		seen[name] = true
	}
	for name, text := range map[string]string{pastedCompose: compose, pastedEnv: env} {
		if strings.TrimSpace(text) != "" && seen[name] {
			return fmt.Sprintf("%s was both uploaded and pasted; choose one copy.", name)
		}
	}
	return ""
}

func intakeFailure(err error) (int, string) {
	var known statusError
	if errors.As(err, &known) {
		return known.status, known.message
	}
	return http.StatusInternalServerError, "The analysis could not start. Your pasted text is kept; try again."
}

func (server *Server) renderIntakeDraft(writer http.ResponseWriter, request *http.Request, current *visit, ask generationRequest, problem string, status int) error {
	writer.Header().Set("Cache-Control", pageCache)
	page := server.page(request, current, "New project").flow(stepSource)
	view := server.newView(ask.source, ask.ref, ask.name, ask.engine)
	view.Compose, view.Env, view.Reselect, view.FilesOnly = ask.compose, ask.env, len(ask.files) > 0, ask.filesOnly
	page.Error, page.Data, page.Draft = problem, view, true
	return server.render(writer, status, "new", page)
}
