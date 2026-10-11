package fetch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	SourceLocal  = "local"
	SourceGitHub = "github"
)

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
	cleanup    func() error
}

// Close removes anything fetching created on disk.
func (source *Source) Close() error {
	if source.cleanup == nil {
		return nil
	}
	return source.cleanup()
}

// Local uses a directory the user already has; nothing is copied. upstream
// (or the checkout's origin remote) names the repository it came from, which
// decides the app name and whose image namespaces are trusted.
func Local(input, upstream string) (*Source, error) {
	directory, err := filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is neither a repository URL nor a directory: %w", ErrUnsupportedSource, input, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsupportedSource, input)
	}
	source := &Source{Kind: SourceLocal, URL: directory, Directory: directory, Repository: filepath.Base(directory)}
	if upstream == "" {
		upstream = originRemote(directory)
	}
	if owner, repository, ok := ParseGitHub(upstream); ok {
		source.Owner, source.Repository, source.URL = owner, repository, CanonicalGitHubURL(owner, repository)
	}
	return source, nil
}
