// Package policy is the model-independent gate. It judges a plan and the
// compose file rendered from it; it never fixes them.
package policy

import (
	"fmt"
	"slices"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
)

type Severity int

const (
	OK Severity = iota
	Fix
	Warn
	Need
	Deny
)

var severityNames = []string{"ok", "fix", "warn", "need", "deny"}

func (severity Severity) String() string {
	return severityNames[severity]
}

func (severity Severity) MarshalText() ([]byte, error) {
	return []byte(severity.String()), nil
}

func (severity *Severity) UnmarshalText(text []byte) error {
	index := slices.Index(severityNames, string(text))
	if index < 0 {
		return fmt.Errorf("unknown severity %q", text)
	}
	*severity = Severity(index)
	return nil
}

type Finding struct {
	Rule       string   `json:"rule"`
	Severity   Severity `json:"severity"`
	Service    string   `json:"service,omitempty"`
	Path       string   `json:"path,omitempty"`
	Capability string   `json:"capability,omitempty"`
	Message    string   `json:"message"`
}

type Report struct {
	Findings []Finding `json:"findings"`
}

type Input struct {
	Plan            *ir.IR
	Profile         config.Profile
	AllowRegistries []string
	ExtraRegistries []string
	RepositoryOwner string
	GrantedNeeds    []string
	// Images holds registry verification results keyed by
	// registry/repository:tag; an image missing from the map was not checked.
	Images map[string]ImageCheck
}

// ImageCheck is what the registry said about one image.
type ImageCheck struct {
	Found   bool
	Message string
}

// Blocked reports whether anything denies the plan or a need is still waiting
// for a human grant.
func (report Report) Blocked() bool {
	return slices.ContainsFunc(report.Findings, func(finding Finding) bool {
		return finding.Severity == Need || finding.Severity == Deny
	})
}

func (report Report) Has(rule string, severity Severity) bool {
	return slices.ContainsFunc(report.Findings, func(finding Finding) bool {
		return finding.Rule == rule && finding.Severity == severity
	})
}

func (report Report) Worst() Severity {
	worst := OK
	for _, finding := range report.Findings {
		worst = max(worst, finding.Severity)
	}
	return worst
}

func (report Report) Count(severity Severity) int {
	count := 0
	for _, finding := range report.Findings {
		if finding.Severity == severity {
			count++
		}
	}
	return count
}

// Merge combines the IR gate and the YAML gate into one report. The YAML
// gate checks the rendered file again, so a finding it repeats for the same
// rule and service is listed once.
func (report Report) Merge(other Report) Report {
	merged := slices.Clone(report.Findings)
	for _, finding := range other.Findings {
		repeated := slices.ContainsFunc(report.Findings, func(existing Finding) bool {
			return existing.Rule == finding.Rule && existing.Severity == finding.Severity && existing.Service == finding.Service
		})
		if !repeated {
			merged = append(merged, finding)
		}
	}
	return Report{Findings: merged}
}

type checker struct {
	input    Input
	findings []Finding
}

func (check *checker) add(rule string, severity Severity, service, path, format string, arguments ...any) {
	check.findings = append(check.findings, Finding{
		Rule: rule, Severity: severity, Service: service, Path: path, Message: fmt.Sprintf(format, arguments...),
	})
}

func (check *checker) granted(capability string) bool {
	return slices.Contains(check.input.GrantedNeeds, capability)
}

// Evaluate runs P1..P12 and the consistency rules on the plan.
func Evaluate(input Input) Report {
	check := &checker{input: input}
	plan := input.Plan
	check.consistency(plan)
	check.size(plan)
	for index := range plan.Services {
		service := &plan.Services[index]
		path := fmt.Sprintf("services.%d", index)
		check.image(service, path)
		check.healthcheck(service, path)
		check.ports(service, path)
		check.user(service, path)
		check.commands(service, path)
		check.serviceEnv(plan, service, path)
		check.volumes(service, path)
		check.resources(service, path)
	}
	check.envFile(plan)
	check.optionalEnv(plan)
	check.needs(plan)
	check.dependencies(plan)
	return Report{Findings: check.findings}
}
