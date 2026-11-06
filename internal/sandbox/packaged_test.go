package sandbox

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDockerPackagedFiles checks exact transferred bytes without assuming
// cgroup metrics, which an isolated rootless daemon may not expose.
func TestDockerPackagedFiles(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_PACKAGED_TESTS") != "1" {
		t.Skip("run scripts/container-smoke.sh with a disposable daemon")
	}
	docker, err := FindDocker("")
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	sandbox := New(ExecRunner(docker, ""), limits, 30*time.Second, "policy", t.TempDir())
	t.Cleanup(func() { assertNoDockerSandboxes(t, sandbox.run) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	verdict, err := sandbox.Validate(ctx, Input{Project: "packaged-files", Env: []byte(""), Compose: []byte(`services:
  app:
    image: docker.io/library/alpine:3.20
    command: [sleep, "300"]
    volumes: ["./config/check.txt:/etc/cw-config:ro", "data:/data"]
    secrets: [{source: check, target: custom-name}]
    healthcheck:
      test: [CMD-SHELL, "printf 'dummy-$$literal\\n' | cmp - /etc/cw-config && cmp /etc/cw-config /run/secrets/custom-name"]
      interval: 1s
      timeout: 2s
      retries: 10
secrets:
  check: {file: ./secrets/check}
volumes:
  data: {}
`), ConfigFiles: map[string][]byte{"config/check.txt": []byte("dummy-$literal\n")},
		SecretFiles: map[string][]byte{"secrets/check": []byte("dummy-$literal\n")}})
	if err != nil || !verdict.Passed() {
		t.Fatalf("transferred file bytes did not pass healthcheck: %s/%s %s %s: %v", verdict.Result, verdict.Reason, verdict.StartOutput, verdict.Logs, err)
	}
}

// TestDockerPackagedCancellation runs only inside the disposable daemon suite.
// Cancel after Docker has created a service, rather than relying on pull timing.
func TestDockerPackagedCancellation(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_PACKAGED_TESTS") != "1" {
		t.Skip("run scripts/container-smoke.sh with a disposable daemon")
	}
	docker, err := FindDocker("")
	if err != nil {
		t.Fatal(err)
	}
	base := ExecRunner(docker, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := func(call context.Context, dir string, live io.Writer, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "compose" && slices.Contains(args, "up") {
			monitor, stop := context.WithCancel(call)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for monitor.Err() == nil {
					out, _, _ := base(monitor, "", nil, "ps", "--filter", "label="+LabelProject, "-q")
					if strings.TrimSpace(string(out)) != "" {
						cancel()
						return
					}
					select {
					case <-monitor.Done():
					case <-time.After(100 * time.Millisecond):
					}
				}
			}()
			out, stderr, err := base(call, dir, live, args...)
			stop()
			<-done
			return out, stderr, err
		}
		return base(call, dir, live, args...)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	sandbox := New(runner, limits, time.Minute, "policy", t.TempDir())
	_, _ = sandbox.Validate(ctx, Input{Project: "cancel", Env: []byte(""), Compose: []byte(`services:
  app:
    image: docker.io/library/alpine:3.20
    command: [sleep, "300"]
    volumes: [data:/data]
    healthcheck: {test: [CMD, "false"], interval: 1s, retries: 60}
volumes:
  data: {}
`)})
	if ctx.Err() != context.Canceled {
		t.Fatalf("did not cancel a started run: %v", ctx.Err())
	}
	assertNoDockerSandboxes(t, base)
}

func assertNoDockerSandboxes(t *testing.T, run Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range resourceKinds {
		args := append(append([]string{}, kind...), "--filter", "label="+LabelProject, "-q")
		out, stderr, err := run(ctx, "", nil, args...)
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("sandbox resources remain (%v): %s %s %v", kind, out, stderr, err)
		}
	}
}
