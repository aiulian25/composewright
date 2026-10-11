package web

import (
	"regexp"
	"strings"
)

// Token classes; app.css colours them with the code-pane tokens.
const (
	tokenComment = "c"
	tokenKey     = "k"
	tokenName    = "n"
	tokenValue   = "v"
	tokenPlain   = ""

	languageYAML     = "yaml"
	languageEnv      = "env"
	languageMarkdown = "markdown"

	nameIndent = "  "
)

type segment struct {
	Class string
	Text  string
}

type codeLine []segment

var (
	yamlKey      = regexp.MustCompile(`^(\s*)(- )?([A-Za-z0-9_.\-]+)(:)(.*)$`)
	yamlListItem = regexp.MustCompile(`^(\s*- )(.*)$`)
	envAssign    = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(=)(.*)$`)
	namedSection = map[string]bool{"services": true, "volumes": true, "networks": true, "secrets": true}
)

// highlight splits a file into coloured segments. It is a display aid that
// never changes the text: joining the segments gives the file back.
func highlight(text, language string) []codeLine {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	result := make([]codeLine, 0, len(lines))
	section := ""
	for _, line := range lines {
		switch language {
		case languageYAML:
			result = append(result, yamlLine(line, &section))
		case languageEnv:
			result = append(result, envLine(line))
		default:
			result = append(result, markdownLine(line))
		}
	}
	return result
}

func yamlLine(line string, section *string) codeLine {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return codeLine{{Class: tokenComment, Text: line}}
	}
	if match := yamlKey.FindStringSubmatch(line); match != nil {
		indent, dash, key, colon, rest := match[1], match[2], match[3], match[4], match[5]
		class := tokenKey
		if indent == "" {
			*section = key
		}
		if indent == nameIndent && dash == "" && namedSection[*section] {
			class = tokenName
		}
		return codeLine{{Text: indent + dash}, {Class: class, Text: key}, {Text: colon}, {Class: valueClass(rest), Text: rest}}
	}
	if match := yamlListItem.FindStringSubmatch(line); match != nil {
		return codeLine{{Text: match[1]}, {Class: tokenValue, Text: match[2]}}
	}
	return codeLine{{Text: line}}
}

func valueClass(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.Contains(trimmed, "${") || strings.HasPrefix(trimmed, `"`) {
		return tokenValue
	}
	return tokenPlain
}

func envLine(line string) codeLine {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return codeLine{{Class: tokenComment, Text: line}}
	}
	if match := envAssign.FindStringSubmatch(line); match != nil {
		return codeLine{{Class: tokenKey, Text: match[1]}, {Text: match[2]}, {Class: tokenValue, Text: match[3]}}
	}
	return codeLine{{Text: line}}
}

func markdownLine(line string) codeLine {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "#"):
		return codeLine{{Class: tokenName, Text: line}}
	case strings.HasPrefix(trimmed, "```"), strings.HasPrefix(line, "    "):
		return codeLine{{Class: tokenValue, Text: line}}
	}
	return codeLine{{Text: line}}
}
