package composefile

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

// keySeparator joins the keys of a path in KeyLines.
const keySeparator = "/"

// KeyPath is the KeyLines key of a path of YAML keys.
func KeyPath(keys ...string) string {
	return strings.Join(keys, keySeparator)
}

// KeyLines maps every key of a YAML document, by its KeyPath, to the line
// it starts on.
func KeyLines(text string) map[string]int {
	lines := map[string]int{}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil || len(document.Content) == 0 {
		return lines
	}
	Walk(document.Content[0], nil, func(path []string, line int) {
		key := KeyPath(path...)
		if _, seen := lines[key]; !seen {
			lines[key] = line
		}
	})
	return lines
}

// Walk visits every mapping key and sequence item with its path and line.
func Walk(node *yaml.Node, path []string, visit func([]string, int)) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			child := append(append([]string{}, path...), key.Value)
			visit(child, key.Line)
			Walk(value, child, visit)
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			visit(path, item.Line)
			Walk(item, path, visit)
		}
	}
}
