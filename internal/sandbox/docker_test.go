package sandbox

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/ir"
)

// postgresWithSecretFile starts postgres with its password read from a
// secret file, as a plan passes a secret whose *_FILE twin is documented;
// the healthcheck signs in over TCP, so it passes only with that password.
const postgresWithSecretFile = `services:
  db:
    image: docker.io/library/postgres:17-alpine
    environment:
      POSTGRES_PASSWORD_FILE: /run/secrets/postgres_password
    secrets: [postgres_password]
    healthcheck:
      test: ["CMD-SHELL", "PGPASSWORD=$$(cat /run/secrets/postgres_password) psql -h 127.0.0.1 -U postgres -c 'select 1'"]
      interval: 2s
      timeout: 2s
      retries: 15
secrets:
  postgres_password:
    file: ./secrets/postgres_password
`

// nginxWithBundledFile serves /ready only from a config file bundled beside
// compose.yaml, so its healthcheck passes only when the run mounts it.
const nginxWithBundledFile = `services:
  web:
    image: docker.io/library/nginx:1.27-alpine
    volumes:
      - ./config/default.conf:/etc/nginx/conf.d/default.conf:ro
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1/ready"], interval: 2s, timeout: 2s, retries: 5}
`

// bundledNginxConfig is that config file.
const bundledNginxConfig = "server {\n  listen 80;\n  location = /ready { return 200 'ready'; }\n}\n"

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
	accepted := []int{200, 301, 302}
	healthPath := ir.Probe{Service: "web", Port: 80, Path: "/index.html", Expect: accepted}
	missingPath := ir.Probe{Service: "web", Port: 80, Path: "/healthz", Expect: accepted}
	const nginx = `services:
  web:
    image: docker.io/library/nginx:1.27-alpine
    healthcheck: {test: ["CMD", "wget", "-qO-", "http://127.0.0.1/"], interval: 2s, timeout: 2s, retries: 5}
`
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
		{"expected answer on the health path", nginx, healthPath, ResultPass, ""},
		{"404 on the health path", nginx, missingPath, ResultFail, ReasonUnreachable},
		{"loopback only", "services:\n  app:\n    image: " + alpine + "\n    command: [\"nc\", \"-lk\", \"-s\", \"127.0.0.1\", \"-p\", \"8080\", \"-e\", \"true\"]\n" +
			"    healthcheck: {test: [\"CMD\", \"nc\", \"-z\", \"127.0.0.1\", \"8080\"], interval: 1s, timeout: 1s, retries: 5}\n", loopback, ResultFail, ReasonUnreachable},
		{"crash", "services:\n  job:\n    image: " + alpine + "\n    command: [\"sh\", \"-c\", \"echo boom; exit 3\"]\n", ir.Probe{}, ResultFail, ReasonCrash},
		{"unhealthy", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sleep\", \"300\"]\n    healthcheck: {test: [\"CMD\", \"false\"], interval: 1s, timeout: 1s, retries: 2}\n", ir.Probe{}, ResultFail, ReasonUnhealthy},
		{"timeout", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sleep\", \"300\"]\n    healthcheck: {test: [\"CMD\", \"sleep\", \"60\"], interval: 60s, timeout: 90s, retries: 1}\n", ir.Probe{}, ResultFail, ReasonTimeout},
		{"upstream hardening", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sleep\", \"300\"]\n    read_only: true\n    cap_drop: [ALL]\n" +
			"    tmpfs: [/tmp]\n    shm_size: 1g\n    healthcheck: {test: [\"CMD\", \"touch\", \"/tmp/ready\"], interval: 1s, timeout: 1s, retries: 5}\n", ir.Probe{}, ResultPass, ""},
		{"fatal log line", "services:\n  app:\n    image: " + alpine + "\n    command: [\"sh\", \"-c\", \"echo 'panic: test failure'; sleep 300\"]\n", ir.Probe{}, ResultFail, ReasonLogs},
		{"secret file", postgresWithSecretFile, ir.Probe{}, ResultPass, ""},
		{"bundled file", nginxWithBundledFile, ir.Probe{Service: "web", Port: 80, Path: "/ready", Expect: accepted}, ResultPass, ""},
	}
	if os.Getenv("COMPOSEWRIGHT_PACKAGED_TESTS") == "1" {
		t.Cleanup(func() { assertNoDockerSandboxes(t, sandbox.run) })
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict, err := sandbox.Validate(context.Background(), Input{Project: "dockertest-" + testCase.name, Compose: []byte(testCase.compose),
				Env: []byte(""), SecretFiles: map[string][]byte{"secrets/postgres_password": []byte("s3cret-from-a-file")},
				ConfigFiles: map[string][]byte{"config/default.conf": []byte(bundledNginxConfig)},
				Published:   map[string][]string{"web": {"80/tcp"}}, Probe: testCase.probe})
			if err != nil {
				t.Fatal(err)
			}
			if verdict.Result != testCase.result || verdict.Reason != testCase.reason {
				t.Fatalf("verdict %s/%s (%s)\nstart output:\n%s\nlogs:\n%s", verdict.Result, verdict.Reason, verdict.Detail, verdict.StartOutput, verdict.Logs)
			}
			if testCase.name == "healthy" {
				assertLoopbackEphemeral(t, verdict)
				assertPeaks(t, verdict)
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

// TestDockerIgnoredLogLine: the text of a line that failed a real run,
// kept as normal for the app, lets the same stack pass.
func TestDockerIgnoredLogLine(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_DOCKER_TESTS") == "" {
		t.Skip("set COMPOSEWRIGHT_DOCKER_TESTS=1 to run against a real Docker daemon")
	}
	docker, err := FindDocker("")
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	sandbox := New(ExecRunner(docker, ""), limits, 15*time.Second, "policy", t.TempDir())
	input := Input{Project: "dockertest-ignored", Env: []byte(""),
		Compose: []byte("services:\n  app:\n    image: docker.io/library/alpine:3.20\n    command: [\"sh\", \"-c\", \"echo 'FATAL: optional plugin missing'; sleep 300\"]\n")}
	failed, err := sandbox.Validate(context.Background(), input)
	if err != nil || failed.Reason != ReasonLogs || len(failed.LogMatches) != 1 {
		t.Fatalf("first run: %+v %v", failed, err)
	}
	message := LogMessage(failed.LogMatches[0])
	if message != "FATAL: optional plugin missing" {
		t.Fatalf("the line %q reads as %q", failed.LogMatches[0], message)
	}
	input.Benign = []string{regexp.QuoteMeta(message)}
	passed, err := sandbox.Validate(context.Background(), input)
	if err != nil || !passed.Passed() {
		t.Fatalf("with the line ignored: %s/%s %v %v", passed.Result, passed.Reason, passed.LogMatches, err)
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

// assertPeaks checks that a passing run measured every service.
func assertPeaks(t *testing.T, verdict Verdict) {
	t.Helper()
	for _, state := range verdict.Services {
		if state.PeakMemoryBytes <= 0 {
			t.Fatalf("no peak memory for %s: %+v", state.Service, state)
		}
	}
}
