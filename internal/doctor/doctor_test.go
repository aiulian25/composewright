package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store/storetest"
	"github.com/aiulian25/composewright/internal/workspace"
)

// TestAuditCheck: doctor reports an intact chain with its size, and fails,
// naming the entry, once a row is edited behind composewright's back.
func TestAuditCheck(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("COMPOSEWRIGHT_MASTER_KEY_FILE", "")
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(root, "config.yaml"), DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Init(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	loaded.FileExists = true
	if check := auditCheck(ctx, loaded); check.Status != StatusOK || check.Detail != "hash chain intact, 1 entry" {
		t.Fatalf("untouched log: %+v", check)
	}
	storetest.EditAuditActor(t, loaded.Paths.Database(), 1)
	if check := auditCheck(ctx, loaded); check.Status != StatusFail || !strings.Contains(check.Detail, "entry 1 does not match the chain") {
		t.Fatalf("edited log: %+v", check)
	}
}

// TestTierCheck: the proxy tier is only a label, so doctor warns when Docker
// is not reached over TCP, where a socket proxy listens; otherwise the
// daemon's own containment decides.
func TestTierCheck(t *testing.T) {
	cases := []struct {
		name        string
		environment sandbox.Environment
		dockerHost  string
		status      string
		detail      string
	}{
		{"proxy on the default socket", sandbox.Environment{Tier: config.SandboxTierProxy}, "", StatusWarn, "reached at the default socket, not through a socket proxy"},
		{"proxy on a unix socket", sandbox.Environment{Tier: config.SandboxTierProxy}, "unix:///var/run/docker.sock", StatusWarn, "not through a socket proxy"},
		{"proxy over tcp, rootless", sandbox.Environment{Tier: config.SandboxTierProxy, Rootless: true}, "tcp://socket-proxy:2375", StatusOK, "rootless daemon"},
		{"policy on a rootful daemon", sandbox.Environment{Tier: config.SandboxTierPolicy}, "", StatusWarn, "rootful daemon"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			check := tierCheck(testCase.environment, testCase.dockerHost)
			if check.Status != testCase.status || !strings.Contains(check.Detail, testCase.detail) {
				t.Fatalf("%+v, want %s with %q", check, testCase.status, testCase.detail)
			}
		})
	}
}
