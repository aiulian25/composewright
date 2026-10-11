// Package export writes a stored project to disk: a plain folder, or the
// template layout Arcane watches (data/templates/<name>/).
package export

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
)

var ErrExists = errors.New("file exists; pass --overwrite to replace it")

// Bundle is what an export writes. Env carries secrets in plain text;
// EnvExample carries none.
type Bundle struct {
	Project    string
	Compose    []byte
	Env        []byte
	EnvExample []byte
	Deploy     []byte
}

type Written struct {
	Directory      string   `json:"directory"`
	Files          []string `json:"files"`
	ContainsSecret bool     `json:"contains_secrets"`
}

// Write exports to target: "arcane:DIR" writes an Arcane template without
// secrets into DIR/<project>/; any other value is a folder for compose.yaml,
// .env (0600) and DEPLOY.md. An empty target uses profile.storage.save_dir.
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
	return write(directory, files, overwrite, true)
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
	written := Written{Directory: directory, ContainsSecret: secrets}
	for _, name := range names {
		content, present := files[name]
		if !present {
			continue
		}
		path := filepath.Join(directory, name)
		if _, err := os.Lstat(path); err == nil && !overwrite {
			return written, fmt.Errorf("%s: %w", path, ErrExists)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return written, err
		}
		if err := atomicfile.Write(path, content, fileMode); err != nil {
			return written, err
		}
		written.Files = append(written.Files, path)
	}
	return written, nil
}
