package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/rules"
)

const (
	catalogueExtension = ".yaml"
	// maxCatalogueBytes caps one override file, so a stray large file is
	// reported instead of read.
	maxCatalogueBytes = 256 << 10
	problemSeparator  = ": "
)

var (
	errNotAFile = errors.New("not a regular file")
	errTooLarge = fmt.Errorf("larger than %d KiB", maxCatalogueBytes>>10)
)

// LoadCatalogue is the rules catalogue with the override files of the data
// directory's catalogue.d folder merged over it. Each of its problems names
// a file left out.
func LoadCatalogue(paths config.Paths) *rules.Catalogue {
	files, problems := CatalogueFiles(paths.CatalogueDir())
	catalogue := rules.LoadCatalogue(files)
	catalogue.Problems = append(problems, catalogue.Problems...)
	slices.Sort(catalogue.Problems)
	return catalogue
}

// CatalogueFiles reads the override files of a catalogue.d folder by name:
// each .yaml file in it up to maxCatalogueBytes. Problems name those that
// cannot be read; a folder that does not exist holds none.
func CatalogueFiles(directory string) (map[string][]byte, []string) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{err.Error()}
	}
	files, problems := map[string][]byte{}, []string(nil)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, catalogueExtension) {
			continue
		}
		content, err := readCatalogueFile(filepath.Join(directory, name))
		if err != nil {
			problems = append(problems, name+problemSeparator+err.Error())
			continue
		}
		files[name] = content
	}
	return files, problems
}

func readCatalogueFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, errNotAFile
	case info.Size() > maxCatalogueBytes:
		return nil, errTooLarge
	}
	return os.ReadFile(path)
}

// ValidCatalogueFile reports whether an override file can be written back
// as CatalogueFiles reads it: a .yaml name right in the folder, and no
// larger than it reads.
func ValidCatalogueFile(name string, content []byte) bool {
	inFolder := filepath.Base(name) == name && filepath.IsLocal(name)
	return inFolder && strings.HasSuffix(name, catalogueExtension) && len(content) <= maxCatalogueBytes
}

// WriteCatalogueFiles writes override files into a catalogue.d folder,
// readable only by composewright, in place of any of the same name.
func WriteCatalogueFiles(directory string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	if err := os.MkdirAll(directory, config.DirMode); err != nil {
		return err
	}
	for name, content := range files {
		if !ValidCatalogueFile(name, content) {
			return fmt.Errorf("catalogue override %q cannot be written", name)
		}
		if err := atomicfile.Write(filepath.Join(directory, name), content, config.FileMode); err != nil {
			return err
		}
	}
	return nil
}
