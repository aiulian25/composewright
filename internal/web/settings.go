package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/doctor"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	sectionProfile    = "profile"
	sectionEngine     = "engine"
	sectionAppearance = "appearance"
	sectionAccount    = "account"
	maxNetworkRows    = 20
	// newRowIndex stands for the row number in the network row app.js
	// copies when someone adds a network.
	newRowIndex = "__index__"
	// adoptSeparator joins name, kind and provider in the value of a found
	// network's Add button.
	adoptSeparator = "|"
	savedNotice    = "Saved."
	savedQuery     = "saved"
	settingsPath   = "/settings"
	sectionQuery   = "section"
	profileFile    = "profile.yaml"
	configFile     = "config.yaml"
	// passwordAttemptPrefix keys the throttle by account for a password
	// change, where sign-in keys it by client address.
	passwordAttemptPrefix = "password:"
)

var (
	// settingsSections are the Settings pages in order; each page is one
	// form, saved in one go.
	settingsSections = []string{sectionProfile, sectionEngine, sectionModels, sectionAppearance, sectionAccount}
	settingsTitles   = map[string]string{sectionProfile: "Environment profile", sectionEngine: "Engine",
		sectionModels: "Model providers", sectionAppearance: "Appearance", sectionAccount: "Account"}
)

type settingsView struct {
	Page        string
	Title       string
	Engines     []option
	File        settingsFile
	Config      config.Config
	Networks    []networkRow
	NewNetwork  networkRow
	MaxNetworks int
	// Found are proxy networks running on this Docker host that the profile
	// does not list; HostChecked says the host answered, HostProblem why not.
	Found       []doctor.FoundNetwork
	Proxies     []string
	HostChecked bool
	HostProblem string
	Models      modelsView
	Restarts    []string
	Themes      []string
	Private     string
	Registries  string
	ConfigFile  string
	Minimum     int
}

// networkRow is one row of the networks table. It carries the kinds and
// providers because it is also the row app.js copies to add a network.
type networkRow struct {
	Index     string
	Name      string
	Kind      string
	Provider  string
	Default   bool
	Domain    string
	Kinds     []string
	Providers []string
	// Checked says the Docker host was asked; OnHost that the network is
	// there. One that is not is a placeholder for the host you deploy to.
	Checked bool
	OnHost  bool
	// Detected is the proxy or tunnel running on the network; Suggest its
	// kind and provider to adopt when the row says otherwise.
	Detected     string
	Suggest      string
	SuggestLabel string
}

// settingsFile is the YAML a Settings page writes, shown beside it.
type settingsFile struct {
	Name    string
	Lines   []badgedLine
	Command string
}

// settingsState is what a Settings page shows: the saved configuration, or
// what the form sent when saving it failed, so nothing typed is lost.
type settingsState struct {
	shown         string
	notice        string
	problem       string
	status        int
	configuration config.Config
}

func (server *Server) settingsPage(writer http.ResponseWriter, request *http.Request, current *visit) error {
	state := settingsState{shown: request.URL.Query().Get(sectionQuery), status: http.StatusOK, configuration: server.config()}
	if !slices.Contains(settingsSections, state.shown) {
		state.shown = sectionProfile
	}
	if request.URL.Query().Has(savedQuery) {
		state.notice = savedNotice
	}
	return server.renderSettings(writer, request, current, state)
}

// preview renders what a page writes: the profile on its own, as
// `composewright profile export` writes it, or the whole configuration.
func preview(configuration config.Config, shown string) (settingsFile, error) {
	if shown == sectionProfile {
		rendered, err := config.RenderProfile(configuration.Profile)
		return settingsFile{Name: profileFile, Lines: plainLines(string(rendered), languageYAML), Command: "composewright profile export > " + profileFile}, err
	}
	rendered, err := config.Render(configuration)
	return settingsFile{Name: configFile, Lines: plainLines(string(rendered), languageYAML), Command: "composewright config show"}, err
}

func (server *Server) renderSettings(writer http.ResponseWriter, request *http.Request, current *visit, state settingsState) error {
	file, err := preview(server.config(), state.shown)
	if err != nil {
		return err
	}
	configuration := state.configuration
	models, err := server.modelsView(request.Context(), configuration.LLM, request.URL.Query().Get(editQuery))
	if err != nil {
		return err
	}
	view := settingsView{Page: state.shown, Title: settingsTitles[state.shown], Engines: server.engineOptions(configuration.LLM.Engine), File: file,
		Config: configuration, MaxNetworks: maxNetworkRows, Models: models, Restarts: config.RestartModes(), Themes: config.Themes(),
		ConfigFile: server.paths().ConfigFile, Private: strings.Join(configuration.Network.AllowPrivateTargets, "\n"),
		Registries: strings.Join(configuration.Registries.Allow, "\n"), Minimum: auth.MinimumPasswordLength}
	kinds, providers := config.NetworkKinds(), config.NetworkProviders()
	for index, network := range configuration.Profile.Networks {
		view.Networks = append(view.Networks, networkRow{Index: strconv.Itoa(index), Name: network.Name, Kind: network.Kind,
			Provider: network.Provider, Default: network.Default, Domain: network.Domain, Kinds: kinds, Providers: providers})
	}
	blank := networkRow{Index: strconv.Itoa(len(view.Networks)), Kind: kinds[0], Kinds: kinds, Providers: providers}
	view.Networks = append(view.Networks, blank)
	view.NewNetwork, view.NewNetwork.Index = blank, newRowIndex
	if state.shown == sectionProfile {
		server.lookAtHost(request.Context(), &view)
	}
	page := server.page(request, current, "Settings")
	page.Nav, page.Context, page.CLI, page.Notice, page.Error, page.Data = navSettings, true, file.Command, state.notice, state.problem, view
	page.Crumbs = []Crumb{{Text: "Settings", Href: settingsPath}, {Text: view.Title}}
	return server.render(writer, state.status, "settings", page)
}

// lookAtHost asks the Docker host which listed networks it has and which
// running proxies or tunnels the profile could add.
func (server *Server) lookAtHost(ctx context.Context, view *settingsView) {
	docker, err := sandbox.FromConfig(server.config(), server.paths().ProjectsDir())
	if err != nil {
		view.HostProblem = "Docker is not available here, so the networks on this host cannot be listed."
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	host, err := doctor.ProxyNetworks(checkCtx, docker, view.Config.Profile)
	if err != nil {
		view.HostProblem = "Docker did not answer, so the networks on this host cannot be listed."
		return
	}
	view.Found, view.Proxies, view.HostChecked = host.Found, host.Proxies, true
	for index := range view.Networks {
		row := &view.Networks[index]
		row.Checked, row.OnHost = true, slices.Contains(host.Existing, row.Name)
		if owner, attached := host.Owners[row.Name]; attached {
			hostHint(row, owner)
		}
	}
}

// hostHint names the proxy or tunnel that runs on a listed network, and
// offers its kind and provider when the row says otherwise.
func hostHint(row *networkRow, owner doctor.FoundNetwork) {
	row.Detected = owner.Proxy
	if owner.Kind == row.Kind && owner.Provider == row.Provider {
		return
	}
	row.Suggest = strings.Join([]string{owner.Network, owner.Kind, owner.Provider}, adoptSeparator)
	row.SuggestLabel = owner.Kind
	if owner.Provider != "" {
		row.SuggestLabel += " · " + owner.Provider
	}
}

// saveSettings writes one Settings page to config.yaml. The whole file is
// validated before it is written, so a mistake changes nothing and the
// page comes back with what was typed. A save redirects, so reloading the
// page never repeats it.
func (server *Server) saveSettings(writer http.ResponseWriter, request *http.Request, current *visit) error {
	section := request.PathValue("section")
	if !slices.Contains(settingsSections, section) {
		return statusError{status: http.StatusNotFound, message: "There is no such settings section."}
	}
	submitted, err := server.saveSection(request, current, section)
	if err != nil {
		state := settingsState{shown: section, problem: capitalised(err.Error()) + ".", status: http.StatusBadRequest, configuration: submitted}
		return server.renderSettings(writer, request, current, state)
	}
	return redirect(writer, request, settingsURL(section)+"&"+savedQuery)
}

// saveSection reads a page's form into the configuration and writes it. It
// returns what the form sent, to show again when saving fails.
func (server *Server) saveSection(request *http.Request, current *visit, section string) (config.Config, error) {
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	submitted := server.config()
	var err error
	switch section {
	case sectionProfile:
		if submitted.Profile, err = profileForm(request, submitted.Profile); err != nil {
			return submitted, err
		}
		return submitted, server.update(request, current, profileValues(submitted.Profile))
	case sectionEngine:
		submitted = engineForm(request, submitted)
		return submitted, server.saveEngine(request, current, submitted)
	case sectionModels:
		return server.saveModels(request, current, submitted)
	case sectionAppearance:
		submitted.UI.Theme = field(request, "theme")
		return submitted, server.update(request, current, map[string]string{"ui.theme": quote(submitted.UI.Theme)})
	}
	return submitted, server.changePassword(request, current)
}

// update writes values to config.yaml, reloads it and audits the change.
// Callers hold settingsMutex from the moment they read the configuration
// they change, so two saves cannot undo each other. A CLI config set run at
// the same moment is not covered and can still overwrite one.
func (server *Server) update(request *http.Request, current *visit, values map[string]string) error {
	if err := config.UpdateFile(server.paths().ConfigFile, values); err != nil {
		return err
	}
	if err := server.reload(); err != nil {
		return err
	}
	server.status.forget()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return server.workspace().Store.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionSettings, "", map[string]any{"keys": keys})
}

// profileForm reads the Environment profile page into the profile: the
// networks table, the storage fields and the defaults.
func profileForm(request *http.Request, profile config.Profile) (config.Profile, error) {
	profile.Networks = adopt(formNetworks(request), request.PostForm["adopt"])
	profile.Storage = config.Storage{Style: field(request, "style"), BasePath: field(request, "base_path"), Layout: field(request, "layout"),
		Owner: field(request, "owner"), SaveDir: field(request, "save_dir")}
	saved := profile.Defaults
	reader := &numbers{request: request}
	profile.Defaults = config.Defaults{TZ: field(request, "tz"), Restart: field(request, "restart"), DomainSuffix: field(request, "domain_suffix"),
		LogMaxSize: field(request, "log_max_size"), MemoryLimit: field(request, "memory_limit"),
		PUID: reader.whole("puid", "User ID (PUID)", saved.PUID), PGID: reader.whole("pgid", "Group ID (PGID)", saved.PGID),
		LogMaxFile: reader.whole("log_max_file", "Log files kept", saved.LogMaxFile),
		PIDsLimit:  reader.whole("pids_limit", "Processes per service", saved.PIDsLimit),
		CPUs:       reader.decimal("cpus", "CPUs per service", saved.CPUs)}
	return profile, errors.Join(reader.problems...)
}

// formNetworks reads the network rows; a row is left out when its name is
// empty or its remove box is ticked.
func formNetworks(request *http.Request) []config.ProfileNetwork {
	networks := []config.ProfileNetwork{}
	for index := range maxNetworkRows {
		row := strconv.Itoa(index)
		name := field(request, "name-"+row)
		if name == "" || request.PostFormValue("remove-"+row) != "" {
			continue
		}
		networks = append(networks, config.ProfileNetwork{Name: name, Kind: request.PostFormValue("kind-" + row),
			Provider: request.PostFormValue("provider-" + row), Default: request.PostFormValue("default-"+row) != "",
			Domain: domainOf(request.PostFormValue("domain-" + row))})
	}
	return networks
}

// domainOf takes a domain as people type it, so https://apps.example.com/
// counts as apps.example.com.
func domainOf(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if parsed, err := url.Parse(text); err == nil && parsed.Host != "" {
		return parsed.Hostname()
	}
	return strings.TrimRight(text, "/")
}

// adopt adds the networks found on the host that the person chose, or sets
// a listed one's kind and provider to what runs on it. The first one added
// becomes the default when no network is, so new projects use the proxy
// that is already running.
func adopt(networks []config.ProfileNetwork, chosen []string) []config.ProfileNetwork {
	for _, value := range chosen {
		name, rest, _ := strings.Cut(value, adoptSeparator)
		kind, provider, _ := strings.Cut(rest, adoptSeparator)
		listed := slices.IndexFunc(networks, func(network config.ProfileNetwork) bool { return network.Name == name })
		if listed >= 0 {
			networks[listed].Kind, networks[listed].Provider = kind, provider
			continue
		}
		noDefault := !slices.ContainsFunc(networks, func(network config.ProfileNetwork) bool { return network.Default })
		networks = append(networks, config.ProfileNetwork{Name: name, Kind: kind, Provider: provider, Default: noDefault})
	}
	return networks
}

// profileValues are the keys the Environment profile page writes, each set
// on its own so config.yaml keeps its comments.
func profileValues(profile config.Profile) map[string]string {
	type yamlNetwork struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Provider string `json:"provider"`
		Default  bool   `json:"default"`
		Domain   string `json:"domain,omitempty"`
	}
	networks := make([]yamlNetwork, 0, len(profile.Networks))
	for _, network := range profile.Networks {
		networks = append(networks, yamlNetwork(network))
	}
	storage, defaults := profile.Storage, profile.Defaults
	return map[string]string{
		"profile.networks":               jsonText(networks),
		"profile.storage.style":          quote(storage.Style),
		"profile.storage.base_path":      quote(storage.BasePath),
		"profile.storage.layout":         quote(storage.Layout),
		"profile.storage.owner":          quote(storage.Owner),
		"profile.storage.save_dir":       quote(storage.SaveDir),
		"profile.defaults.tz":            quote(defaults.TZ),
		"profile.defaults.puid":          strconv.Itoa(defaults.PUID),
		"profile.defaults.pgid":          strconv.Itoa(defaults.PGID),
		"profile.defaults.restart":       quote(defaults.Restart),
		"profile.defaults.domain_suffix": quote(defaults.DomainSuffix),
		"profile.defaults.log_max_size":  quote(defaults.LogMaxSize),
		"profile.defaults.log_max_file":  strconv.Itoa(defaults.LogMaxFile),
		"profile.defaults.memory_limit":  quote(defaults.MemoryLimit),
		"profile.defaults.cpus":          strconv.FormatFloat(defaults.CPUs, 'f', -1, 64),
		"profile.defaults.pids_limit":    strconv.Itoa(defaults.PIDsLimit),
	}
}

// engineForm reads the Engine page: who writes plans, and what the network
// may reach.
func engineForm(request *http.Request, configuration config.Config) config.Config {
	configuration.LLM.Engine = field(request, "engine")
	configuration.Network.AllowPrivateTargets = lines(request.PostFormValue("private"))
	configuration.Registries.Allow = lines(request.PostFormValue("registries"))
	return configuration
}

func (server *Server) saveEngine(request *http.Request, current *visit, configuration config.Config) error {
	return server.update(request, current, map[string]string{
		"llm.engine":                    quote(configuration.LLM.Engine),
		"network.allow_private_targets": jsonText(configuration.Network.AllowPrivateTargets),
		"registries.allow":              jsonText(configuration.Registries.Allow),
	})
}

func field(request *http.Request, name string) string {
	return strings.TrimSpace(request.PostFormValue(name))
}

// numbers reads number fields. One that does not parse keeps its saved
// value, so the page can show the rest of the form again, and is reported
// by its label.
type numbers struct {
	request  *http.Request
	problems []error
}

func (reader *numbers) whole(name, label string, saved int) int {
	number, err := strconv.Atoi(field(reader.request, name))
	if err != nil {
		reader.problems = append(reader.problems, fmt.Errorf("%s must be a whole number", label))
		return saved
	}
	return number
}

func (reader *numbers) decimal(name, label string, saved float64) float64 {
	number, err := strconv.ParseFloat(field(reader.request, name), 64)
	if err != nil {
		reader.problems = append(reader.problems, fmt.Errorf("%s must be a number", label))
		return saved
	}
	return number
}

// changePassword throttles wrong current passwords per account, so a
// browser left signed in cannot be used to guess the password.
func (server *Server) changePassword(request *http.Request, current *visit) error {
	account, now := passwordAttemptPrefix+current.user.ID, server.options.Now()
	if wait := server.throttle.Wait(account, now); wait > 0 {
		return fmt.Errorf("too many wrong passwords; try again in %s", wait.Round(time.Second))
	}
	release, free := server.hashSlot()
	if !free {
		return errors.New(strings.TrimSuffix(tooManyPasswordChecks, "."))
	}
	defer release()
	matches, err := auth.VerifyPassword(current.user.PasswordHash, request.PostFormValue("current"))
	if err != nil {
		return err
	}
	if !matches {
		server.throttle.Fail(account, now)
		return fmt.Errorf("the current password is wrong")
	}
	server.throttle.Succeed(account)
	if problem := passwordProblem(request.PostFormValue("password"), request.PostFormValue("confirm")); problem != "" {
		return fmt.Errorf("%s", strings.TrimSuffix(problem, "."))
	}
	hash, err := auth.HashPassword(request.PostFormValue("password"))
	if err != nil {
		return err
	}
	database := server.workspace().Store
	if err := database.SetPassword(request.Context(), current.user.ID, hash, current.session.IDHash); err != nil {
		return err
	}
	return database.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionPassword, "", map[string]any{"other_sessions_ended": true})
}

// quote writes a string as YAML text that stays a string, whatever it
// looks like.
func quote(value string) string {
	return jsonText(value)
}

func jsonText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func lines(text string) []string {
	result := []string{}
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func settingsURL(shown string) string {
	return settingsPath + "?" + sectionQuery + "=" + shown
}

// switchEngine is the top bar's engine menu: who writes new plans. It
// returns to the page it was used on.
func (server *Server) switchEngine(writer http.ResponseWriter, request *http.Request, current *visit) error {
	engine := request.PostFormValue("engine")
	if !slices.Contains(pipeline.Engines, engine) {
		return badRequest("Choose rules, assisted or model.")
	}
	server.settingsMutex.Lock()
	defer server.settingsMutex.Unlock()
	if err := server.update(request, current, map[string]string{"llm.engine": quote(engine)}); err != nil {
		return badRequest("%s.", capitalised(err.Error()))
	}
	target := localPath(request.PostFormValue("next"))
	if target == "" {
		target = "/"
	}
	return redirect(writer, request, target)
}
