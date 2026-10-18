// Package extract reads a fetched repository and records deployment facts with
// the file and line each one came from. It never calls a model.
package extract

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
)

const (
	KindCompose    = "compose"
	KindDockerfile = "dockerfile"
	KindEnvSample  = "env_sample"
	KindReadme     = "readme"
	KindCode       = "code"
	KindHelm       = "helm"
	KindWorkflow   = "ci_workflow"
)

// Location is a file path relative to the repository root and a 1-based line.
type Location struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

func (location Location) String() string {
	if location.Line == 0 {
		return location.File
	}
	return fmt.Sprintf("%s:%d", location.File, location.Line)
}

type Facts struct {
	Files        []string       `json:"files"`
	ComposeFiles []ComposeFile  `json:"compose_files"`
	Dockerfiles  []Dockerfile   `json:"dockerfiles"`
	EnvSamples   []EnvSample    `json:"env_samples"`
	Variables    []Variable     `json:"variables"`
	Images       []ImageMention `json:"images"`
	DockerRuns   []DockerRun    `json:"docker_runs"`
	Notes        []Note         `json:"notes"`
	Suspicious   []Note         `json:"suspicious"`
	Readmes      []string       `json:"readmes"`
	Summary      Note           `json:"summary"`
	FileMentions []Note         `json:"file_mentions"`
	// HealthPaths are health endpoints the root README documents.
	HealthPaths []Note   `json:"health_paths,omitempty"`
	StackHints  []string `json:"stack_hints"`
	Problems    []string `json:"problems"`
}

type ComposeFile struct {
	Path      string           `json:"path"`
	Services  []ComposeService `json:"services"`
	Variables []Mention        `json:"variables"`
	// Fenced marks a file read from a fenced block of a README or docs page.
	Fenced bool `json:"fenced,omitempty"`
}

// ComposeCounts are how many compose files the repository ships and how
// many compose examples its README and docs pages show.
func (facts *Facts) ComposeCounts() (files, examples int) {
	for _, file := range facts.ComposeFiles {
		if file.Fenced {
			examples++
			continue
		}
		files++
	}
	return files, examples
}

// Opening is where the file starts: its first line, or the fence its path
// already names.
func (file ComposeFile) Opening() Location {
	if file.Fenced {
		return Location{File: file.Path}
	}
	return Location{File: file.Path, Line: 1}
}

type ComposeService struct {
	Name        string              `json:"name"`
	Location    Location            `json:"location"`
	Image       string              `json:"image"`
	ImageAt     Location            `json:"image_at"`
	Build       bool                `json:"build"`
	Ports       []ComposePort       `json:"ports"`
	Volumes     []ComposeVolume     `json:"volumes"`
	Environment []ComposeVariable   `json:"environment"`
	EnvFiles    []string            `json:"env_files"`
	DependsOn   []ComposeDependency `json:"depends_on"`
	Healthcheck *Healthcheck        `json:"healthcheck,omitempty"`
	Command     []string            `json:"command"`
	Entrypoint  []string            `json:"entrypoint"`
	User        string              `json:"user"`
	// Profiles are the Compose profiles that start the service; without
	// one enabled, Compose leaves it out.
	Profiles []string `json:"profiles,omitempty"`
	// MemoryLimit and ShmSize are sizes as upstream wrote them, such as 2g.
	MemoryLimit string   `json:"memory_limit,omitempty"`
	CPUs        string   `json:"cpus,omitempty"`
	PIDsLimit   int      `json:"pids_limit,omitempty"`
	ShmSize     string   `json:"shm_size,omitempty"`
	LimitsAt    Location `json:"limits_at,omitzero"`
	CapDrop     []string `json:"cap_drop,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	Tmpfs       []string `json:"tmpfs,omitempty"`
	HardeningAt Location `json:"hardening_at,omitzero"`
	// Devices are host:container[:permissions] mappings; they, CapAdd,
	// Privileged and a host NetworkMode are privileges the user grants.
	Devices       []ComposeEntry `json:"devices,omitempty"`
	CapAdd        []ComposeEntry `json:"cap_add,omitempty"`
	Privileged    bool           `json:"privileged,omitempty"`
	PrivilegedAt  Location       `json:"privileged_at,omitzero"`
	NetworkMode   string         `json:"network_mode,omitempty"`
	NetworkModeAt Location       `json:"network_mode_at,omitzero"`
}

// ComposeEntry is one item of a list upstream wrote, with its line.
type ComposeEntry struct {
	Value    string   `json:"value"`
	Location Location `json:"location"`
}

type ComposePort struct {
	Target    int      `json:"target"`
	Published string   `json:"published"`
	HostIP    string   `json:"host_ip"`
	Protocol  string   `json:"protocol"`
	Location  Location `json:"location"`
}

type ComposeVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	// TmpfsSize, in bytes, and TmpfsMode are a tmpfs volume's options.
	TmpfsSize int64    `json:"tmpfs_size,omitempty"`
	TmpfsMode uint32   `json:"tmpfs_mode,omitempty"`
	Location  Location `json:"location"`
}

// ComposeVariable is one environment entry as written upstream; Raw keeps
// ${VAR} references un-interpolated so rules can rebuild them.
type ComposeVariable struct {
	Key      string   `json:"key"`
	Raw      string   `json:"raw"`
	HasValue bool     `json:"has_value"`
	Location Location `json:"location"`
}

type ComposeDependency struct {
	Service   string `json:"service"`
	Condition string `json:"condition"`
}

type Healthcheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval"`
	Timeout     string   `json:"timeout"`
	StartPeriod string   `json:"start_period"`
	Retries     int      `json:"retries"`
	Disabled    bool     `json:"disabled"`
	Location    Location `json:"location"`
}

type Dockerfile struct {
	Path        string       `json:"path"`
	BaseImage   string       `json:"base_image"`
	Exposed     []PortFact   `json:"exposed"`
	Volumes     []PathFact   `json:"volumes"`
	Env         []KeyValue   `json:"env"`
	Args        []KeyValue   `json:"args"`
	User        string       `json:"user"`
	UserAt      Location     `json:"user_at"`
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	Entrypoint  []string     `json:"entrypoint"`
	Command     []string     `json:"command"`
}

type PortFact struct {
	Port     int      `json:"port"`
	Protocol string   `json:"protocol"`
	Location Location `json:"location"`
}

type PathFact struct {
	Path     string   `json:"path"`
	Location Location `json:"location"`
}

type KeyValue struct {
	Key      string   `json:"key"`
	Value    string   `json:"value"`
	Location Location `json:"location"`
}

type EnvSample struct {
	Path    string           `json:"path"`
	Entries []EnvSampleEntry `json:"entries"`
}

type EnvSampleEntry struct {
	Key       string   `json:"key"`
	Value     string   `json:"value"`
	Comment   string   `json:"comment"`
	Commented bool     `json:"commented"`
	Location  Location `json:"location"`
}

// Variable merges every mention of one environment variable name.
type Variable struct {
	Name        string    `json:"name"`
	Default     string    `json:"default"`
	Description string    `json:"description"`
	Required    bool      `json:"required"`
	Mentions    []Mention `json:"mentions"`
	// FileTwin is the documented variable that reads this secret from a
	// file instead, as POSTGRES_PASSWORD_FILE for POSTGRES_PASSWORD.
	FileTwin string `json:"file_twin,omitempty"`
}

type Mention struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Default  string `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Commented marks a sample entry left commented out (# KEY=value):
	// its value is an example, not what the sample sets.
	Commented bool     `json:"commented,omitempty"`
	Text      string   `json:"text,omitempty"`
	Location  Location `json:"location"`
}

type ImageMention struct {
	Reference string   `json:"reference"`
	Kind      string   `json:"kind"`
	Location  Location `json:"location"`
}

type DockerRun struct {
	Image    string     `json:"image"`
	Ports    []string   `json:"ports"`
	Volumes  []string   `json:"volumes"`
	Env      []KeyValue `json:"env"`
	Location Location   `json:"location"`
}

type Note struct {
	Text     string   `json:"text"`
	Location Location `json:"location"`
}

// Variable returns the merged variable with this name.
func (facts *Facts) Variable(name string) (Variable, bool) {
	index := slices.IndexFunc(facts.Variables, func(variable Variable) bool { return variable.Name == name })
	if index < 0 {
		return Variable{}, false
	}
	return facts.Variables[index], true
}

// HasKind reports whether a variable was seen in a source of this kind.
func (variable Variable) HasKind(kind string) bool {
	return slices.ContainsFunc(variable.Mentions, func(mention Mention) bool { return mention.Kind == kind })
}

// First returns the location of the most authoritative mention.
func (variable Variable) First() Location {
	if len(variable.Mentions) == 0 {
		return Location{}
	}
	return variable.Mentions[0].Location
}

// sourceOrder ranks where a description or default is most trustworthy.
var sourceOrder = []string{KindCompose, KindEnvSample, KindReadme, KindDockerfile, KindHelm, KindCode, KindWorkflow}

func mergeVariables(mentions []Mention) []Variable {
	byName := map[string]*Variable{}
	for _, mention := range mentions {
		variable, found := byName[mention.Name]
		if !found {
			variable = &Variable{Name: mention.Name}
			byName[mention.Name] = variable
		}
		variable.Mentions = append(variable.Mentions, mention)
	}
	variables := make([]Variable, 0, len(byName))
	for _, variable := range byName {
		sort.SliceStable(variable.Mentions, func(left, right int) bool {
			return slices.Index(sourceOrder, variable.Mentions[left].Kind) < slices.Index(sourceOrder, variable.Mentions[right].Kind)
		})
		for _, mention := range variable.Mentions {
			variable.Required = variable.Required || mention.Required
			// Copying the sample leaves a commented entry unset, so its value,
			// such as a DATABASE_URL for an outside database, is no default.
			if variable.Default == "" && !mention.Commented {
				variable.Default = mention.Default
			}
			if variable.Description == "" {
				variable.Description = mention.Text
			}
		}
		variables = append(variables, *variable)
	}
	sort.Slice(variables, func(left, right int) bool { return variables[left].Name < variables[right].Name })
	for index := range variables {
		twin := variables[index].Name + ir.FileVariableSuffix
		if _, documented := byName[twin]; documented && redact.IsSecretName(variables[index].Name) {
			variables[index].FileTwin = twin
		}
	}
	return variables
}

// Started returns a copy holding the services Compose starts with the
// optional services named in enabled switched on: the other services behind
// a profile are left out, with the images and variables found in them.
func (facts *Facts) Started(enabled []string) *Facts {
	copied := *facts
	copied.ComposeFiles = make([]ComposeFile, 0, len(facts.ComposeFiles))
	hidden := map[Location]bool{}
	for _, file := range facts.ComposeFiles {
		started, hiddenInFile := file.started(enabled)
		copied.ComposeFiles = append(copied.ComposeFiles, started)
		maps.Copy(hidden, hiddenInFile)
	}
	if len(hidden) == 0 {
		return &copied
	}
	return copied.without(facts, func(_ string, location Location) bool { return hidden[location] })
}

// FromCompose returns a copy holding only the compose file path, with the
// images and variables found in the others left out; empty, the facts as
// they are.
func (facts *Facts) FromCompose(path string) *Facts {
	if path == "" {
		return facts
	}
	copied := *facts
	copied.ComposeFiles = slices.DeleteFunc(slices.Clone(facts.ComposeFiles), func(file ComposeFile) bool { return file.Path != path })
	return copied.without(facts, func(kind string, location Location) bool { return kind == KindCompose && location.File != path })
}

// without fills copied, which holds the compose files kept, with the images
// and variables of facts but the mentions dropped names by their kind and
// line. One mention stands for every service of an image, so it stays while
// a kept service uses that image.
func (copied Facts) without(facts *Facts, dropped func(kind string, location Location) bool) *Facts {
	keptImages := map[string]bool{}
	for _, file := range copied.ComposeFiles {
		for _, service := range file.Services {
			keptImages[service.Image] = true
		}
	}
	copied.Images = slices.DeleteFunc(slices.Clone(facts.Images), func(mention ImageMention) bool {
		return dropped(mention.Kind, mention.Location) && !keptImages[mention.Reference]
	})
	var mentions []Mention
	for _, variable := range facts.Variables {
		for _, mention := range variable.Mentions {
			if !dropped(mention.Kind, mention.Location) {
				mentions = append(mentions, mention)
			}
		}
	}
	copied.Variables = mergeVariables(mentions)
	return &copied
}

// mark marks the lines that hold the service's image, variables, ports and
// volumes.
// ponytail: a variable interpolated on another line of the service, such
// as in a command, stays; mark the service's whole span if that matters.
func (service ComposeService) mark(lines map[Location]bool) {
	lines[service.ImageAt] = true
	for _, variable := range service.Environment {
		lines[variable.Location] = true
	}
	for _, port := range service.Ports {
		lines[port.Location] = true
	}
	for _, volume := range service.Volumes {
		lines[volume.Location] = true
	}
}

// Started is the file with the services Compose starts when the optional
// services named in enabled are switched on, and the variables of those.
func (file ComposeFile) Started(enabled []string) ComposeFile {
	started, _ := file.started(enabled)
	return started
}

// started is Started, with the lines of the services it leaves out.
func (file ComposeFile) started(enabled []string) (ComposeFile, map[Location]bool) {
	hidden := map[Location]bool{}
	file.Services = slices.DeleteFunc(slices.Clone(file.Services), func(service ComposeService) bool {
		switchedOn := slices.ContainsFunc(enabled, func(name string) bool { return strings.EqualFold(name, service.Name) })
		off := service.Optional() && !switchedOn
		if off {
			service.mark(hidden)
		}
		return off
	})
	// ponytail: a line a started service shares, as every line of a
	// flow-style file, keeps all its mentions, those of the service off
	// too; split mentions by column if such files matter.
	startedLines := map[Location]bool{{}: true}
	for _, service := range file.Services {
		service.mark(startedLines)
	}
	for line := range startedLines {
		delete(hidden, line)
	}
	file.Variables = slices.DeleteFunc(slices.Clone(file.Variables), func(mention Mention) bool { return hidden[mention.Location] })
	return file, hidden
}

// Optional is true for a service upstream starts only with a profile.
func (service ComposeService) Optional() bool {
	return len(service.Profiles) > 0
}

// Redacted returns a copy that is safe to display or send to a model: values
// of secret-named variables are masked, and every other string, from the
// summary to a compose command, loses credentials such as URL passwords,
// tokens and secret assignments. Each distinct secret of a variable or a URL
// gets its own numbered mask, so a reader still sees which were equal. A
// value that cannot be secret material, such as a _FILE path, stays.
func (facts *Facts) Redacted() (*Facts, error) {
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var copied Facts
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	masker := redact.NewMasker()
	secretKeys := secretReaders(copied.ComposeFiles)
	redacted := func(name, value string) string {
		if key, readUnderSecret := secretKeys[name]; readUnderSecret {
			name = key
		}
		if redact.IsSecretName(name) && ir.IsSettingValue(name, value) {
			return value
		}
		return masker.Value(name, value)
	}
	for index := range copied.Variables {
		variable := &copied.Variables[index]
		variable.Default = redacted(variable.Name, variable.Default)
		for mention := range variable.Mentions {
			variable.Mentions[mention].Default = redacted(variable.Name, variable.Mentions[mention].Default)
		}
	}
	for sample := range copied.EnvSamples {
		for index, entry := range copied.EnvSamples[sample].Entries {
			copied.EnvSamples[sample].Entries[index].Value = redacted(entry.Key, entry.Value)
		}
	}
	for file := range copied.ComposeFiles {
		for index, mention := range copied.ComposeFiles[file].Variables {
			copied.ComposeFiles[file].Variables[index].Default = redacted(mention.Name, mention.Default)
		}
		for service := range copied.ComposeFiles[file].Services {
			environment := copied.ComposeFiles[file].Services[service].Environment
			for index, variable := range environment {
				environment[index].Raw = redacted(variable.Key, variable.Raw)
			}
		}
	}
	for run := range copied.DockerRuns {
		for index, variable := range copied.DockerRuns[run].Env {
			copied.DockerRuns[run].Env[index].Value = redacted(variable.Key, variable.Value)
		}
	}
	return redactStrings(&copied)
}

// secretReaders maps each variable whose name sounds harmless to a secret
// key that a compose file reads it under, as PG in POSTGRES_PASSWORD:
// ${PG:-hunter2}, so its defaults are masked as the secret they are.
func secretReaders(files []ComposeFile) map[string]string {
	readers := map[string]string{}
	for _, variable := range secretEnvironment(files) {
		for _, name := range harmlessReferences(variable.Raw) {
			readers[name] = variable.Key
		}
	}
	return readers
}

// secretEnvironment are the environment entries of the files' services
// under a secret key.
func secretEnvironment(files []ComposeFile) []ComposeVariable {
	var entries []ComposeVariable
	for _, file := range files {
		for _, service := range file.Services {
			entries = append(entries, service.Environment...)
		}
	}
	return slices.DeleteFunc(entries, func(variable ComposeVariable) bool { return !redact.IsSecretName(variable.Key) })
}

// harmlessReferences are the variables raw refers to whose names do not
// sound secret.
func harmlessReferences(raw string) []string {
	var names []string
	for _, match := range referenceName.FindAllStringSubmatch(raw, -1) {
		if !redact.IsSecretName(match[1]) {
			names = append(names, match[1])
		}
	}
	return names
}

// redactStrings runs redact.Text over every string of the facts, whichever
// field holds it.
func redactStrings(facts *Facts) (*Facts, error) {
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(redactTree(tree))
	if err != nil {
		return nil, err
	}
	var redacted Facts
	return &redacted, json.Unmarshal(encoded, &redacted)
}

func redactTree(node any) any {
	switch value := node.(type) {
	case string:
		return redact.Text(value)
	case []any:
		for index := range value {
			value[index] = redactTree(value[index])
		}
	case map[string]any:
		for key := range value {
			value[key] = redactTree(value[key])
		}
	}
	return node
}
