package cli

import (
	"io"

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
		rows = append(rows, []string{"  " + application.severityLabel(finding.Severity), finding.Rule, sanitize(finding.Service), sanitize(finding.Message)})
	}
	return table(output, rows...)
}
