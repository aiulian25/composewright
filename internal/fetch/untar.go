// Package fetch turns a source (GitHub repository, local directory) into a
// private directory that holds only the files fact extraction reads.
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
	// small text files are kept, so it matches the expansion cap.
	// ponytail: the whole archive is streamed to keep a few files; fetch just
	// those through the git trees API if large downloads get too slow.
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
	var result Extracted
	var keptBytes int64
	for entries := 0; ; entries++ {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return result, fmt.Errorf("read archive: %w", err)
		}
		if entries >= maxEntries {
			return result, ErrTooLarge
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if comment := header.PAXRecords["comment"]; commitPattern.MatchString(comment) {
				result.Commit = comment
			}
			continue
		}
		name, err := relativeName(header.Name, &result.RootName)
		if err != nil {
			return result, err
		}
		if name == "" || header.Typeflag != tar.TypeReg {
			result.Skipped++
			continue
		}
		kind := Classify(name)
		if kind == Skip || header.Size > Limit(kind) {
			result.Skipped++
			continue
		}
		if len(result.Kept) >= maxKeptFiles || keptBytes+header.Size > maxKeptBytes {
			return result, ErrTooLarge
		}
		kept, err := keep(root, name, kind, archive, header.Size)
		if err != nil {
			return result, err
		}
		if !kept {
			result.Skipped++
			continue
		}
		keptBytes += header.Size
		result.Kept = append(result.Kept, name)
	}
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
		return "", fmt.Errorf("%w: %q is outside the top-level directory %q", ErrUnsafePath, name, *rootName)
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
