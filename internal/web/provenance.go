package web

import (
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
)

// Badges say where a line of compose.yaml came from: a rule reading the
// repository (R1 to R6), the person's profile (P), the policy gate (G), the
// person (U) or a model (M).
const (
	badgeProfile = "P"
	badgeGate    = "G"
	badgeYou     = "U"
	badgeModel   = "M"
	toneRule     = "rule"
	toneProfile  = "profile"
	toneGate     = "gate"
	toneYou      = "you"
	keyServices  = "services"
	keyEnv       = "environment"
	keyImage     = "image"
	// imageKeyDepth is services, the service's name, image.
	imageKeyDepth = 3
	commentMark   = "#"
	rulePrefix    = "R"
	refSeparator  = ":"
)

var (
	sourceBadges = map[string]string{
		ir.SourceCompose: "R1", ir.SourceEnvSample: "R1", ir.SourceDockerfile: "R2", ir.SourceOCILabel: "R3",
		ir.SourceSidecarRule: "R4", ir.SourceHealthchecks: "R5", ir.SourceReadme: "R6", ir.SourceCode: "R6",
		ir.SourceHelm: "R6", ir.SourceCIWorkflow: "R6", ir.SourceModel: badgeModel,
		ir.SourceUser: badgeYou, ir.SourceDefault: badgeProfile,
	}
	// serviceKeyBadges are compose keys the renderer writes from the
	// profile or the gate, whatever the plan says.
	serviceKeyBadges = map[string]string{
		"container_name": badgeYou, "restart": badgeProfile, "networks": badgeProfile, "logging": badgeProfile,
		"env_file": badgeGate, "security_opt": badgeGate, "ports": badgeProfile,
	}
	hardeningKeys = []string{composefile.KeyCapDrop, composefile.KeyReadOnly, composefile.KeyTmpfs}
	topKeyBadges  = map[string]string{"name": badgeYou, "networks": badgeProfile, "volumes": badgeProfile}
	badgeTones    = map[string]string{badgeProfile: toneProfile, badgeGate: toneGate, badgeYou: toneYou, badgeModel: toneYou}
)

// ruleNames are the rules in the order they run.
var ruleNames = []struct{ Badge, Name string }{
	{"R1", "Upstream compose"}, {"R2", "Dockerfile"}, {"R3", "Image metadata"}, {"R4", "Sidecars"},
	{"R5", "Healthchecks"}, {"R6", "README and code"},
}

// ruleRow is one rule on the Analyse page: how many of the plan's fields it
// filled and where it first read one.
type ruleRow struct {
	Badge  string
	Name   string
	Result string
	Fields int
	Most   int
}

// ruleRows counts the plan's fields by the rule that filled them; filled
// is how many rules filled out of every field.
func ruleRows(plan *ir.IR) (rows []ruleRow, filled, total int) {
	counts, firsts := map[string]int{}, map[string]string{}
	fields := plan.Fields()
	for _, field := range fields {
		badge := sourceBadges[field.Source]
		if !strings.HasPrefix(badge, rulePrefix) {
			continue
		}
		filled++
		counts[badge]++
		if firsts[badge] == "" {
			firsts[badge] = field.SourceRef
		}
	}
	most := 1
	for _, count := range counts {
		most = max(most, count)
	}
	for _, rule := range ruleNames {
		rows = append(rows, ruleRow{Badge: rule.Badge, Name: rule.Name, Result: firsts[rule.Badge], Fields: counts[rule.Badge], Most: most})
	}
	return rows, filled, len(fields)
}

// fileRead is a file extraction read, badged with the rule that used it.
type fileRead struct {
	Path  string
	Badge string
}

func filesRead(files []string, plan *ir.IR) []fileRead {
	badges := ruleFiles(plan)
	read := make([]fileRead, 0, len(files))
	for _, path := range files {
		read = append(read, fileRead{Path: path, Badge: badges[path]})
	}
	return read
}

// ruleFiles maps each file a rule filled a field from to that rule.
func ruleFiles(plan *ir.IR) map[string]string {
	badges := map[string]string{}
	if plan == nil {
		return badges
	}
	for _, field := range plan.Fields() {
		file, _, _ := strings.Cut(field.SourceRef, refSeparator)
		file, _, _ = strings.Cut(file, extract.FencedLineMark)
		badge := sourceBadges[field.Source]
		if badges[file] == "" && strings.HasPrefix(badge, rulePrefix) {
			badges[file] = badge
		}
	}
	return badges
}

// badgedLine is one highlighted line of a file in the context pane, with
// the badge that says where it came from.
type badgedLine struct {
	Segments codeLine
	Badge    string
	Tone     string
	Marked   bool
	// Title is shown on hover: what the registry said about an image line.
	Title string
}

// plainLines shows a file without badges.
func plainLines(text, language string) []badgedLine {
	highlighted := highlight(text, language)
	lines := make([]badgedLine, 0, len(highlighted))
	for _, segments := range highlighted {
		lines = append(lines, badgedLine{Segments: segments})
	}
	return lines
}

// composeLines badges every line of compose.yaml by the key it belongs to.
// rendered is the plan's own rendering when the file was edited: lines it
// lacks are the person's (U). imageFacts by service title its image line
// unless the person wrote it.
func composeLines(plan *ir.IR, compose, rendered string, imageFacts map[string]string) []badgedLine {
	texts := strings.Split(strings.TrimRight(compose, "\n"), "\n")
	highlighted := highlight(compose, languageYAML)
	badges, titles := make([]string, len(texts)), make([]string, len(texts))
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &document); err == nil && len(document.Content) > 0 {
		composefile.Walk(document.Content[0], nil, func(path []string, line int) {
			if line < 1 || line > len(badges) {
				return
			}
			badges[line-1] = badgeFor(plan, path)
			if isImageKey(path) {
				titles[line-1] = imageFacts[path[1]]
			}
		})
	}
	original := map[string]bool{}
	for _, text := range strings.Split(rendered, "\n") {
		original[strings.TrimSpace(text)] = true
	}
	lines := make([]badgedLine, 0, len(texts))
	for index, text := range texts {
		line := badgedLine{Segments: highlighted[index], Badge: badges[index], Title: titles[index]}
		trimmed := strings.TrimSpace(text)
		changed := rendered != "" && trimmed != "" && !strings.HasPrefix(trimmed, commentMark) && !original[trimmed]
		// A line the person wrote may name another image than the facts.
		if changed {
			line.Badge, line.Marked, line.Title = badgeYou, true, ""
		}
		line.Tone = badgeTones[line.Badge]
		if line.Tone == "" && line.Badge != "" {
			line.Tone = toneRule
		}
		lines = append(lines, line)
	}
	return lines
}

// isImageKey is the path of a service's image: key.
func isImageKey(path []string) bool {
	return len(path) == imageKeyDepth && path[0] == keyServices && path[2] == keyImage
}

// lineText is a line of text without its indentation; "" for no line.
func lineText(texts []string, line int) string {
	if line < 1 || line > len(texts) {
		return ""
	}
	return strings.TrimSpace(texts[line-1])
}

func badgeFor(plan *ir.IR, path []string) string {
	if len(path) == 0 {
		return ""
	}
	if path[0] != keyServices {
		return topKeyBadges[path[0]]
	}
	if len(path) < 2 {
		return ""
	}
	service, found := plan.Service(path[1])
	if !found {
		return ""
	}
	if len(path) == 2 {
		return sourceBadges[service.Provenance.Source]
	}
	return serviceBadge(plan, service, path[2:])
}

func serviceBadge(plan *ir.IR, service *ir.Service, path []string) string {
	if badge, fixed := serviceKeyBadges[path[0]]; fixed {
		return badge
	}
	switch path[0] {
	case keyImage:
		return sourceBadges[service.Image.Provenance.Source]
	case "healthcheck":
		return sourceBadges[service.Healthcheck.Provenance.Source]
	case keyEnv:
		return environmentBadge(plan, service, path[1:])
	}
	if planned, isLimit := plannedLimits(service.Resources)[path[0]]; isLimit {
		return limitBadge(service.Resources, planned)
	}
	if slices.Contains(hardeningKeys, path[0]) {
		return sourceBadges[service.Hardening.Provenance.Source]
	}
	return sourceBadges[service.Provenance.Source]
}

// plannedLimits says, for each limit the renderer writes, whether the plan
// holds a value for it; the profile fills the others. shm_size is not
// among them: only upstream or a model sets it, as the service's source.
func plannedLimits(resources ir.Resources) map[string]bool {
	return map[string]bool{
		composefile.KeyMemLimit: resources.MemoryLimit != "", composefile.KeyCPUs: resources.CPUs != "", composefile.KeyPIDsLimit: resources.PIDsLimit > 0,
	}
}

// limitBadge is where a limit came from: the plan's source when it holds
// one, else the profile. Resources' provenance is the memory limit's, so
// the catalogue's memory figure is the profile's.
// ponytail: a plan's memory below the profile default renders the
// default yet keeps the plan's badge, and upstream cpus beside a catalogue
// memory figure show P; give each limit its own provenance if that matters.
func limitBadge(resources ir.Resources, planned bool) string {
	if !planned {
		return badgeProfile
	}
	return sourceBadges[resources.Provenance.Source]
}

func environmentBadge(plan *ir.IR, service *ir.Service, path []string) string {
	if len(path) == 0 {
		return sourceBadges[service.Provenance.Source]
	}
	entry, found := plan.EnvEntry(path[0])
	if !found {
		return sourceBadges[service.Provenance.Source]
	}
	return sourceBadges[entry.Provenance.Source]
}
