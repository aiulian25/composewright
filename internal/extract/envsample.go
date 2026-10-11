package extract

import (
	"regexp"
	"strings"
)

var (
	assignmentLine = regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)
	commentedLine  = regexp.MustCompile(`^#+\s*(?:export\s+)?([A-Z_][A-Z0-9_]*)\s*=\s*(.*)$`)
)

// parseEnvSample reads an .env.example style file line by line. Comment lines
// directly above an entry become its description; commented-out assignments
// (# KEY=value) are optional variables.
func parseEnvSample(relative string, content []byte) EnvSample {
	sample := EnvSample{Path: relative}
	var comment []string
	for index, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		at := Location{File: relative, Line: index + 1}
		if match := assignmentLine.FindStringSubmatch(line); match != nil {
			sample.Entries = append(sample.Entries, EnvSampleEntry{Key: match[1], Value: sampleValue(match[2]), Comment: strings.Join(comment, " "), Location: at})
			comment = nil
			continue
		}
		if match := commentedLine.FindStringSubmatch(line); match != nil {
			sample.Entries = append(sample.Entries, EnvSampleEntry{Key: match[1], Value: sampleValue(match[2]), Comment: strings.Join(comment, " "), Commented: true, Location: at})
			comment = nil
			continue
		}
		text, isComment := strings.CutPrefix(line, "#")
		if !isComment {
			comment = nil
			continue
		}
		text = strings.TrimSpace(strings.TrimLeft(text, "#"))
		if text != "" && !isDecoration(text) {
			comment = append(comment, text)
		}
	}
	return sample
}

// sampleValue strips quotes and a trailing inline comment from a value.
func sampleValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
			return value[1 : end+1]
		}
	}
	if position := strings.Index(value, " #"); position >= 0 {
		value = value[:position]
	}
	return strings.TrimSpace(value)
}

func isDecoration(text string) bool {
	return strings.Trim(text, "#=-*_ ") == ""
}

func sampleMentions(sample EnvSample) []Mention {
	mentions := make([]Mention, 0, len(sample.Entries))
	for _, entry := range sample.Entries {
		mentions = append(mentions, Mention{Name: entry.Key, Kind: KindEnvSample, Default: entry.Value, Text: entry.Comment, Location: entry.Location})
	}
	return mentions
}
