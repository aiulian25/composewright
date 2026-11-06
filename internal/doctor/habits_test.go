package doctor

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/sandbox"
)

// TestHostHabits: the time zone and the proxy network most apps share are
// the host's habits; sandbox runs, the proxy itself and a value one app
// alone has do not count.
func TestHostHabits(t *testing.T) {
	answers := [][2]string{
		{"ps -q", "a\nb\nc\nd\ne\n"},
		{"inspect", "|Europe/London\n|Europe/London\n|America/New_York\ncw-run|Europe/Paris\ncw-run|Europe/Paris\n|\n"},
		{"network ls", "bridge\nnpm_default\nnewt\n"},
		{"ps", `{"Names":"npm","Image":"jc21/nginx-proxy-manager:latest","Networks":"npm_default"}` + "\n" +
			`{"Names":"newt","Image":"fosrl/newt:1","Networks":"newt"}` + "\n" +
			`{"Names":"notes","Image":"example/notes:1","Networks":"npm_default"}` + "\n" +
			`{"Names":"feeds","Image":"example/feeds:1","Networks":"npm_default,bridge"}` + "\n" +
			`{"Names":"chat","Image":"example/chat:1","Networks":"newt"}` + "\n" +
			`{"Names":"run","Image":"example/run:1","Networks":"newt","Labels":"io.composewright.sandbox=cw-run"}`},
	}
	run := func(_ context.Context, _ string, _ io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := strings.Join(arguments, " ")
		for _, answer := range answers {
			if strings.HasPrefix(command, answer[0]) {
				return []byte(answer[1]), nil, nil
			}
		}
		return nil, nil, nil
	}
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	habits, err := HostHabits(context.Background(), sandbox.New(run, limits, time.Minute, "policy", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if habits.TimeZone != (Habit{Value: "Europe/London", Apps: 2}) {
		t.Errorf("time zone %+v", habits.TimeZone)
	}
	if habits.Network.Network != "npm_default" || habits.Network.Provider != "npm" || habits.NetworkApps != 2 {
		t.Errorf("network %+v with %d apps", habits.Network, habits.NetworkApps)
	}
}

// TestOneAppIsNoHabit: what a single app does is its own choice.
func TestOneAppIsNoHabit(t *testing.T) {
	if habit := commonest([]string{"Europe/London", "Asia/Tokyo"}); habit != (Habit{}) {
		t.Errorf("habit %+v", habit)
	}
	owners := map[string]FoundNetwork{"newt": {Network: "newt", Container: "newt"}}
	if network, apps := busiestProxyNetwork(owners, []sandbox.Container{{Name: "chat", Networks: []string{"newt"}}}); apps != 0 || network.Network != "" {
		t.Errorf("network %+v with %d apps", network, apps)
	}
}
