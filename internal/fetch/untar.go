// Package fetch turns a source (GitHub repository, local directory, local
// .zip or .tar.gz archive) into a private directory that holds only the
// files fact extraction reads.
package fetch

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// maxExpandedBytes caps what the decompressor may produce, so a small
	// archive cannot expand without bound.
	maxExpandedBytes = 1 << 30
	// MaxArchiveBytes caps the compressed download. Apps that ship sample
	// media or screenshots run to hundreds of megabytes although only their
	// small text files are kept, so it matches the expansion cap. A GitHub
	// repository past it is fetched file by file through the trees API.
	MaxArchiveBytes = maxExpandedBytes
	maxEntries      = 200000
	maxKeptFiles    = 5000
	maxKeptBytes    = 50 << 20
	keptFileMode    = 0o600
	keptDirMode     = 0o700
)

var (
	ErrTooLarge   = errors.New("archive exceeds the size or file-count limits")
	ErrUnsafePath = errors.New("archive entry has an unsafe path")
	// ErrSeveralFolders is a harmless archive packed from inside the
	// repository folder, or of several folders.
	ErrSeveralFolders = errors.New("the archive should hold one folder with the repository in it")
)

// Extracted describes the result of unpacking an archive.
type Extracted struct {
	RootName string   `json:"root_name"`
	Commit   string   `json:"commit"`
	Kept     []string `json:"kept"`
	Skipped  int      `json:"skipped"`
}

// git archive (and therefore GitHub) records the commit id in the pax global
// header comment.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Untar unpacks a gzip-compressed tarball into destination, which must be an
// empty directory. Only regular files that Classify keeps are written, through
// os.Root so no entry can land outside destination. The single top-level
// directory GitHub adds (owner-repo-sha/) is stripped and reported.
func Untar(reader io.Reader, destination string) (Extracted, error) {
	return untar(reader, destination, MaxArchiveBytes)
}

// untar fails with ErrTooLarge once more than maxDownload compressed bytes
// were read; a reader that merely stopped there would surface as a truncated
// archive ("unexpected EOF").
func untar(reader io.Reader, destination string, maxDownload int64) (Extracted, error) {
	compressed, err := gzip.NewReader(&countingReader{reader: reader, limit: maxDownload})
	if err != nil {
		return Extracted{}, fmt.Errorf("not a gzip archive: %w", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return Extracted{}, err
	}
	defer root.Close()
	expanded := &countingReader{reader: compressed, limit: maxExpandedBytes}
	archive := tar.NewReader(expanded)
	unpacking := &extraction{root: root}
	for entries := 0; ; entries++ {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return unpacking.result, nil
		}
		if err != nil {
			return unpacking.result, fmt.Errorf("read archive: %w", err)
		}
		if entries >= maxEntries {
			return unpacking.result, ErrTooLarge
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if comment := header.PAXRecords["comment"]; commitPattern.MatchString(comment) {
				unpacking.result.Commit = comment
			}
			continue
		}
		content := func() (io.ReadCloser, error) { return io.NopCloser(archive), nil }
		regular, directory := header.Typeflag == tar.TypeReg, header.Typeflag == tar.TypeDir
		if err := unpacking.add(header.Name, directory, regular, header.Size, content); err != nil {
			return unpacking.result, err
		}
	}
}

// extraction keeps the entries of one archive that Classify wants, within
// the kept-file limits.
type extraction struct {
	root      *os.Root
	result    Extracted
	keptBytes int64
}

// add checks one entry's name and keeps it if it is a regular file worth
// reading; open is called only for a file that is read. Directories are
// not counted as skipped files.
func (unpacking *extraction) add(entryName string, directory, regular bool, size int64, open func() (io.ReadCloser, error)) error {
	name, err := relativeName(entryName, &unpacking.result.RootName)
	if err != nil || directory {
		return err
	}
	if name == "" || !regular {
		unpacking.result.Skipped++
		return nil
	}
	kind := Classify(name)
	if kind == Skip || size > Limit(kind) {
		unpacking.result.Skipped++
		return nil
	}
	if len(unpacking.result.Kept) >= maxKeptFiles || unpacking.keptBytes+size > maxKeptBytes {
		return ErrTooLarge
	}
	content, err := open()
	if err != nil {
		return err
	}
	kept, err := keep(unpacking.root, name, kind, content, size)
	if err = errors.Join(err, content.Close()); err != nil || !kept {
		unpacking.result.Skipped++
		return err
	}
	unpacking.keptBytes += size
	unpacking.result.Kept = append(unpacking.result.Kept, name)
	return nil
}

// relativeName strips the archive's top-level directory and rejects any name
// that could escape the destination.
func relativeName(name string, rootName *string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	top, rest, _ := strings.Cut(name, "/")
	if top == "" || top == ".." || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}
	if *rootName == "" {
		*rootName = top
	}
	if top != *rootName {
		return "", fmt.Errorf("%w: %q is outside %q", ErrSeveralFolders, name, *rootName)
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", nil
	}
	if !filepath.IsLocal(rest) || strings.Contains(rest, "\\") {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}
	return path.Clean(rest), nil
}

func keep(root *os.Root, name string, kind Kind, source io.Reader, size int64) (bool, error) {
	content, err := io.ReadAll(io.LimitReader(source, size))
	if err != nil {
		return false, err
	}
	if kind == Code && !EnvReference.Match(content) {
		return false, nil
	}
	if err := root.MkdirAll(path.Dir(name), keptDirMode); err != nil {
		return false, err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keptFileMode)
	if err != nil {
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	_, err = file.Write(content)
	return true, errors.Join(err, file.Close())
}

type countingReader struct {
	reader io.Reader
	read   int64
	limit  int64
}

func (counter *countingReader) Read(buffer []byte) (int, error) {
	count, err := counter.reader.Read(buffer)
	counter.read += int64(count)
	if counter.read > counter.limit {
		return count, ErrTooLarge
	}
	return count, err
}
