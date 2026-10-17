package fetch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	SourceLocal  = "local"
	SourceGitHub = "github"
)

// unpackedNote says what an archive held and what of it was kept.
const unpackedNote = "unpacked %d files, kept %d"

// zipExtension and the gzip-compressed tarballs are the archives a local
// source may be.
const (
	zipExtension     = ".zip"
	tarballExtension = ".tar.gz"
)

var archiveExtensions = []string{zipExtension, tarballExtension, ".tgz"}

var ErrUnsupportedSource = errors.New("unsupported source")

// Source is a repository made available as a directory of kept files.
type Source struct {
	Kind       string `json:"kind"`
	URL        string `json:"url"`
	Ref        string `json:"ref"`
	Commit     string `json:"commit"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Directory  string `json:"-"`
	// Note says how the source was fetched or unpacked, for the fetch step.
	Note    string `json:"-"`
	cleanup func() error
}

// Close removes anything fetching created on disk.
func (source *Source) Close() error {
	if source.cleanup == nil {
		return nil
	}
	return source.cleanup()
}

// Local uses a directory the user already has; nothing is copied. A .zip
// or .tar.gz is unpacked instead, into a private temporary directory
// removed on Close. upstream (or the checkout's origin remote) names the
// repository it came from, which decides the app name and whose image
// namespaces are trusted.
func Local(input, upstream string) (*Source, error) {
	directory, err := filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is neither a repository URL nor a directory: %w", ErrUnsupportedSource, input, err)
	}
	if !info.IsDir() && IsArchive(directory) {
		return unpackArchive(directory, upstream)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is neither a directory nor a %s or %s archive", ErrUnsupportedSource, input, zipExtension, tarballExtension)
	}
	source := &Source{Kind: SourceLocal, URL: directory, Directory: directory, Repository: filepath.Base(directory)}
	if upstream == "" {
		upstream = originRemote(directory)
	}
	source.cameFrom(upstream)
	return source, nil
}

// cameFrom names the GitHub repository upstream points at, if it does.
func (source *Source) cameFrom(upstream string) {
	if owner, repository, ok := ParseGitHub(upstream); ok {
		source.Owner, source.Repository, source.URL = owner, repository, CanonicalGitHubURL(owner, repository)
	}
}

// IsArchive reports whether name is a .zip or .tar.gz Local unpacks.
func IsArchive(name string) bool {
	return archiveExtension(name) != ""
}

func archiveExtension(name string) string {
	lower := strings.ToLower(name)
	index := slices.IndexFunc(archiveExtensions, func(extension string) bool { return strings.HasSuffix(lower, extension) })
	if index < 0 {
		return ""
	}
	return archiveExtensions[index]
}

// unpackArchive extracts a .zip or .tar.gz with the limits and path checks
// of a GitHub tarball.
func unpackArchive(path, upstream string) (*Source, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", temporaryPattern)
	if err != nil {
		return nil, err
	}
	name, extension := filepath.Base(path), archiveExtension(path)
	source := &Source{Kind: SourceLocal, URL: path, Directory: directory, Repository: name[:len(name)-len(extension)],
		cleanup: func() error { return os.RemoveAll(directory) }}
	var extracted Extracted
	if extension == zipExtension {
		extracted, err = Unzip(file, info.Size(), directory)
	} else {
		extracted, err = Untar(file, directory)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("unpack %s: %w", name, err), source.Close())
	}
	source.Commit = extracted.Commit
	source.Note = fmt.Sprintf(unpackedNote, len(extracted.Kept)+extracted.Skipped, len(extracted.Kept))
	source.cameFrom(upstream)
	return source, nil
}
