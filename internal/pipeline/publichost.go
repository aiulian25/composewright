package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// maxHostLength is the longest host name DNS allows.
	maxHostLength = 253
	pathSeparator = "/"
	// publicHostProblem is shown on the Review page and by gen alike.
	publicHostProblem = "the public address %q must be a host name with an optional path, as rss.example.com or home.example.com/rss"
)

// publicHostPattern is a host name of lowercase DNS labels with an optional
// path, as rss.example.com or home.example.com/rss.
var publicHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(/[A-Za-z0-9._~-]+)*$`)

// dotSegments name the current or parent folder, which a browser resolves
// away, so a proxy rule under one never matches.
var dotSegments = []string{".", ".."}

// TidyPublicHost tidies a typed public address: spaces and a trailing
// slash trimmed and the host, which is case-insensitive, lowercased; the
// path stays as typed.
func TidyPublicHost(text string) string {
	host, path, found := strings.Cut(strings.TrimRight(strings.TrimSpace(text), pathSeparator), pathSeparator)
	if !found {
		return strings.ToLower(host)
	}
	return strings.ToLower(host) + pathSeparator + path
}

// validPublicHost accepts a blank address, which keeps the derived one.
func validPublicHost(text string) error {
	if text == "" {
		return nil
	}
	host, path, _ := strings.Cut(text, pathSeparator)
	segments := strings.Split(path, pathSeparator)
	dotted := slices.ContainsFunc(segments, func(segment string) bool { return slices.Contains(dotSegments, segment) })
	if publicHostPattern.MatchString(text) && len(host) <= maxHostLength && !dotted {
		return nil
	}
	return fmt.Errorf(publicHostProblem, text)
}
