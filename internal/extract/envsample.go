package extract

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
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
	bareEnvKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
	sample, _ := readEnvSample(relative, content)
	return sample
}

// readEnvSample separates lexical evidence from semantic values. Lookups
// use only earlier active assignments in this file, never process variables.
// Invalid statements are omitted and diagnosed without including their text.
func readEnvSample(relative string, content []byte) (EnvSample, []string) {
	lines := strings.Split(strings.TrimPrefix(string(content), "\ufeff"), "\n")
	entries := make([][]EnvSampleEntry, len(lines))
	continuations := make([]bool, len(lines))
	values := map[string]string{}
	var problems []string
	for index := 0; index < len(lines); index++ {
		first := index
		raw := strings.TrimSpace(lines[index])
		optional := false
		optionalComment := ""
		statement := lines[index]
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "#") {
			entry, ok := sampleEntry(raw)
			if !ok || !entry.Commented {
				continue
			}
			optional = true
			statement = strings.TrimSpace(strings.TrimLeft(raw, "#"))
			optionalComment = dotenvComment(statement)
		}
		if !optional && !assignmentLine.MatchString(raw) && !bareEnvKey.MatchString(raw) {
			problems = append(problems, fmt.Sprintf("%s:%d: invalid dotenv statement", relative, first+1))
			continue
		}
		// Find a quote span once in the remaining input, matching Compose's
		// escaped-quote handling. Continuation lines are never parsed as entries.
		if offset := strings.IndexAny(statement, "=:"); offset >= 0 {
			value := strings.TrimLeft(statement[offset+1:], " \t\r")
			if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
				quote := value[0]
				escaped, closed := false, false
				for !closed {
					for i := 1; i < len(value); i++ {
						c := value[i]
						if c == quote && !escaped {
							closed = true
							break
						}
						if c == '\\' && !escaped {
							escaped = true
						} else {
							escaped = false
						}
					}
					if closed || optional || index+1 == len(lines) {
						break
					}
					index++
					continuations[index] = true
					escaped = false // A physical newline ends an escape pair.
					value = "\n" + lines[index]
				}
			}
		}
		if index > first {
			statement = strings.Join(lines[first:index+1], "\n")
		}
		// Comments on optional example assignments are lexical evidence;
		// they are not part of an active Compose assignment.
		if optional {
			if offset := strings.IndexAny(statement, "=:"); offset >= 0 {
				value := statement[offset+1:]
				trimmed := strings.TrimSpace(value)
				if len(trimmed) == 0 || (trimmed[0] != '\'' && trimmed[0] != '"') {
					if start := valueComment.FindStringIndex(value); start != nil {
						statement = statement[:offset+1] + value[:start[0]]
					}
				}
			}
		}
		lookup := func(name string) (string, bool) { v, ok := values[name]; return v, ok }
		parsed, err := dotenv.ParseWithLookup(strings.NewReader(statement+"\n"), lookup)
		if err != nil {
			if !optional {
				problems = append(problems, fmt.Sprintf("%s:%d: invalid dotenv statement", relative, first+1))
			}
			continue
		}
		comment := dotenvComment(statement)
		if optional {
			comment = optionalComment
		}
		for _, key := range slices.Sorted(maps.Keys(parsed)) {
			if !assignmentLine.MatchString(key + "=") {
				if !optional {
					problems = append(problems, fmt.Sprintf("%s:%d: unsupported dotenv variable name", relative, first+1))
				}
				delete(parsed, key)
				continue
			}
			entries[first] = append(entries[first], EnvSampleEntry{Key: key, Value: parsed[key], Comment: comment, Commented: optional, Location: Location{File: relative, Line: first + 1}})
		}
		if !optional {
			maps.Copy(values, parsed)
		}
	}
	sample := EnvSample{Path: relative}
	var block []string
	for index, raw := range lines {
		if continuations[index] {
			continue
		}
		if len(entries[index]) == 0 {
			block = commentBlock(block, strings.TrimSpace(raw))
			continue
		}
		for _, entry := range entries[index] {
			next := index + 1
			for next < len(entries) && continuations[next] {
				next++
			}
			followed := next < len(entries) && len(entries[next]) > 0
			if text := strings.Join(block, " "); entry.Comment == "" && describes(text, entry.Key, followed) {
				entry.Comment = text
			}
			sample.Entries = append(sample.Entries, entry)
			block = nil
		}
	}
	return sample, problems
}

// dotenvComment locates only a comment outside the value's quoted span.
func dotenvComment(statement string) string {
	offset := strings.IndexAny(statement, "=:")
	if offset < 0 {
		return ""
	}
	value := strings.TrimLeft(statement[offset+1:], " \t\r")
	if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
		escaped := false
		for i := 1; i < len(value); i++ {
			if value[i] == value[0] && !escaped {
				return commentOf(value[i+1:])
			}
			if value[i] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		return ""
	}
	_, comment := sampleValue(statement[offset+1:])
	return comment
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
		mentions = append(mentions, Mention{Name: entry.Key, Kind: KindEnvSample, Default: entry.Value, Commented: entry.Commented, Text: entry.Comment, Location: entry.Location})
	}
	return mentions
}
