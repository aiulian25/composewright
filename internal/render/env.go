package render

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
)

var (
	plainValue     = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=~-]*$`)
	referenceValue = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?[-?][^}]*)?\}`)
	bareDollar     = regexp.MustCompile(`\$([^{]|$)`)
)

// Env writes the .env file: one documented entry per variable, ordered so
// every ${REFERENCE} is defined before it is used, each value quoted so
// Compose reads back exactly what was meant.
func Env(plan *ir.IR, values map[string]string, header string) ([]byte, error) {
	ordered, err := referenceOrder(plan.EnvFile)
	if err != nil {
		return nil, err
	}
	var builder strings.Builder
	builder.WriteString(header)
	for _, entry := range ordered {
		builder.WriteString("\n")
		for _, line := range commentLines(entry) {
			builder.WriteString("# " + line + "\n")
		}
		value := values[entry.Key]
		builder.WriteString(entry.Key + "=" + FormatValue(value, referenceValue.MatchString(value)) + "\n")
	}
	builder.WriteString(optionalEnv(plan))
	return []byte(builder.String()), nil
}

func commentLines(entry ir.EnvFileEntry) []string {
	var lines []string
	if entry.Description != "" {
		lines = append(lines, singleLine(entry.Description))
	}
	var facts []string
	if entry.Secret {
		facts = append(facts, "secret")
	}
	if entry.Generate != "" {
		facts = append(facts, "generated ("+entry.Generate+")")
	}
	if entry.Required {
		facts = append(facts, "required")
	}
	if entry.Provenance.SourceRef != "" {
		facts = append(facts, "from "+entry.Provenance.SourceRef)
	}
	if len(facts) > 0 {
		lines = append(lines, strings.Join(facts, " · "))
	}
	return lines
}

// FormatValue quotes a value for a Compose .env file. Literal values never
// interpolate: they are written bare when safe, else single-quoted, else
// double-quoted with escapes. Values meant to interpolate keep ${NAME}.
func FormatValue(value string, interpolate bool) string {
	if interpolate {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
		escaped = bareDollar.ReplaceAllString(escaped, `\$$$1`)
		return `"` + escaped + `"`
	}
	switch {
	case plainValue.MatchString(value):
		return value
	case !strings.ContainsAny(value, `'\`):
		return "'" + value + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\t", `\t`).Replace(value) + `"`
}

// referenceOrder sorts entries so a value that refers to ${OTHER} comes
// after OTHER, keeping the plan's order otherwise.
func referenceOrder(entries []ir.EnvFileEntry) ([]ir.EnvFileEntry, error) {
	byKey := map[string]ir.EnvFileEntry{}
	for _, entry := range entries {
		byKey[entry.Key] = entry
	}
	written := map[string]bool{}
	visiting := map[string]bool{}
	var ordered []ir.EnvFileEntry
	var visit func(entry ir.EnvFileEntry) error
	visit = func(entry ir.EnvFileEntry) error {
		if written[entry.Key] {
			return nil
		}
		if visiting[entry.Key] {
			return fmt.Errorf(".env value of %s refers back to itself", entry.Key)
		}
		visiting[entry.Key] = true
		for _, match := range referenceValue.FindAllStringSubmatch(entry.Value, -1) {
			dependency, found := byKey[match[1]]
			if !found {
				continue
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		written[entry.Key] = true
		ordered = append(ordered, entry)
		return nil
	}
	for _, entry := range entries {
		if err := visit(entry); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func singleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
