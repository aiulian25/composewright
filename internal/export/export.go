// Package export writes a stored project to disk: a plain folder, or the
// template layout Arcane watches (data/templates/<name>/).
package export

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	arcanePrefix = "arcane:"
	fileMode     = 0o600
	dirMode      = 0o700

	ComposeName    = "compose.yaml"
	EnvName        = ".env"
	EnvExampleName = ".env.example"
	DeployName     = "DEPLOY.md"

	// shellDelimiter ends each heredoc of Shell.
	shellDelimiter = "COMPOSEWRIGHT_EOF"
)

var ErrExists = errors.New("file exists; pass --overwrite to replace it")

// Bundle is what an export writes. Env carries secrets in plain text;
// EnvExample carries none. Secrets are the secrets passed as files and
// Config the repository's files the services mount, each by its path
// beside compose.yaml, such as secrets/postgres_password or
// config/nginx.conf.
type Bundle struct {
	Project    string
	Compose    []byte
	Env        []byte
	EnvExample []byte
	Deploy     []byte
	Secrets    map[string][]byte
	Config     map[string][]byte
}

// file is one file an export writes, with its mode.
type file struct {
	content []byte
	mode    fs.FileMode
}

type Written struct {
	Directory      string   `json:"directory"`
	Files          []string `json:"files"`
	ContainsSecret bool     `json:"contains_secrets"`
}

// Write exports to target: "arcane:DIR" writes an Arcane template without
// secrets into DIR/<project>/; any other value is a folder for compose.yaml,
// .env (0600), DEPLOY.md and the secret files (0600, in a 0700 folder). The
// config files go to both, readable by the container's user (0644). An
// empty target uses profile.storage.save_dir.
func Write(target string, profile config.Profile, bundle Bundle, overwrite bool) (Written, error) {
	if templates, isArcane := strings.CutPrefix(target, arcanePrefix); isArcane {
		directory := filepath.Join(templates, bundle.Project)
		files := map[string]file{ComposeName: {bundle.Compose, fileMode}, EnvExampleName: {bundle.EnvExample, fileMode}}
		withMode(files, bundle.Config, ir.ConfigFileMode)
		return write(directory, files, overwrite, false)
	}
	directory, err := folder(target, profile, bundle.Project)
	if err != nil {
		return Written{}, err
	}
	files := map[string]file{ComposeName: {bundle.Compose, fileMode}, EnvName: {bundle.Env, fileMode}, DeployName: {bundle.Deploy, fileMode}}
	withMode(files, bundle.Secrets, fileMode)
	withMode(files, bundle.Config, ir.ConfigFileMode)
	return write(directory, files, overwrite, true)
}

// withMode adds contents to files, each with mode.
func withMode(files map[string]file, contents map[string][]byte, mode fs.FileMode) {
	for name, content := range contents {
		files[name] = file{content: content, mode: mode}
	}
}

// Shell is the bundle as commands that write compose.yaml, .env, each
// secret file and each config file with the modes Write gives them. Those
// are written with printf, so each holds its content exactly: no newline
// ends up in a password.
func Shell(bundle Bundle) []byte {
	var script strings.Builder
	heredoc(&script, ComposeName, bundle.Compose)
	heredoc(&script, EnvName, bundle.Env)
	fmt.Fprintf(&script, "chmod %o %s\n", fileMode, EnvName)
	files := map[string]file{}
	withMode(files, bundle.Secrets, fileMode)
	withMode(files, bundle.Config, ir.ConfigFileMode)
	paths := slices.Sorted(maps.Keys(files))
	for _, folder := range slices.Compact(directoriesOf(paths)) {
		fmt.Fprintf(&script, "mkdir -p -m %o %s\n", dirMode, folder)
	}
	for _, name := range paths {
		fmt.Fprintf(&script, "printf '%%s' %s > %s\nchmod %o %s\n", shellQuoted(string(files[name].content)), name, files[name].mode, name)
	}
	return []byte(script.String())
}

func heredoc(script *strings.Builder, name string, content []byte) {
	body := string(content)
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	fmt.Fprintf(script, "cat > %s <<'%s'\n%s%s\n", name, shellDelimiter, body, shellDelimiter)
}

func directoriesOf(paths []string) []string {
	folders := make([]string, 0, len(paths))
	for _, name := range paths {
		folders = append(folders, path.Dir(name))
	}
	return folders
}

// shellQuoted is value in single quotes, each quote in it closed, escaped
// and reopened, so the shell reads it literally.
func shellQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func folder(target string, profile config.Profile, project string) (string, error) {
	if target != "" {
		return target, nil
	}
	saveDir := profile.Storage.SaveDir
	if strings.Contains(saveDir, config.LayoutBase) && profile.Storage.BasePath == "" {
		return "", errors.New("no destination: pass --to DIR, or set profile.storage.base_path for the default save folder")
	}
	return strings.NewReplacer(config.LayoutBase, profile.Storage.BasePath, config.LayoutProject, project).Replace(saveDir), nil
}

func write(directory string, files map[string]file, overwrite, secrets bool) (Written, error) {
	if err := os.MkdirAll(directory, dirMode); err != nil {
		return Written{}, err
	}
	names := []string{ComposeName, EnvName, EnvExampleName, DeployName}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	written := Written{Directory: directory, ContainsSecret: secrets}
	if !overwrite {
		if err := refuseExisting(directory, names, files); err != nil {
			return written, err
		}
	}
	for _, name := range names {
		entry, present := files[name]
		if !present {
			continue
		}
		target := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
			return written, err
		}
		if err := atomicfile.Write(target, entry.content, entry.mode); err != nil {
			return written, err
		}
		written.Files = append(written.Files, target)
	}
	return written, nil
}

// refuseExisting checks every file an export would write before it writes
// any, so a refused export leaves the folder as it was.
func refuseExisting(directory string, names []string, files map[string]file) error {
	for _, name := range names {
		if _, present := files[name]; !present {
			continue
		}
		target := filepath.Join(directory, filepath.FromSlash(name))
		_, err := os.Lstat(target)
		if err == nil {
			return fmt.Errorf("%s: %w", target, ErrExists)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
