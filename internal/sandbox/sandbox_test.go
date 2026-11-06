package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
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
	mutex    sync.Mutex
	calls    [][]string
	upErr    error
	upOutput string
	ps       string
	psErr    error
	logs     string
	stats    string
	statsErr error
	// sampledWhileUp, when set, holds `up` open until a stats call made
	// while it runs closes it; stats reports stats then, and settledStats
	// once up has returned, when set.
	sampledWhileUp chan struct{}
	upRunning      bool
	settledStats   string
	probe          string
	probeErr       error
	directory      string
	envMode        os.FileMode
}

// upHoldLimit ends a held `up` when no sample comes, so a broken sampler
// fails its test instead of hanging it.
const upHoldLimit = 10 * time.Second

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
		sampled := fake.sampledWhileUp
		fake.upRunning = true
		fake.mutex.Unlock()
		if sampled != nil {
			select {
			case <-sampled:
			case <-time.After(upHoldLimit):
			}
		}
		fake.mutex.Lock()
		fake.upRunning = false
		return []byte(fake.upOutput), nil, fake.upErr
	case strings.HasPrefix(command, "stats ") && !fake.upRunning && fake.settledStats != "":
		return []byte(fake.settledStats), nil, fake.statsErr
	case strings.HasPrefix(command, "stats "):
		if fake.upRunning && fake.sampledWhileUp != nil {
			close(fake.sampledWhileUp)
			fake.sampledWhileUp = nil
		}
		return []byte(fake.stats), nil, fake.statsErr
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
		{"logs", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | panic: runtime error\n"}, ResultFail, ReasonLogs},
		{"refused once is fine", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | dial tcp: connection refused\n"}, ResultPass, ""},
		{"refused repeatedly fails", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: strings.Repeat("app-1  | dial tcp db:5432: connection refused\n", 3)}, ResultFail, ReasonLogs},
		{"postgres startup FATAL is benign", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "db-1  | FATAL:  the database system is starting up\n"}, ResultPass, ""},
		{"info line mentioning a missing file is benign", &fakeDocker{ps: healthyApp + "\n" + healthyDB,
			logs: "app-1  | \x1b[36m2026-10-10\x1b[0m [SETUP] \x1b[36mINFO:\x1b[0m db-config.json is not found: ENOENT: no such file or directory\n"}, ResultPass, ""},
		{"error line with a missing file fails", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | ERROR open /config/app.yml: no such file or directory\n"}, ResultFail, ReasonLogs},
		{"structured info level is benign", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: `app-1  | {"level":"info","msg":"permission denied for optional cache, continuing"}` + "\n"}, ResultPass, ""},
		{"init service may exit 0", &fakeDocker{ps: `{"Service":"migrate","State":"exited","ExitCode":0}` + "\n" + healthyApp + "\n" + healthyDB}, ResultPass, ""},
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
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | connecting with password=0123456789abcdef and token s3cr3t-value-xyz\n"}
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
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
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
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, "", nil)
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
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, "", nil)
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

// TestOverrideNeverPassesTheDockerSocket: a granted socket is a bind mount,
// so the sandbox run gets an empty throw-away volume in its place and the app
// never reaches Docker before the user has seen it pass.
func TestOverrideNeverPassesTheDockerSocket(t *testing.T) {
	compose := []byte("services:\n  app:\n    image: example/app:1\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n")
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("Compose refuses the override: %v\n%s", err, override)
	}
	for _, volume := range merged.Services["app"].Volumes {
		if volume.Type == types.VolumeTypeBind || !strings.HasPrefix(volume.Source, sandboxVolume) {
			t.Fatalf("the sandbox would reach the socket: %+v\n%s", volume, override)
		}
	}
}

// TestPullsPinnedImagesOnlyWhenMissing: a digest names exact content, so a
// run whose every image is pinned uses a local copy and leaves the registry
// alone; one image named by tag alone makes the run pull them all.
func TestPullsPinnedImagesOnlyWhenMissing(t *testing.T) {
	const digest = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pinnedApp := strings.Replace(testCompose, "nginx:1.27", "nginx:1.27"+digest, 1)
	cases := map[string]struct{ compose, pull string }{
		"every image pinned": {strings.Replace(pinnedApp, "postgres:17", "postgres:17"+digest, 1), "missing"},
		"one image by tag":   {pinnedApp, "always"},
		"no image pinned":    {testCompose, "always"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
			sandbox := newTestSandbox(t, fake)
			input := Input{Project: "app", Compose: []byte(testCase.compose), Env: []byte(""), Published: map[string][]string{"app": {"80/tcp"}}}
			if _, err := sandbox.Validate(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			if !fake.called("--pull " + testCase.pull) {
				t.Fatalf("up without --pull %s: %v", testCase.pull, fake.calls)
			}
		})
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
		{"answers", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 200 OK\n  Content-Type: text/html\n"}, nil, ResultPass, true, "http://app:80/ answered 200", "10"},
		{"refuses", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: refused}, nil, ResultFail, true, "nothing answers on app:80 from the sandbox network; is it listening on 127.0.0.1", "10"},
		{"times out", &fakeDocker{ps: runningApp + "\n" + healthyDB, probe: "wget: download timed out\n"}, nil, ResultFail, true, "nothing answers on app:80", "30"},
		{"no HTTP answer", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "wget: error getting response\n"}, nil, ResultPass, false, "not checked: app:80 took the connection but sent no HTTP answer", "10"},
		{"server error", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 500 Internal Server Error\n"}, nil, ResultFail, true, "answered 500; a server error", "10"},
		{"server error the plan expects", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 503 Service Unavailable\n"}, []int{503}, ResultPass, true, "answered 503", "10"},
		{"redirect elsewhere", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 302 Found\n  Location: http://sso.example/\nwget: bad address 'sso.example'\n"},
			nil, ResultPass, true, "answered 302", "10"},
		{"unexpected status", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 404 Not Found\nwget: server returned error: HTTP/1.1 404 Not Found\n"},
			[]int{200}, ResultFail, true, "answered 404; the plan expects / to answer one of 200", "10"},
		{"no address of its own", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "wget: bad address 'app:80'\n"}, nil, ResultPass, false, "not checked: app has no address", "10"},
		{"probe did not run", &fakeDocker{ps: healthyApp + "\n" + healthyDB, probeErr: errors.New("exit status 1")}, nil, ResultPass, false, "pull access denied", "10"},
		{"earlier failure", &fakeDocker{ps: healthyApp + "\n" + healthyDB, logs: "app-1  | panic: boom\n", probe: refused}, nil, ResultFail, false, notRun, ""},
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
			unproven := slices.ContainsFunc(verdict.Unproven, func(item string) bool { return strings.Contains(item, testCase.said) })
			if verdict.Passed() && reach != nil && !reach.Checked && !unproven {
				t.Fatalf("an unchecked probe left the pass unqualified: %q", verdict.Unproven)
			}
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

// TestReadReachChecksTheExpectedStatus: with statuses expected, any other
// answer on the probe path fails and names the path; without, any answers.
func TestReadReachChecksTheExpectedStatus(t *testing.T) {
	accepted := []int{200, 204, 302}
	const url = "http://app:8080/healthz"
	cases := []struct {
		name   string
		expect []int
		said   string
		failed bool
		want   string
	}{
		{"404 expected away", accepted, "  HTTP/1.1 404 Not Found\n", true, url + " answered 404; the plan expects /healthz to answer one of 200, 204, 302"},
		{"500 expected away", accepted, "  HTTP/1.1 500 Internal Server Error\n", true, "answered 500"},
		{"200 expected", accepted, "  HTTP/1.1 200 OK\n", false, url + " answered 200"},
		{"redirect expected", accepted, "  HTTP/1.1 302 Found\n", false, url + " answered 302"},
		{"404 with nothing expected", nil, "  HTTP/1.1 404 Not Found\n", false, url + " answered 404"},
	}
	for _, testCase := range cases {
		reach, failed := readReach(ir.Probe{Service: "app", Port: 8080, Path: "/healthz", Expect: testCase.expect}, url, testCase.said)
		if failed != testCase.failed || !strings.Contains(reach.Said, testCase.want) || !reach.Checked {
			t.Errorf("%s: failed %v, said %q", testCase.name, failed, reach.Said)
		}
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
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")})
	if err != nil || !verdict.Passed() || verdict.Reach != nil || fake.called(" run ") {
		t.Fatalf("verdict %+v, err %v", verdict, err)
	}
}

// TestValidateCancelledDuringProbe: a run cancelled while the probe waits
// is an error, never a pass, and is still taken down.
func TestValidateCancelledDuringProbe(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
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

func TestOverrideRunsEveryContainerUnderTheRuntime(t *testing.T) {
	project, err := composefile.Load(context.Background(), composeFile, []byte(testCompose), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	for runtime, want := range map[string]string{runscRuntime: runscRuntime, "": ""} {
		override, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, runtime, nil)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Services map[string]struct{ Runtime string }
		}
		if err := yaml.Unmarshal(override, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"app", "db", probeService} {
			if got := parsed.Services[name].Runtime; got != want {
				t.Errorf("runtime %q: %s runs under %q\n%s", runtime, name, got, override)
			}
		}
		details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
			ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: []byte(testCompose)}, {Filename: overrideFile, Content: override}}}
		merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
		if err != nil || merged.Services["app"].Runtime != want {
			t.Fatalf("runtime %q after the merge: %v %+v", runtime, err, merged)
		}
	}
}

func TestInfoReadsTheRuntimes(t *testing.T) {
	run := func(context.Context, string, io.Writer, ...string) ([]byte, []byte, error) {
		return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd","Runtimes":{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}}`), nil, nil
	}
	environment, err := New(run, Limits{}, time.Second, "runsc", t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment.Runtimes, ",") != "runc,runsc" || environment.MissingRuntime() != nil {
		t.Fatalf("runtimes %v, missing %v", environment.Runtimes, environment.MissingRuntime())
	}
}

func TestMissingRuntime(t *testing.T) {
	cases := []struct {
		environment Environment
		missing     bool
	}{
		{Environment{Tier: "policy"}, false},
		{Environment{Tier: "runsc", Runtimes: []string{"runc", "runsc"}}, false},
		{Environment{Tier: "runsc", Runtimes: []string{"runc"}}, true},
	}
	for _, testCase := range cases {
		if err := testCase.environment.MissingRuntime(); (err != nil) != testCase.missing {
			t.Errorf("%+v: %v, want missing %v", testCase.environment, err, testCase.missing)
		}
	}
}

// TestValidateRefusesRunscWithoutGVisor: the gVisor tier never falls back
// to the default runtime; the run does not start.
func TestValidateRefusesRunscWithoutGVisor(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	sandbox := newTestSandbox(t, fake)
	sandbox.tier = "runsc"
	_, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose)})
	if !errors.Is(err, ErrRunscMissing) || fake.called(" up ") {
		t.Fatalf("err %v, up called %v", err, fake.called(" up "))
	}
}

// TestValidateRunsUnderGVisorOnTheRunscTier: a run on the gVisor tier writes
// an override that puts every container, the probe included, under runsc,
// and its verdict records that tier.
func TestValidateRunsUnderGVisorOnTheRunscTier(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	sandbox := newTestSandbox(t, fake)
	sandbox.tier = runscRuntime
	upRan := false
	sandbox.run = func(ctx context.Context, dir string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := strings.Join(arguments, " ")
		if strings.HasPrefix(command, "info") {
			return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd","Runtimes":{"runc":{},"runsc":{}}}`), nil, nil
		}
		if strings.Contains(command, " up ") {
			upRan = true
			content, err := os.ReadFile(filepath.Join(dir, overrideFile))
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Services map[string]struct{ Runtime string }
			}
			if err := yaml.Unmarshal(content, &parsed); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"app", "db", probeService} {
				if got := parsed.Services[name].Runtime; got != runscRuntime {
					t.Errorf("%s runs under %q\n%s", name, got, content)
				}
			}
		}
		return fake.run(ctx, dir, live, arguments...)
	}
	verdict, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""),
		Published: map[string][]string{"app": {"80/tcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !upRan || verdict.Sandbox.Tier != runscRuntime {
		t.Fatalf("up ran %v, verdict tier %q", upRan, verdict.Sandbox.Tier)
	}
}

// TestOverrideCapsSharedMemory: merged the way Compose merges, a shm_size
// above the sandbox memory limit is lowered to it, a smaller one is kept,
// and a service without one gets none.
func TestOverrideCapsSharedMemory(t *testing.T) {
	compose := []byte(`services:
  large:
    image: example/large:1
    shm_size: 1g
  small:
    image: example/small:1
    shm_size: 64m
  plain:
    image: example/plain:1
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("%v\n%s", err, override)
	}
	large, small, plain := merged.Services["large"].ShmSize, merged.Services["small"].ShmSize, merged.Services["plain"].ShmSize
	if int64(large) != limits.MemoryBytes || small != 64*units.MiB || plain != 0 {
		t.Fatalf("shm_size large %d, small %d, plain %d\n%s", large, small, plain, override)
	}
}

func TestParseContainersReadsHostPortsAndLabels(t *testing.T) {
	output := `{"Names":"uptime-kuma","Image":"louislam/uptime-kuma:1","Networks":"bridge","Ports":"0.0.0.0:3001->3001/tcp, [::]:3001->3001/tcp","Labels":"com.docker.compose.project=kuma,maintainer=x"}
{"Names":"db","Image":"postgres:16","Networks":"","Ports":"5432/tcp","Labels":""}
{"Names":"range","Image":"nginx","Networks":"","Ports":"127.0.0.1:8000-8001->8000-8001/tcp","Labels":""}`
	containers, err := parseContainers([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	wantPorts := [][]int{{3001}, nil, {8000, 8001}}
	for index, container := range containers {
		if !slices.Equal(container.Ports, wantPorts[index]) {
			t.Errorf("%s ports = %v, want %v", container.Name, container.Ports, wantPorts[index])
		}
	}
	if project := containers[0].Labels[LabelComposeProject]; project != "kuma" {
		t.Errorf("compose project label = %q, want kuma", project)
	}
}

// TestPlatformNoteOnlyWhenTheTargetDiffers: the run says when the files
// target another platform than the sandbox's own.
func TestPlatformNoteOnlyWhenTheTargetDiffers(t *testing.T) {
	if note := platformNote(""); note != "" {
		t.Errorf("no platform chosen: %q", note)
	}
	if note := platformNote(config.HostPlatform()); note != "" {
		t.Errorf("this machine's platform: %q", note)
	}
	other := "linux/riscv64"
	if note := platformNote(other); !strings.Contains(note, "the files target "+other) || !strings.Contains(note, "this sandbox runs "+config.HostPlatform()) {
		t.Errorf("another platform: %q", note)
	}
}

// TestRunFilesAreCopiedNotBound: a secret and a bundled config file reach
// their services as configs the override carries, at the paths compose.yaml
// gives them, since a bind would send the Docker daemon to a run folder that
// exists only inside a containerised composewright. A $ survives Compose's
// interpolation.
func TestRunFilesAreCopiedNotBound(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    volumes: ["./config/app.conf:/etc/app/app.conf:ro", "data:/data"]
    secrets: [db_password, smtp_password]
volumes:
  data: {}
secrets:
  db_password:
    file: ./secrets/db_password
  smtp_password:
    file: ./secrets/smtp_password
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	files := map[string][]byte{"secrets/db_password": []byte("pa$word"), "config/app.conf": []byte("listen 80\n"), "secrets/smtp_password": nil}
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, false, "", files)
	if err != nil {
		t.Fatal(err)
	}
	text := string(override)
	for _, want := range []string{`target: "/etc/app/app.conf"`, `target: "/run/secrets/db_password"`, `content: "pa$$word"`, `content: "listen 80\n"`, "secrets: !reset []"} {
		if !strings.Contains(text, want) {
			t.Errorf("the override lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "./config/app.conf") {
		t.Errorf("the bundled file is still bound:\n%s", text)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("the override does not merge: %v", err)
	}
	app := merged.Services["app"]
	// An empty file, a secret not supplied yet, is /dev/null: Compose would
	// bind its project folder for empty content.
	if len(app.Secrets) != 0 || len(app.Configs) != 3 || merged.Configs["cw-file-1"].Content != "pa$word" || merged.Configs["cw-file-2"].File != emptyFile {
		t.Fatalf("merged: secrets %v, configs %v, %+v", app.Secrets, app.Configs, merged.Configs)
	}
}

// TestOverrideWithholdsGrantedPrivileges: merged the way Compose merges, a
// test run gets none of the host privileges granted for the deployment, its
// ports stay on loopback even where the file asks for host networking, a
// capability every container has is kept, and Withheld names the rest.
func TestOverrideWithholdsGrantedPrivileges(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    privileged: true
    network_mode: host
    devices: ["/dev/dri:/dev/dri"]
    cap_add: [SYS_ADMIN, CAP_CHOWN, NET_ADMIN]
    cap_drop: [ALL]
  plain:
    image: example/plain:1
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, _, err := Override(project, map[string][]string{"app": {"8080/tcp"}}, limits, "cw-app-1234", 0, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("Compose refuses the override: %v\n%s", err, override)
	}
	app := merged.Services["app"]
	if app.Privileged || app.NetworkMode != "" || len(app.Devices) != 0 || !slices.Equal(app.CapAdd, []string{"CAP_CHOWN"}) {
		t.Fatalf("the run keeps host privileges: privileged %v, network_mode %q, devices %v, cap_add %v\n%s",
			app.Privileged, app.NetworkMode, app.Devices, app.CapAdd, override)
	}
	if len(app.Ports) != 1 || app.Ports[0].HostIP != loopbackAddress || len(app.Networks) != 1 {
		t.Fatalf("ports %+v, networks %v\n%s", app.Ports, app.Networks, override)
	}
	want := []string{"app: privileged mode", "app: network_mode host", "app: device /dev/dri", "app: capability SYS_ADMIN", "app: capability NET_ADMIN"}
	if withheld := Withheld(project); !slices.Equal(withheld, want) {
		t.Fatalf("withheld %q, want %q", withheld, want)
	}
}

// TestOverrideOwnsEveryResource: merged the way Compose merges, a run never
// reuses a volume, network or container a file names explicitly, since such
// a name is global on the daemon; every one it creates carries the run's
// project and label, so two runs never share one and down -v removes only
// its own, and a name kept for the sandbox's helpers is refused.
func TestOverrideOwnsEveryResource(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    container_name: app
    volumes: [data:/data]
    networks: [default, back]
volumes:
  data:
    name: existing-user-data
networks:
  default:
    name: existing-network
  back:
    name: existing-back
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	for _, sandboxName := range []string{"cw-app-1234", "cw-app-5678"} {
		override, _, err := Override(project, nil, limits, sandboxName, 0, true, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
			ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
		merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName(sandboxName, true) })
		if err != nil {
			t.Fatalf("Compose refuses the override: %v\n%s", err, override)
		}
		if name := merged.Services["app"].ContainerName; name != "" {
			t.Fatalf("the run keeps container_name %q\n%s", name, override)
		}
		volume := merged.Volumes["data"]
		if volume.Name != sandboxName+"_data" || volume.Labels[LabelProject] != sandboxName {
			t.Fatalf("volume %+v\n%s", volume, override)
		}
		for _, key := range []string{"default", "back"} {
			network := merged.Networks[key]
			if network.Name != sandboxName+"_"+key || network.Labels[LabelProject] != sandboxName {
				t.Fatalf("network %s %+v\n%s", key, network, override)
			}
		}
	}
	for _, reserved := range []string{
		"services:\n  " + probeService + ":\n    image: example/app:1\n",
		"services:\n  app:\n    image: example/app:1\n    volumes: [cw-bind-x:/data]\nvolumes:\n  cw-bind-x: {}\n",
	} {
		project, err := composefile.Load(context.Background(), composeFile, []byte(reserved), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, "", nil); err == nil {
			t.Fatalf("a reserved name was accepted:\n%s", reserved)
		}
	}
}

// TestOverrideRunsOneCopyOfEachService: merged the way Compose merges, a
// service the file scales runs once, whether by scale or deploy.replicas,
// and Withheld says it was tested as one.
func TestOverrideRunsOneCopyOfEachService(t *testing.T) {
	compose := []byte("services:\n  app:\n    image: example/app:1\n    deploy:\n      replicas: 1000\n  worker:\n    image: example/worker:1\n    scale: 5\n")
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	override, _, err := Override(project, nil, limits, "cw-app-1234", 0, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("Compose refuses the override: %v\n%s", err, override)
	}
	for _, name := range []string{"app", "worker"} {
		service := merged.Services[name]
		if copies := service.GetScale(); copies != 1 {
			t.Fatalf("%s runs %d copies\n%s", name, copies, override)
		}
	}
	want := []string{"app: 1000 copies, tested as one", "worker: 5 copies, tested as one"}
	if withheld := Withheld(project); !slices.Equal(withheld, want) {
		t.Fatalf("withheld %q", withheld)
	}
}

// TestValidateBoundsEveryDaemonQuery: a daemon that cannot say what it
// enforces stops the run before anything starts, and every question asked
// after up has a deadline, so a stalled daemon ends the run.
func TestValidateBoundsEveryDaemonQuery(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	sandbox := newTestSandbox(t, fake)
	sandbox.run = func(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		if arguments[0] == "info" {
			return nil, []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
		}
		return fake.run(ctx, directory, live, arguments...)
	}
	input := Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""), Published: map[string][]string{"app": {"80/tcp"}}}
	if _, err := sandbox.Validate(context.Background(), input); err == nil || fake.called(" up ") {
		t.Fatalf("a run started without the daemon's limits: %v", err)
	}
	var unbounded []string
	sandbox.run = func(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := strings.Join(arguments, " ")
		deadline, bounded := ctx.Deadline()
		asks := arguments[0] == "info" || arguments[0] == "inspect" || strings.Contains(command, " ps ") || strings.Contains(command, " logs ")
		if asks && (!bounded || time.Until(deadline) > commandTimeout) {
			unbounded = append(unbounded, command)
		}
		return fake.run(ctx, directory, live, arguments...)
	}
	if _, err := sandbox.Validate(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(unbounded) != 0 {
		t.Fatalf("asked without a deadline: %q", unbounded)
	}
}

// TestLiveOutputClipsALineThatNeverEnds: output with no newline is held
// only up to a bound, sent clipped once, and the next line arrives whole.
func TestLiveOutputClipsALineThatNeverEnds(t *testing.T) {
	var sent []string
	writer := &lineWriter{send: func(line string) { sent = append(sent, line) }}
	chunk := strings.Repeat("x", maxPendingLine/4)
	for range 40 {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if writer.pending.Len() > maxPendingLine {
		t.Fatalf("held %d bytes of one line", writer.pending.Len())
	}
	if _, err := writer.Write([]byte("rest of it\nnext line\n")); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !strings.HasSuffix(sent[0], clipMarker) || len(sent[0]) > maxPendingLine+len(clipMarker) || sent[1] != "next line" {
		t.Fatalf("sent %d lines, the last %q", len(sent), sent[len(sent)-1])
	}
}

// TestLimitsTextNeverClaimsUnenforcedLimits: a daemon that cannot enforce
// limits is said to, rather than the run being called capped.
func TestLimitsTextNeverClaimsUnenforcedLimits(t *testing.T) {
	limits, _ := ParseLimits("256m", 1, 128)
	if text := limitsText(limits, Environment{}); strings.Contains(text, "at most") || !strings.Contains(text, "NOT enforced") {
		t.Fatalf("unenforced: %q", text)
	}
	if text := limitsText(limits, Environment{LimitsEnforced: true}); !strings.Contains(text, "at most 256MiB") {
		t.Fatalf("enforced: %q", text)
	}
}

// TestFailedPreparationLeavesNoFiles: a run that fails while writing its
// files, after the secrets are down, deletes them and its lease, and asks
// Docker to start nothing.
func TestFailedPreparationLeavesNoFiles(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	sandbox := newTestSandbox(t, fake)
	input := Input{Project: "app", Compose: []byte(testCompose), Env: []byte("PASSWORD=s3cret\n"),
		SecretFiles: map[string][]byte{"secrets/db_password": []byte("s3cret")}, ConfigFiles: map[string][]byte{composeFile + "/blocked": []byte("x")}}
	if _, err := sandbox.Validate(context.Background(), input); err == nil {
		t.Fatal("a run whose files could not be written went ahead")
	}
	runs, _ := filepath.Glob(filepath.Join(sandbox.root, "*", runFolder, "*"))
	if len(runs) != 0 || len(sandbox.leaseNames()) != 0 || fake.called(" up ") {
		t.Fatalf("left run folders %v, leases %v", runs, sandbox.leaseNames())
	}
}

// TestJanitorReapsAbandonedRunsOnly: a run whose lease nobody holds is
// removed whatever is left of it, a volume alone included, with its folder
// and its lease; a run whose lease is held stays however old; a run whose
// down fails loses its folder, secrets included, and keeps its lease for
// the next sweep; and a run that ended with a failed teardown is left with
// an unlocked lease, so the next sweep removes it.
func TestJanitorReapsAbandonedRunsOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := itoa(now.Add(-2 * time.Hour).Unix())
	volumes := `"io.composewright.sandbox=cw-gone-1a2b,io.composewright.started=` + old + `"` + "\n" +
		`"io.composewright.sandbox=cw-live-3c4d,io.composewright.started=` + old + `"` + "\n" +
		`"io.composewright.sandbox=cw-stuck-5e6f,io.composewright.started=` + old + `"`
	var downs []string
	run := func(_ context.Context, _ string, _ io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := strings.Join(arguments, " ")
		switch {
		case arguments[0] == "volume":
			return []byte(volumes), nil, nil
		case strings.Contains(command, " down "):
			downs = append(downs, arguments[2])
			if arguments[2] == "cw-stuck-5e6f" {
				return nil, []byte("daemon gone"), errors.New("exit status 1")
			}
		}
		return nil, nil, nil
	}
	limits, _ := ParseLimits("1g", 1, 100)
	sandbox := New(run, limits, 5*time.Minute, "policy", t.TempDir())
	sandbox.now = func() time.Time { return now }
	for _, name := range []string{"cw-gone-1a2b", "cw-stuck-5e6f"} {
		lease, err := sandbox.takeLease(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := endLease(lease, false); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sandbox.root, "app", runFolder, name, "secrets"), runDirMode); err != nil {
			t.Fatal(err)
		}
	}
	live, err := sandbox.takeLease("cw-live-3c4d")
	if err != nil {
		t.Fatal(err)
	}
	defer endLease(live, true)
	removed, err := sandbox.Janitor(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cw-stuck-5e6f") {
		t.Fatalf("a failed down was not reported: %v", err)
	}
	if !slices.Equal(removed, []string{"cw-gone-1a2b"}) || slices.Contains(downs, "cw-live-3c4d") {
		t.Fatalf("removed %v, took down %v", removed, downs)
	}
	runs, _ := filepath.Glob(filepath.Join(sandbox.root, "*", runFolder, "*"))
	if len(runs) != 0 || !slices.Equal(sandbox.leaseNames(), []string{"cw-live-3c4d", "cw-stuck-5e6f"}) {
		t.Fatalf("left run folders %v and leases %v", runs, sandbox.leaseNames())
	}
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	failing := newTestSandbox(t, fake)
	failing.run = func(ctx context.Context, directory string, writer io.Writer, arguments ...string) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(arguments, " "), " down ") {
			return nil, []byte("daemon gone"), errors.New("exit status 1")
		}
		return fake.run(ctx, directory, writer, arguments...)
	}
	if _, err := failing.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")}); err == nil {
		t.Fatal("a failed teardown was not reported")
	}
	if names := failing.leaseNames(); len(names) != 1 || !failing.abandoned(names[0]) {
		t.Fatalf("a failed teardown left leases %v, held or gone", names)
	}
}

// TestValidateLooksAgainAfterTheChecks: a service that crashes while the
// reachability probe runs fails the run, instead of passing on what the
// containers showed before it.
func TestValidateLooksAgainAfterTheChecks(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB, probe: "  HTTP/1.1 200 OK\n"}
	sandbox := newTestSandbox(t, fake)
	probed := false
	sandbox.run = func(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := strings.Join(arguments, " ")
		if strings.Contains(command, " run ") {
			probed = true
		}
		if probed && strings.Contains(command, " ps ") {
			return []byte(`{"Service":"app","State":"exited","ExitCode":137}` + "\n" + healthyDB), nil, nil
		}
		return fake.run(ctx, directory, live, arguments...)
	}
	verdict, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""), Probe: ir.Probe{Service: "app", Port: 80}})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Passed() || verdict.Reason != ReasonCrash || !strings.Contains(verdict.Detail, "after the checks") {
		t.Fatalf("a crash during the probe passed: %s/%s %s", verdict.Result, verdict.Reason, verdict.Detail)
	}
}

// TestReadOnlyServiceGetsItsFilesFromTheRunVolume: Compose cannot copy a
// config into a read-only service, so such a service, as bookorbit's app
// is, mounts each secret and bundled file read-only from the run's files
// volume by subpath, which Validate fills before up; a writable service
// keeps its configs, and the volume goes with the run's project.
func TestReadOnlyServiceGetsItsFilesFromTheRunVolume(t *testing.T) {
	compose := []byte(`services:
  app:
    image: example/app:1
    read_only: true
    volumes: ["./config/app.conf:/etc/app/app.conf:ro", "/srv/books:/books"]
    secrets: [db_password]
  worker:
    image: example/worker:1
    secrets: [db_password]
secrets:
  db_password:
    file: ./secrets/db_password
`)
	project, err := composefile.Load(context.Background(), composeFile, compose, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := ParseLimits("256m", 1, 128)
	files := map[string][]byte{"secrets/db_password": []byte("pa$word"), "config/app.conf": []byte("listen 80\n")}
	override, staged, err := Override(project, nil, limits, "cw-app-1234", 0, false, "", files)
	if err != nil {
		t.Fatal(err)
	}
	details := types.ConfigDetails{WorkingDir: t.TempDir(), Environment: types.Mapping{},
		ConfigFiles: []types.ConfigFile{{Filename: composeFile, Content: compose}, {Filename: overrideFile, Content: override}}}
	merged, err := loader.LoadWithContext(context.Background(), details, func(options *loader.Options) { options.SetProjectName("cw-app-1234", true) })
	if err != nil {
		t.Fatalf("Compose refuses the override: %v\n%s", err, override)
	}
	app, worker := merged.Services["app"], merged.Services["worker"]
	mounted := map[string]string{}
	for _, volume := range app.Volumes {
		if volume.Source == filesVolume && volume.ReadOnly && volume.Volume != nil {
			mounted[volume.Target] = string(staged[volume.Volume.Subpath])
		}
	}
	if len(app.Configs) != 0 || len(app.Secrets) != 0 || mounted["/etc/app/app.conf"] != "listen 80\n" || mounted["/run/secrets/db_password"] != "pa$word" {
		t.Fatalf("the read-only app: configs %v, secrets %v, mounted %v\n%s", app.Configs, app.Secrets, mounted, override)
	}
	if len(worker.Configs) != 1 || merged.Volumes[filesVolume].Name != "cw-app-1234_"+filesVolume {
		t.Fatalf("worker configs %v, files volume %+v", worker.Configs, merged.Volumes[filesVolume])
	}

	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB}
	sandbox := newTestSandbox(t, fake)
	readOnly := strings.Replace(testCompose, "  db:\n", "    read_only: true\n    secrets: [db_password]\n  db:\n", 1) +
		"secrets:\n  db_password:\n    file: ./secrets/db_password\n"
	if _, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(readOnly), Env: []byte(""),
		SecretFiles: map[string][]byte{"secrets/db_password": []byte("s3cret")}}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, call := range fake.calls {
		command := strings.Join(call, " ")
		for _, step := range []string{"volume create", "create --name", "cp ", "rm --force", " up "} {
			if strings.HasPrefix(command, step) || (step == " up " && strings.Contains(command, step)) {
				order = append(order, strings.TrimSpace(step))
			}
		}
	}
	if !slices.Equal(order, []string{"volume create", "create --name", "cp", "rm --force", "up"}) {
		t.Fatalf("the files volume was not filled before up: %v", order)
	}
}
