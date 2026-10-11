//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnose(t *testing.T) {
	directory := t.TempDir()
	locked := filepath.Join(directory, "locked.sock")
	open := filepath.Join(directory, "open.sock")
	for path, mode := range map[string]os.FileMode{locked: 0, open: 0o600} {
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		host, status, mentions string
	}{
		{"tcp://socket-proxy:2375", statusUnreachable, "tcp://socket-proxy:2375"},
		{"unix://" + filepath.Join(directory, "missing.sock"), statusNoSocket, "missing.sock"},
		{"unix://" + open, statusNotAnswering, "open.sock"},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct{ host, status, mentions string }{"unix://" + locked, statusNoAccess, "group"})
	}
	for _, testCase := range cases {
		got := Diagnose(testCase.host)
		if got.Status != testCase.status || !strings.Contains(got.Fix, testCase.mentions) {
			t.Errorf("Diagnose(%q) = %+v, want status %q mentioning %q", testCase.host, got, testCase.status, testCase.mentions)
		}
	}
	if SocketPath("") != defaultSocket || SocketPath("unix:///run/user/1000/docker.sock") != "/run/user/1000/docker.sock" || SocketPath("ssh://host") != "" {
		t.Error("SocketPath maps hosts wrongly")
	}
}
