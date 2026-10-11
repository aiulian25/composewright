// Package rules builds a plan from extracted facts deterministically with
// rules R1-R6. Every value carries the file and line it came from and a
// confidence; what the rules cannot fill is listed in plan.Unfilled.
package rules

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
)

const (
	maxNameLength     = 40
	minIdentityLength = 3
	maxSummaryLength  = 200
	defaultProbePath  = "/"
)

var nameCleaner = regexp.MustCompile(`[^a-z0-9-]+`)

// nameSeparators are dropped to compare names spelled with and without them.
var nameSeparators = strings.NewReplacer("-", "", "_", "", ".", "")

// genericServiceNames say nothing about the application, so the repository
// name is used for the project instead.
var genericServiceNames = []string{"app", "web", "webserver", "server", "application", "backend", "frontend", "main", "api", "service"}

type Input struct {
	Facts  *extract.Facts
	Source *fetch.Source
	Name   string
}

type builder struct {
	ctx      context.Context
	input    Input
	facts    *extract.Facts
	plan     *ir.IR
	identity []string
	// secretOwners maps a literal secret value written upstream to the
	// variable that now generates it, so other values can refer to it.
	secretOwners map[string]string
	// namedVolumes are the volume names upstream declares; bindVolumes maps
	// each bind-mounted folder to the volume that replaces it.
	namedVolumes map[string]bool
	bindVolumes  map[string]string
	// own are the secrets from the person's own .env (OwnSecrets).
	own map[string]string
}

// Build turns facts into a plan. It never fails: gaps become plan.Unfilled
// entries and low confidence, which the review step turns into questions.
func Build(ctx context.Context, input Input) *ir.IR {
	builder := &builder{
		ctx: ctx, input: input, facts: input.Facts, secretOwners: map[string]string{},
		namedVolumes: map[string]bool{}, bindVolumes: map[string]string{}, own: OwnSecrets(input.Facts, input.Source),
		plan:     &ir.IR{IRVersion: ir.Version},
		identity: identityNames(input.Source),
	}
	compose := chooseCompose(builder.facts, builder.identity)
	if compose != nil {
		builder.fromCompose(compose)
	} else {
		builder.fromDocumentation()
	}
	builder.enrich()
	builder.generateStackSecrets()
	builder.finish()
	builder.plan.Normalize()
	return builder.plan
}

// identityNames are lowercase names that identify the project's own image:
// the repository owner and name, when long enough to be meaningful, also
// without separators, since pi-hole publishes pihole/pihole.
func identityNames(source *fetch.Source) []string {
	var names []string
	for _, candidate := range []string{source.Repository, source.Owner} {
		lowered := strings.ToLower(candidate)
		for _, name := range []string{lowered, nameSeparators.Replace(lowered)} {
			if len(name) >= minIdentityLength && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func (builder *builder) matchesIdentity(image ir.Image) bool {
	repository := strings.ToLower(image.Repository)
	for _, name := range builder.identity {
		if strings.Contains(repository, name) {
			return true
		}
	}
	return false
}

func (builder *builder) evidence(claim string, location extract.Location) {
	builder.plan.Evidence = append(builder.plan.Evidence, ir.Evidence{Claim: claim, File: location.File, Line: location.Line})
}

func provenance(source string, location extract.Location, confidence string) ir.Provenance {
	return ir.Provenance{Source: source, SourceRef: location.String(), Confidence: confidence}
}

func slug(text string) string {
	cleaned := strings.Trim(nameCleaner.ReplaceAllString(strings.ToLower(text), "-"), "-")
	if len(cleaned) > maxNameLength {
		cleaned = strings.Trim(cleaned[:maxNameLength], "-")
	}
	return cleaned
}

func (builder *builder) note(service *ir.Service, format string, arguments ...any) {
	service.Notes = append(service.Notes, fmt.Sprintf(format, arguments...))
}
