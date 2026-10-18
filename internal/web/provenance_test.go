package web

import (
	"testing"

	"github.com/aiulian25/composewright/internal/ir"
)

// TestFilesReadBadgesAFencedPage: a field read from a block fenced in
// README.md badges README.md itself.
func TestFilesReadBadgesAFencedPage(t *testing.T) {
	plan := &ir.IR{Services: []ir.Service{{
		Name:  "app",
		Image: ir.Image{Provenance: ir.Provenance{Source: ir.SourceCompose, SourceRef: "README.md#L3:6"}},
	}}}
	read := filesRead([]string{"README.md"}, plan)
	if len(read) != 1 || read[0].Badge != sourceBadges[ir.SourceCompose] {
		t.Fatalf("files read %+v", read)
	}
}
