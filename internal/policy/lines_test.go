package policy

import (
	"testing"

	"github.com/aiulian25/composewright/internal/composefile"
)

// TestFindingLine: a finding points at the key its field is written to, a
// name at the service's own line, and one without a service nowhere.
func TestFindingLine(t *testing.T) {
	compose := "name: snap\nservices:\n  SnapOtter:\n    image: snapotter/snapotter\n    mem_limit: 512m\n"
	lines := composefile.KeyLines(compose)
	cases := map[Finding]int{
		{Service: "SnapOtter", Path: "services.0.name"}:      3,
		{Service: "SnapOtter", Path: "services.0.image.tag"}: 4,
		{Service: "SnapOtter", Path: "services.0.resources"}: 5,
		{Service: "SnapOtter", Path: "services.SnapOtter"}:   3,
		{Path: "app.name"}: 0,
	}
	for finding, want := range cases {
		if got := finding.Line(lines); got != want {
			t.Errorf("%+v points at line %d, want %d", finding, got, want)
		}
	}
}
