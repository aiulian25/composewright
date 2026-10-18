package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/rules"
	"github.com/aiulian25/composewright/internal/store"
)

// The Advanced page holds the keys `composewright config set` changes that
// no other page shows: the sandbox, Docker, code hosts and sign-in length,
// and the GitHub token, sealed like a model key. It also lists the rules
// catalogue's override files, which are edited on the host.

const (
	sectionAdvanced        = "advanced"
	advancedCommand        = "composewright config set docker.validate.timeout 5m"
	githubKeyName          = "github"
	githubTokenRef         = config.SecretRefKeyring + githubKeyName
	githubReferenceSetting = "scm.github_token_ref"
	tokenField             = "token"
	clearField             = "clear"
	doneGitHubStored       = "github-stored"
	doneGitHubRemoved      = "github-removed"
)

var errNoToken = errors.New("paste the GitHub token first")

// advancedView is what the Advanced page shows besides the configuration.
type advancedView struct {
	Tiers      []string
	AllowHosts string
	// TokenStored says a GitHub token is sealed in the database, and
	// TokenUnused that the configuration does not read it; TokenElsewhere
	// says it reads one from TokenRef instead.
	TokenStored    bool
	TokenUnused    bool
	TokenElsewhere bool
	TokenRef       string
	// Catalogue is the rules catalogue new runs use, with the override
	// files read from CatalogueDir.
	Catalogue    *rules.Catalogue
	CatalogueDir string
}

func (server *Server) advancedView(ctx context.Context, configuration config.Config) (advancedView, error) {
	stored, err := server.workspace().Store.HasSecret(ctx, githubKeyName)
	reference := configuration.SCM.GitHubTokenRef
	return advancedView{Tiers: config.SandboxTiers(), AllowHosts: strings.Join(configuration.SCM.AllowHosts, "\n"), TokenStored: stored,
		TokenUnused: stored && reference != githubTokenRef, TokenElsewhere: reference != "" && reference != githubTokenRef, TokenRef: reference,
		Catalogue: server.catalogue.Load(), CatalogueDir: server.paths().CatalogueDir()}, err
}

// advancedForm reads the Advanced page into the configuration.
func advancedForm(request *http.Request, configuration config.Config) (config.Config, error) {
	reader := &numbers{request: request}
	saved := configuration.Docker.Validate
	configuration.Docker.Host = field(request, "docker_host")
	configuration.Docker.CLIPath = field(request, "cli_path")
	configuration.Docker.SandboxTier = field(request, "sandbox_tier")
	configuration.Docker.Validate = config.Validate{MemoryLimit: field(request, "validate_memory"),
		Timeout:       reader.duration("timeout", "Run timeout", saved.Timeout),
		CPUs:          reader.decimal("validate_cpus", "CPUs per service", saved.CPUs),
		PIDsLimit:     reader.whole("validate_pids", "Processes per service", saved.PIDsLimit),
		MaxIterations: reader.whole("max_iterations", "Repair rounds", saved.MaxIterations)}
	configuration.SCM.AllowHosts = lines(request.PostFormValue("allow_hosts"))
	configuration.UI.SessionTTL = reader.duration("session_ttl", "Session length", configuration.UI.SessionTTL)
	return configuration, errors.Join(reader.problems...)
}

// advancedValues are the keys the Advanced page writes.
func advancedValues(configuration config.Config) map[string]string {
	docker, limits := configuration.Docker, configuration.Docker.Validate
	return map[string]string{
		"docker.host":                    quote(docker.Host),
		"docker.cli_path":                quote(docker.CLIPath),
		"docker.sandbox_tier":            quote(docker.SandboxTier),
		"docker.validate.timeout":        quote(limits.Timeout.String()),
		"docker.validate.memory_limit":   quote(limits.MemoryLimit),
		"docker.validate.cpus":           strconv.FormatFloat(limits.CPUs, 'f', -1, 64),
		"docker.validate.pids_limit":     strconv.Itoa(limits.PIDsLimit),
		"docker.validate.max_iterations": strconv.Itoa(limits.MaxIterations),
		"scm.allow_hosts":                jsonText(configuration.SCM.AllowHosts),
		"ui.session_ttl":                 quote(configuration.UI.SessionTTL.String()),
	}
}

// saveGitHubToken stores a GitHub token and points scm.github_token_ref at
// it, or removes the stored one.
func (server *Server) saveGitHubToken(writer http.ResponseWriter, request *http.Request, current *visit) error {
	done, err := server.changeGitHubToken(request, current)
	if err != nil {
		state := settingsState{shown: sectionAdvanced, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: server.config()}
		return server.renderSettings(writer, request, current, state)
	}
	return redirect(writer, request, doneURL(sectionAdvanced, done))
}

func (server *Server) changeGitHubToken(request *http.Request, current *visit) (string, error) {
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if request.PostFormValue(clearField) != "" {
		return doneGitHubRemoved, server.clearGitHubToken(request, current)
	}
	token := field(request, tokenField)
	if token == "" {
		return "", errNoToken
	}
	// The token is stored before the configuration names it, so no run
	// reads a reference to a token that is not there yet.
	if err := server.storeKey(request, current, githubKeyName, token); err != nil {
		return "", err
	}
	return doneGitHubStored, server.update(request, current, map[string]string{githubReferenceSetting: quote(githubTokenRef)})
}

func (server *Server) clearGitHubToken(request *http.Request, current *visit) error {
	if server.config().SCM.GitHubTokenRef == githubTokenRef {
		if err := server.update(request, current, map[string]string{githubReferenceSetting: quote("")}); err != nil {
			return err
		}
	}
	database := server.workspace().Store
	deleted, err := database.DeleteSecret(request.Context(), githubKeyName)
	if err != nil || !deleted {
		return err
	}
	return database.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionDeleteSecret, "", map[string]any{"name": githubKeyName})
}
