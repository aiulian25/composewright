package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/atomicfile"
)

const yamlIndent = 2

// SetInFile changes one dotted key in the config file while keeping its
// comments, and refuses the change when the resulting file does not validate.
func SetInFile(path, key, value string) error {
	return UpdateFile(path, map[string]string{key: value})
}

// UpdateFile changes several dotted keys at once, keeping comments. Values
// are YAML text. The file is written only when the result validates as a
// whole, so a set of related changes never lands half applied.
func UpdateFile(path string, values map[string]string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		content = DefaultTemplate
	} else if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		replacement, err := parseValue(values[key])
		if err != nil {
			return fmt.Errorf("value for %s: %w", key, err)
		}
		if err := setNode(rootMapping(&document), strings.Split(key, "."), replacement); err != nil {
			return err
		}
	}
	updated, err := encodeNode(&document)
	if err != nil {
		return err
	}
	config, err := decodeOnto(Default(), updated)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.Join(keys, ", "), err)
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return err
	}
	return atomicfile.Write(path, updated, FileMode)
}

// Lookup returns the effective value of a dotted key as YAML text.
func Lookup(config Config, key string) (string, error) {
	var document yaml.Node
	if err := document.Encode(config); err != nil {
		return "", err
	}
	node := &document
	for _, segment := range strings.Split(key, ".") {
		node = mappingValue(node, segment)
		if node == nil {
			return "", fmt.Errorf("unknown config key %q", key)
		}
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value, nil
	}
	rendered, err := encodeNode(node)
	return strings.TrimRight(string(rendered), "\n"), err
}

func Render(config Config) ([]byte, error) {
	var document yaml.Node
	if err := document.Encode(config); err != nil {
		return nil, err
	}
	return encodeNode(&document)
}

func parseValue(value string) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(value), &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}, nil
	}
	return document.Content[0], nil
}

func rootMapping(document *yaml.Node) *yaml.Node {
	if document.Kind == yaml.DocumentNode && len(document.Content) > 0 {
		return document.Content[0]
	}
	if document.Kind == 0 {
		document.Kind = yaml.DocumentNode
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		return document.Content[0]
	}
	return document
}

func setNode(mapping *yaml.Node, path []string, replacement *yaml.Node) error {
	if mapping.Kind != yaml.MappingNode {
		return fmt.Errorf("cannot set %q inside a non-mapping value", strings.Join(path, "."))
	}
	existing := mappingValue(mapping, path[0])
	if len(path) == 1 {
		if existing == nil {
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: path[0]}, replacement)
			return nil
		}
		replacement.LineComment = existing.LineComment
		*existing = *replacement
		return nil
	}
	if existing == nil {
		existing = &yaml.Node{Kind: yaml.MappingNode}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: path[0]}, existing)
	}
	return setNode(existing, path[1:], replacement)
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func encodeNode(node *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(yamlIndent)
	if err := encoder.Encode(node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// RenderProfile writes a profile as a standalone profile.yaml document.
func RenderProfile(profile Profile) ([]byte, error) {
	var document yaml.Node
	if err := document.Encode(profile); err != nil {
		return nil, err
	}
	return encodeNode(&document)
}
