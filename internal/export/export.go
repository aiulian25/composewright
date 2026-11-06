// Package export writes a stored project to disk: a plain folder, or the
// template layout Arcane watches (data/templates/<name>/).
package export

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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

	// privateUmask makes every file Shell creates readable by its owner
	// alone until its chmod; currentFolder is where a top-level file sits.
	privateUmask  = 0o077
	currentFolder = "."
	// backupMarker names the link that keeps a replaced file until the
	// whole bundle is in.
	backupMarker = ".replaced-"
)

var ErrExists = errors.New("file exists; pass --overwrite to replace it")

// ErrPartial is a save that failed and could not put every earlier file
// back: the folder holds part of it, as the error says.
var ErrPartial = errors.New("the folder holds part of this save")

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

// Shell is the bundle as commands that write compose.yaml, .env, DEPLOY.md,
// each secret file and each config file with the modes Write gives them.
// Every content and path is one single-quoted printf argument, so the shell
// reads it as data whatever it holds, and each file holds its content
// exactly: no newline ends up in a password. The umask keeps every file
// private from the moment it is created.
func Shell(bundle Bundle) []byte {
	var script strings.Builder
	fmt.Fprintf(&script, "umask %03o\n", privateUmask)
	files := map[string]file{ComposeName: {bundle.Compose, fileMode}, EnvName: {bundle.Env, fileMode}, DeployName: {bundle.Deploy, fileMode}}
	withMode(files, bundle.Secrets, fileMode)
	withMode(files, bundle.Config, ir.ConfigFileMode)
	paths := slices.Sorted(maps.Keys(files))
	for _, folder := range slices.Compact(directoriesOf(paths)) {
		if folder != currentFolder {
			fmt.Fprintf(&script, "mkdir -p -m %o %s\n", dirMode, shellQuoted(folder))
		}
	}
	for _, name := range paths {
		quotedName := shellQuoted(name)
		fmt.Fprintf(&script, "printf '%%s' %s > %s\nchmod %o %s\n", shellQuoted(string(files[name].content)), quotedName, files[name].mode, quotedName)
	}
	return []byte(script.String())
}

// DeployCommand is the command that starts the stack saved in directory,
// the folder quoted so the shell reads it literally.
func DeployCommand(directory string) string {
	return "cd " + shellQuoted(directory) + " && docker compose up -d"
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

// write writes files inside directory, the folder the person chose, opened
// once as a root: every folder and file below it resolves inside it, so a
// symbolic link there can never lead a file elsewhere. The chosen folder may
// itself be a link or a mount.
func write(directory string, files map[string]file, overwrite, secrets bool) (Written, error) {
	if err := os.MkdirAll(directory, dirMode); err != nil {
		return Written{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Written{}, err
	}
	defer root.Close()
	names := []string{ComposeName, EnvName, EnvExampleName, DeployName}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	names = slices.DeleteFunc(names, func(name string) bool {
		_, present := files[name]
		return !present
	})
	written := Written{Directory: directory, ContainsSecret: secrets}
	if err := refuseEscapes(root, names); err != nil {
		return written, err
	}
	if !overwrite {
		if err := refuseExisting(root, names); err != nil {
			return written, err
		}
	}
	unlock, err := lockFolder(root)
	if err != nil {
		return written, err
	}
	defer unlock()
	staged, err := stage(root, names, files)
	if err == nil {
		err = commit(root, names, staged, overwrite)
	}
	if cleanupErr := removeStaged(root, staged); err != nil || cleanupErr != nil {
		return written, errors.Join(err, cleanupErr)
	}
	for _, name := range names {
		written.Files = append(written.Files, filepath.Join(directory, filepath.FromSlash(name)))
	}
	return written, nil
}

// placed is a file a commit put in place, with the hard link that keeps
// the one it replaced, if any, until the whole bundle is in.
type placed struct {
	name   string
	backup string
}

// lockFolder holds the destination for one save at a time, so two saves
// to one folder never interleave their files.
func lockFolder(root *os.Root) (func(), error) {
	folder, err := root.Open(currentFolder)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(folder.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // G115: a file descriptor fits an int
		return nil, errors.Join(err, folder.Close())
	}
	return func() { _ = folder.Close() }, nil
}

// stage writes every file of the bundle in full beside where it goes,
// before any is put in place, so a full disk or a folder that cannot be
// made fails the save with the destination unchanged.
func stage(root *os.Root, names []string, files map[string]file) (map[string]string, error) {
	staged := map[string]string{}
	for _, name := range names {
		if err := root.MkdirAll(path.Dir(name), dirMode); err != nil {
			return staged, err
		}
		temporaryName, err := atomicfile.StageIn(root, name, files[name].content, files[name].mode)
		if err != nil {
			return staged, err
		}
		staged[name] = temporaryName
	}
	return staged, nil
}

// commit puts every staged file in place, or none: a file that appeared
// since the check is never replaced without overwrite, and when one cannot
// be placed every file put in before it is put back as it was.
func commit(root *os.Root, names []string, staged map[string]string, overwrite bool) error {
	done := make([]placed, 0, len(names))
	for _, name := range names {
		backup, err := place(root, name, staged[name], overwrite)
		if err != nil {
			if rollbackErr := rollback(root, done); rollbackErr != nil {
				return fmt.Errorf("%w: %w; putting the earlier files back: %w", ErrPartial, err, rollbackErr)
			}
			return fmt.Errorf("nothing in the folder was changed: %w", err)
		}
		done = append(done, placed{name: name, backup: backup})
	}
	var cleanupErr error
	for _, file := range done {
		if file.backup != "" {
			cleanupErr = errors.Join(cleanupErr, root.Remove(file.backup))
		}
		cleanupErr = errors.Join(cleanupErr, atomicfile.SyncIn(root, file.name))
	}
	return cleanupErr
}

// place puts one staged file at name. Without overwrite it is linked
// there, which fails rather than replace a file; with overwrite a file
// already there is first kept under a backup name, returned.
func place(root *os.Root, name, temporaryName string, overwrite bool) (string, error) {
	if !overwrite {
		err := root.Link(temporaryName, name)
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s: %w", name, ErrExists)
		}
		return "", err
	}
	backup := ""
	if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() {
		backup = name + backupMarker + rand.Text()
		if err := root.Link(name, backup); err != nil {
			return "", err
		}
	}
	if err := root.Rename(temporaryName, name); err != nil {
		if backup != "" {
			err = errors.Join(err, root.Remove(backup))
		}
		return "", err
	}
	return backup, nil
}

// rollback puts back, newest first, what a failed commit replaced, and
// removes what it added.
func rollback(root *os.Root, done []placed) error {
	var err error
	for _, file := range slices.Backward(done) {
		if file.backup != "" {
			err = errors.Join(err, root.Rename(file.backup, file.name))
			continue
		}
		err = errors.Join(err, root.Remove(file.name))
	}
	return err
}

// removeStaged removes the temporary files a save leaves: a link put them
// in place, or the save failed before they were.
func removeStaged(root *os.Root, staged map[string]string) error {
	var err error
	for _, temporaryName := range staged {
		if removeErr := root.Remove(temporaryName); !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

// refuseEscapes checks, before anything is written, that every folder on
// the way to each file either does not exist yet or resolves inside root: a
// symbolic link there, dangling or leading out, refuses the export, which
// leaves the folder as it was.
func refuseEscapes(root *os.Root, names []string) error {
	for _, name := range names {
		folder := ""
		for part := range strings.SplitSeq(path.Dir(name), "/") {
			folder = path.Join(folder, part)
			info, err := root.Lstat(folder)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return fmt.Errorf("%s: %w", folder, err)
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			if _, err := root.Stat(folder); err != nil {
				return fmt.Errorf("%s is a link that leads outside %s: %w", folder, root.Name(), err)
			}
		}
	}
	return nil
}

// refuseExisting checks every file an export would write before it writes
// any, so a refused export leaves the folder as it was.
func refuseExisting(root *os.Root, names []string) error {
	for _, name := range names {
		_, err := root.Lstat(name)
		if err == nil {
			return fmt.Errorf("%s: %w", name, ErrExists)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
