package rules

import (
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
)

const (
	maxOptionalDescription = 300
	defaultNote            = "Default: "
	quoteCharacters        = "`\"'"
)

var (
	settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// tableSettingName is what a README or docs table row must look like to
	// be a setting: UPPER_SNAKE with an underscore, unlike AVI or C7401.
	tableSettingName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)
	// notDocumentation holds variables for tests, CI or a development setup,
	// not for the deployed app.
	notDocumentation = regexp.MustCompile(`(?i)(^|/)(tests?|e2e|spec|fixtures?|\.github|\.gitlab|ci)/|(^|/)\.env\.(test|testing|ci|dev|development|local)$`)
	// noDefault are table cells that mean there is no default.
	noDefault = []string{"", "-", "none", "not set", "unset", "empty", "n/a", "null"}
)

// optionalSettings lists, for the app, the variables its upstream documents
// in a .env sample or a README or docs table and the plan does not set, so
// .env can show them commented out and compose.yaml can pass them through.
func (builder *builder) optionalSettings() {
	app := builder.plan.AppService()
	if app == nil {
		return
	}
	used := usedVariables(builder.plan)
	for _, variable := range builder.facts.Variables {
		mention, documented := documentation(variable)
		if used[variable.Name] || !documented {
			continue
		}
		value, prose := optionalDefault(variable.Name, mention.Default)
		description := optionalDescription(firstNonEmpty(mention.Text, variable.Description), prose)
		builder.plan.OptionalEnv = append(builder.plan.OptionalEnv, ir.OptionalSetting{Service: app.Name, Key: variable.Name, Default: value,
			Description: description, Provenance: provenance(mentionSource(mention), mention.Location, ir.ConfidenceMedium)})
	}
}

// documentation picks the mention that documents a variable as a setting: a
// .env sample entry, else a README or docs table or list with a description.
func documentation(variable extract.Variable) (extract.Mention, bool) {
	if !settingName.MatchString(variable.Name) {
		return extract.Mention{}, false
	}
	for _, mention := range variable.Mentions {
		if mention.Kind == extract.KindEnvSample && !notDocumentation.MatchString(mention.Location.File) {
			return mention, true
		}
	}
	for _, mention := range variable.Mentions {
		described := mention.Kind == extract.KindReadme && mention.Text != "" && tableSettingName.MatchString(variable.Name)
		if described && !notDocumentation.MatchString(mention.Location.File) {
			return mention, true
		}
	}
	return extract.Mention{}, false
}

func usedVariables(plan *ir.IR) map[string]bool {
	used := map[string]bool{}
	for _, entry := range plan.EnvFile {
		used[entry.Key] = true
	}
	for _, service := range plan.Services {
		for _, variable := range service.Env {
			used[variable.Key], used[variable.Ref] = true, true
		}
	}
	return used
}

// optionalDefault is the value .env shows commented out. An upstream example
// secret or command is never copied, and a cell such as "not set" holds no
// value; prose, such as "the mounted socket", goes to the description instead.
func optionalDefault(name, cell string) (value, prose string) {
	value = strings.TrimSpace(strings.Trim(strings.TrimSpace(cell), quoteCharacters))
	command := strings.ContainsAny(value, "\n\r`") || strings.Contains(value, "$(")
	switch {
	case redact.IsSecretName(name), command, slices.Contains(noDefault, strings.ToLower(value)):
		return "", ""
	case strings.ContainsAny(value, " \t"):
		return "", value
	}
	return value, ""
}

func optionalDescription(text, prose string) string {
	parts := []string{strings.TrimSpace(text)}
	if prose != "" {
		parts = append(parts, defaultNote+prose)
	}
	return truncate(strings.TrimSpace(strings.Join(parts, " ")), maxOptionalDescription)
}

func mentionSource(mention extract.Mention) string {
	if mention.Kind == extract.KindEnvSample {
		return ir.SourceEnvSample
	}
	return ir.SourceReadme
}
