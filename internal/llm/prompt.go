package llm

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/redact"
)

const (
	// ponytail: four characters per token is a rough estimate; a provider
	// token counter would make the README budget exact.
	charactersPerToken = 4
	blockSeparator     = "\n\n"
	rankPriority       = 0
	rankIntroduction   = 1
	rankOther          = 2
	codeFence          = "```"
	tildeFence         = "~~~"
)

var (
	//go:embed prompts/system.txt
	systemPrompt string
	//go:embed prompts/draft.txt
	draftTask string
	//go:embed prompts/fill.txt
	fillTask string
	//go:embed prompts/repair.txt
	repairTask string

	markdownHeading = regexp.MustCompile(`^#{1,6}\s`)
	// priorityHeading picks the README sections that describe deployment;
	// they are kept first when the budget is tight.
	priorityHeading = regexp.MustCompile(`(?i)\b(install\w*|docker\w*|compose|containers?|config\w*|environment|variables?|setup|deploy\w*|getting started|quick ?start|self-?host\w*|usage|running|requirements)\b`)
)

// Document is a README or documentation page from the repository.
type Document struct {
	Path string
	Text string
}

type block struct {
	label string
	value any
}

type repoContent struct {
	Files           []repoFile `json:"files"`
	OmittedSections int        `json:"omitted_sections"`
}

type repoFile struct {
	Path     string   `json:"path"`
	Sections []string `json:"sections"`
}

type section struct {
	document int
	order    int
	rank     int
	text     string
}

// prompt joins the task and its labelled JSON blocks, then adds as much of
// the documentation as the input budget leaves room for once the system
// prompt and the answer schema are counted. The budget is a hard limit: a
// small local model past its context answers with fluent nonsense, so a
// prompt whose facts and plan alone do not fit is never sent.
func prompt(task string, blocks []block, documents []Document, schemaSize, maxInputTokens int) (string, error) {
	parts := []string{strings.TrimSpace(task)}
	for _, item := range blocks {
		encoded, err := json.Marshal(item.value)
		if err != nil {
			return "", err
		}
		parts = append(parts, item.label+":\n"+string(encoded))
	}
	used := len(systemPrompt) + schemaSize
	for _, part := range parts {
		used += len(part) + len(blockSeparator)
	}
	budget := maxInputTokens * charactersPerToken
	if used > budget {
		return "", fmt.Errorf("this repository's facts and plan need about %d tokens, more than llm.max_input_tokens (%d); raise it for a model with a larger context",
			used/charactersPerToken, maxInputTokens)
	}
	if len(documents) == 0 {
		return strings.Join(parts, blockSeparator), nil
	}
	content, err := json.Marshal(selectSections(documents, budget-used))
	if err != nil {
		return "", err
	}
	parts = append(parts, "REPO_CONTENT:\n"+string(content))
	return strings.Join(parts, blockSeparator), nil
}

// selectSections keeps deployment sections first, then each document's
// introduction, then the rest, as long as they fit; the kept sections stay
// in document order.
func selectSections(documents []Document, budget int) repoContent {
	var all []section
	for index, document := range documents {
		all = append(all, splitSections(index, document)...)
	}
	slices.SortStableFunc(all, func(left, right section) int { return left.rank - right.rank })
	var chosen []section
	omitted := 0
	for _, candidate := range all {
		if len(candidate.text) > budget {
			omitted++
			continue
		}
		budget -= len(candidate.text)
		chosen = append(chosen, candidate)
	}
	slices.SortFunc(chosen, func(left, right section) int {
		if left.document != right.document {
			return left.document - right.document
		}
		return left.order - right.order
	})
	content := repoContent{Files: []repoFile{}, OmittedSections: omitted}
	for _, kept := range chosen {
		path := documents[kept.document].Path
		last := len(content.Files) - 1
		if last < 0 || content.Files[last].Path != path {
			content.Files = append(content.Files, repoFile{Path: path})
			last++
		}
		content.Files[last].Sections = append(content.Files[last].Sections, kept.text)
	}
	return content
}

// splitSections cuts a Markdown document at headings outside code blocks
// and redacts each piece.
func splitSections(index int, document Document) []section {
	var sections []section
	var current strings.Builder
	fenced := false
	flush := func() {
		text := strings.TrimSpace(current.String())
		current.Reset()
		if text == "" {
			return
		}
		rank := rankOther
		heading, _, _ := strings.Cut(text, "\n")
		switch {
		case len(sections) == 0:
			rank = rankIntroduction
		case priorityHeading.MatchString(heading):
			rank = rankPriority
		}
		sections = append(sections, section{document: index, order: len(sections), rank: rank, text: redact.Text(text)})
	}
	for _, line := range strings.SplitAfter(document.Text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, codeFence) || strings.HasPrefix(trimmed, tildeFence) {
			fenced = !fenced
		}
		if !fenced && markdownHeading.MatchString(line) {
			flush()
		}
		current.WriteString(line)
	}
	flush()
	return sections
}

// fillFacts is what gap filling needs from the facts: where variables are
// mentioned and described, and what Dockerfiles and documentation say.
// Compose files are left out because RULE_PLAN already carries what the
// rules took from them, and .env samples because every entry is a mention
// of its variable; on a small local model every token counts.
type fillFacts struct {
	Summary     extract.Note           `json:"summary"`
	Variables   []extract.Variable     `json:"variables"`
	Dockerfiles []extract.Dockerfile   `json:"dockerfiles"`
	Images      []extract.ImageMention `json:"images"`
	DockerRuns  []extract.DockerRun    `json:"docker_runs"`
	Notes       []extract.Note         `json:"notes"`
	Suspicious  []extract.Note         `json:"suspicious"`
}

// fillFactsOf keeps the facts the FILL fields can use, so a repository with
// hundreds of documented variables still fits a small model: the variables
// whose description is asked, and Dockerfiles, images and docker run
// commands when a service field is. The summary, notes and suspicious lines
// always go, since every answer may add post-install notes and must report
// injected instructions.
func fillFactsOf(facts *extract.Facts, fields []string) fillFacts {
	described := map[string]bool{}
	serviceAsked := false
	for _, field := range fields {
		if key, isDescription := ir.DescribedVariable(field); isDescription {
			described[key] = true
		}
		serviceAsked = serviceAsked || ir.IsServiceField(field)
	}
	kept := fillFacts{Summary: facts.Summary, Variables: []extract.Variable{}, Notes: facts.Notes, Suspicious: facts.Suspicious}
	for _, variable := range facts.Variables {
		if described[variable.Name] {
			kept.Variables = append(kept.Variables, variable)
		}
	}
	// ponytail: a listed variable goes with all its mentions and a service
	// field with every Dockerfile, so a repository full of examples, such as
	// nextcloud's 27 Dockerfiles, can still exceed a small budget; send only
	// the asked service's Dockerfiles and drop mention locations if it matters.
	if serviceAsked {
		kept.Dockerfiles, kept.Images, kept.DockerRuns = facts.Dockerfiles, facts.Images, facts.DockerRuns
	}
	return kept
}

// fillPlan is RULE_PLAN as a fill sees it: secret-free, without the
// optional settings, which only the rules list and no fill answer changes,
// and with each variable's description once, in env_file, rather than again
// on every service that reads it.
func fillPlan(plan *ir.IR) *ir.IR {
	view := secretFree(plan)
	view.OptionalEnv = []ir.OptionalSetting{}
	for service := range view.Services {
		for index := range view.Services[service].Env {
			view.Services[service].Env[index].Description = ""
		}
	}
	return view
}

// secretFree is the plan as a model may see it: no secret value, and
// credentials removed from every other value.
func secretFree(plan *ir.IR) *ir.IR {
	copied := plan.Clone()
	for index := range copied.EnvFile {
		entry := &copied.EnvFile[index]
		entry.Value = hiddenValue(entry.Key, entry.Value, entry.Secret)
	}
	for service := range copied.Services {
		for index := range copied.Services[service].Env {
			variable := &copied.Services[service].Env[index]
			variable.Literal = hiddenValue(variable.Key, variable.Literal, variable.Secret)
		}
	}
	return copied
}

func hiddenValue(name, value string, secret bool) string {
	reference := strings.HasPrefix(value, "$")
	if secret && value != "" && !reference {
		return redact.Mask
	}
	return redact.Value(name, value)
}

func promptHash(system, user string) string {
	sum := sha256.Sum256([]byte(system + "\x00" + user))
	return hex.EncodeToString(sum[:])
}
