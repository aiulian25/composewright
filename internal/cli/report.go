package cli

import (
	"fmt"
	"io"

	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/policy"
)

func (application *app) severityLabel(severity policy.Severity) string {
	label := severity.String()
	switch severity {
	case policy.Deny:
		return application.colours.danger(label)
	case policy.Need, policy.Warn:
		return application.colours.warn(label)
	case policy.OK:
		return application.colours.ok(label)
	}
	return application.colours.muted(label)
}

// printFindings lists what needs attention; automatic fixes are counted by
// the caller instead of listed.
func (application *app) printFindings(output io.Writer, report policy.Report) error {
	rows := make([][]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		if finding.Severity == policy.Fix {
			continue
		}
		message := finding.Message
		if finding.FreePort > 0 {
			message = fmt.Sprintf(portSuggestion, message, portFlag, pipeline.PortChoice(finding.Service, finding.ContainerPort, finding.FreePort))
		}
		rows = append(rows, []string{"  " + application.severityLabel(finding.Severity), finding.Rule, sanitize(finding.Service), sanitize(message)})
	}
	return table(output, rows...)
}
