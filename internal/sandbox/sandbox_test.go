package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
)

const testCompose = `services:
  app:
    image: docker.io/library/nginx:1.27
    ports: ["127.0.0.1:8080:80/tcp"]
    networks: [default, npm_default]
    volumes: ["/srv/docker/app/data:/data"]
    mem_limit: 8g
    cpus: "4"
    pids_limit: 4096
  db:
    image: docker.io/library/postgres:17
    volumes: ["db-data:/var/lib/postgresql/data"]
networks:
  npm_default: {external: true}
volumes:
  db-data: {}
`

type fakeDocker struct {
	mutex     sync.Mutex
	calls     [][]string
	upErr     error
	upOutput  string
	ps        string
	psErr     error
	logs      string
	probe     string
	probeErr  error
	directory string
	envMode   os.FileMode
}

func (fake *fakeDocker) run(_ context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.calls = append(fake.calls, arguments)
	command := strings.Join(arguments, " ")
	switch {
	case strings.Contains(command, " up "):
		fake.directory = directory
		if info, err := os.Stat(filepath.Join(directory, envFile)); err == nil {
			fake.envMode = info.Mode().Perm()
		}
		if live != nil {
			_, _ = io.WriteString(live, fake.upOutput+"\n")
		}
		return []byte(fake.upOutput), nil, fake.upErr
	case strings.Contains(command, " run "):
		return []byte(fake.probe), []byte("Error response from daemon: pull access denied for busybox"), fake.probeErr
	case strings.Contains(command, " ps "):
		return []byte(fake.ps), nil, fake.psErr
	case strings.Contains(command, " logs "):
		return []byte(fake.logs), nil, nil
	case strings.HasPrefix(command, "info"):
		return []byte(`{"SecurityOptions":["name=seccomp"],"CgroupVersion":"2","CgroupDriver":"systemd"}`), nil, nil
	}
	return nil, nil, nil
}

func (fake *fakeDocker) called(fragment string) bool {
	for _, call := range fake.calls {
		if strings.Contains(strings.Join(call, " "), fragment) {
			return true
		}
	}
	return false
}

func newTestSandbox(t *testing.T, fake *fakeDocker) *Sandbox {
	t.Helper()
	limits, err := ParseLimits("2g", 2, 512)
	if err != nil {
		t.Fatal(err)
	}
	return New(fake.run, limits, 30*time.Second, "policy", t.TempDir())
}

const healthyApp = `{"Service":"app","Name":"cw-x-app-1","State":"running","Health":"healthy","ExitCode":0,"Publishers":[{"URL":"127.0.0.1","TargetPort":80,"PublishedPort":49153,"Protocol":"tcp"}]}`
const healthyDB = `{"Service":"db","Name":"cw-x-db-1","State":"running","Health":"healthy","ExitCode":0}`

func TestValidateVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		fake   *fakeDocker
		result string
		reason string
	}{
		{"pass", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | started\n"}, ResultPass, ""},
		{"crash", &fakeDocker{upErr: errors.New("exit 1"), upOutput: "container cw-x-app-1 exited (1)",
			ps: `{"Service":"app","State":"exited","ExitCode":1}` + "\n" + healthyDB}, ResultFail, ReasonCrash},
		{"unhealthy", &fakeDocker{upErr: errors.New("exit 1"), upOutput: "container cw-x-app-1 is unhealthy",
			ps: `{"Service":"app","State":"running","Health":"unhealthy"}`}, ResultFail, ReasonUnhealthy},
		{"timeout", &fakeDocker{upErr: errors.New("exit 1"), upOutput: "application not healthy after 30s",
			ps: `{"Service":"app","State":"running","Health":"starting"}`}, ResultFail, ReasonTimeout},
		{"start", &fakeDocker{upErr: errors.New("exit 1"), upOutput: "Error response from daemon: manifest unknown"}, ResultFail, ReasonStart},
		{"logs", &fakeDocker{ps: healthyApp, logs: "app-1  | panic: runtime error\n"}, ResultFail, ReasonLogs},
		{"refused once is fine", &fakeDocker{ps: healthyApp, logs: "app-1  | dial tcp: connection refused\n"}, ResultPass, ""},
		{"refused repeatedly fails", &fakeDocker{ps: healthyApp, logs: strings.Repeat("app-1  | dial tcp db:5432: connection refused\n", 3)}, ResultFail, ReasonLogs},
		{"postgres startup FATAL is benign", &fakeDocker{ps: healthyDB, logs: "db-1  | FATAL:  the database system is starting up\n"}, ResultPass, ""},
		{"info line mentioning a missing file is benign", &fakeDocker{ps: healthyApp,
			logs: "app-1  | \x1b[36m2026-10-10\x1b[0m [SETUP] \x1b[36mINFO:\x1b[0m db-config.json is not found: ENOENT: no such file or directory\n"}, ResultPass, ""},
		{"error line with a missing file fails", &fakeDocker{ps: healthyApp, logs: "app-1  | ERROR open /config/app.yml: no such file or directory\n"}, ResultFail, ReasonLogs},
		{"structured info level is benign", &fakeDocker{ps: healthyApp, logs: `app-1  | {"level":"info","msg":"permission denied for optional cache, continuing"}` + "\n"}, ResultPass, ""},
		{"init service may exit 0", &fakeDocker{ps: `{"Service":"migrate","State":"exited","ExitCode":0}` + "\n" + healthyApp}, ResultPass, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := testCase.fake
			sandbox := newTestSandbox(t, fake)
			verdict, err := sandbox.Validate(context.Background(), Input{Project: "Mini Flux", Compose: []byte(testCompose), Env: []byte("A=1\n"),
				Published: map[string][]string{"app": {"80/tcp"}}, CompletedOK: map[string]bool{"migrate": true}, Iteration: 1})
			if err != nil {
				t.Fatal(err)
			}
			if verdict.Result != testCase.result || verdict.Reason != testCase.reason {
				t.Fatalf("verdict %s/%s (%s), want %s/%s", verdict.Result, verdict.Reason, verdict.Detail, testCase.result, testCase.reason)
			}
			if !strings.HasPrefix(verdict.Project, "cw-mini-flux-") || !verdict.Sandbox.LimitsEnforced {
				t.Fatalf("project %q sandbox %+v", verdict.Project, verdict.Sandbox)
			}
			if !fake.called("down -v --remove-orphans") {
				t.Fatal("the sandbox was not taken down")
			}
			if fake.envMode != runFileMode {
				t.Fatalf(".env mode during the run: %04o", fake.envMode)
			}
			if _, err := os.Stat(fake.directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("run directory with .env left behind: %v", err)
			}
		})
	}
}

func TestValidateTearsDownWhenStatesCannotBeRead(t *testing.T) {
	fake := &fakeDocker{psErr: errors.New("daemon gone")}
	if _, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")}); err == nil {
		t.Fatal("unreadable states accepted")
	}
	if !fake.called("down -v --remove-orphans") {
		t.Fatal("teardown skipped after an error")
	}
}

func TestValidateScrubsSecretsFromLogs(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp, logs: "app-1  | connecting with password=0123456789abcdef and token s3cr3t-value-xyz\n"}
	verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""),
		Secrets: []string{"s3cr3t-value-xyz"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(verdict.Logs, "0123456789abcdef") || strings.Contains(verdict.Logs, "s3cr3t-value-xyz") || strings.Contains(verdict.Logs, "\x1b") {
		t.Fatalf("secret in stored logs: %s", verdict.Logs)
	}
}

func TestJanitorRemovesOnlyStaleSandboxes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-2 * time.Hour).Unix()
	recent := now.Add(-10 * time.Second).Unix()
	listing := strings.Join([]string{
		`"io.composewright.sandbox=cw-old-1a2b,io.composewright.started=` + itoa(old) + `,com.docker.compose.project=cw-old-1a2b"`,
		`"io.composewright.sandbox=cw-new-3c4d,io.composewright.started=` + itoa(recent) + `"`,
		`"io.composewright.sandbox=other,io.composewright.started=` + itoa(old) + `"`,
	}, "\n")
	var calls []string
	run := func(_ context.Context, _ string, _ io.Writer, arguments ...string) ([]byte, []byte, error) {
		calls = append(calls, strings.Join(arguments, " "))
		if arguments[0] == "ps" {
			return []byte(listing), nil, nil
		}
		return nil, nil, nil
	}
	limits, _ := ParseLimits("1g", 1, 100)
	sandbox := New(run, limits, 5*time.Minute, "policy", t.TempDir())
	sandbox.now = func() time.Time { return now }
	removed, err := sandbox.Janitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "cw-old-1a2b" || !strings.Contains(strings.Join(calls, "\n"), "compose -p cw-old-1a2b down -v") {
		t.Fatalf("removed %v, calls %v", removed, calls)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestParseStatesAcceptsLinesAndArrays(t *testing.T) {
	lines, err := parseStates([]byte(healthyApp + "\n" + healthyDB + "\n"))
	if err != nil || len(lines) != 2 || lines[0].Publishers[0].PublishedPort != 49153 {
		t.Fatalf("lines: %+v %v", lines, err)
	}
	array, err := parseStates([]byte("[" + healthyApp + "," + healthyDB + "]"))
	if err != nil || len(array) != 2 {
		t.Fatalf("array: %+v %v", array, err)
	}
}

func TestOverrideKeepsTheRunInsideTheSandbox(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp}
	sandbox := newTestSandbox(t, fake)
	directory := ""
	sandbox.run = func(ctx context.Context, dir string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(arguments, " "), " up ") {
			directory = dir
			content, _ := os.ReadFile(filepath.Join(dir, overrideFile))
			text := string(content)
			for _, want := range []string{`restart: "no"`, `- "127.0.0.1:0:80/tcp"`, "npm_default: !reset null", "mem_limit: 2147483648",
				`cpus: "2"`, "pids_limit: 512", "cw-bind-app-0:/data", "security_opt: !override", "io.composewright.sandbox",
				probeService + ":", `image: "` + probeImage + `"`, `- "` + probeService + `"`, `user: "65534:65534"`, "read_only: true", "- ALL", "mem_limit: 33554432"} {
				if !strings.Contains(text, want) {
					t.Errorf("override lacks %q:\n%s", want, text)
				}
			}
		}
		return fake.run(ctx, dir, live, arguments...)
	}
	if _, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""),
		Published: map[string][]string{"app": {"80/tcp"}}}); err != nil {
		t.Fatal(err)
	}
	if directory == "" {
		t.Fatal("up never ran")
	}
}

func TestAtLeast(t *testing.T) {
	cases := map[string]bool{"2.24.4": true, "2.24.3": false, "2.25.0": true, "5.6.0": true, "1.29.2": false, "2.24": false, "2.24.4-desktop.1": true}
	for version, want := range cases {
		if got := AtLeast(version, minimumCompose); got != want {
			t.Errorf("AtLeast(%q) = %v", version, got)
		}
	}
}

// TestStartProbesKeepTheCheckAndItsTolerance: merged the way Compose merges
// the override, a healthcheck keeps its command and interval, probes every
// two seconds during a start period as long as its own interval × retries,
// and a service without a healthcheck gets none.
func TestStartProbesKeepTheCheckAndItsTolerance(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    healthcheck: {test: ["CMD", "check"], interval: 30s}
  plain:
    image: example/plain:1
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, err := Override(project, nil, limits, "cw-app-1234", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("%v\n%s", err, override)
	}
	check := merged.Services["app"].HealthCheck
	if check == nil || check.Test[1] != "check" || time.Duration(*check.Interval) != 30*time.Second ||
		time.Duration(*check.StartPeriod) != 90*time.Second || time.Duration(*check.StartInterval) != earlyProbe {
		t.Fatalf("merged healthcheck: %+v\n%s", check, override)
	}
	if merged.Services["plain"].HealthCheck != nil {
		t.Fatalf("a service without a healthcheck got one:\n%s", override)
	}
}

// TestOverrideKeepsTmpfsAndReadOnlyMounts: once a bind mount makes the
// override replace a service's volumes, merged the way Compose merges, the
// bind becomes a throw-away volume, a read-only volume stays read-only, and
// a tmpfs and an anonymous volume, which have no source, are kept.
func TestOverrideKeepsTmpfsAndReadOnlyMounts(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    volumes:
      - ./data:/data
      - cache:/cache:ro
      - type: tmpfs
        target: /tmp/scratch
      - /anonymous
volumes:
  cache: {}
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, err := Override(project, nil, limits, "cw-app-1234", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("Compose refuses the override: %v\n%s", err, override)
	}
	mounts := map[string]types.ServiceVolumeConfig{}
	for _, volume := range merged.Services["app"].Volumes {
		mounts[volume.Target] = volume
	}
	data, cache, scratch, anonymous := mounts["/data"], mounts["/cache"], mounts["/tmp/scratch"], mounts["/anonymous"]
	if len(mounts) != 4 || data.Type != types.VolumeTypeVolume || !strings.HasPrefix(data.Source, sandboxVolume) ||
		!cache.ReadOnly || cache.Source != "cache" || scratch.Type != types.VolumeTypeTmpfs || anonymous.Type != types.VolumeTypeVolume {
		t.Fatalf("merged mounts %+v\n%s", merged.Services["app"].Volumes, override)
	}
}

// TestLastHealthOutput keeps what the latest check printed, without secrets.
func TestLastHealthOutput(t *testing.T) {
	inspected := []byte(`{"Status":"unhealthy","FailingStreak":3,"Log":[{"ExitCode":1,"Output":"first try"},` +
		`{"ExitCode":127,"Output":"/bin/sh: curl: not found, token hunter2hunter2\n"}]}`)
	said := lastHealthOutput(inspected, []string{"hunter2hunter2"})
	if !strings.HasPrefix(said, "/bin/sh: curl: not found") || strings.Contains(said, "hunter2hunter2") {
		t.Fatalf("health output %q", said)
	}
	if said := lastHealthOutput([]byte(`{"Status":"starting","Log":[]}`), nil); said != "" {
		t.Fatalf("no checks yet, but %q", said)
	}
}

// TestValidateReachesTheApp: once every service is up, the probe asks the
// app from another container on the sandbox network. An HTTP status is an
// answer; a refused or timed-out connection fails the run.
func TestValidateReachesTheApp(t *testing.T) {
	const runningApp = `{"Service":"app","Name":"cw-x-app-1","State":"running","Health":"","ExitCode":0}`
	const refused = "wget: can't connect to remote host (172.18.0.2): Connection refused\n"
	cases := []struct {
		name    string
		fake    *fakeDocker
		expect  []int
		result  string
		checked bool
		said    string
		window  string
	}{
		{"answers", &fakeDocker{ps: healthyApp, probe: "  HTTP/1.1 200 OK\n  Content-Type: text/html\n"}, nil, ResultPass, true, "http://app:80/ answered 200", "10"},
		{"refuses", &fakeDocker{ps: healthyApp, probe: refused}, nil, ResultFail, true, "nothing answers on app:80 from the sandbox network; is it listening on 127.0.0.1", "10"},
		{"times out", &fakeDocker{ps: runningApp, probe: "wget: download timed out\n"}, nil, ResultFail, true, "nothing answers on app:80", "30"},
		{"no HTTP answer", &fakeDocker{ps: healthyApp, probe: "wget: error getting response\n"}, nil, ResultPass, true, "took the connection but sent no HTTP answer", "10"},
		{"redirect elsewhere", &fakeDocker{ps: healthyApp, probe: "  HTTP/1.1 302 Found\n  Location: http://sso.example/\nwget: bad address 'sso.example'\n"},
			nil, ResultPass, true, "answered 302", "10"},
		{"unexpected status", &fakeDocker{ps: healthyApp, probe: "  HTTP/1.1 404 Not Found\nwget: server returned error: HTTP/1.1 404 Not Found\n"},
			[]int{200}, ResultFail, true, "answered 404; the plan expects one of [200]", "10"},
		{"no address of its own", &fakeDocker{ps: healthyApp, probe: "wget: bad address 'app:80'\n"}, nil, ResultPass, false, "not checked: app has no address", "10"},
		{"probe did not run", &fakeDocker{ps: healthyApp, probeErr: errors.New("exit status 1")}, nil, ResultPass, false, "pull access denied", "10"},
		{"earlier failure", &fakeDocker{ps: healthyApp, logs: "app-1  | panic: boom\n", probe: refused}, nil, ResultFail, false, notRun, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := testCase.fake
			verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""),
				Probe: ir.Probe{Service: "app", Port: 80, Expect: testCase.expect}})
			if err != nil {
				t.Fatal(err)
			}
			reach := verdict.Reach
			if verdict.Result != testCase.result || reach == nil || reach.Checked != testCase.checked || !strings.Contains(reach.Said, testCase.said) {
				t.Fatalf("verdict %s/%s (%s), reach %+v", verdict.Result, verdict.Reason, verdict.Detail, reach)
			}
			if verdict.Result == ResultFail && testCase.said != notRun && (verdict.Reason != ReasonUnreachable || verdict.Detail != reach.Said) {
				t.Fatalf("failed as %s: %s", verdict.Reason, verdict.Detail)
			}
			window := "run --rm -T " + probeService + " sh -c " + probeScript + " " + probeService + " http://app:80/ " + testCase.window + " 2 5"
			if testCase.window != "" && !fake.called(window) {
				t.Fatalf("the probe did not run as expected; calls: %v", fake.calls)
			}
			if testCase.window == "" && fake.called(" run ") {
				t.Fatal("the probe ran after an earlier check failed")
			}
		})
	}
}

// TestValidateWithoutProbe: a plan without a probe records none.
// TestProbeScriptStopsAtTheFirstAnswer runs the probe script with a wget
// that gets a redirect to the app's public address, which the sandbox
// network refuses: the status line answers, so it asks once.
func TestProbeScriptStopsAtTheFirstAnswer(t *testing.T) {
	directory := t.TempDir()
	calls := filepath.Join(directory, "calls")
	fakeWget := "#!/bin/sh\necho call >> " + calls + "\necho '  HTTP/1.1 302 Found' >&2\n" +
		"echo 'wget: can'\\''t connect to remote host (127.0.0.1): Connection refused' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(directory, "wget"), []byte(fakeWget), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", probeScript, probeService, "http://app:80/", "30", "1", "5")
	command.Env = append(os.Environ(), "PATH="+directory+":"+os.Getenv("PATH"))
	said, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	asked, _ := os.ReadFile(calls)
	if strings.Count(string(asked), "call") != 1 || !strings.Contains(string(said), "HTTP/1.1 302") {
		t.Fatalf("asked %d times, said %q", strings.Count(string(asked), "call"), said)
	}
}

func TestValidateWithoutProbe(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp}
	verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")})
	if err != nil || !verdict.Passed() || verdict.Reach != nil || fake.called(" run ") {
		t.Fatalf("verdict %+v, err %v", verdict, err)
	}
}

// TestValidateCancelledDuringProbe: a run cancelled while the probe waits
// is an error, never a pass, and is still taken down.
func TestValidateCancelledDuringProbe(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp}
	sandbox := newTestSandbox(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sandbox.run = func(runCtx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(arguments, " "), " run ") {
			cancel()
			return nil, nil, context.Canceled
		}
		return fake.run(runCtx, directory, live, arguments...)
	}
	_, err := sandbox.Validate(ctx, Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""), Probe: ir.Probe{Service: "app", Port: 80}})
	if !errors.Is(err, context.Canceled) || !fake.called("down -v --remove-orphans") {
		t.Fatalf("err %v, taken down %v", err, fake.called("down -v --remove-orphans"))
	}
}

// TestLimitedBufferKeepsTheEnd: past its limit the buffer keeps the newest
// bytes, where a failing service's last lines are, whether they arrive in
// small writes or in one write larger than the limit.
func TestLimitedBufferKeepsTheEnd(t *testing.T) {
	const limit, chunk, total, period = 1 << 20, 64 << 10, 3 << 20, 251
	written := make([]byte, 0, total)
	chunked := &limitedBuffer{limit: limit}
	for len(written) < total {
		data := make([]byte, chunk)
		for index := range data {
			data[index] = byte((len(written) + index) % period)
		}
		if _, err := chunked.Write(data); err != nil {
			t.Fatal(err)
		}
		written = append(written, data...)
	}
	newest := written[len(written)-limit:]
	if !bytes.Equal(chunked.Bytes(), newest) {
		t.Fatalf("kept %d bytes that are not the last %d written", chunked.Len(), limit)
	}
	whole := &limitedBuffer{limit: limit}
	if _, err := whole.Write(written); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(whole.Bytes(), newest) {
		t.Fatalf("one large write kept %d bytes that are not its last %d", whole.Len(), limit)
	}
}
