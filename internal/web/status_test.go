package web

import (
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
)

func TestDaemonState(t *testing.T) {
	cases := []struct {
		name        string
		environment sandbox.Environment
		want        dockerState
	}{
		{"rootful", sandbox.Environment{Tier: config.SandboxTierPolicy, EngineVersion: "29.0.0"}, dockerState{Text: "Docker 29.0.0, rootful", Tone: toneWarn}},
		{"rootless", sandbox.Environment{Tier: config.SandboxTierRootless, Rootless: true, EngineVersion: "29.0.0"}, dockerState{Text: "Docker 29.0.0, rootless", Tone: toneOk}},
		{"gVisor", sandbox.Environment{Tier: config.SandboxTierRunsc, EngineVersion: "29.0.0", Runtimes: []string{"runc", "runsc"}},
			dockerState{Text: "Docker 29.0.0, rootful, gVisor", Tone: toneWarn}},
		{"no gVisor", sandbox.Environment{Tier: config.SandboxTierRunsc, EngineVersion: "29.0.0", Runtimes: []string{"runc"}},
			dockerState{Text: "Docker 29.0.0, no gVisor", Tone: toneDanger, Fix: runscMissingFix}},
	}
	for _, testCase := range cases {
		if got := daemonState(testCase.environment); got != testCase.want {
			t.Errorf("%s: %+v, want %+v", testCase.name, got, testCase.want)
		}
	}
}
