package policy

import (
	"strings"

	"github.com/aiulian25/composewright/internal/composefile"
)

// Rules are the rules of the gate: plan consistency, P1 to P13 and P9a.
var Rules = []string{"C", "P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P9a", "P10", "P11", "P12", "P13"}

const (
	servicesKey = "services"
	// planSeparator splits a plan path such as services.0.image, whose
	// field is fieldSegment.
	planSeparator = "."
	fieldSegment  = 2
)

// planFieldKeys are the compose.yaml keys plan fields are written to.
var planFieldKeys = map[string]string{"image": "image", "ports": "ports", "volumes": "volumes", "env": "environment", "command": "command",
	"entrypoint": "entrypoint", "user": "user", "healthcheck": "healthcheck", "depends_on": "depends_on", "resources": "mem_limit"}

// Line is the compose.yaml line the finding is about, from
// composefile.KeyLines: the key its field is written to, else its service's
// own line; 0 when it names no service.
func (finding Finding) Line(lines map[string]int) int {
	service := composefile.KeyPath(servicesKey, finding.Service)
	segments := strings.Split(finding.Path, planSeparator)
	field := ""
	if len(segments) > fieldSegment {
		field = planFieldKeys[segments[fieldSegment]]
	}
	if line, found := lines[composefile.KeyPath(service, field)]; found && field != "" {
		return line
	}
	return lines[service]
}
