package extract

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

const (
	healthcheckNone   = "NONE"
	healthcheckPrefix = "--"
	continuation      = `\`
	heredocMarker     = "<<"
)

type instruction struct {
	keyword string
	text    string
	line    int
}

// publishedStages are stage names that usually build the image a project
// publishes; without one, the last stage is used.
var publishedStages = []string{"release", "production", "prod", "runtime", "final", "app", "main"}

type stage struct {
	name  string
	facts Dockerfile
}

// parseDockerfile keeps the facts of the stage that builds the published
// image: what it exposes, declares as volumes, sets in its environment, runs
// as, and how it checks its own health. A stage built FROM an earlier stage,
// named directly or through an ARG's default, inherits that stage's facts.
func parseDockerfile(relative string, content []byte) Dockerfile {
	var stages []stage
	current := stage{facts: Dockerfile{Path: relative}}
	var args []KeyValue
	for _, step := range instructions(string(content)) {
		at := Location{File: relative, Line: step.line}
		if step.keyword == "FROM" {
			if current.facts.BaseImage != "" {
				stages = append(stages, current)
			}
			current = startStage(relative, expandArgs(step.text, args), stages)
			continue
		}
		if step.keyword == "ARG" {
			key, value, _ := strings.Cut(firstField(step.text), "=")
			args = append(args, KeyValue{Key: key, Value: unquote(value), Location: at})
			continue
		}
		applyInstruction(&current.facts, step, at)
	}
	stages = append(stages, current)
	chosen := chooseStage(stages)
	chosen.Args = args
	return chosen
}

// expandArgs replaces $NAME and ${NAME} with the default of an ARG declared
// earlier, so FROM ${FTL_SOURCE}-ftl-install finds the stage it builds on;
// a name without a default stays as written.
func expandArgs(text string, args []KeyValue) string {
	defaults := map[string]string{}
	for _, arg := range args {
		if arg.Value != "" {
			defaults[arg.Key] = arg.Value
		}
	}
	return os.Expand(text, func(name string) string {
		if value, found := defaults[name]; found {
			return value
		}
		return "${" + name + "}"
	})
}

func startStage(relative, text string, earlier []stage) stage {
	fields := strings.Fields(text)
	base, name := "", ""
	for index, field := range fields {
		if strings.HasPrefix(field, "--") {
			continue
		}
		if base == "" {
			base = field
		}
		if strings.EqualFold(field, "AS") && index+1 < len(fields) {
			name = strings.ToLower(fields[index+1])
		}
	}
	for _, parent := range earlier {
		if parent.name != "" && strings.EqualFold(parent.name, base) {
			inherited := parent.facts
			return stage{name: name, facts: inherited}
		}
	}
	return stage{name: name, facts: Dockerfile{Path: relative, BaseImage: base}}
}

func applyInstruction(facts *Dockerfile, step instruction, at Location) {
	switch step.keyword {
	case "ENV":
		facts.Env = append(facts.Env, envPairs(step.text, at)...)
	case "EXPOSE":
		facts.Exposed = append(facts.Exposed, exposed(step.text, at)...)
	case "VOLUME":
		for _, path := range listOrFields(step.text) {
			facts.Volumes = append(facts.Volumes, PathFact{Path: path, Location: at})
		}
	case "USER":
		facts.User, facts.UserAt = firstField(step.text), at
	case "HEALTHCHECK":
		facts.Healthcheck = dockerHealthcheck(step.text, at)
	case "ENTRYPOINT":
		facts.Entrypoint = execForm(step.text)
	case "CMD":
		facts.Command = execForm(step.text)
	}
}

func chooseStage(stages []stage) Dockerfile {
	for _, preferred := range publishedStages {
		for _, candidate := range stages {
			if candidate.name == preferred {
				return candidate.facts
			}
		}
	}
	return stages[len(stages)-1].facts
}

// instructions joins continuation lines, drops comments and skips heredoc
// bodies so their lines are never read as instructions.
func instructions(content string) []instruction {
	lines := strings.Split(content, "\n")
	var result []instruction
	for index := 0; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		start := index + 1
		text := trimmed
		for strings.HasSuffix(text, continuation) && index+1 < len(lines) {
			index++
			next := strings.TrimSpace(lines[index])
			if strings.HasPrefix(next, "#") {
				continue
			}
			text = strings.TrimSuffix(text, continuation) + " " + next
		}
		keyword, rest, _ := strings.Cut(text, " ")
		result = append(result, instruction{keyword: strings.ToUpper(keyword), text: strings.TrimSpace(rest), line: start})
		index = skipHeredoc(lines, index, rest)
	}
	return result
}

func skipHeredoc(lines []string, index int, text string) int {
	position := strings.Index(text, heredocMarker)
	if position < 0 {
		return index
	}
	marker := strings.TrimLeft(text[position+len(heredocMarker):], "-")
	marker = strings.Trim(firstField(marker), `"'`)
	if marker == "" {
		return index
	}
	for index+1 < len(lines) {
		index++
		if strings.TrimSpace(lines[index]) == marker {
			return index
		}
	}
	return index
}

func envPairs(text string, at Location) []KeyValue {
	fields := shellFields(text)
	if len(fields) == 0 {
		return nil
	}
	if !strings.Contains(fields[0], "=") {
		return []KeyValue{{Key: fields[0], Value: strings.Join(fields[1:], " "), Location: at}}
	}
	pairs := make([]KeyValue, 0, len(fields))
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if found {
			pairs = append(pairs, KeyValue{Key: key, Value: value, Location: at})
		}
	}
	return pairs
}

func exposed(text string, at Location) []PortFact {
	var ports []PortFact
	for _, field := range strings.Fields(text) {
		number, protocol, _ := strings.Cut(field, "/")
		port, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		if protocol == "" {
			protocol = "tcp"
		}
		ports = append(ports, PortFact{Port: port, Protocol: strings.ToLower(protocol), Location: at})
	}
	return ports
}

func dockerHealthcheck(text string, at Location) *Healthcheck {
	if strings.EqualFold(strings.TrimSpace(text), healthcheckNone) {
		return &Healthcheck{Disabled: true, Location: at}
	}
	result := &Healthcheck{Location: at}
	rest := text
	for strings.HasPrefix(rest, healthcheckPrefix) {
		option, remainder, _ := strings.Cut(rest, " ")
		name, value, _ := strings.Cut(strings.TrimPrefix(option, healthcheckPrefix), "=")
		switch name {
		case "interval":
			result.Interval = value
		case "timeout":
			result.Timeout = value
		case "start-period":
			result.StartPeriod = value
		case "retries":
			result.Retries, _ = strconv.Atoi(value)
		}
		rest = strings.TrimSpace(remainder)
	}
	keyword, command, _ := strings.Cut(rest, " ")
	if !strings.EqualFold(keyword, "CMD") {
		return nil
	}
	command = strings.TrimSpace(command)
	if exec, ok := jsonList(command); ok {
		result.Test = append([]string{"CMD"}, exec...)
		return result
	}
	result.Test = []string{"CMD-SHELL", command}
	return result
}

func execForm(text string) []string {
	if exec, ok := jsonList(text); ok {
		return exec
	}
	return []string{"/bin/sh", "-c", text}
}

func listOrFields(text string) []string {
	if list, ok := jsonList(text); ok {
		return list
	}
	return strings.Fields(text)
}

func jsonList(text string) ([]string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(text), "[") {
		return nil, false
	}
	var list []string
	err := json.Unmarshal([]byte(text), &list)
	return list, err == nil
}

// shellFields splits on whitespace while keeping quoted strings together and
// removing the quotes, which is how Docker reads ENV key="a b".
func shellFields(text string) []string {
	var fields []string
	var current strings.Builder
	var quote rune
	inField := false
	for _, character := range text {
		switch {
		case quote != 0 && character == quote:
			quote = 0
		case quote != 0:
			current.WriteRune(character)
		case character == '"' || character == '\'':
			quote, inField = character, true
		case character == ' ' || character == '\t':
			if inField {
				fields = append(fields, current.String())
				current.Reset()
				inField = false
			}
		default:
			current.WriteRune(character)
			inField = true
		}
	}
	if inField {
		fields = append(fields, current.String())
	}
	return fields
}

func firstField(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func unquote(value string) string {
	return strings.Trim(value, `"'`)
}
