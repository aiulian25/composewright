package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

// Model connections are where composewright asks a model. Several can be
// saved, for other services or other accounts, each with its own encrypted
// key; the one in use is the llm block every run reads.

const (
	sectionModels    = "models"
	keyNamePrefix    = "llm-"
	editQuery        = "edit"
	presetCustom     = "custom"
	useField         = "use"
	removeField      = "remove"
	actionField      = "action"
	actionSaveAndUse = "save-use"
	nameSuffix       = "-%d"
	firstFreeSuffix  = 2
)

// service is how the Model providers page presents a preset: its name, what
// it costs and where an account and a key come from. The steps themselves
// are in the template.
type service struct {
	config.Preset
	Title    string
	Cost     string
	Free     bool
	NeedsKey bool
	Console  string
	KeyPage  string
	Models   []string
	// Example is an address to show, and FreeName a name not taken yet.
	Example  string
	FreeName string
}

var serviceDetails = map[string]service{
	"anthropic": {Title: "Claude by Anthropic", Cost: "Paid per use, from $5 of prepaid credits", NeedsKey: true,
		Console: "https://console.anthropic.com", KeyPage: "https://console.anthropic.com/settings/keys",
		Models: []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}},
	"openai": {Title: "OpenAI", Cost: "Paid per use", NeedsKey: true,
		Console: "https://platform.openai.com", KeyPage: "https://platform.openai.com/api-keys", Models: []string{"gpt-5-mini", "gpt-5"}},
	"ollama": {Title: "Ollama on your own machine", Cost: "Free, and nothing leaves your network", Free: true,
		Console: "https://ollama.com/download", Models: []string{"qwen2.5-coder:7b", "qwen2.5-coder:14b"}, Example: "http://192.168.1.20:11434"},
	"gemini": {Title: "Google Gemini", Cost: "Free tier with limits, paid beyond", Free: true, NeedsKey: true,
		Console: "https://aistudio.google.com", KeyPage: "https://aistudio.google.com/app/apikey", Models: []string{"gemini-2.5-flash", "gemini-2.5-pro"}},
	"openrouter": {Title: "OpenRouter", Cost: "Some models free, one key for many services", Free: true, NeedsKey: true,
		Console: "https://openrouter.ai/models", KeyPage: "https://openrouter.ai/settings/keys"},
	presetCustom: {Title: "Another OpenAI-compatible service", Cost: "Depends on the service"},
}

// services are the presets in the order the page offers them.
func services() []service {
	all := make([]service, 0, len(config.Presets))
	for _, preset := range config.Presets {
		details := serviceDetails[preset.ID]
		details.Preset = preset
		all = append(all, details)
	}
	return all
}

// serviceOf finds the service a connection uses: by provider, and by
// address among OpenAI-compatible ones.
func serviceOf(connection config.Connection) service {
	host := hostOf(connection.BaseURL)
	all := services()
	index := slices.IndexFunc(all, func(candidate service) bool {
		sameProvider := candidate.Provider == connection.Provider
		return sameProvider && (candidate.Provider != config.ProviderOpenAICompatible || hostOf(candidate.BaseURL) == host)
	})
	if index < 0 {
		return all[len(all)-1]
	}
	return all[index]
}

func hostOf(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// connectionRow is a connection on the page: its service and its key.
type connectionRow struct {
	config.Connection
	Service    service
	InUse      bool
	KeyStored  bool
	KeyFromEnv bool
}

// connectionForm is what the add or edit form shows.
type connectionForm struct {
	Preset         string
	Name           string
	BaseURL        string
	Model          string
	MaxInputTokens int
	KeyStored      bool
	KeyFromEnv     bool
	Editing        bool
}

type modelsView struct {
	Current     connectionRow
	Connections []connectionRow
	Services    []service
	Form        connectionForm
	EngineLabel string
}

func (server *Server) modelsView(ctx context.Context, llm config.LLM, editing string) (modelsView, error) {
	view := modelsView{Services: services(), EngineLabel: engineLabels[llm.Engine]}
	for index := range view.Services {
		view.Services[index].FreeName = freeName(llm.Connections, view.Services[index].ID)
	}
	for _, connection := range llm.Connections {
		row, err := server.connectionRow(ctx, llm, connection)
		if err != nil {
			return view, err
		}
		view.Connections = append(view.Connections, row)
	}
	current, err := server.connectionRow(ctx, llm, llm.Current(savedName(llm)))
	if err != nil {
		return view, err
	}
	view.Current = current
	index := slices.IndexFunc(view.Connections, func(row connectionRow) bool { return row.Name == editing })
	if index >= 0 {
		row := view.Connections[index]
		view.Form = connectionForm{Preset: row.Service.ID, Name: row.Name, BaseURL: row.BaseURL, Model: row.Model,
			MaxInputTokens: row.MaxInputTokens, KeyStored: row.KeyStored, KeyFromEnv: row.KeyFromEnv, Editing: true}
	}
	return view, nil
}

// savedName is the name of the saved connection in use, if it is saved.
func savedName(llm config.LLM) string {
	index := slices.IndexFunc(llm.Connections, llm.InUse)
	if index < 0 {
		return ""
	}
	return llm.Connections[index].Name
}

func (server *Server) connectionRow(ctx context.Context, llm config.LLM, connection config.Connection) (connectionRow, error) {
	row := connectionRow{Connection: connection, Service: serviceOf(connection), InUse: llm.InUse(connection),
		KeyFromEnv: strings.HasPrefix(connection.APIKeyRef, config.SecretRefEnv)}
	name, inKeyring := strings.CutPrefix(connection.APIKeyRef, config.SecretRefKeyring)
	if !inKeyring {
		return row, nil
	}
	stored, err := server.workspace().Store.HasSecret(ctx, name)
	row.KeyStored = stored
	return row, err
}

// freeName is a connection name not taken yet: base, then base-2 and on.
func freeName(saved []config.Connection, base string) string {
	name := base
	for number := firstFreeSuffix; slices.ContainsFunc(saved, func(connection config.Connection) bool { return connection.Name == name }); number++ {
		name = base + fmt.Sprintf(nameSuffix, number)
	}
	return name
}

// saveModels saves, uses or removes a connection.
func (server *Server) saveModels(request *http.Request, current *visit, configuration config.Config) (config.Config, error) {
	if name := field(request, useField); name != "" {
		return server.useConnection(request, current, configuration, name)
	}
	if name := field(request, removeField); name != "" {
		return server.removeConnection(request, current, configuration, name)
	}
	return server.saveConnection(request, current, configuration)
}

func (server *Server) saveConnection(request *http.Request, current *visit, configuration config.Config) (config.Config, error) {
	llm := configuration.LLM
	chosen := serviceByID(field(request, "preset"))
	reader := &numbers{request: request}
	connection := config.Connection{Name: field(request, "name"), Provider: chosen.Provider, BaseURL: strings.TrimRight(field(request, "base_url"), "/"),
		Model: field(request, "model"), MaxInputTokens: reader.whole("max_input_tokens", "The input budget", chosen.MaxInputTokens)}
	previous := slices.IndexFunc(llm.Connections, func(saved config.Connection) bool { return saved.Name == connection.Name })
	editing := request.PostFormValue(editQuery) != ""
	if previous >= 0 && !editing {
		return configuration, fmt.Errorf("a connection named %s is saved already: pick another name, or edit that one", connection.Name)
	}
	if previous >= 0 && llm.Connections[previous].Provider != connection.Provider {
		return configuration, fmt.Errorf("%s is a %s connection and stays one: add a new connection for %s",
			connection.Name, serviceOf(llm.Connections[previous]).Title, chosen.Title)
	}
	if previous >= 0 {
		connection.APIKeyRef = llm.Connections[previous].APIKeyRef
	}
	key := request.PostFormValue("api_key")
	if key != "" {
		connection.APIKeyRef = config.SecretRefKeyring + keyNamePrefix + connection.Name
	}
	llm.Connections = withConnection(llm.Connections, connection)
	if request.PostFormValue(actionField) == actionSaveAndUse {
		llm = keepCurrent(llm).Use(connection)
	}
	configuration.LLM = llm
	if err := errors.Join(reader.problems...); err != nil {
		return configuration, err
	}
	if chosen.NeedsKey && connection.APIKeyRef == "" {
		return configuration, fmt.Errorf("%s needs an API key: the steps above show where to get one", chosen.Title)
	}
	// The key is stored before the config names it, so a failed store leaves
	// no connection without its key; checking the config first keeps a
	// refused connection from storing one.
	if err := configuration.Validate(); err != nil {
		return configuration, err
	}
	if err := server.storeKey(request, current, keyNamePrefix+connection.Name, key); err != nil {
		return configuration, err
	}
	return configuration, server.writeModels(request, current, llm)
}

func (server *Server) useConnection(request *http.Request, current *visit, configuration config.Config, name string) (config.Config, error) {
	llm := configuration.LLM
	index := slices.IndexFunc(llm.Connections, func(saved config.Connection) bool { return saved.Name == name })
	if index < 0 {
		return configuration, fmt.Errorf("there is no saved connection %q", name)
	}
	configuration.LLM = keepCurrent(llm).Use(llm.Connections[index])
	return configuration, server.writeModels(request, current, configuration.LLM)
}

// removeConnection forgets a saved connection and its stored key, unless
// the connection in use still needs that key. Deleting the key is audited,
// like storing it.
func (server *Server) removeConnection(request *http.Request, current *visit, configuration config.Config, name string) (config.Config, error) {
	llm := configuration.LLM
	index := slices.IndexFunc(llm.Connections, func(saved config.Connection) bool { return saved.Name == name })
	if index < 0 {
		return configuration, fmt.Errorf("there is no saved connection %q", name)
	}
	removed := llm.Connections[index]
	llm.Connections = slices.Delete(slices.Clone(llm.Connections), index, index+1)
	configuration.LLM = llm
	if err := server.writeModels(request, current, llm); err != nil {
		return configuration, err
	}
	keyName, inKeyring := strings.CutPrefix(removed.APIKeyRef, config.SecretRefKeyring)
	if !inKeyring || removed.APIKeyRef == llm.APIKeyRef {
		return configuration, nil
	}
	database := server.workspace().Store
	deleted, err := database.DeleteSecret(request.Context(), keyName)
	if err != nil || !deleted {
		return configuration, err
	}
	return configuration, database.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionDeleteSecret, "", map[string]any{"name": keyName})
}

func serviceByID(id string) service {
	all := services()
	index := slices.IndexFunc(all, func(candidate service) bool { return candidate.ID == id })
	if index < 0 {
		return all[len(all)-1]
	}
	return all[index]
}

// withConnection replaces the saved connection of the same name, or adds it.
func withConnection(saved []config.Connection, connection config.Connection) []config.Connection {
	updated := slices.Clone(saved)
	index := slices.IndexFunc(updated, func(other config.Connection) bool { return other.Name == connection.Name })
	if index < 0 {
		return append(updated, connection)
	}
	updated[index] = connection
	return updated
}

// keepCurrent saves the connection in use before another replaces it, if
// nobody saved it yet, so switching never loses a working setup. The
// shipped default is nobody's setup and is not kept.
func keepCurrent(llm config.LLM) config.LLM {
	untouched := llm.Current("") == config.Default().LLM.Current("")
	if untouched || llm.Provider == config.ProviderNone || slices.ContainsFunc(llm.Connections, llm.InUse) {
		return llm
	}
	current := llm.Current(freeName(llm.Connections, serviceOf(llm.Current("")).ID))
	llm.Connections = append(slices.Clone(llm.Connections), current)
	return llm
}

// writeModels writes the saved connections and the one in use.
func (server *Server) writeModels(request *http.Request, current *visit, llm config.LLM) error {
	return server.update(request, current, modelsValues(llm))
}

// modelsValues are the keys the Model providers page writes.
func modelsValues(llm config.LLM) map[string]string {
	type yamlConnection struct {
		Name           string `json:"name"`
		Provider       string `json:"provider"`
		BaseURL        string `json:"base_url"`
		Model          string `json:"model"`
		APIKeyRef      string `json:"api_key_ref"`
		MaxInputTokens int    `json:"max_input_tokens"`
	}
	saved := make([]yamlConnection, 0, len(llm.Connections))
	for _, connection := range llm.Connections {
		saved = append(saved, yamlConnection(connection))
	}
	return map[string]string{
		"llm.connections":      jsonText(saved),
		"llm.provider":         quote(llm.Provider),
		"llm.base_url":         quote(llm.BaseURL),
		"llm.model":            quote(llm.Model),
		"llm.api_key_ref":      quote(llm.APIKeyRef),
		"llm.max_input_tokens": fmt.Sprint(llm.MaxInputTokens),
	}
}

// storeKey seals an API key in the database and audits that it was set,
// never the key itself. An empty key leaves any stored one as it is.
func (server *Server) storeKey(request *http.Request, current *visit, name, key string) error {
	if key == "" {
		return nil
	}
	database := server.workspace().Store
	if err := database.SetSecret(request.Context(), name, key); err != nil {
		return err
	}
	return database.Audit(request.Context(), webActorPrefix+current.user.Username, store.ActionSetSecret, "", map[string]any{"name": name})
}
