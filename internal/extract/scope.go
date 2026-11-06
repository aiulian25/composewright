package extract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/fetch"
)

// ExtractSource keeps repository-relative evidence while excluding sibling
// projects before reading their files. Shared configs are bundled as opaque
// files, not treated as another project's Compose/README deployment facts.
func ExtractSource(ctx context.Context, source *fetch.Source) (*Facts, error) {
	if source.Subdir == "" {
		return Extract(ctx, source.Directory)
	}
	if !fetch.SafeRepositoryPath(source.Subdir, false) {
		return nil, errors.New("unsafe subproject root")
	}
	inside := func(name string) bool { return name == source.Subdir || strings.HasPrefix(name, source.Subdir+"/") }
	files, unreadable, err := readFilesMatching(source.Directory, func(name string, dir bool) bool {
		if dir {
			return name == "." || name == source.Subdir || strings.HasPrefix(source.Subdir, name+"/") || inside(name)
		}
		return inside(name)
	})
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(source.Directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	known := map[string]bool{}
	for _, file := range files {
		known[file.relative] = true
	}
	var shared []string
	add := func(filename, raw string, required, sample bool) error {
		if strings.Contains(raw, "$") {
			return fmt.Errorf("%s: dynamic shared-file path cannot be scoped; use a literal path or the repository root", filename)
		}
		target := path.Join(path.Dir(filename), raw)
		if path.IsAbs(raw) || !fetch.SafeRepositoryPath(target, false) {
			return fmt.Errorf("%s: referenced file leaves the repository; choose repository-contained deployment files", filename)
		}
		candidates := []string{target}
		if sample {
			for _, marker := range []string{".example", ".sample", ".template", ".dist"} {
				candidates = append(candidates, target+marker)
			}
		} else {
			extension := path.Ext(target)
			for _, marker := range exampleMarkers {
				candidates = append(candidates, strings.TrimSuffix(target, extension)+marker+extension, target+marker)
			}
		}
		for _, candidate := range candidates {
			if known[candidate] {
				return nil
			}
			info, err := root.Lstat(candidate)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("shared input %s: %w", candidate, err)
			}
			if info.IsDir() {
				return fmt.Errorf("shared directory %s needs an explicit file selection; use file mounts or a repository-root link", candidate)
			}
			kind := fetch.Classify(candidate)
			if kind == fetch.Skip || !info.Mode().IsRegular() || info.Size() > fetch.Limit(kind) {
				return fmt.Errorf("shared input %s is not a retained deployment file within the size limit", candidate)
			}
			data, err := root.ReadFile(candidate)
			if err != nil {
				return err
			}
			if len(files) >= maxFiles {
				return errors.New("too many scoped deployment files")
			}
			known[candidate] = true
			files = append(files, sourceFile{relative: candidate, kind: kind, content: data, shared: !inside(candidate)})
			if !inside(candidate) {
				shared = append(shared, candidate)
			}
			return nil
		}
		if required {
			return fmt.Errorf("referenced deployment file %s was not retained; provide its example or choose the repository root", target)
		}
		return nil
	}
	// Scan only the selected subtree's YAML. Shared inputs cannot recursively
	// expand scope or cause arbitrary files/includes to be followed.
	selected := slices.Clone(files)
	for _, file := range selected {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isYAMLFile(file.relative) {
			continue
		}
		var document yaml.Node
		if yaml.Unmarshal(file.content, &document) != nil {
			continue
		}
		model := documentRoot(&document)
		services := child(model, "services")
		if !isCompose(model, services) {
			continue
		}
		if child(model, "include") != nil {
			return nil, fmt.Errorf("%s: includes need a repository-root source or a standalone Compose file", file.relative)
		}
		for _, service := range entries(services) {
			if child(service.value, "extends") != nil {
				return nil, fmt.Errorf("%s: extends needs a repository-root source or a standalone Compose file", file.relative)
			}
			env := child(service.value, "env_file")
			if env != nil {
				values := items(env)
				if env.Kind != yaml.SequenceNode {
					values = []*yaml.Node{env}
				}
				for _, value := range values {
					name, required := value.Value, true
					if value.Kind == yaml.MappingNode {
						if node := child(value, "path"); node != nil {
							name = node.Value
						}
						if node := child(value, "required"); node != nil {
							required = node.Value != "false"
						}
					}
					if name != "" {
						if err := add(file.relative, name, required, true); err != nil {
							return nil, err
						}
					}
				}
			}
			mounts := child(service.value, "volumes")
			if mounts != nil {
				for _, mount := range items(mounts) {
					name, required := "", false
					if mount.Kind == yaml.ScalarNode {
						parts := strings.Split(mount.Value, ":")
						if len(parts) > 1 && strings.HasPrefix(parts[0], ".") {
							name = parts[0]
							required = len(parts) > 2 && strings.Contains(parts[2], "ro")
						}
					}
					if mount.Kind == yaml.MappingNode {
						if node := child(mount, "type"); node != nil && node.Value == "bind" {
							if node := child(mount, "source"); node != nil && !path.IsAbs(node.Value) {
								name = node.Value
							}
							if node := child(mount, "read_only"); node != nil {
								required = node.Value == "true"
							}
						}
					}
					if name != "" {
						target := path.Join(path.Dir(file.relative), name)
						if inside(target) {
							continue
						} // Existing local-directory bind behavior.
						if err := add(file.relative, name, required, false); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		for _, kind := range []string{"secrets", "configs"} {
			for _, item := range entries(child(model, kind)) {
				if node := child(item.value, "file"); node != nil {
					if err := add(file.relative, node.Value, true, false); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	slices.Sort(shared)
	slices.SortFunc(files, func(a, b sourceFile) int { return strings.Compare(a.relative, b.relative) })
	facts, err := factsFromFiles(ctx, files, unreadable, &Facts{SourceRoot: source.Subdir, SharedFiles: shared})
	if err != nil {
		return nil, err
	}
	note := "Selected subproject: " + source.Subdir + "."
	if len(shared) > 0 {
		note += " Shared deployment inputs included: " + strings.Join(shared, ", ") + "."
	}
	facts.Problems = append(facts.Problems, note)
	return facts, nil
}
