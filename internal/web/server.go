// Package web serves the browser UI: sign-in, the five steps from a
// repository to validated files, projects and settings. Handlers translate
// between HTTP and the pipeline and hold no deployment logic of their own.
package web

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/release"
	"github.com/aiulian25/composewright/internal/rules"
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
	sessionIdle         = 2 * time.Hour
	touchEvery          = time.Minute
	formLimit           = 1 << 20
	uploadLimit         = 50 << 20
	uploadMemory        = 8 << 20
	shutdownGrace       = 2 * time.Minute
	staticMaxAge        = "public, max-age=86400"
	pageCache           = "no-store"
	csrfField           = "csrf"
	csrfHeader          = "X-CSRF-Token"
	contentSecurity     = "default-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; style-src 'self'; script-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"
	permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=(), midi=(), display-capture=()"
	// maxConcurrentHashes caps Argon2id work: each hash holds 19 MiB while
	// it runs, so a request beyond the cap is answered at once instead.
	maxConcurrentHashes = 4
)

// Connector builds a run's dependencies; pipeline.Connect in production,
// fakes in tests.
type Connector func(ctx context.Context, setup pipeline.Setup) (pipeline.Connection, error)

// ReleaseLookup asks for composewright's latest release; release.Latest in
// production, a test server in tests.
type ReleaseLookup func(ctx context.Context, guard *netguard.Guard) (release.Release, error)

type Options struct {
	Loaded    *config.Loaded
	Flags     config.Flags
	Workspace *workspace.Workspace
	Version   string
	Listen    string
	// TrustedProxies are reverse proxies whose X-Forwarded-For names the
	// visitor; nothing is trusted by default.
	TrustedProxies []netip.Prefix
	SetupToken     string
	Connect        Connector
	Latest         ReleaseLookup
	// Habits reads how the Docker host's apps are set up, for a new
	// install to follow; nil leaves the profile as it is.
	Habits HabitsLookup
	// Sweep removes what interrupted sandbox runs left on the Docker host;
	// nil never sweeps.
	Sweep  SandboxSweep
	Logger *slog.Logger
	Now    func() time.Time
}

type Server struct {
	options  Options
	assets   string
	pages    map[string]*template.Template
	mux      *http.ServeMux
	jobs     *jobs
	throttle *auth.Throttle
	hashing  chan struct{}
	dummy    string
	status   *statusCache

	mutex      sync.RWMutex
	loaded     *config.Loaded
	setupToken string
	// stopBackups ends the backup scheduler, which closes backupsDone.
	stopBackups context.CancelFunc
	backupsDone chan struct{}
	// settingsMutex makes Settings saves take turns, from reading the
	// configuration they change to writing it back.
	settingsMutex sync.Mutex
	// proposals keeps each repair a person was shown until they apply it.
	proposals shownProposals
	// keyMutex keeps readers of the master key out while a rotation or
	// purge swaps it.
	keyMutex sync.RWMutex
	// catalogue is the rules catalogue new runs use: the workspace's, until
	// a restore writes override files back and it is read again.
	catalogue atomic.Pointer[rules.Catalogue]
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
	if options.Latest == nil {
		options.Latest = release.Latest
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	server := &Server{options: options, assets: assets, pages: pages, mux: http.NewServeMux(), jobs: newJobs(options.Now, options.Logger), throttle: auth.NewThrottle(),
		dummy: dummy, loaded: options.Loaded, setupToken: options.SetupToken,
		hashing: make(chan struct{}, maxConcurrentHashes)}
	journal, found, err := server.restoreJournal(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read restore journal: %w", err)
	}
	server.jobs.restoring = found && !journal.Completed
	if options.Loaded.Config.Network.JobTimeout > 0 {
		server.jobs.timeout = options.Loaded.Config.Network.JobTimeout
	}
	server.status = newStatusCache(server)
	server.catalogue.Store(options.Workspace.Catalogue)
	server.routes()
	server.sweepUploads()
	scheduling, stop := context.WithCancel(context.Background())
	server.stopBackups, server.backupsDone = stop, make(chan struct{})
	go server.scheduleBackups(scheduling)
	go server.learnHabits()
	go server.sweepSandboxes(scheduling)
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
	mux.HandleFunc("POST /projects/{name}/organization", server.authed(server.organizeProject))
	mux.HandleFunc("GET /projects/{name}/analyse", server.authed(server.analysePage))
	mux.HandleFunc("GET /projects/{name}/review", server.authed(server.reviewPage))
	mux.HandleFunc("POST /projects/{name}/review", server.authed(server.review))
	mux.HandleFunc("POST /projects/{name}/compose", server.authed(server.editCompose))
	mux.HandleFunc("GET /projects/{name}/plan.json", server.authed(server.planJSON))
	mux.HandleFunc("POST /projects/{name}/reveal", server.authed(server.reveal))
	mux.HandleFunc("GET /projects/{name}/files", server.authed(server.filesPage))
	mux.HandleFunc("POST /projects/{name}/env", server.authed(server.copyEnv))
	mux.HandleFunc("POST /learned/dismiss", server.authed(server.dismissLearned))
	mux.HandleFunc("POST /projects/{name}/shell", server.authed(server.copyShell))
	mux.HandleFunc("POST /projects/{name}/download", server.authed(server.download))
	mux.HandleFunc("POST /projects/{name}/save", server.authed(server.save))
	mux.HandleFunc("POST /projects/{name}/files/check", server.authed(server.checkFiles))
	mux.HandleFunc("POST /projects/{name}/files/save", server.authed(server.saveFiles))
	mux.HandleFunc("GET /projects/{name}/validate", server.authed(server.validatePage))
	mux.HandleFunc("POST /projects/{name}/validate", server.authed(server.validate))
	mux.HandleFunc("POST /projects/{name}/benign", server.authed(server.markBenign))
	mux.HandleFunc("POST /projects/{name}/repair/propose", server.authed(server.proposeRepair))
	mux.HandleFunc("POST /projects/{name}/repair/apply", server.authed(server.applyRepair))
	mux.HandleFunc("POST /projects/{name}/images", server.authed(server.checkImages))
	mux.HandleFunc("POST /projects/{name}/images/use", server.authed(server.useTag))
	mux.HandleFunc("GET /projects/{name}/delete", server.authed(server.deletePage))
	mux.HandleFunc("POST /projects/{name}/delete", server.authed(server.delete))
	mux.HandleFunc("GET /settings", server.authed(server.settingsPage))
	mux.HandleFunc("POST /settings/{section}", server.authed(server.saveSettings))
	mux.HandleFunc("POST /settings/models/test", server.authed(server.testConnection))
	mux.HandleFunc("POST /settings/backup/passphrase", server.authed(server.setBackupPassphrase))
	mux.HandleFunc("POST /settings/backup/now", server.authed(server.backUpNow))
	mux.HandleFunc("POST /settings/backup/download", server.authed(server.downloadBackup))
	mux.HandleFunc("POST /settings/backup/restore", server.authed(server.restoreBackup))
	mux.HandleFunc("POST /settings/backup/resume", server.authed(server.resumeBackup))
	mux.HandleFunc("POST /settings/backup/cancel", server.authed(server.cancelBackupRestore))
	mux.HandleFunc("POST /settings/backup/recovery", server.authed(server.downloadRestoreRecovery))
	mux.HandleFunc("POST /settings/backup/delete", server.authed(server.deleteBackup))
	mux.HandleFunc("POST /settings/audit/verify", server.authed(server.verifyAudit))
	mux.HandleFunc("POST /settings/audit/export", server.authed(server.exportAudit))
	mux.HandleFunc("POST /settings/maintenance/rotate", server.authed(server.rotateKey))
	mux.HandleFunc("POST /settings/maintenance/purge", server.authed(server.purgeKeys))
	mux.HandleFunc("POST /settings/maintenance/update-check", server.authed(server.checkForUpdate))
	mux.HandleFunc("POST /settings/maintenance/doctor", server.authed(server.runDoctor))
	mux.HandleFunc("POST /settings/advanced/token", server.authed(server.saveGitHubToken))
	mux.HandleFunc("GET /settings/profile/export", server.authed(server.exportProfile))
	mux.HandleFunc("POST /settings/profile/import", server.authed(server.importProfile))
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

// loadedConfig is a copy of the configuration with where it came from.
func (server *Server) loadedConfig() *config.Loaded {
	server.mutex.RLock()
	defer server.mutex.RUnlock()
	copied := *server.loaded
	return &copied
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
