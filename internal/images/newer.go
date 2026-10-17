package images

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	versionSeparator = "."
	noBuild          = "0"
)

var (
	// unstableTag marks tags that follow development builds; a documented
	// stable tag is preferred over them.
	unstableTag = regexp.MustCompile(`(?i)(^|[._-])(nightly|dev|develop|edge|beta|alpha|rc[0-9]*|canary|unstable|snapshot|preview|main|master)([._-]|$)`)
	// versionTag splits a tag such as v2.2.3-alpine into its prefix, its
	// dotted numbers and its suffix.
	versionTag = regexp.MustCompile(`^([^0-9]*)([0-9]+(?:\.[0-9]+)*)(.*)$`)
	// buildNumber ends a suffix such as -ls45 or -r5, counted up on each
	// rebuild. Other trailing digits, as in -php8 or -alpine3.19, name a
	// variant and are compared as written.
	buildNumber = regexp.MustCompile(`(?i)[._-](?:ls|r)([0-9]+)$`)
)

// Unstable reports whether a tag follows development builds.
func Unstable(tag string) bool {
	return unstableTag.MatchString(tag)
}

// Versioned reports whether a tag is written as a dotted version, the only
// kind Newer finds newer tags for.
func Versioned(tag string) bool {
	_, isVersion := parseTagVersion(tag)
	return isVersion
}

// tagVersion is a tag read as a dotted version. Its numbers end with the
// suffix's build number, 0 when the suffix has none.
type tagVersion struct {
	prefix  string
	numbers []int
	suffix  string
}

func parseTagVersion(tag string) (tagVersion, bool) {
	match := versionTag.FindStringSubmatch(tag)
	if match == nil {
		return tagVersion{}, false
	}
	suffix, build := match[3], noBuild
	if found := buildNumber.FindStringSubmatchIndex(suffix); found != nil {
		suffix, build = match[3][:found[2]], match[3][found[2]:]
	}
	parts := strings.Split(match[2], versionSeparator)
	numbers := make([]int, 0, len(parts)+1)
	for _, part := range append(parts, build) {
		number, err := strconv.Atoi(part)
		if err != nil {
			return tagVersion{}, false
		}
		numbers = append(numbers, number)
	}
	return tagVersion{prefix: match[1], numbers: numbers, suffix: suffix}, true
}

// sameShape is a tag written like the pinned one: the same prefix, as many
// numbers, the same suffix apart from its build number and the same major
// version.
func (version tagVersion) sameShape(pinned tagVersion) bool {
	return version.prefix == pinned.prefix && version.suffix == pinned.suffix && len(version.numbers) == len(pinned.numbers) &&
		version.numbers[0] == pinned.numbers[0]
}

// Newer are the stable tags of the pinned tag's family that are newer than
// it, oldest first: for 2.2.3, the 2.2.x and 2.x tags written the same way.
func Newer(pinned string, tags []string) []string {
	current, isVersion := parseTagVersion(pinned)
	if !isVersion {
		return nil
	}
	type candidate struct {
		tag     string
		version tagVersion
	}
	var candidates []candidate
	for _, tag := range tags {
		version, isVersion := parseTagVersion(tag)
		newer := isVersion && version.sameShape(current) && slices.Compare(version.numbers, current.numbers) > 0
		if newer && !Unstable(tag) {
			candidates = append(candidates, candidate{tag: tag, version: version})
		}
	}
	slices.SortFunc(candidates, func(left, right candidate) int { return slices.Compare(left.version.numbers, right.version.numbers) })
	newer := make([]string, 0, len(candidates))
	for _, found := range candidates {
		newer = append(newer, found.tag)
	}
	return newer
}
