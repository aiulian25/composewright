package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/ir"
)

// TestDocker runs real stacks against the local daemon; it pulls small
// public images. Run with COMPOSEWRIGHT_DOCKER_TESTS=1 (make test-docker).
func TestDocker(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_DOCKER_TESTS") == "" {
		t.Skip("set COMPOSEWRIGHT_DOCKER_TESTS=1 to run against a real Docker daemon")
	}
	docker, err := FindDocker("")
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	sandbox := New(ExecRunner(docker, ""), limits, 15*time.Second, "policy", t.TempDir())
	const alpine = "docker.io/library/alpine:3.20"
	web := ir.Probe{Service: "web", Port: 80, Path: "/"}
	loopback := ir.Probe{Service: "app", Port: 8080, Path: "/"}
	cases := []struct {
		name    string
		compose string
		probe   ir.Probe
		result  string
		reason  string
	}{
		{"healthy", `services:
  web:
    image: docker.io/library/nginx:1.27-alpine
    ports: ["127.0.0.1:8080:80/tcp"]
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1/"], interval: 2s, timeout: 2s, retries: 5}
`, web, ResultPass, ""},
		{"loopback only", "services:\n  app:\n    image: " + alpine + "\n    command: [\"nc\", \"-lk\", \"-s\", \"127.0.0.1\", \"-p\", \"8080\", \"-e\", \"true\"]\n" +
			"    healthcheck: {test: [\"CMD\", \"nc\", \"-z\", \"127.0.0.1\", \"8080\"], interval: 1s, timeout: 1s, retries: 5}\n", loopback, ResultFail, ReasonUnreachable},
		{"crash", "services:\n  job:\n    image: " + alpine + "\n    command: [\"sh\", \"-c\", \"echo boom; exit 3\"]\n", ir.Probe{}, ResultFail, ReasonCrash},
		{"unhealthy", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sleep\", \"300\"]\n    healthcheck: {test: [\"CMD\", \"false\"], interval: 1s, timeout: 1s, retries: 2}\n", ir.Probe{}, ResultFail, ReasonUnhealthy},
		{"timeout", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sleep\", \"300\"]\n    healthcheck: {test: [\"CMD\", \"sleep\", \"60\"], interval: 60s, timeout: 90s, retries: 1}\n", ir.Probe{}, ResultFail, ReasonTimeout},
		{"fatal log line", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sh\", \"-c\", \"echo 'panic: test failure'; sleep 300\"]\n", ir.Probe{}, ResultFail, ReasonLogs},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict, err := sandbox.Validate(context.Background(), Input{Project: "dockertest-" + testCase.name, Compose: []byte(testCase.compose),
				Env: []byte(""), Published: map[string][]string{"web": {"80/tcp"}}, Probe: testCase.probe})
			if err != nil {
				t.Fatal(err)
			}
			if verdict.Result != testCase.result || verdict.Reason != testCase.reason {
				t.Fatalf("verdict %s/%s (%s)\nstart output:\n%s\nlogs:\n%s", verdict.Result, verdict.Reason, verdict.Detail, verdict.StartOutput, verdict.Logs)
			}
			if testCase.name == "healthy" {
				assertLoopbackEphemeral(t, verdict)
			}
			if testCase.probe.Port != 0 && (verdict.Reach == nil || !verdict.Reach.Checked) {
				t.Fatalf("the probe did not run: %+v", verdict.Reach)
			}
			output, _, _ := sandbox.run(context.Background(), "", nil, "ps", "-a", "--filter", "label="+LabelProject+"="+verdict.Project, "-q")
			if strings.TrimSpace(string(output)) != "" {
				t.Fatalf("containers of %s survived the run: %s", verdict.Project, output)
			}
		})
	}
}

// assertLoopbackEphemeral settles U1: 127.0.0.1:0:<port> publishes on a
// random loopback port.
func assertLoopbackEphemeral(t *testing.T, verdict Verdict) {
	t.Helper()
	var publishers []Publisher
	for _, state := range verdict.Services {
		publishers = append(publishers, state.Publishers...)
	}
	if len(publishers) == 0 {
		t.Fatalf("no published port reported: %+v", verdict.Services)
	}
	for _, publisher := range publishers {
		if publisher.URL != "127.0.0.1" || publisher.PublishedPort == 0 || publisher.PublishedPort == 8080 {
			t.Fatalf("port not on an ephemeral loopback port: %+v", publisher)
		}
	}
}

// TestDockerLiveLogs checks that service logs arrive while `up --wait` is
// still waiting, which proves the follower's --since format works with the
// real CLI, and that every line arrives once.
func TestDockerLiveLogs(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_DOCKER_TESTS") == "" {
		t.Skip("set COMPOSEWRIGHT_DOCKER_TESTS=1 to run against a real Docker daemon")
	}
	docker, err := FindDocker("")
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	sandbox := New(ExecRunner(docker, ""), limits, 30*time.Second, "policy", t.TempDir())
	compose := `services:
  ticker:
    image: docker.io/library/alpine:3.20
    command: ["sh", "-c", "for i in 1 2 3 4 5 6; do echo tick $$i; sleep 1; done; touch /tmp/ready; sleep 300"]
    healthcheck: {test: ["CMD", "test", "-f", "/tmp/ready"], interval: 1s, timeout: 1s, retries: 30}
`
	var mutex sync.Mutex
	var transcript []string
	live := func(line Line) {
		mutex.Lock()
		defer mutex.Unlock()
		transcript = append(transcript, line.Source+": "+line.Text)
	}
	verdict, err := sandbox.Validate(context.Background(), Input{Project: "dockertest-live", Compose: []byte(compose), Env: []byte(""), Live: live})
	if err != nil || !verdict.Passed() {
		t.Fatalf("verdict %+v %v", verdict, err)
	}
	joined := strings.Join(transcript, "\n")
	for tick := 1; tick <= 6; tick++ {
		if count := strings.Count(joined, "ticker: tick "+strconv.Itoa(tick)+"\n"); count != 1 {
			t.Errorf("tick %d arrived %d times:\n%s", tick, count, joined)
		}
	}
	firstTick, healthy := strings.Index(joined, "ticker: tick 1"), strings.Index(joined, "Healthy")
	if firstTick < 0 || healthy < 0 || firstTick > healthy {
		t.Errorf("no log line arrived while up --wait was still waiting:\n%s", joined)
	}
}
