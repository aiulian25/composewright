// Package web serves the browser UI: sign-in, the five steps from a
// repository to validated files, projects and settings. Handlers translate
// between HTTP and the pipeline and hold no deployment logic of their own.
package web

import (
	"context"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/workspace"
	"github.com/aiulian25/composewright/web"
)

const (
	// Over HTTPS the session cookie is Secure and carries the __Host- prefix,
	// which pins it to Path=/ without a Domain. Over plain HTTP, the usual
	// case on a home network, it is the same cookie without Secure, which
	// browsers would drop there.
	secureSessionCookie = "__Host-cw-session"
	plainSessionCookie  = "cw-session"
	forwardedProto      = "X-Forwarded-Proto"
	forwardedFor        = "X-Forwarded-For"
	httpsScheme         = "https"
	mdnsSuffix          = ".local"
	// SetupWindow is how long after start the first account can be created
	// by opening the page: enough to set it up, too short for a stranger to
	// find it later. A restart opens it again.
	SetupWindow     = 15 * time.Minute
	sessionIdle     = 2 * time.Hour
	touchEvery      = time.Minute
	formLimit       = 1 << 20
	uploadLimit     = 50 << 20
	uploadMemory    = 8 << 20
	shutdownGrace   = 2 * time.Minute
	staticMaxAge    = "public, max-age=86400"
	pageCache       = "no-store"
	csrfField       = "csrf"
	csrfHeader      = "X-CSRF-Token"
	contentSecurity = "default-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; style-src 'self'; script-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"
	permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=(), midi=(), display-capture=()"
	// maxConcurrentHashes caps Argon2id work: each hash holds 19 MiB while
	// it runs, so a request beyond the cap is answered at once instead.
	maxConcurrentHashes = 4
)

// Connector builds a run's dependencies; pipeline.Connect in production,
// fakes in tests.
type Connector func(ctx context.Context, setup pipeline.Setup) (pipeline.Connection, error)

type Options struct {
	Loaded       *config.Loaded
	Flags        config.Flags
	Workspace    *workspace.Workspace
	Version      string
	Listen       string
	AllowedHosts []string
	// TrustedProxies are reverse proxies whose X-Forwarded-For names the
	// visitor; nothing is trusted by default.
	TrustedProxies []netip.Prefix
	SetupToken     string
	Connect        Connector
	Logger         *slog.Logger
	Now            func() time.Time
}

type Server struct {
	options  Options
	assets   string
	pages    map[string]*template.Template
	mux      *http.ServeMux
	jobs     *jobs
	throttle *auth.Throttle
	hashing  chan struct{}
	hosts    map[string]bool
	dummy    string
	status   *statusCache

	started time.Time

	mutex      sync.RWMutex
	loaded     *config.Loaded
	setupToken string
	// stopBackups ends the backup scheduler, which closes backupsDone.
	stopBackups context.CancelFunc
	backupsDone chan struct{}
	// settingsMutex makes Settings saves take turns, from reading the
	// configuration they change to writing it back.
	settingsMutex sync.Mutex
}

func New(options Options) (*Server, error) {
	pages, err := web.Templates(functions())
	if err != nil {
		return nil, err
	}
	dummy, err := auth.HashPassword("timing equaliser for unknown users")
	if err != nil {
		return nil, err
	}
	assets, err := web.AssetVersion()
	if err != nil {
		return nil, err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	server := &Server{options: options, assets: assets, pages: pages, mux: http.NewServeMux(), jobs: newJobs(options.Now, options.Logger), throttle: auth.NewThrottle(),
		hosts: allowedHosts(options.Listen, options.AllowedHosts), dummy: dummy, loaded: options.Loaded, setupToken: options.SetupToken,
		started: options.Now(), hashing: make(chan struct{}, maxConcurrentHashes)}
	server.status = newStatusCache(server)
	server.routes()
	server.sweepUploads()
	scheduling, stop := context.WithCancel(context.Background())
	server.stopBackups, server.backupsDone = stop, make(chan struct{})
	go server.scheduleBackups(scheduling)
	return server, nil
}

// Close stops the backup scheduler and every running job; their sandboxes
// are torn down before it returns.
func (server *Server) Close() {
	server.stopBackups()
	<-server.backupsDone
	server.jobs.shutdown(shutdownGrace)
}

// hashSlot takes one of the few Argon2id slots; false when all are busy.
func (server *Server) hashSlot() (func(), bool) {
	select {
	case server.hashing <- struct{}{}:
		return func() { <-server.hashing }, true
	default:
		return nil, false
	}
}

func (server *Server) routes() {
	mux := server.mux
	mux.Handle("GET /static/", server.static())
	mux.HandleFunc("GET /setup", server.public(server.setupPage))
	mux.HandleFunc("POST /setup", server.public(server.setup))
	mux.HandleFunc("GET /login", server.public(server.loginPage))
	mux.HandleFunc("GET /login/forgot", server.public(server.forgotPage))
	mux.HandleFunc("POST /login", server.public(server.login))
	mux.HandleFunc("POST /logout", server.authed(server.logout))
	mux.HandleFunc("GET /{$}", server.authed(server.home))
	mux.HandleFunc("GET /projects", server.authed(server.projects))
	mux.HandleFunc("GET /new", server.authed(server.newPage))
	mux.HandleFunc("POST /new", server.authed(server.startGeneration))
	mux.HandleFunc("POST /start", server.authed(server.start))
	mux.HandleFunc("GET /jobs/{id}", server.authed(server.jobPage))
	mux.HandleFunc("GET /jobs/{id}/events", server.authed(server.jobEvents))
	mux.HandleFunc("POST /jobs/{id}/cancel", server.authed(server.cancelJob))
	mux.HandleFunc("GET /projects/{name}", server.authed(server.projectPage))
	mux.HandleFunc("GET /projects/{name}/analyse", server.authed(server.analysePage))
	mux.HandleFunc("GET /projects/{name}/review", server.authed(server.reviewPage))
	mux.HandleFunc("POST /projects/{name}/review", server.authed(server.review))
	mux.HandleFunc("POST /projects/{name}/compose", server.authed(server.editCompose))
	mux.HandleFunc("GET /projects/{name}/plan.json", server.authed(server.planJSON))
	mux.HandleFunc("POST /projects/{name}/reveal", server.authed(server.reveal))
	mux.HandleFunc("GET /projects/{name}/files", server.authed(server.filesPage))
	mux.HandleFunc("POST /projects/{name}/env", server.authed(server.copyEnv))
	mux.HandleFunc("POST /projects/{name}/download", server.authed(server.download))
	mux.HandleFunc("POST /projects/{name}/save", server.authed(server.save))
	mux.HandleFunc("POST /projects/{name}/files/check", server.authed(server.checkFiles))
	mux.HandleFunc("POST /projects/{name}/files/save", server.authed(server.saveFiles))
	mux.HandleFunc("GET /projects/{name}/validate", server.authed(server.validatePage))
	mux.HandleFunc("POST /projects/{name}/validate", server.authed(server.validate))
	mux.HandleFunc("GET /projects/{name}/delete", server.authed(server.deletePage))
	mux.HandleFunc("POST /projects/{name}/delete", server.authed(server.delete))
	mux.HandleFunc("GET /settings", server.authed(server.settingsPage))
	mux.HandleFunc("POST /settings/{section}", server.authed(server.saveSettings))
	mux.HandleFunc("POST /settings/backup/passphrase", server.authed(server.setBackupPassphrase))
	mux.HandleFunc("POST /settings/backup/now", server.authed(server.backUpNow))
	mux.HandleFunc("POST /settings/backup/download", server.authed(server.downloadBackup))
	mux.HandleFunc("POST /settings/backup/restore", server.authed(server.restoreBackup))
	mux.HandleFunc("POST /settings/backup/delete", server.authed(server.deleteBackup))
	mux.HandleFunc("POST /engine", server.authed(server.switchEngine))
}

func (server *Server) config() config.Config {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	return server.loaded.Config
}

func (server *Server) paths() config.Paths {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	return server.loaded.Paths
}

// reload reads config.yaml again after Settings changed it.
func (server *Server) reload() error {
	loaded, err := config.Load(server.options.Flags)
	if err != nil {
		return err
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.loaded = loaded
	return nil
}

func (server *Server) workspace() *workspace.Workspace {
	return server.options.Workspace
}

// allowedHosts are the names the UI answers to. Anything else is refused,
// which stops DNS rebinding from turning a web page into a client of this
// loopback service.
func allowedHosts(listen string, extra []string) map[string]bool {
	hosts := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	host, _, err := net.SplitHostPort(listen)
	if err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		hosts[strings.ToLower(host)] = true
	}
	for _, name := range extra {
		hosts[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if name, err := os.Hostname(); err == nil {
		hosts[strings.ToLower(name)] = true
		hosts[strings.ToLower(name)+mdnsSuffix] = true
	}
	return hosts
}
