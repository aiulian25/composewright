package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/ir"
)

// printPlan is the CLI form of the Review screen.
func (application *app) printPlan(output io.Writer, plan *ir.IR) error {
	fmt.Fprintf(output, "%s  %s\n", application.colours.ok(sanitize(plan.App.Name)), application.colours.muted(sanitize(plan.App.Summary)))
	fmt.Fprintf(output, "confidence: %s · %d services · %d variables\n\n", plan.Confidence, len(plan.Services), len(plan.EnvFile))
	rows := [][]string{{"SERVICE", "ROLE", "IMAGE", "PORTS", "VOLUMES", "HEALTHCHECK"}}
	for _, service := range plan.Services {
		rows = append(rows, []string{sanitize(service.Name), service.Role, sanitize(imageText(service.Image)),
			portsText(service.Ports), volumesText(service.Volumes), healthcheckText(service.Healthcheck)})
	}
	if err := table(output, rows...); err != nil {
		return err
	}
	fmt.Fprintln(output)
	variables := [][]string{{"VARIABLE", "VALUE", "WHERE IT CAME FROM"}}
	for _, entry := range plan.EnvFile {
		variables = append(variables, []string{sanitize(entry.Key), sanitize(application.envValue(entry)), sanitize(provenanceText(entry.Provenance))})
	}
	if err := table(output, variables...); err != nil {
		return err
	}
	for _, service := range plan.Services {
		for _, note := range service.Notes {
			fmt.Fprintf(output, "%s %s: %s\n", application.colours.muted("note"), sanitize(service.Name), sanitize(note))
		}
	}
	for _, text := range plan.SuspiciousInstructions {
		fmt.Fprintf(output, "%s %s\n", application.colours.danger("suspicious"), sanitize(text))
	}
	gaps, undescribed := splitGaps(plan.Unfilled)
	if len(gaps) > 0 {
		fmt.Fprintf(output, "%s %s\n", application.colours.warn("not determined:"), strings.Join(gaps, ", "))
	}
	if undescribed > 0 {
		fmt.Fprintf(output, "%s %d variables have no description upstream\n", application.colours.muted("note"), undescribed)
	}
	return nil
}

func splitGaps(unfilled []string) ([]string, int) {
	var gaps []string
	undescribed := 0
	for _, gap := range unfilled {
		if strings.HasPrefix(gap, "env_file.") && strings.HasSuffix(gap, ".description") {
			undescribed++
			continue
		}
		gaps = append(gaps, gap)
	}
	return gaps, undescribed
}

func (application *app) envValue(entry ir.EnvFileEntry) string {
	switch {
	case entry.Generate != "":
		return application.colours.warn("generated " + entry.Generate)
	case entry.Secret && entry.Value == "":
		return application.colours.warn("secret, you supply it")
	case entry.Secret && !strings.Contains(entry.Value, "${"):
		return "••••••"
	}
	return entry.Value
}

func imageText(image ir.Image) string {
	if image.Registry == "" {
		return "?"
	}
	return image.String()
}

func portsText(ports []ir.Port) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		text := strconv.Itoa(port.Container) + "/" + port.Protocol
		if !port.Publish {
			text += " (internal)"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

func volumesText(volumes []ir.Volume) string {
	parts := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		parts = append(parts, volume.Name+":"+volume.ContainerPath)
	}
	return strings.Join(parts, " ")
}

func healthcheckText(healthcheck ir.Healthcheck) string {
	if !healthcheck.Defined() {
		return "-"
	}
	return healthcheck.Provenance.Source
}

func provenanceText(provenance ir.Provenance) string {
	if provenance.SourceRef == "" {
		return provenance.Source
	}
	return provenance.SourceRef
}
