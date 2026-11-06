package extract

import (
	"path"
	"slices"
	"strings"
)

// MatchingEnvSample prefers the exact requested file, including an empty
// file, to its documented example. It never merges unrelated named files.
// When only examples exist, prefer conventional suffixes, then path order.
func MatchingEnvSample(samples []EnvSample, wanted string) (EnvSample, bool) {
	for _, sample := range samples {
		if sample.Path == wanted {
			return sample, true
		}
	}
	for _, suffix := range []string{".example", ".sample", ".template", ".dist"} {
		for _, sample := range samples {
			if sample.Path == wanted+suffix {
				return sample, true
			}
		}
	}
	candidates := slices.Clone(samples)
	slices.SortFunc(candidates, func(a, b EnvSample) int { return strings.Compare(a.Path, b.Path) })
	for _, sample := range candidates {
		if DocumentsEnv(sample.Path, wanted) {
			return sample, true
		}
	}
	return EnvSample{}, false
}

// DocumentsEnv reports whether sample names an example of wanted in the
// same directory. Filename identity is retained by MatchingEnvSample.
func DocumentsEnv(sample, wanted string) bool {
	if path.Dir(sample) != path.Dir(wanted) {
		return false
	}
	name, base := path.Base(sample), path.Base(wanted)
	bare := strings.TrimPrefix(base, ".")
	return name == base || slices.ContainsFunc([]string{"example", "sample", "template", "dist"}, func(word string) bool {
		return name == base+"."+word || name == word+base || name == word+"."+bare || name == bare+"."+word
	})
}
