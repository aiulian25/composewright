package web

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/web"
)

const (
	fetchSiteHeader = "Sec-Fetch-Site"
	sameOriginSite  = "same-origin"
	userAction      = "none"
	multipartPrefix = "multipart/form-data"
	staticPrefix    = "static"
)

// handler is a page or action; a returned error becomes an error page.
type handler func(writer http.ResponseWriter, request *http.Request, visit *visit) error

// visit is the signed-in user and session of a request.
type visit struct {
	user    store.User
	session store.Session
}

// statusError is an error whose message is safe to show.
type statusError struct {
	status  int
	message string
}

func (failure statusError) Error() string {
	return failure.message
}

func badRequest(format string, arguments ...any) error {
	return statusError{status: http.StatusBadRequest, message: fmt.Sprintf(format, arguments...)}
}

func conflict(format string, arguments ...any) error {
	return statusError{status: http.StatusConflict, message: fmt.Sprintf(format, arguments...)}
}

// ServeHTTP sets the security headers every response carries, refuses
// unknown hosts and cross-site writes, then routes.
func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	header := writer.Header()
	header.Set("Content-Security-Policy", contentSecurity)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	header.Set("Permissions-Policy", permissionsPolicy)
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	if !server.knownHost(request.Host) {
		http.Error(writer, "This address is not one composewright answers to; start it with --allow-host for a proxy name.", http.StatusMisdirectedRequest)
		return
	}
	if unsafeMethod(request.Method) && !sameOrigin(request) {
		http.Error(writer, "Cross-site request refused.", http.StatusForbidden)
		return
	}
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) knownHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	// An address typed as an IP cannot be DNS rebinding, which needs a name
	// the attacker controls; names must be known.
	return net.ParseIP(host) != nil || server.hosts[host]
}

func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// sameOrigin accepts a write when the browser says it comes from this
// origin; requests without either header (scripts, curl) still need the
// CSRF token of a session.
func sameOrigin(request *http.Request) bool {
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		return err == nil && strings.EqualFold(parsed.Host, request.Host)
	}
	site := request.Header.Get(fetchSiteHeader)
	return site == "" || site == sameOriginSite || site == userAction
}

// static serves the embedded assets; nothing is read from disk.
func (server *Server) static() http.Handler {
	files, err := fs.Sub(web.Files, staticPrefix)
	if err != nil {
		panic(err)
	}
	fileServer := http.StripPrefix("/static/", http.FileServerFS(files))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", staticMaxAge)
		fileServer.ServeHTTP(writer, request)
	})
}

// public serves pages that need no session: setup and sign-in.
func (server *Server) public(next handler) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", pageCache)
		request.Body = http.MaxBytesReader(writer, request.Body, formLimit)
		current, err := server.visit(request)
		if err == nil {
			err = next(writer, request, current)
		}
		if err != nil {
			server.fail(writer, request, current, err)
		}
	}
}

// authed serves pages for a signed-in user. Every write must carry the
// session's CSRF token.
func (server *Server) authed(next handler) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", pageCache)
		current, err := server.visit(request)
		if err != nil {
			server.fail(writer, request, nil, err)
			return
		}
		if current == nil {
			server.toLogin(writer, request)
			return
		}
		if unsafeMethod(request.Method) {
			if err := parseForm(writer, request); err != nil {
				server.fail(writer, request, current, badRequest("The form could not be read: %v", err))
				return
			}
			if !auth.SameToken(csrfOf(request), current.session.CSRF) {
				server.fail(writer, request, current, statusError{status: http.StatusForbidden, message: "This form has expired. Reload the page and try again."})
				return
			}
		}
		if err := next(writer, request, current); err != nil {
			server.fail(writer, request, current, err)
		}
	}
}

func parseForm(writer http.ResponseWriter, request *http.Request) error {
	limit := int64(formLimit)
	multipart := strings.HasPrefix(request.Header.Get("Content-Type"), multipartPrefix)
	if multipart {
		limit = uploadLimit
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	if multipart {
		return request.ParseMultipartForm(uploadMemory) //nolint:gosec // G120: the body is capped by MaxBytesReader above
	}
	return request.ParseForm()
}

func csrfOf(request *http.Request) string {
	if token := request.Header.Get(csrfHeader); token != "" {
		return token
	}
	return request.PostFormValue(csrfField)
}

func (server *Server) toLogin(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "Sign in first.", http.StatusUnauthorized)
		return
	}
	target := "/login"
	if request.URL.Path != "/" {
		target += "?next=" + url.QueryEscape(request.URL.RequestURI())
	}
	http.Redirect(writer, request, target, http.StatusSeeOther)
}

// visit loads the signed-in session from the first session cookie that
// names a live one. A browser keeps cookies per host, not per port, so it
// may still send a stale one from another instance or from before an HTTPS
// proxy, and that must not hide the valid one.
func (server *Server) visit(request *http.Request) (*visit, error) {
	for _, token := range sessionTokens(request) {
		current, err := server.session(request, token)
		if err != nil || current != nil {
			return current, err
		}
	}
	return nil, nil
}

// session loads one session, ending it when it is past its absolute expiry
// or idle for too long.
func (server *Server) session(request *http.Request, token string) (*visit, error) {
	ctx := request.Context()
	database := server.workspace().Store
	session, err := database.SessionByHash(ctx, auth.TokenHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil //nolint:nilerr // an unknown session means the visitor is not signed in
	}
	if err != nil {
		return nil, err
	}
	now := server.options.Now()
	expired := !now.Before(session.ExpiresAt) || now.Sub(session.LastSeen) > sessionIdle
	if expired {
		return nil, database.DeleteSession(ctx, session.IDHash)
	}
	user, err := database.UserByID(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	if now.Sub(session.LastSeen) > touchEvery {
		if err := database.TouchSession(ctx, session.IDHash, now); err != nil {
			return nil, err
		}
	}
	return &visit{user: user, session: session}, nil
}

// fail shows an error page. Messages of statusError are shown as written;
// anything else is logged and replaced by a generic message, so internal
// details and paths never reach the page.
func (server *Server) fail(writer http.ResponseWriter, request *http.Request, current *visit, err error) {
	status, message := http.StatusInternalServerError, "Something went wrong; the details are in the server log."
	var known statusError
	switch {
	case errors.As(err, &known):
		status, message = known.status, known.message
	case errors.Is(err, store.ErrNotFound):
		status, message = http.StatusNotFound, "There is nothing here. It may have been deleted."
	default:
		server.options.Logger.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	}
	page := server.page(request, current, "Error")
	page.Error = message
	if renderErr := server.render(writer, status, "error", page); renderErr != nil {
		http.Error(writer, message, status)
	}
}

func sessionTokens(request *http.Request) []string {
	var tokens []string
	for _, cookie := range request.Cookies() {
		if cookie.Name == secureSessionCookie || cookie.Name == plainSessionCookie {
			tokens = append(tokens, cookie.Value)
		}
	}
	return tokens
}
