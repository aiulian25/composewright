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
// EnvExample carries none. Secrets are the secrets passed as files, by their
// path beside compose.yaml, such as secrets/postgres_password.
type Bundle struct {
	Project    string
	Compose    []byte
	Env        []byte
	EnvExample []byte
	Deploy     []byte
	Secrets    map[string][]byte
}

type Written struct {
	Directory      string   `json:"directory"`
	Files          []string `json:"files"`
	ContainsSecret bool     `json:"contains_secrets"`
}

// Write exports to target: "arcane:DIR" writes an Arcane template without
// secrets into DIR/<project>/; any other value is a folder for compose.yaml,
// .env (0600), DEPLOY.md and the secret files (0600, in a 0700 folder). An
// empty target uses profile.storage.save_dir.
func Write(target string, profile config.Profile, bundle Bundle, overwrite bool) (Written, error) {
	if templates, isArcane := strings.CutPrefix(target, arcanePrefix); isArcane {
		directory := filepath.Join(templates, bundle.Project)
		files := map[string][]byte{ComposeName: bundle.Compose, EnvExampleName: bundle.EnvExample}
		return write(directory, files, overwrite, false)
	}
	directory, err := folder(target, profile, bundle.Project)
	if err != nil {
		return Written{}, err
	}
	files := map[string][]byte{ComposeName: bundle.Compose, EnvName: bundle.Env, DeployName: bundle.Deploy}
	maps.Copy(files, bundle.Secrets)
	return write(directory, files, overwrite, true)
}

// Shell is the bundle as commands that write compose.yaml, .env and each
// secret file with the modes Write gives them. A secret is written with
// printf, so its file holds the value without a newline.
func Shell(bundle Bundle) []byte {
	var script strings.Builder
	heredoc(&script, ComposeName, bundle.Compose)
	heredoc(&script, EnvName, bundle.Env)
	fmt.Fprintf(&script, "chmod %o %s\n", fileMode, EnvName)
	paths := slices.Sorted(maps.Keys(bundle.Secrets))
	for _, folder := range slices.Compact(directoriesOf(paths)) {
		fmt.Fprintf(&script, "mkdir -p -m %o %s\n", dirMode, folder)
	}
	for _, name := range paths {
		fmt.Fprintf(&script, "printf '%%s' %s > %s\nchmod %o %s\n", shellQuoted(string(bundle.Secrets[name])), name, fileMode, name)
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

func write(directory string, files map[string][]byte, overwrite, secrets bool) (Written, error) {
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
		content, present := files[name]
		if !present {
			continue
		}
		target := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
			return written, err
		}
		if err := atomicfile.Write(target, content, fileMode); err != nil {
			return written, err
		}
		written.Files = append(written.Files, target)
	}
	return written, nil
}

// refuseExisting checks every file an export would write before it writes
// any, so a refused export leaves the folder as it was.
func refuseExisting(directory string, names []string, files map[string][]byte) error {
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
