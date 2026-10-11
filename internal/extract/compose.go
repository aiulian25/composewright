package extract

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/mattn/go-shellwords"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
)

var errNotCompose = errors.New("not a compose file")

// requiredPlaceholder satisfies ${VAR:?message} so a file that demands a
// value from the deployer can still be read; raw values come from the YAML.
const requiredPlaceholder = "composewright-required"

// maxHealthRetries bounds an upstream retries value before it becomes an int.
const maxHealthRetries uint64 = 1000

// nullTag is the tag the YAML parser gives a key with no value.
const nullTag = "!!null"

// scalarKey holds one YAML value for template.ExtractVariables, which reads
// a mapping.
const scalarKey = "value"

// nonComposeKeys mark YAML documents that have a services key but are
// something else (CI workflows, Kubernetes manifests).
var nonComposeKeys = []string{"jobs", "on", "apiVersion", "kind", "steps", "stages"}

var referenceName = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// extractCompose reads one compose file twice: as YAML nodes for line numbers
// and raw environment values, and through compose-go for normalised ports,
// volumes and healthchecks, interpolated with the upstream defaults.
func extractCompose(ctx context.Context, relative string, content []byte, interpolation map[string]string) (ComposeFile, []Mention, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return ComposeFile{}, nil, errNotCompose
	}
	root := documentRoot(&document)
	services := child(root, "services")
	if !isCompose(root, services) {
		return ComposeFile{}, nil, errNotCompose
	}
	loadable, err := withoutEmptyKeys(&document, services, content)
	if err != nil {
		return ComposeFile{}, nil, fmt.Errorf("%s: %w", relative, err)
	}
	if _, err := composefile.LoadModel(ctx, relative, loadable); err != nil {
		return ComposeFile{}, nil, fmt.Errorf("%s: %w", relative, err)
	}
	extracted := variablesInOrder(root)
	mentions := interpolationMentions(relative, content, extracted)
	environment := map[string]string{}
	for name, value := range interpolation {
		environment[name] = value
	}
	for name, variable := range extracted {
		_, set := environment[name]
		switch {
		case set:
		case variable.DefaultValue != "":
			environment[name] = variable.DefaultValue
		case variable.Required:
			environment[name] = requiredPlaceholder
		}
	}
	project, err := composefile.Load(ctx, relative, loadable, environment, true)
	if err != nil {
		return ComposeFile{}, mentions, fmt.Errorf("%s: %w", relative, err)
	}
	file := ComposeFile{Path: relative, Variables: mentions}
	for _, entry := range entries(services) {
		service, found := project.Services[entry.key.Value]
		if !found {
			continue
		}
		extractedService := composeService(relative, entry.key, entry.value, service)
		file.Services = append(file.Services, extractedService)
		for _, variable := range extractedService.Environment {
			mentions = append(mentions, environmentMention(variable))
		}
	}
	return file, mentions, nil
}

// withoutEmptyKeys drops service keys left without a value, such as an
// environment: whose entries are all commented out. Upstream files often ship
// them that way; compose-go rejects them although they say nothing. Line
// numbers still come from the original nodes.
func withoutEmptyKeys(document, services *yaml.Node, content []byte) ([]byte, error) {
	removed := false
	for _, entry := range entries(services) {
		removed = dropEmptyValues(entry.value) || removed
	}
	if !removed {
		return content, nil
	}
	return yaml.Marshal(document)
}

func dropEmptyValues(mapping *yaml.Node) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	kept := make([]*yaml.Node, 0, len(mapping.Content))
	for _, entry := range entries(mapping) {
		if entry.value.Tag != nullTag {
			kept = append(kept, entry.key, entry.value)
		}
	}
	removed := len(kept) != len(mapping.Content)
	mapping.Content = kept
	return removed
}

// variablesInOrder finds the ${VAR} references of a file in document order,
// each value read by compose-go's own parser. When a name is written with
// different defaults, the first default counts, on every run; compose-go's
// ExtractVariables walks a map, so it could pick either.
func variablesInOrder(root *yaml.Node) map[string]template.Variable {
	variables := map[string]template.Variable{}
	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		for _, child := range node.Content {
			walk(child)
		}
		if node.Kind != yaml.ScalarNode {
			return
		}
		for name, variable := range template.ExtractVariables(map[string]any{scalarKey: node.Value}, template.DefaultPattern) {
			known, seen := variables[name]
			if !seen || known.DefaultValue == "" && variable.DefaultValue != "" {
				variables[name] = variable
			}
		}
	}
	walk(root)
	return variables
}

func isCompose(root, services *yaml.Node) bool {
	if root == nil || root.Kind != yaml.MappingNode || services == nil || services.Kind != yaml.MappingNode {
		return false
	}
	for _, key := range nonComposeKeys {
		if child(root, key) != nil {
			return false
		}
	}
	for _, entry := range entries(services) {
		if child(entry.value, "image") != nil || child(entry.value, "build") != nil {
			return true
		}
	}
	return false
}

func composeService(relative string, key, node *yaml.Node, service types.ServiceConfig) ComposeService {
	at := func(field *yaml.Node) Location { return lineOf(relative, field, key) }
	result := ComposeService{
		Name: key.Value, Location: at(key), Image: service.Image, ImageAt: at(keyNode(node, "image")),
		Build: service.Build != nil, User: service.User,
		Command: rawCommand(child(node, "command"), service.Command), Entrypoint: rawCommand(child(node, "entrypoint"), service.Entrypoint),
		Environment: environmentEntries(relative, child(node, "environment")),
		EnvFiles:    envFiles(child(node, "env_file")),
	}
	portItems := items(child(node, "ports"))
	for index, port := range service.Ports {
		result.Ports = append(result.Ports, ComposePort{
			Target: int(port.Target), Published: port.Published, HostIP: port.HostIP, Protocol: port.Protocol,
			Location: at(itemOrKey(portItems, index, keyNode(node, "ports"))),
		})
	}
	volumeItems := items(child(node, "volumes"))
	for index, volume := range service.Volumes {
		result.Volumes = append(result.Volumes, ComposeVolume{
			Type: volume.Type, Source: volume.Source, Target: volume.Target, ReadOnly: volume.ReadOnly,
			Location: at(itemOrKey(volumeItems, index, keyNode(node, "volumes"))),
		})
	}
	for _, name := range sortedDependencies(service.DependsOn) {
		result.DependsOn = append(result.DependsOn, ComposeDependency{Service: name, Condition: service.DependsOn[name].Condition})
	}
	if service.HealthCheck != nil {
		result.Healthcheck = healthcheck(service.HealthCheck, at(keyNode(node, "healthcheck")))
		result.Healthcheck.Test = rawTest(child(child(node, "healthcheck"), "test"), result.Healthcheck.Test)
	}
	return result
}

// rawCommand is a command as upstream wrote it, before interpolation, so a
// ${VAR} in it stays a reference to the .env value instead of becoming the
// upstream default; a string is split into words as Compose splits it.
func rawCommand(node *yaml.Node, interpolated []string) []string {
	if node == nil || (node.Kind != yaml.SequenceNode && node.Kind != yaml.ScalarNode) {
		return interpolated
	}
	if node.Kind == yaml.SequenceNode {
		return scalars(node)
	}
	words, err := shellwords.Parse(node.Value)
	if err != nil {
		return interpolated
	}
	return words
}

// rawTest is a healthcheck test as upstream wrote it, like rawCommand; a
// string is a CMD-SHELL test, as Compose reads it.
func rawTest(node *yaml.Node, interpolated []string) []string {
	if node == nil || (node.Kind != yaml.SequenceNode && node.Kind != yaml.ScalarNode) {
		return interpolated
	}
	if node.Kind == yaml.SequenceNode {
		return scalars(node)
	}
	return []string{ir.HealthcheckShell, node.Value}
}

func scalars(node *yaml.Node) []string {
	values := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		values = append(values, item.Value)
	}
	return values
}

func healthcheck(config *types.HealthCheckConfig, location Location) *Healthcheck {
	result := &Healthcheck{Test: config.Test, Disabled: config.Disable, Location: location}
	result.Interval = duration(config.Interval)
	result.Timeout = duration(config.Timeout)
	result.StartPeriod = duration(config.StartPeriod)
	if config.Retries != nil {
		result.Retries = int(min(*config.Retries, maxHealthRetries))
	}
	return result
}

func duration(value *types.Duration) string {
	if value == nil {
		return ""
	}
	return time.Duration(*value).String()
}

func environmentEntries(relative string, node *yaml.Node) []ComposeVariable {
	var variables []ComposeVariable
	switch {
	case node == nil:
	case node.Kind == yaml.MappingNode:
		for _, entry := range entries(node) {
			hasValue := entry.value.Tag != nullTag
			variables = append(variables, ComposeVariable{Key: entry.key.Value, Raw: entry.value.Value, HasValue: hasValue,
				Location: Location{File: relative, Line: entry.key.Line}})
		}
	case node.Kind == yaml.SequenceNode:
		for _, item := range node.Content {
			key, value, hasValue := strings.Cut(item.Value, "=")
			variables = append(variables, ComposeVariable{Key: strings.TrimSpace(key), Raw: value, HasValue: hasValue,
				Location: Location{File: relative, Line: item.Line}})
		}
	}
	return variables
}

func envFiles(node *yaml.Node) []string {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		return []string{node.Value}
	}
	var files []string
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode {
			files = append(files, item.Value)
			continue
		}
		if path := child(item, "path"); path != nil {
			files = append(files, path.Value)
		}
	}
	return files
}

func environmentMention(variable ComposeVariable) Mention {
	mention := Mention{Name: variable.Key, Kind: KindCompose, Location: variable.Location}
	if !strings.Contains(variable.Raw, "$") {
		mention.Default = variable.Raw
	}
	return mention
}

func interpolationMentions(relative string, content []byte, variables map[string]template.Variable) []Mention {
	lines := strings.Split(string(content), "\n")
	names := make([]string, 0, len(variables))
	for name := range variables {
		names = append(names, name)
	}
	sort.Strings(names)
	mentions := make([]Mention, 0, len(names))
	for _, name := range names {
		variable := variables[name]
		line := 0
		for index, text := range lines {
			if strings.Contains(text, "${"+name) || strings.Contains(text, "$"+name) {
				line = index + 1
				break
			}
		}
		mentions = append(mentions, Mention{Name: name, Kind: KindCompose, Default: variable.DefaultValue, Required: variable.Required,
			Location: Location{File: relative, Line: line}})
	}
	return mentions
}

// References lists the ${VAR} names used inside a raw compose value.
func References(raw string) []string {
	var names []string
	for _, match := range referenceName.FindAllStringSubmatch(raw, -1) {
		names = append(names, match[1])
	}
	return names
}

type pair struct {
	key   *yaml.Node
	value *yaml.Node
}

func documentRoot(document *yaml.Node) *yaml.Node {
	if document.Kind == yaml.DocumentNode && len(document.Content) > 0 {
		return document.Content[0]
	}
	return document
}

func entries(mapping *yaml.Node) []pair {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	pairs := make([]pair, 0, len(mapping.Content)/2)
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		pairs = append(pairs, pair{key: mapping.Content[index], value: mapping.Content[index+1]})
	}
	return pairs
}

func child(mapping *yaml.Node, key string) *yaml.Node {
	for _, entry := range entries(mapping) {
		if entry.key.Value == key {
			return entry.value
		}
	}
	return nil
}

func keyNode(mapping *yaml.Node, key string) *yaml.Node {
	for _, entry := range entries(mapping) {
		if entry.key.Value == key {
			return entry.key
		}
	}
	return nil
}

func items(node *yaml.Node) []*yaml.Node {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	return node.Content
}

func itemOrKey(nodes []*yaml.Node, index int, fallback *yaml.Node) *yaml.Node {
	if index < len(nodes) {
		return nodes[index]
	}
	return fallback
}

func lineOf(relative string, node, fallback *yaml.Node) Location {
	if node == nil {
		node = fallback
	}
	return Location{File: relative, Line: node.Line}
}

func sortedDependencies(dependencies types.DependsOnConfig) []string {
	names := make([]string, 0, len(dependencies))
	for name := range dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
