package fetch

import (
	"archive/zip"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// finderFolder is the resource-fork folder macOS Finder adds beside the
// one it compressed.
const finderFolder = "__MACOSX/"

// Unzip unpacks a zip archive into destination with the same rules as
// Untar: only regular files Classify keeps are written, through os.Root,
// and the single top-level directory is stripped and reported. A symlink
// is skipped, never followed; Finder's __MACOSX folder is ignored. GitHub
// records the commit id as the archive comment.
func Unzip(reader io.ReaderAt, size int64, destination string) (Extracted, error) {
	if size > MaxArchiveBytes {
		return Extracted{}, ErrTooLarge
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return Extracted{}, fmt.Errorf("not a zip archive: %w", err)
	}
	if len(archive.File) > maxEntries {
		return Extracted{}, ErrTooLarge
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return Extracted{}, err
	}
	defer root.Close()
	unpacking := &extraction{root: root}
	if commitPattern.MatchString(archive.Comment) {
		unpacking.result.Commit = archive.Comment
	}
	for _, file := range archive.File {
		mode := file.Mode()
		if strings.HasPrefix(file.Name, finderFolder) {
			continue
		}
		declared := int64(min(file.UncompressedSize64, math.MaxInt64))
		if err := unpacking.add(file.Name, mode.IsDir(), mode.IsRegular(), declared, file.Open); err != nil {
			return unpacking.result, err
		}
	}
	return unpacking.result, nil
}
