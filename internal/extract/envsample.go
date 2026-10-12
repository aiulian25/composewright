package extract

import (
	"regexp"
	"strings"
)

const (
	// maxTitleWords is how short a comment above several entries must be to
	// read as a section title, such as "# Server", rather than as a
	// description of the first entry.
	maxTitleWords = 3
	// maxValueWords: a commented-out "assignment" with a longer unquoted
	// value is a sentence, such as "# ANALYTICS_ENABLED=false is honored as an alias".
	maxValueWords = 3
	commentMark   = "#"
	sentenceEnd   = "."
)

var (
	assignmentLine = regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)
	commentedLine  = regexp.MustCompile(`^#+\s*(?:export\s+)?([A-Z_][A-Z0-9_]*)\s*=(.*)$`)
	// valueComment starts an inline comment, as in Compose's own .env
	// parser: a # after a space, so KEY=#E58325 is a colour.
	valueComment = regexp.MustCompile(`\s#`)
	// sectionHeading is a decorated title such as "--- Analytics ---".
	sectionHeading = regexp.MustCompile(`^[-=*#]{2,}\s|\s[-=*#]{2,}$`)
	// aside is a parenthesis such as "(0 = unlimited)", left out when
	// counting a title's words.
	aside = regexp.MustCompile(`\([^)]*\)`)
	// leadingName is an UPPER_SNAKE name a comment starts with.
	leadingName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)
	// developmentNote is advice for upstream's own development setup, such
	// as starting docker-compose.dev.yml, which a deployed stack never uses.
	developmentNote = regexp.MustCompile(`(?i)compose\.dev\.ya?ml|\bdev(elopment)? (stack|setup|server|environment)\b`)
)

// parseEnvSample reads an .env.example style file line by line. An inline
// comment describes its entry, and so does the comment block directly
// above it, unless that block is a section title over several entries or a
// note about another variable. Commented-out assignments (# KEY=value) are
// optional variables.
func parseEnvSample(relative string, content []byte) EnvSample {
	lines := strings.Split(string(content), "\n")
	entries := make([]*EnvSampleEntry, len(lines))
	for index, raw := range lines {
		if entry, isEntry := sampleEntry(strings.TrimSpace(raw)); isEntry {
			entry.Location = Location{File: relative, Line: index + 1}
			entries[index] = &entry
		}
	}
	sample := EnvSample{Path: relative}
	var block []string
	for index, raw := range lines {
		entry := entries[index]
		if entry == nil {
			block = commentBlock(block, strings.TrimSpace(raw))
			continue
		}
		followed := index+1 < len(entries) && entries[index+1] != nil
		if text := strings.Join(block, " "); entry.Comment == "" && describes(text, entry.Key, followed) {
			entry.Comment = text
		}
		sample.Entries = append(sample.Entries, *entry)
		block = nil
	}
	return sample
}

// sampleEntry reads an assignment, or a commented-out one, with its inline
// comment. A commented line whose value reads as a sentence is prose.
func sampleEntry(line string) (EnvSampleEntry, bool) {
	if match := assignmentLine.FindStringSubmatch(line); match != nil {
		value, comment := sampleValue(match[2])
		return EnvSampleEntry{Key: match[1], Value: value, Comment: comment}, true
	}
	match := commentedLine.FindStringSubmatch(line)
	if match == nil {
		return EnvSampleEntry{}, false
	}
	value, comment := sampleValue(match[2])
	if isSentence(match[2], value) {
		return EnvSampleEntry{}, false
	}
	return EnvSampleEntry{Key: match[1], Value: value, Comment: comment, Commented: true}, true
}

// commentBlock adds a comment line to the block that may describe the next
// entry. Any other line ends the block, and so does a section heading;
// notes about upstream's development setup are left out.
func commentBlock(block []string, line string) []string {
	text, isComment := strings.CutPrefix(line, commentMark)
	if !isComment {
		return nil
	}
	text = strings.TrimSpace(strings.TrimLeft(text, commentMark))
	switch {
	case sectionHeading.MatchString(text):
		return nil
	case text == "" || isDecoration(text) || developmentNote.MatchString(text):
		return block
	}
	return append(block, text)
}

// describes decides whether the comment block above an entry is its
// description: not a short title over several entries, such as "# Cleanup",
// and not a note that starts with another variable, such as "DB_PATH
// removed in 2.0, see DATABASE_URL below" above WORKSPACE_PATH.
func describes(block, key string, followed bool) bool {
	title := followed && len(strings.Fields(aside.ReplaceAllString(block, ""))) <= maxTitleWords
	leading := leadingName.FindString(block)
	aboutAnother := leading != "" && leading != key
	return block != "" && !title && !aboutAnother
}

// sampleValue splits what follows "=" into the value, without quotes, and
// its inline comment, so "KEY=   # note" has no value and the comment "note".
func sampleValue(raw string) (value, comment string) {
	text := strings.TrimSpace(raw)
	if len(text) >= 2 && (text[0] == '"' || text[0] == '\'') {
		if end := strings.IndexByte(text[1:], text[0]); end >= 0 {
			return text[1 : end+1], commentOf(text[end+2:])
		}
	}
	if start := valueComment.FindStringIndex(raw); start != nil {
		return strings.TrimSpace(raw[:start[0]]), commentOf(raw[start[0]:])
	}
	return text, ""
}

// commentOf is the text of an inline comment, or nothing when rest is not
// one.
func commentOf(rest string) string {
	text, isComment := strings.CutPrefix(strings.TrimSpace(rest), commentMark)
	if !isComment {
		return ""
	}
	return strings.TrimSpace(strings.TrimLeft(text, commentMark))
}

// isSentence tells prose such as "# ANALYTICS_ENABLED=false is honored as
// an alias." from a commented-out setting: an unquoted value of several
// words that ends a sentence or runs long.
func isSentence(raw, value string) bool {
	raw = strings.TrimSpace(raw)
	quoted := strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, "'")
	words := len(strings.Fields(value))
	return !quoted && words > 1 && (strings.HasSuffix(value, sentenceEnd) || words > maxValueWords)
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
