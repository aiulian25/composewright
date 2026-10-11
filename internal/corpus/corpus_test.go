package corpus

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aiulian25/composewright/internal/policy"
)

// TestLoadReadsTheVerdict: "pass" means the rules-only plan passes the gate;
// any other verdict lists what blocks it, sorted, and "-" lists no
// registries.
func TestLoadReadsTheVerdict(t *testing.T) {
	root := t.TempDir()
	write := func(slug, verdict, registries string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, slug, "files"), 0o700); err != nil {
			t.Fatal(err)
		}
		source := fmt.Sprintf(`{"url": "https://github.com/example/%s", "commit": "abc123", "allow_registries": %q, "verdict": %q}`,
			slug, registries, verdict)
		if err := os.WriteFile(filepath.Join(root, slug, "source.json"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("clean", verdictPass, noneListed)
	write("blocked", "need:docker_socket,deny:P2", "ghcr.io/example/*,docker.io/example/*")
	entries, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Slug != "blocked" || entries[1].Slug != "clean" {
		t.Fatalf("entries %+v, want blocked then clean", entries)
	}
	blocked, clean := entries[0], entries[1]
	if blocked.Passes() || !slices.Equal(blocked.Verdict, []string{"deny:P2", "need:docker_socket"}) || len(blocked.AllowRegistries) != 2 {
		t.Fatalf("blocked entry: %+v", blocked)
	}
	if !clean.Passes() || clean.AllowRegistries != nil {
		t.Fatalf("clean entry: %+v", clean)
	}
}

// TestBlockingNamesWhatStopsAPlan: needs by capability and denials by rule,
// each once and sorted; warnings and passed rules block nothing.
func TestBlockingNamesWhatStopsAPlan(t *testing.T) {
	report := policy.Report{Findings: []policy.Finding{
		{Rule: "P9", Severity: policy.Need, Capability: "docker_socket"},
		{Rule: "P2", Severity: policy.Deny},
		{Rule: "P2", Severity: policy.Deny, Service: "worker"},
		{Rule: "P7", Severity: policy.Warn},
		{Rule: "P1", Severity: policy.OK},
	}}
	if blocking := Blocking(report); !slices.Equal(blocking, []string{"deny:P2", "need:docker_socket"}) {
		t.Fatalf("blocking %v", blocking)
	}
	if blocking := Blocking(policy.Report{}); len(blocking) != 0 {
		t.Fatalf("an empty report blocks on %v", blocking)
	}
}
