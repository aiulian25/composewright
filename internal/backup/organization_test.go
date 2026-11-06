package backup

import (
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
)

func TestPreflightRejectsInvalidProjectOrganization(t *testing.T) {
	for name, project := range map[string]store.Project{
		"negative pin":       {PinOrder: -1},
		"oversized pin":      {PinOrder: 1000001},
		"unsafe label":       {Labels: []string{"a/b"}},
		"duplicate label":    {Labels: []string{"Media", "media"}},
		"noncanonical label": {Labels: []string{" Media "}},
	} {
		t.Run(name, func(t *testing.T) {
			archive := sampleArchive()
			archive.Settings.Profile = config.Default().Profile
			archive.Projects[0].Project.Labels = project.Labels
			archive.Projects[0].Project.PinOrder = project.PinOrder
			err := Preflight(archive, config.Default())
			if err == nil || !strings.Contains(err.Error(), "invalid organization metadata") {
				t.Fatalf("invalid organization reached plan validation: %v", err)
			}
		})
	}
}
