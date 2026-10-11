//go:build linux

package runas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimAndTighten(t *testing.T) {
	ours := t.TempDir()
	if err := os.Chmod(ours, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claim(ours, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatalf("claim of our own folder: %v", err)
	}
	if err := tighten(ours); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(ours); info.Mode().Perm() != privateDirectoryMode {
		t.Fatalf("tighten left mode %04o", info.Mode().Perm())
	}
	other := os.Geteuid() + 1
	if err := os.WriteFile(filepath.Join(ours, "kept"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claim(ours, other, other); err == nil || !strings.Contains(err.Error(), "not an empty folder") {
		t.Fatalf("a folder with files was taken over: %v", err)
	}
	if os.Geteuid() == rootID {
		t.Skip("root may chown")
	}
	if err := claim(t.TempDir(), other, other); err == nil || !strings.Contains(err.Error(), "cap_add") {
		t.Fatalf("an empty folder without the capability should name cap_add: %v", err)
	}
	if err := claim(filepath.Join(ours, "missing"), other, other); err != nil {
		t.Fatalf("a missing folder: %v", err)
	}
}
