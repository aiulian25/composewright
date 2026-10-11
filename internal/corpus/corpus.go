// Package corpus reads the pinned evaluation repositories in
// testdata/corpus: each has files/ (the kept snapshot) and source.json.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/policy"
)

const (
	// verdictPass is the verdict of an entry whose rules-only plan passes the
	// policy gate; noneListed marks an empty column.
	verdictPass = "pass"
	noneListed  = "-"
	needPrefix  = "need:"
	denyPrefix  = "deny:"
	listSep     = ","
)

type Entry struct {
	Slug            string
	Directory       string
	Source          *fetch.Source
	AllowRegistries []string
	// Verdict is what is expected to block the rules-only plan, in the form
	// Blocking returns; empty when the plan passes the policy gate.
	Verdict []string
}

type sourceFile struct {
	URL             string `json:"url"`
	Owner           string `json:"owner"`
	Commit          string `json:"commit"`
	AllowRegistries string `json:"allow_registries"`
	Verdict         string `json:"verdict"`
}

// Load returns every corpus entry under root, sorted by slug.
func Load(root string) ([]Entry, error) {
	directories, err := filepath.Glob(filepath.Join(root, "*", "files"))
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(directories))
	for _, directory := range directories {
		entry, err := load(directory)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Slug < entries[right].Slug })
	return entries, nil
}

func load(directory string) (Entry, error) {
	base := filepath.Dir(directory)
	content, err := os.ReadFile(filepath.Join(base, "source.json"))
	if err != nil {
		return Entry{}, err
	}
	var described sourceFile
	if err := json.Unmarshal(content, &described); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", base, err)
	}
	owner, repository, ok := fetch.ParseGitHub(described.URL)
	if !ok {
		return Entry{}, fmt.Errorf("%s: source url %q is not a GitHub repository", base, described.URL)
	}
	entry := Entry{
		Slug:      filepath.Base(base),
		Directory: directory,
		Source: &fetch.Source{Kind: fetch.SourceLocal, URL: described.URL, Commit: described.Commit, Owner: owner,
			Repository: repository, Directory: directory},
	}
	if described.AllowRegistries != "" && described.AllowRegistries != noneListed {
		entry.AllowRegistries = strings.Split(described.AllowRegistries, listSep)
	}
	if described.Verdict != "" && described.Verdict != verdictPass {
		entry.Verdict = strings.Split(described.Verdict, listSep)
		slices.Sort(entry.Verdict)
	}
	return entry, nil
}

// Passes reports whether the entry's rules-only plan is expected to pass the
// policy gate.
func (entry Entry) Passes() bool {
	return len(entry.Verdict) == 0
}

// Blocking lists what blocks a report: need:<capability> for every
// capability it asks for and deny:<rule> for every rule it breaks, sorted
// and without repeats.
func Blocking(report policy.Report) []string {
	var blocking []string
	for _, finding := range report.Findings {
		switch finding.Severity {
		case policy.Need:
			blocking = append(blocking, needPrefix+finding.Capability)
		case policy.Deny:
			blocking = append(blocking, denyPrefix+finding.Rule)
		}
	}
	slices.Sort(blocking)
	return slices.Compact(blocking)
}
