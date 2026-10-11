package render

import (
	"fmt"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
)

const (
	optionalComposeComment = "optional settings the upstream documents: set them in .env"
	optionalEnvHeading     = "# Optional settings the upstream documents, left unset. To use one, remove the \"# \"\n# before it; compose.yaml already passes it to the service.\n"
	optionalExamples       = 5
)

// OptionalSettings are the plan's optional settings that nothing in it sets:
// a key in .env or in a service's environment is no longer optional.
func OptionalSettings(plan *ir.IR) []ir.OptionalSetting {
	set := map[string]bool{}
	for _, entry := range plan.EnvFile {
		set[entry.Key] = true
	}
	for _, service := range plan.Services {
		for _, variable := range service.Env {
			set[variable.Key] = true
		}
	}
	var settings []ir.OptionalSetting
	for _, setting := range plan.OptionalEnv {
		if set[setting.Key] {
			continue
		}
		set[setting.Key] = true
		settings = append(settings, setting)
	}
	return settings
}

func optionalFor(plan *ir.IR, service string) []ir.OptionalSetting {
	var settings []ir.OptionalSetting
	for _, setting := range OptionalSettings(plan) {
		if setting.Service == service {
			settings = append(settings, setting)
		}
	}
	return settings
}

// optionalEnv is the .env section listing optional settings commented out.
func optionalEnv(plan *ir.IR) string {
	settings := OptionalSettings(plan)
	if len(settings) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n" + optionalEnvHeading)
	for _, setting := range settings {
		builder.WriteString("\n")
		if setting.Description != "" {
			builder.WriteString("# " + singleLine(setting.Description) + "\n")
		}
		fmt.Fprintf(&builder, "# for %s · from %s\n", setting.Service, setting.Provenance.SourceRef)
		builder.WriteString("# " + setting.Key + "=" + FormatValue(setting.Default, referenceValue.MatchString(setting.Default)) + "\n")
	}
	return builder.String()
}

func optionalSection(plan *ir.IR, write func(string, ...any)) {
	settings := OptionalSettings(plan)
	if len(settings) == 0 {
		return
	}
	names := make([]string, 0, optionalExamples)
	for _, setting := range settings[:min(len(settings), optionalExamples)] {
		names = append(names, setting.Key)
	}
	write("## Optional settings")
	write("")
	write(".env lists %d settings the upstream documents, commented out, such as %s. To use one, remove the \"# \" before it in .env and run `docker compose up -d` again.",
		len(settings), strings.Join(names, ", "))
	write("")
}
