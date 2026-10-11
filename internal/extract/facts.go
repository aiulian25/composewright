// Package extract reads a fetched repository and records deployment facts with
// the file and line each one came from. It never calls a model.
package extract

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

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
	StackHints   []string       `json:"stack_hints"`
	Problems     []string       `json:"problems"`
}

type ComposeFile struct {
	Path      string           `json:"path"`
	Services  []ComposeService `json:"services"`
	Variables []Mention        `json:"variables"`
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
}

type ComposePort struct {
	Target    int      `json:"target"`
	Published string   `json:"published"`
	HostIP    string   `json:"host_ip"`
	Protocol  string   `json:"protocol"`
	Location  Location `json:"location"`
}

type ComposeVolume struct {
	Type     string   `json:"type"`
	Source   string   `json:"source"`
	Target   string   `json:"target"`
	ReadOnly bool     `json:"read_only"`
	Location Location `json:"location"`
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
}

type Mention struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Default  string   `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
	Text     string   `json:"text,omitempty"`
	Location Location `json:"location"`
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
			if variable.Default == "" {
				variable.Default = mention.Default
			}
			if variable.Description == "" {
				variable.Description = mention.Text
			}
		}
		variables = append(variables, *variable)
	}
	sort.Slice(variables, func(left, right int) bool { return variables[left].Name < variables[right].Name })
	return variables
}

// Redacted returns a copy that is safe to display or send to a model: values
// of secret-named variables are masked, and every other string, from the
// summary to a compose command, loses credentials such as URL passwords,
// tokens and secret assignments.
func (facts *Facts) Redacted() (*Facts, error) {
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var copied Facts
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	for index := range copied.Variables {
		variable := &copied.Variables[index]
		variable.Default = redact.Value(variable.Name, variable.Default)
		for mention := range variable.Mentions {
			variable.Mentions[mention].Default = redact.Value(variable.Name, variable.Mentions[mention].Default)
		}
	}
	for sample := range copied.EnvSamples {
		for index, entry := range copied.EnvSamples[sample].Entries {
			copied.EnvSamples[sample].Entries[index].Value = redact.Value(entry.Key, entry.Value)
		}
	}
	for file := range copied.ComposeFiles {
		for service := range copied.ComposeFiles[file].Services {
			environment := copied.ComposeFiles[file].Services[service].Environment
			for index, variable := range environment {
				environment[index].Raw = redact.Value(variable.Key, variable.Raw)
			}
		}
	}
	for run := range copied.DockerRuns {
		for index, variable := range copied.DockerRuns[run].Env {
			copied.DockerRuns[run].Env[index].Value = redact.Value(variable.Key, variable.Value)
		}
	}
	return redactStrings(&copied)
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
