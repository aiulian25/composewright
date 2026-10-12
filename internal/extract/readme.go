package extract

import (
	"regexp"
	"slices"
	"strings"
)

var (
	fenceLine     = regexp.MustCompile("^\\s*(```|~~~)")
	listVariable  = regexp.MustCompile("^\\s*[-*+]\\s+`?([A-Z][A-Z0-9_]{1,})`?\\s*(?:[:=–—]|\\s-\\s|\\s\\|)\\s*(.+)$")
	variableToken = regexp.MustCompile("^`?\\$?([A-Z][A-Z0-9_]+)`?$")
	// snakeName is what a table without a name column must hold to count:
	// UPPER_SNAKE with an underscore, unlike a format (AVI) or a method (GET).
	snakeName     = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$`)
	credentials   = regexp.MustCompile(`(?i)\bdefault\s+(user(name)?|login|password|credentials?|admin)\b`)
	imageInText   = regexp.MustCompile(`\b((?:ghcr\.io|docker\.io|quay\.io|lscr\.io|registry\.gitlab\.com|codeberg\.org)/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?)`)
	imageKeyLine  = regexp.MustCompile(`^\s*image:\s*["']?([^\s"'#]+)`)
	dockerPull    = regexp.MustCompile(`\bdocker\s+pull\s+([^\s\\]+)`)
	dockerRunLine = regexp.MustCompile(`\b(?:docker|podman)\s+(?:container\s+)?run\b`)
	yamlFileName  = regexp.MustCompile(`[A-Za-z0-9_.-]+\.ya?ml\b`)
	proseLine     = regexp.MustCompile(`^[A-Za-z*_"'(]`)
	markdownLink  = regexp.MustCompile(`\[\[?([^\]]*)\]\([^)]*\)\]?`)
)

// commonWords appear in upper case in READMEs without being variables.
var commonWords = []string{
	"API", "HTTP", "HTTPS", "JSON", "YAML", "URL", "URI", "MIT", "GPL", "AGPL", "BSD", "NOTE", "WARNING", "IMPORTANT",
	"TODO", "README", "FAQ", "LICENSE", "CHANGELOG", "OS", "UI", "CPU", "RAM", "SSL", "TLS", "DNS", "SQL", "CLI",
	"AND", "OR", "NOT", "THE", "YES", "NO", "TRUE", "FALSE", "ID", "IP", "TCP", "UDP", "SSO", "OIDC", "LDAP",
}

// inlineComment starts `#optional`, a comment inside a command substitution
// that the shell expands to nothing; linuxserver.io READMEs mark optional
// flags with it.
const inlineComment = "`#"

// dockerRunValueFlags take a separate value; everything else starting with a
// dash is a boolean flag or carries its value after "=".
var dockerRunValueFlags = []string{
	"-e", "--env", "-p", "--publish", "-v", "--volume", "--name", "--restart", "-w", "--workdir", "-u", "--user",
	"--env-file", "--network", "--net", "-h", "--hostname", "--mount", "-l", "--label", "--log-driver", "--log-opt",
	"-m", "--memory", "--cpus", "--device", "--cap-add", "--cap-drop", "--entrypoint", "--platform", "--pull",
	"--health-cmd", "--health-interval", "--health-retries", "--health-timeout", "--add-host", "--dns", "--tmpfs",
	"--security-opt", "--ulimit", "--shm-size", "--ip", "--mac-address", "--stop-timeout", "--group-add",
}

type markdownScan struct {
	relative     string
	mentions     []Mention
	images       []ImageMention
	dockerRuns   []DockerRun
	notes        []Note
	summary      Note
	fileMentions []Note
}

// scanMarkdown mines a README or docs page: docker run commands, image names,
// tables and lists that name UPPER_SNAKE variables, and sentences about
// default credentials.
func scanMarkdown(relative string, content []byte) markdownScan {
	scan := markdownScan{relative: relative}
	lines := strings.Split(string(content), "\n")
	inFence := false
	var header []string
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		at := Location{File: relative, Line: index + 1}
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		scan.imagesIn(line, at)
		for _, name := range yamlFileName.FindAllString(line, -1) {
			scan.fileMentions = append(scan.fileMentions, Note{Text: name, Location: at})
		}
		if dockerRunLine.MatchString(line) {
			command, last := joinContinued(lines, index)
			scan.dockerRun(command, at)
			index = last
			continue
		}
		if inFence {
			continue
		}
		header = scan.tableRow(line, header, at)
		if match := listVariable.FindStringSubmatch(line); match != nil && isVariableName(match[1]) {
			scan.mentions = append(scan.mentions, Mention{Name: match[1], Kind: KindReadme, Text: cleanCell(match[2]), Location: at})
		}
		if scan.summary.Text == "" && isProse(line) {
			scan.summary = Note{Text: plainText(line), Location: at}
		}
		if credentials.MatchString(line) {
			scan.notes = append(scan.notes, Note{Text: strings.TrimSpace(cleanCell(line)), Location: at})
		}
	}
	return scan
}

func (scan *markdownScan) imagesIn(line string, at Location) {
	for _, match := range imageInText.FindAllStringSubmatch(line, -1) {
		scan.images = append(scan.images, ImageMention{Reference: match[1], Kind: KindReadme, Location: at})
	}
	for _, pattern := range []*regexp.Regexp{dockerPull, imageKeyLine} {
		if match := pattern.FindStringSubmatch(line); match != nil && !strings.Contains(match[1], "$") {
			scan.images = append(scan.images, ImageMention{Reference: match[1], Kind: KindReadme, Location: at})
		}
	}
}

// tableRow records variables from markdown tables. The header row decides
// which column holds the name, the description and the default; without a
// name column the first one is read, for UPPER_SNAKE names only.
func (scan *markdownScan) tableRow(line string, header []string, at Location) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return nil
	}
	cells := splitRow(trimmed)
	if isSeparatorRow(cells) {
		return header
	}
	if header == nil {
		return lowerAll(cells)
	}
	nameColumn := columnIndex(header, []string{"variable", "name", "env", "key", "option", "setting", "parameter"}, -1)
	named := nameColumn >= 0
	nameColumn = max(nameColumn, 0)
	descriptionColumn := columnIndex(header, []string{"description", "meaning", "purpose", "explanation"}, len(cells)-1)
	defaultColumn := columnIndex(header, []string{"default"}, -1)
	if nameColumn >= len(cells) {
		return header
	}
	match := variableToken.FindStringSubmatch(strings.TrimSpace(cells[nameColumn]))
	if match == nil || !isVariableName(match[1]) || (!named && !snakeName.MatchString(match[1])) {
		return header
	}
	mention := Mention{Name: match[1], Kind: KindReadme, Location: at}
	if descriptionColumn >= 0 && descriptionColumn < len(cells) && descriptionColumn != nameColumn {
		mention.Text = cleanCell(cells[descriptionColumn])
	}
	if defaultColumn >= 0 && defaultColumn < len(cells) {
		mention.Default = cleanCell(cells[defaultColumn])
	}
	scan.mentions = append(scan.mentions, mention)
	return header
}

// dockerRun parses one `docker run` command into image, ports, volumes and
// environment.
func (scan *markdownScan) dockerRun(command string, at Location) {
	fields := shellFields(command[dockerRunLine.FindStringIndex(command)[1]:])
	run := DockerRun{Location: at}
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if strings.HasPrefix(field, inlineComment) {
			continue
		}
		if !strings.HasPrefix(field, "-") {
			run.Image = field
			break
		}
		flag, value, inline := strings.Cut(field, "=")
		if !inline && slices.Contains(dockerRunValueFlags, flag) && index+1 < len(fields) {
			index++
			value = fields[index]
		}
		switch flag {
		case "-e", "--env":
			key, setting, _ := strings.Cut(value, "=")
			run.Env = append(run.Env, KeyValue{Key: key, Value: setting, Location: at})
			scan.mentions = append(scan.mentions, Mention{Name: key, Kind: KindReadme, Default: setting, Location: at})
		case "-p", "--publish":
			run.Ports = append(run.Ports, value)
		case "-v", "--volume":
			run.Volumes = append(run.Volumes, value)
		}
	}
	if run.Image == "" || strings.Contains(run.Image, "$") {
		return
	}
	scan.dockerRuns = append(scan.dockerRuns, run)
	scan.images = append(scan.images, ImageMention{Reference: run.Image, Kind: KindReadme, Location: at})
}

func joinContinued(lines []string, index int) (string, int) {
	command := strings.TrimSpace(lines[index])
	for strings.HasSuffix(command, continuation) && index+1 < len(lines) {
		index++
		command = strings.TrimSuffix(command, continuation) + " " + strings.TrimSpace(lines[index])
	}
	return command, index
}

func splitRow(row string) []string {
	row = strings.Trim(strings.TrimSpace(row), "|")
	cells := strings.Split(row, "|")
	for index := range cells {
		cells[index] = strings.TrimSpace(cells[index])
	}
	return cells
}

func isSeparatorRow(cells []string) bool {
	return !slices.ContainsFunc(cells, func(cell string) bool { return strings.Trim(cell, ":- ") != "" })
}

func columnIndex(header, names []string, fallback int) int {
	for index, cell := range header {
		for _, name := range names {
			if strings.Contains(cell, name) {
				return index
			}
		}
	}
	return fallback
}

func lowerAll(cells []string) []string {
	lowered := make([]string, len(cells))
	for index, cell := range cells {
		lowered[index] = strings.ToLower(cell)
	}
	return lowered
}

func cleanCell(text string) string {
	return strings.TrimSpace(strings.NewReplacer("`", "", "**", "", "<br>", " ", "<br/>", " ").Replace(text))
}

func isVariableName(name string) bool {
	return len(name) >= 2 && !slices.Contains(commonWords, name)
}

// isProse picks the first sentence-like line of a README: not a heading,
// badge, image, HTML tag, table row or list item.
func isProse(line string) bool {
	trimmed := strings.TrimSpace(line)
	const minimumLength = 20
	return len(trimmed) >= minimumLength && proseLine.MatchString(trimmed) && !strings.HasPrefix(trimmed, "* ")
}

// plainText drops Markdown links (keeping their text), emphasis and code
// marks, so a README sentence reads cleanly as a summary.
func plainText(line string) string {
	text := markdownLink.ReplaceAllString(strings.TrimSpace(line), "$1")
	text = strings.NewReplacer("**", "", "__", "", "`", "").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}
