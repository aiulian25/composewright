package sandbox

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// growingLogs answers `logs` with more lines on every call, as a running
// stack does, and holds `up` until the follower has polled twice.
type growingLogs struct {
	mutex  sync.Mutex
	polls  int
	polled chan struct{}
}

var logBatches = []string{
	"app-1  | 2026-10-10T08:00:01.000000001Z starting with password=hunter2hunter2\n",
	"app-1  | 2026-10-10T08:00:01.000000001Z starting with password=hunter2hunter2\n" +
		"db-1   | 2026-10-10T08:00:00.5Z database system is ready\n" +
		"app-1  | 2026-10-10T08:00:02Z listening on :8080\n",
}

func (docker *growingLogs) run(ctx context.Context, _ string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
	command := strings.Join(arguments, " ")
	switch {
	case strings.Contains(command, " up "):
		_, _ = io.WriteString(live, " Container cw-shop-1a2b-app-1 Started\n Container cw-shop-1a2b-app-1 Healthy\n")
		select {
		case <-docker.polled:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
		return nil, nil, nil
	case strings.Contains(command, " logs "):
		docker.mutex.Lock()
		defer docker.mutex.Unlock()
		batch := logBatches[min(docker.polls, len(logBatches)-1)]
		docker.polls++
		if docker.polls == len(logBatches) {
			close(docker.polled)
		}
		return []byte(batch), nil, nil
	case strings.Contains(command, " ps "):
		return []byte(healthyApp), nil, nil
	case strings.HasPrefix(command, "info"):
		return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd"}`), nil, nil
	}
	return nil, nil, nil
}

func TestLiveOutputIsCompleteRedactedAndOnce(t *testing.T) {
	docker := &growingLogs{polled: make(chan struct{})}
	limits, _ := ParseLimits("1g", 1, 128)
	sandbox := New(docker.run, limits, time.Minute, "policy", t.TempDir())
	sandbox.followInterval = 10 * time.Millisecond
	var mutex sync.Mutex
	var lines []Line
	live := func(line Line) {
		mutex.Lock()
		defer mutex.Unlock()
		lines = append(lines, line)
	}
	verdict, err := sandbox.Validate(context.Background(), Input{Project: "shop", Compose: []byte(testCompose), Env: []byte("PASSWORD=hunter2hunter2\n"),
		Secrets: []string{"hunter2hunter2"}, Published: map[string][]string{"app": {"80/tcp"}}, Live: live})
	if err != nil || !verdict.Passed() {
		t.Fatalf("verdict %+v %v", verdict, err)
	}
	var transcript []string
	for _, line := range lines {
		transcript = append(transcript, line.Source+": "+line.Text)
	}
	joined := strings.Join(transcript, "\n")
	if strings.Contains(joined, "hunter2hunter2") {
		t.Fatalf("a secret reached the live output:\n%s", joined)
	}
	for _, want := range []string{"sandbox: project cw-shop-", "compose: Container cw-shop-1a2b-app-1 Started", "app: starting with password=<redacted>",
		"db: database system is ready", "app: listening on :8080", "sandbox: PASS in", "sandbox: removed project cw-shop-"} {
		if strings.Count(joined, want) != 1 {
			t.Errorf("%q appears %d times in:\n%s", want, strings.Count(joined, want), joined)
		}
	}
	if strings.Index(joined, "sandbox: PASS") < strings.Index(joined, "app: listening") {
		t.Errorf("the verdict came before the last log line:\n%s", joined)
	}
}

func TestSplitLogLine(t *testing.T) {
	container, service, stamp, message, ok := splitLogLine("db-2  | 2026-10-10T08:15:02.123456789Z LOG:  ready | to accept")
	if !ok || container != "db-2" || service != "db" || message != "LOG:  ready | to accept" || stamp.Nanosecond() != 123456789 {
		t.Fatalf("got %q %q %v %q %v", container, service, stamp, message, ok)
	}
	for _, bad := range []string{"", "no separator here", "app-1  | not-a-time message"} {
		if _, _, _, _, ok := splitLogLine(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
