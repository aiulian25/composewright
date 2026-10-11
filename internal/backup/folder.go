package backup

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/aiulian25/composewright/internal/atomicfile"
)

const (
	KindAutomatic  = "auto"
	KindManual     = "manual"
	fileMode       = 0o600
	directoryMode  = 0o700
	nameLayout     = "20060102-150405"
	namePrefix     = "composewright-"
	nameSeparator  = "-"
	nameKindGroup  = 1
	nameTimeGroup  = 2
	storedNameRule = `^composewright-(auto|manual)-(\d{8}-\d{6})\.cwbackup$`
)

var (
	ErrNoSuchBackup = errors.New("there is no such stored backup")
	storedName      = regexp.MustCompile(storedNameRule)
)

// Stored is a backup kept in the folder.
type Stored struct {
	Name    string
	Kind    string
	Created time.Time
	Size    int64
}

// Folder is where composewright keeps its own backups: one file each, named
// by its kind and time and readable only by its owner. Names that do not
// match are never read, so no request can reach another file.
type Folder string

// List returns the stored backups, newest first.
func (folder Folder) List() ([]Stored, error) {
	entries, err := os.ReadDir(string(folder))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stored []Stored
	for _, entry := range entries {
		parts := storedName.FindStringSubmatch(entry.Name())
		if parts == nil || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		created, err := time.Parse(nameLayout, parts[nameTimeGroup])
		if err != nil {
			continue
		}
		stored = append(stored, Stored{Name: entry.Name(), Kind: parts[nameKindGroup], Created: created, Size: info.Size()})
	}
	slices.SortFunc(stored, func(left, right Stored) int {
		return cmp.Or(right.Created.Compare(left.Created), cmp.Compare(right.Name, left.Name))
	})
	return stored, nil
}

// IsStoredName reports whether name is a stored backup's name.
func IsStoredName(name string) bool {
	return storedName.MatchString(name)
}

// Write keeps a new backup of a kind made at created.
func (folder Folder) Write(kind string, created time.Time, content []byte) (Stored, error) {
	name := namePrefix + kind + nameSeparator + created.UTC().Format(nameLayout) + FileExtension
	if err := folder.Rewrite(name, content); err != nil {
		return Stored{}, err
	}
	return Stored{Name: name, Kind: kind, Created: created.UTC().Truncate(time.Second), Size: int64(len(content))}, nil
}

// Rewrite replaces the content of a stored backup, or writes it first.
func (folder Folder) Rewrite(name string, content []byte) error {
	if !storedName.MatchString(name) {
		return ErrNoSuchBackup
	}
	if err := os.MkdirAll(string(folder), directoryMode); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(string(folder), name), content, fileMode)
}

// Read returns a stored backup.
func (folder Folder) Read(name string) ([]byte, error) {
	if !storedName.MatchString(name) {
		return nil, ErrNoSuchBackup
	}
	content, err := os.ReadFile(filepath.Join(string(folder), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoSuchBackup
	}
	return content, err
}

// Delete removes a stored backup.
func (folder Folder) Delete(name string) error {
	if !storedName.MatchString(name) {
		return ErrNoSuchBackup
	}
	err := os.Remove(filepath.Join(string(folder), name))
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoSuchBackup
	}
	return err
}

// Prune deletes the oldest automatic backups beyond keep and names them.
// Manual backups stay until someone deletes them.
func (folder Folder) Prune(keep int) ([]string, error) {
	stored, err := folder.List()
	if err != nil {
		return nil, err
	}
	automatic := slices.DeleteFunc(stored, func(item Stored) bool { return item.Kind != KindAutomatic })
	var pruned []string
	for _, item := range automatic[min(keep, len(automatic)):] {
		if err := folder.Delete(item.Name); err != nil {
			return pruned, err
		}
		pruned = append(pruned, item.Name)
	}
	return pruned, nil
}

// LastAutomatic is when the newest automatic backup was made; zero when
// there is none.
func LastAutomatic(stored []Stored) time.Time {
	index := slices.IndexFunc(stored, func(item Stored) bool { return item.Kind == KindAutomatic })
	if index < 0 {
		return time.Time{}
	}
	return stored[index].Created
}
