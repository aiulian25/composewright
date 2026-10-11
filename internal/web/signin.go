package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	wrongCredential = "Wrong username or password."
	firstPage       = "/"
)

type signInView struct {
	Username string
	Next     string
	Token    string
	Minimum  int
	Closed   bool
}

func (server *Server) hasUsers(ctx context.Context) (bool, error) {
	return server.workspace().Store.HasUsers(ctx)
}

// setupPage creates the administrator on first use: whoever opens it in the
// first minutes after start, or later with the setup link from the log.
// Setup, sign-in and password help never show the app around them, so their
// pages are made without the visit.
func (server *Server) setupPage(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	exists, err := server.hasUsers(request.Context())
	if err != nil {
		return err
	}
	if exists {
		return redirect(writer, request, "/login")
	}
	token := request.URL.Query().Get("token")
	page := server.page(request, nil, "Set up")
	page.Data = signInView{Token: token, Minimum: auth.MinimumPasswordLength}
	if !server.setupOpen(token) {
		page.Error = setupClosed()
		page.Data = signInView{Closed: true, Minimum: auth.MinimumPasswordLength}
		return server.render(writer, http.StatusForbidden, "setup", page)
	}
	return server.render(writer, http.StatusOK, "setup", page)
}

// setupOpen lets the first account be made in the setup window, or later
// with the token from the log; an existing administrator closes it.
func (server *Server) setupOpen(token string) bool {
	early := server.options.Now().Sub(server.started) < SetupWindow
	return early || server.validSetupToken(token)
}

func setupClosed() string {
	return fmt.Sprintf("The first account can be created in the first %d minutes after composewright starts, so nobody else on the network can claim it later. Restart composewright and open this page again.",
		int(SetupWindow.Minutes()))
}

func (server *Server) setup(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	ctx := request.Context()
	exists, err := server.hasUsers(ctx)
	if err != nil {
		return err
	}
	token := request.PostFormValue("token")
	if exists {
		return statusError{status: http.StatusForbidden, message: "An administrator exists already. Sign in instead."}
	}
	if !server.setupOpen(token) {
		return statusError{status: http.StatusForbidden, message: setupClosed()}
	}
	username := strings.TrimSpace(request.PostFormValue("username"))
	password := request.PostFormValue("password")
	page := server.page(request, nil, "Set up")
	page.Data = signInView{Username: username, Token: token, Minimum: auth.MinimumPasswordLength}
	problem := passwordProblem(password, request.PostFormValue("confirm"))
	if username == "" {
		problem = "Choose a username."
	}
	if problem != "" {
		page.Error = problem
		return server.render(writer, http.StatusBadRequest, "setup", page)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	user, err := server.workspace().Store.CreateUser(ctx, username, hash)
	if err != nil {
		return err
	}
	server.closeSetup()
	return server.startSession(writer, request, user, firstPage)
}

func passwordProblem(password, confirmation string) string {
	if err := auth.CheckPassword(password); err != nil {
		return capitalised(err.Error()) + "."
	}
	if password != confirmation {
		return "The two passwords differ."
	}
	return ""
}

func (server *Server) validSetupToken(token string) bool {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	return auth.SameToken(token, server.setupToken)
}

func (server *Server) closeSetup() {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.setupToken = ""
}

func (server *Server) loginPage(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	exists, err := server.hasUsers(request.Context())
	if err != nil {
		return err
	}
	if !exists {
		return redirect(writer, request, "/setup")
	}
	page := server.page(request, nil, "Sign in")
	page.Data = signInView{Next: localPath(request.URL.Query().Get("next"))}
	return server.render(writer, http.StatusOK, "login", page)
}

// forgotPage says how to set a new password: composewright sends no email,
// so `composewright password` does it on the machine it runs on.
func (server *Server) forgotPage(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	return server.render(writer, http.StatusOK, "forgot", server.page(request, nil, "Forgot password"))
}

// login slows down guessing per client and compares against a dummy hash
// for unknown names, so the answer time does not reveal which names exist.
func (server *Server) login(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	ctx := request.Context()
	client := clientAddress(request)
	now := server.options.Now()
	username := strings.TrimSpace(request.PostFormValue("username"))
	page := server.page(request, nil, "Sign in")
	page.Data = signInView{Username: username, Next: localPath(request.PostFormValue("next"))}
	if wait := server.throttle.Wait(client, now); wait > 0 {
		page.Error = fmt.Sprintf("Too many failed attempts. Try again in %s.", wait.Round(time.Second))
		return server.render(writer, http.StatusTooManyRequests, "login", page)
	}
	user, err := server.workspace().Store.UserByName(ctx, username)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	hash := server.dummy
	if known {
		hash = user.PasswordHash
	}
	matches, err := auth.VerifyPassword(hash, request.PostFormValue("password"))
	if err != nil {
		return err
	}
	if !known || !matches {
		server.throttle.Fail(client, now)
		page.Error = wrongCredential
		return server.render(writer, http.StatusUnauthorized, "login", page)
	}
	server.throttle.Succeed(client)
	target := localPath(request.PostFormValue("next"))
	if target == "" {
		target = "/"
	}
	return server.startSession(writer, request, user, target)
}

// startSession issues a new session after every sign-in, so a session id
// planted before signing in is worthless.
func (server *Server) startSession(writer http.ResponseWriter, request *http.Request, user store.User, target string) error {
	ctx := request.Context()
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	csrf, err := auth.NewCSRF()
	if err != nil {
		return err
	}
	now := server.options.Now()
	lifetime := server.config().UI.SessionTTL
	database := server.workspace().Store
	if err := database.DeleteStaleSessions(ctx, now, sessionIdle); err != nil {
		return err
	}
	session := store.Session{IDHash: hash, UserID: user.ID, CSRF: csrf, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(lifetime)}
	if err := database.CreateSession(ctx, session); err != nil {
		return err
	}
	if err := database.Audit(ctx, user.Username, store.ActionLogin, "", map[string]any{"client": clientAddress(request)}); err != nil {
		return err
	}
	name, secure := sessionCookieFor(request)
	http.SetCookie(writer, &http.Cookie{Name: name, Value: token, Path: "/", MaxAge: int(lifetime.Seconds()), //nolint:gosec // G124: Secure follows the scheme on purpose, plain HTTP on a trusted network is supported
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	return redirect(writer, request, target)
}

// sessionCookieFor names the session cookie: Secure when the browser talks
// HTTPS, directly or through a proxy that says so. A forged header only
// makes the forger's own browser drop the cookie.
func sessionCookieFor(request *http.Request) (string, bool) {
	secure := request.TLS != nil || strings.EqualFold(request.Header.Get(forwardedProto), httpsScheme)
	if secure {
		return secureSessionCookie, true
	}
	return plainSessionCookie, false
}

func (server *Server) logout(writer http.ResponseWriter, request *http.Request, current *visit) error {
	if err := server.workspace().Store.DeleteSession(request.Context(), current.session.IDHash); err != nil {
		return err
	}
	for _, name := range []string{secureSessionCookie, plainSessionCookie} {
		http.SetCookie(writer, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: name == secureSessionCookie, //nolint:gosec // G124: clears both cookie names
			SameSite: http.SameSiteStrictMode})
	}
	return redirect(writer, request, "/login")
}

// localPath keeps redirects on this site: only absolute paths, never
// "//host", a backslash or a scheme. Browsers drop TAB and newline
// characters from a target, which would make "/\t/host" another site, so
// any control or space character is refused too.
func localPath(target string) string {
	hidden := strings.ContainsFunc(target, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	})
	if hidden || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") {
		return ""
	}
	return target
}

func clientAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

func capitalised(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}
