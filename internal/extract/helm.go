package extract

import (
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

var helmEnvKeys = []string{"env", "extraEnv", "environment", "extraEnvVars"}

func isHelmValues(relative string) bool {
	return path.Base(relative) == "values.yaml" && strings.Contains("/"+relative, "/charts/")
}

// helmMentions reads env blocks in a chart's values.yaml, either a list of
// {name, value} objects or a NAME: value mapping.
func helmMentions(relative string, content []byte) []Mention {
	var document yaml.Node
	if yaml.Unmarshal(content, &document) != nil {
		return nil
	}
	var mentions []Mention
	var visit func(node *yaml.Node)
	visit = func(node *yaml.Node) {
		for _, entry := range entries(node) {
			if isHelmEnvKey(entry.key.Value) {
				mentions = append(mentions, helmEnvBlock(relative, entry.value)...)
			}
			visit(entry.value)
		}
	}
	visit(documentRoot(&document))
	return mentions
}

func isHelmEnvKey(key string) bool {
	for _, candidate := range helmEnvKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func helmEnvBlock(relative string, node *yaml.Node) []Mention {
	var mentions []Mention
	if node.Kind == yaml.MappingNode {
		for _, entry := range entries(node) {
			if isVariableName(entry.key.Value) && strings.ToUpper(entry.key.Value) == entry.key.Value {
				mentions = append(mentions, Mention{Name: entry.key.Value, Kind: KindHelm, Default: entry.value.Value, Location: Location{File: relative, Line: entry.key.Line}})
			}
		}
	}
	for _, item := range items(node) {
		name, value := child(item, "name"), child(item, "value")
		if name == nil {
			continue
		}
		mention := Mention{Name: name.Value, Kind: KindHelm, Location: Location{File: relative, Line: name.Line}}
		if value != nil {
			mention.Default = value.Value
		}
		mentions = append(mentions, mention)
	}
	return mentions
}
