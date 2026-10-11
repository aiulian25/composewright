package web

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/doctor"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/workspace"
)

const (
	testPassword = "correct horse battery staple"
	healthyPS    = `{"Service":"miniflux","Name":"cw-miniflux-1-miniflux-1","State":"running","Health":"healthy"}` + "\n" +
		`{"Service":"db","Name":"cw-miniflux-1-db-1","State":"running","Health":"healthy"}`
)

var (
	csrfPattern  = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	minifluxPath = func() string { path, _ := filepath.Abs("../../testdata/corpus/miniflux/files"); return path }()
)

// clock is a fake time source the throttle and sessions read.
type clock struct {
	mutex sync.Mutex
	now   time.Time
}

func (fake *clock) Now() time.Time {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.now
}

func (fake *clock) advance(step time.Duration) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.now = fake.now.Add(step)
}

// crashedPS and crashLog are a run whose app exits; a test asks for one by
// editing crashMark into compose.yaml.
const (
	crashedPS = `{"Service":"miniflux","Name":"cw-miniflux-1-miniflux-1","State":"exited","Health":"","ExitCode":1}` + "\n" +
		`{"Service":"db","Name":"cw-miniflux-1-db-1","State":"running","Health":"healthy"}`
	crashLog  = "miniflux-1  | 2026-10-10T08:15:02.123456789Z level=error msg=\"cannot open the database\"\n"
	crashMark = `["sh", "-c", "exit 1"]`
)

func crashes(directory string) bool {
	content, err := os.ReadFile(filepath.Join(directory, "compose.yaml"))
	return err == nil && strings.Contains(string(content), crashMark)
}

// docker answers like a healthy Compose project and streams a line, or
// like one whose app exits when the compose file asks for it.
func docker(_ context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
	command := strings.Join(arguments, " ")
	crashed := crashes(directory)
	switch {
	case strings.Contains(command, "version --short"):
		return []byte("5.6.0"), nil, nil
	case strings.Contains(command, " up "):
		if live != nil {
			_, _ = io.WriteString(live, " Container cw-miniflux-1-miniflux-1 Healthy\n")
		}
	case strings.Contains(command, " ps ") && crashed:
		return []byte(crashedPS), nil, nil
	case strings.Contains(command, " ps "):
		return []byte(healthyPS), nil, nil
	case strings.Contains(command, " logs ") && crashed:
		return []byte(crashLog), nil, nil
	case strings.Contains(command, " logs "):
		return []byte("miniflux-1  | 2026-10-10T08:15:02.123456789Z Listening on :8080\n"), nil, nil
	case strings.HasPrefix(command, "info"):
		return []byte(`{"CgroupVersion":"2","CgroupDriver":"systemd"}`), nil, nil
	}
	return nil, nil, nil
}

// offline connects every run offline, with the scripted Docker as sandbox.
func offline(ctx context.Context, setup pipeline.Setup) (pipeline.Connection, error) {
	validate := setup.Validate
	setup.Offline, setup.Validate = true, false
	connection, err := pipeline.Connect(ctx, setup)
	if err != nil || !validate {
		return connection, err
	}
	limits, _ := sandbox.ParseLimits("1g", 1, 128)
	connection.Dependencies.Sandbox = sandbox.New(docker, limits, time.Minute, "policy", setup.Paths.ProjectsDir())
	return connection, nil
}

type harness struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	client *http.Client
	clock  *clock
	token  string
	root   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return startHarness(t, httptest.NewTLSServer)
}

// newPlainHarness serves over plain HTTP, as on a home network.
func newPlainHarness(t *testing.T) *harness {
	t.Helper()
	return startHarness(t, httptest.NewServer)
}

func startHarness(t *testing.T, serve func(http.Handler) *httptest.Server) *harness {
	t.Helper()
	root := t.TempDir()
	loaded := &config.Loaded{Config: config.Default(), Paths: config.Paths{ConfigFile: filepath.Join(root, "config.yaml"),
		DataDir: filepath.Join(root, "data"), KeyFile: filepath.Join(root, "data", "keys", "master.key")}}
	loaded.Config.LLM.Provider = config.ProviderNone
	if _, err := workspace.Init(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	if err := config.SetInFile(loaded.Paths.ConfigFile, "llm.provider", "none"); err != nil {
		t.Fatal(err)
	}
	opened, err := workspace.Open(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	fake := &clock{now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	server, err := New(Options{Loaded: loaded, Flags: config.Flags{ConfigFile: loaded.Paths.ConfigFile, DataDir: loaded.Paths.DataDir},
		Workspace: opened, Version: "test", Listen: "127.0.0.1:8484", SetupToken: "setup-token", Connect: offline, Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	listener := serve(server)
	jar, _ := cookiejar.New(nil)
	client := listener.Client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(func() {
		listener.Close()
		server.Close()
		opened.Close()
	})
	return &harness{t: t, server: server, http: listener, client: client, clock: fake, root: root}
}

func (harness *harness) do(method, path string, form url.Values, headers map[string]string) (*http.Response, string) {
	harness.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, harness.http.URL+path, body)
	if err != nil {
		harness.t.Fatal(err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := harness.client.Do(request)
	if err != nil {
		harness.t.Fatal(err)
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	return response, string(content)
}

func (harness *harness) get(path string) (*http.Response, string) {
	return harness.do(http.MethodGet, path, nil, nil)
}

// post sends a form with the CSRF token of the last page seen.
func (harness *harness) post(path string, form url.Values) (*http.Response, string) {
	if form == nil {
		form = url.Values{}
	}
	form.Set(csrfField, harness.token)
	return harness.do(http.MethodPost, path, form, nil)
}

func (harness *harness) remember(page string) {
	if match := csrfPattern.FindStringSubmatch(page); match != nil {
		harness.token = match[1]
	}
}

func (harness *harness) signUp() {
	harness.t.Helper()
	form := url.Values{"token": {"setup-token"}, "username": {"admin"}, "password": {testPassword}, "confirm": {testPassword}}
	response, _ := harness.do(http.MethodPost, "/setup", form, nil)
	if response.StatusCode != http.StatusSeeOther {
		harness.t.Fatalf("setup: %d", response.StatusCode)
	}
	_, page := harness.get("/new")
	harness.remember(page)
}

func status(t *testing.T, response *http.Response, want int, page string) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d\n%s", response.Request.Method, response.Request.URL.Path, response.StatusCode, want, page)
	}
}

func TestSetupSignInAndOut(t *testing.T) {
	harness := newHarness(t)
	response, page := harness.get("/setup")
	status(t, response, http.StatusOK, page)
	if strings.Contains(page, `value="admin"`) {
		t.Fatal("the setup page fills in a username")
	}
	harness.clock.advance(SetupWindow)
	response, page = harness.get("/setup")
	status(t, response, http.StatusForbidden, page)
	late := url.Values{"username": {"intruder"}, "password": {testPassword}, "confirm": {testPassword}}
	response, page = harness.do(http.MethodPost, "/setup", late, nil)
	status(t, response, http.StatusForbidden, page)
	response, page = harness.get("/new")
	status(t, response, http.StatusSeeOther, page)
	if location := response.Header.Get("Location"); location != "/login?next=%2Fnew" {
		t.Fatalf("redirect to %q", location)
	}
	response, page = harness.get("/setup?token=setup-token")
	status(t, response, http.StatusOK, page)
	weak := url.Values{"token": {"setup-token"}, "username": {"admin"}, "password": {"short"}, "confirm": {"short"}}
	response, page = harness.do(http.MethodPost, "/setup", weak, nil)
	status(t, response, http.StatusBadRequest, page)
	harness.signUp()
	cookie := harness.client.Jar.Cookies(mustURL(harness.http.URL))
	if len(cookie) != 1 || cookie[0].Name != secureSessionCookie {
		t.Fatalf("session cookie: %+v", cookie)
	}
	response, page = harness.do(http.MethodPost, "/setup", url.Values{"token": {"setup-token"}, "username": {"second"},
		"password": {testPassword}, "confirm": {testPassword}}, nil)
	status(t, response, http.StatusForbidden, page)
	response, page = harness.post("/logout", nil)
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/projects")
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/login")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `href="/login/forgot"`) || strings.Contains(page, `value="admin"`) {
		t.Fatal("the sign-in page fills in a username or has no way back from a forgotten password")
	}
	response, page = harness.get("/login/forgot")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "composewright password") {
		t.Fatal("the password help does not name the command")
	}
	for attempt := range 3 {
		response, page = harness.do(http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {"wrong password!"}}, nil)
		status(t, response, http.StatusUnauthorized, page)
		if !strings.Contains(page, wrongCredential) {
			t.Fatalf("attempt %d: no generic error", attempt)
		}
	}
	response, page = harness.do(http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {testPassword}}, nil)
	status(t, response, http.StatusTooManyRequests, page)
	harness.clock.advance(2 * time.Second)
	response, page = harness.do(http.MethodPost, "/login", url.Values{"username": {"admin"}, "password": {testPassword}, "next": {"//evil.example/"}}, nil)
	status(t, response, http.StatusSeeOther, page)
	if location := response.Header.Get("Location"); location != "/" {
		t.Fatalf("login redirected to %q", location)
	}
	harness.clock.advance(sessionIdle + time.Minute)
	response, page = harness.get("/projects")
	status(t, response, http.StatusSeeOther, page)
}

// TestLocalPathKeepsRedirectsOnThisSite: browsers drop TAB and newline
// characters from a redirect target, so "/\t/evil.example" would take them
// to //evil.example, another site.
func TestLocalPathKeepsRedirectsOnThisSite(t *testing.T) {
	for _, target := range []string{"/\t/evil.example", "/\n/evil.example", "//evil.example", "/\\evil.example"} {
		if kept := localPath(target); kept != "" {
			t.Errorf("localPath(%q) = %q, want it refused", target, kept)
		}
	}
	const local = "/projects/miniflux/review?pane=env"
	if kept := localPath(local); kept != local {
		t.Errorf("localPath(%q) = %q, want it unchanged", local, kept)
	}
}

// TestPasswordHashingIsCapped: each Argon2id hash holds 19 MiB, so while
// maxConcurrentHashes are running, setup, a password change and a sign-in
// are answered at once instead of hashing too; the next free slot serves them.
func TestPasswordHashingIsCapped(t *testing.T) {
	harness := newHarness(t)
	occupy := func() {
		for range maxConcurrentHashes {
			harness.server.hashing <- struct{}{}
		}
	}
	vacate := func() {
		for range maxConcurrentHashes {
			<-harness.server.hashing
		}
	}
	busy := func(response *http.Response, page string, want int) {
		t.Helper()
		status(t, response, want, page)
		if !strings.Contains(page, tooManyPasswordChecks) {
			t.Fatal("the busy answer does not say so")
		}
	}
	occupy()
	setup := url.Values{"token": {"setup-token"}, "username": {"admin"}, "password": {testPassword}, "confirm": {testPassword}}
	response, page := harness.do(http.MethodPost, "/setup", setup, nil)
	busy(response, page, http.StatusTooManyRequests)
	vacate()
	harness.signUp()
	occupy()
	response, page = harness.post("/settings/account", url.Values{"current": {testPassword}, "password": {"another long password"}, "confirm": {"another long password"}})
	busy(response, page, http.StatusBadRequest)
	signIn := url.Values{"username": {"admin"}, "password": {testPassword}}
	response, page = harness.do(http.MethodPost, "/login", signIn, nil)
	busy(response, page, http.StatusTooManyRequests)
	vacate()
	response, page = harness.do(http.MethodPost, "/login", signIn, nil)
	status(t, response, http.StatusSeeOther, page)
}

// TestPlainHTTPOnTheLAN: over plain HTTP, the usual case on a home network,
// the first account is made from the page without a token, the session
// cookie works without Secure, and an address typed as an IP is accepted.
func TestPlainHTTPOnTheLAN(t *testing.T) {
	harness := newPlainHarness(t)
	form := url.Values{"username": {"admin"}, "password": {testPassword}, "confirm": {testPassword}}
	response, page := harness.do(http.MethodPost, "/setup", form, nil)
	status(t, response, http.StatusSeeOther, page)
	cookies := response.Cookies()
	if len(cookies) != 1 || cookies[0].Name != plainSessionCookie || cookies[0].Secure {
		t.Fatalf("session cookie over HTTP: %+v", cookies)
	}
	response, page = harness.get("/new")
	status(t, response, http.StatusOK, page)
	response, page = harness.do(http.MethodGet, "/new", nil, map[string]string{"Cookie": secureSessionCookie + "=stale-from-another-port"})
	status(t, response, http.StatusOK, page)
	request, _ := http.NewRequest(http.MethodGet, harness.http.URL+"/login", nil)
	request.Host = "192.168.1.83:8484"
	typed, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	typed.Body.Close()
	if typed.StatusCode != http.StatusOK {
		t.Fatalf("an address typed as an IP answered %d", typed.StatusCode)
	}
}

func TestHeadersHostsOriginsAndTokens(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.get("/new")
	status(t, response, http.StatusOK, page)
	for name, want := range map[string]string{"Content-Security-Policy": "default-src 'self'", "X-Frame-Options": "DENY",
		"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Referrer-Policy": "strict-origin-when-cross-origin"} {
		if !strings.Contains(response.Header.Get(name), want) {
			t.Errorf("%s = %q", name, response.Header.Get(name))
		}
	}
	if strings.Contains(page, "style=") || strings.Contains(page, "<script>") {
		t.Fatal("inline style or script in a page")
	}
	request, _ := http.NewRequest(http.MethodGet, harness.http.URL+"/login", nil)
	request.Host = "rebound.example"
	rebound, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	rebound.Body.Close()
	if rebound.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host answered %d", rebound.StatusCode)
	}
	response, page = harness.do(http.MethodPost, "/settings/appearance", url.Values{"theme": {"light"}, csrfField: {harness.token}},
		map[string]string{"Origin": "https://evil.example"})
	status(t, response, http.StatusForbidden, page)
	response, page = harness.do(http.MethodPost, "/settings/appearance", url.Values{"theme": {"light"}}, nil)
	status(t, response, http.StatusForbidden, page)
	response, page = harness.post("/settings/appearance", url.Values{"theme": {"light"}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `data-theme="light"`) {
		t.Fatal("the theme setting was not applied")
	}
	response, _ = harness.get("/static/tokens.css")
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("static: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
}

func (harness *harness) waitForJob(location string) snapshot {
	harness.t.Helper()
	identifier := strings.TrimPrefix(location, "/jobs/")
	running, found := harness.server.jobs.get(identifier)
	if !found {
		harness.t.Fatalf("no job %s", identifier)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !running.isFinished() {
		if time.Now().After(deadline) {
			harness.t.Fatal("the job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return running.snapshot()
}

func TestTheWholeFlow(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	job := harness.waitForJob(response.Header.Get("Location"))
	if job.Failure != "" || !strings.HasPrefix(job.Next, "/projects/miniflux/review?job=") || job.Facts == nil {
		t.Fatalf("generation: %+v", job.Failure)
	}
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/projects/miniflux/review")
	status(t, response, http.StatusOK, page)
	harness.remember(page)
	for _, want := range []string{"Expose via", "ADMIN_USERNAME", "blocks the plan", "Trust docker.io/miniflux/*", "compose.yaml",
		"of compose.yaml</span><code>image: docker.io/miniflux/miniflux"} {
		if !strings.Contains(page, want) {
			t.Fatalf("review lacks %q", want)
		}
	}
	response, page = harness.post("/projects/miniflux/review", url.Values{"trust": {"docker.io/miniflux/*"}, "value:ADMIN_USERNAME": {"boss"}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/projects/miniflux/review?applied")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `value="boss"`) || strings.Contains(page, "blocks the plan") || !strings.Contains(page, appliedNotice) {
		t.Fatal("the revision was not applied")
	}
	response, page = harness.post("/projects/miniflux/review", url.Values{"registry": {"docker.io/miniflux/*"}, "value:ADMIN_USERNAME": {"boss"},
		"action": {actionValidate}})
	status(t, response, http.StatusSeeOther, page)
	validation := harness.waitForJob(response.Header.Get("Location"))
	if validation.Failure != "" || len(validation.Lines) == 0 {
		t.Fatalf("validation: %q, %d lines", validation.Failure, len(validation.Lines))
	}
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusSeeOther, page)
	if response.Header.Get("Location") != "/projects/miniflux/files" {
		t.Fatalf("a passed run leads to %q, not the files", response.Header.Get("Location"))
	}
	response, page = harness.get("/projects/miniflux/validate")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "Every service is up") || !strings.Contains(page, "Attempt 1 passed") || !strings.Contains(page, "Listening on :8080") {
		t.Fatal("the kept run does not show its verdict and logs")
	}
	response, page = harness.get("/projects/miniflux/analyse")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "fields filled by rules") || !strings.Contains(page, "Upstream compose") {
		t.Fatal("the analysis does not count the fields each rule filled")
	}
	harness.checkFiles()
	response, page = harness.get("/projects/miniflux")
	status(t, response, http.StatusOK, page)
	harness.remember(page)
	if !strings.Contains(page, "PASS") || !strings.Contains(page, "web:admin") {
		t.Fatal("the project page lacks the verdict or the web actor")
	}
	response, page = harness.post("/projects/miniflux/delete", nil)
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/projects/miniflux")
	status(t, response, http.StatusNotFound, page)
}

func (harness *harness) checkFiles() {
	t := harness.t
	response, page := harness.get("/projects/miniflux/files?file=env")
	status(t, response, http.StatusOK, page)
	harness.remember(page)
	if !strings.Contains(page, "Passed on attempt 1") || !strings.Contains(page, "&lt;hidden") {
		t.Fatal("the files page does not mask the .env")
	}
	response, page = harness.post("/projects/miniflux/reveal", url.Values{"view": {actionFiles}})
	status(t, response, http.StatusOK, page)
	if strings.Contains(page, "&lt;hidden") {
		t.Fatal("reveal kept the secrets hidden")
	}
	response, env := harness.do(http.MethodPost, "/projects/miniflux/env", url.Values{}, map[string]string{csrfHeader: harness.token})
	status(t, response, http.StatusOK, env)
	if !strings.Contains(env, "POSTGRES_PASSWORD=") || strings.Contains(env, "<hidden") {
		t.Fatalf("copied .env: %s", env)
	}
	response, archive := harness.post("/projects/miniflux/download", nil)
	status(t, response, http.StatusOK, "")
	reader, err := zip.NewReader(bytes.NewReader([]byte(archive)), int64(len(archive)))
	if err != nil || len(reader.File) != 3 || reader.File[1].Mode().Perm() != 0o600 {
		t.Fatalf("zip: %v", err)
	}
	target := filepath.Join(harness.root, "saved")
	response, page = harness.post("/projects/miniflux/save", url.Values{"folder": {"feeds"}, "location": {locationCustom}, "custom": {target}})
	status(t, response, http.StatusOK, page)
	info, err := os.Stat(filepath.Join(target, "feeds", ".env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved .env: %v", err)
	}
	response, page = harness.post("/projects/miniflux/save", url.Values{"folder": {"feeds"}, "location": {locationCustom}, "custom": {target}})
	status(t, response, http.StatusBadRequest, page)
}

func TestEventsStream(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, _ := harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	location := response.Header.Get("Location")
	harness.waitForJob(location)
	request, _ := http.NewRequest(http.MethodGet, harness.http.URL+location+"/events", nil)
	stream, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.Header.Get("Content-Type") != eventStream {
		t.Fatalf("content type %q", stream.Header.Get("Content-Type"))
	}
	var kinds []string
	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		data, found := strings.CutPrefix(scanner.Text(), "data: ")
		if !found {
			continue
		}
		var item event
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, item.Kind)
	}
	joined := strings.Join(kinds, " ")
	if !strings.HasPrefix(joined, "step step") || !strings.Contains(joined, "facts") || !strings.HasSuffix(joined, "done") {
		t.Fatalf("events: %s", joined)
	}
	response, _ = harness.post("/new", url.Values{"source": {filepath.Join(t.TempDir(), "missing")}, "engine": {pipeline.EngineRules}})
	location = response.Header.Get("Location")
	harness.waitForJob(location)
	response, page := harness.get(location)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "The analysis stopped") || !strings.Contains(page, `data-state="failed"`) {
		t.Fatal("a failed generation does not show where it stopped")
	}
}

func TestUploadsBecomeASource(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField(csrfField, harness.token)
	_ = writer.WriteField("engine", pipeline.EngineRules)
	_ = writer.WriteField("name", "pasted")
	_ = writer.WriteField("compose", "services:\n  web:\n    image: docker.io/library/nginx:1.27\n    ports: [\"8080:80\"]\n")
	part, _ := writer.CreateFormFile("files", "../../README.md")
	_, _ = part.Write([]byte("# Pasted\n\nA test page.\n"))
	archive, _ := writer.CreateFormFile("files", "evil.tar.gz")
	_, _ = archive.Write([]byte("ignored"))
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, harness.http.URL+"/new", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	status(t, response, http.StatusSeeOther, "")
	job := harness.waitForJob(response.Header.Get("Location"))
	if job.Failure != "" || !strings.HasPrefix(job.Next, "/projects/pasted/review?job=") {
		t.Fatalf("upload generation: %q %q", job.Failure, job.Next)
	}
	leftovers, _ := os.ReadDir(filepath.Join(harness.root, "data", uploadsDir))
	if len(leftovers) != 0 {
		t.Fatalf("%d upload folders left behind", len(leftovers))
	}
	response, page := harness.post("/new", url.Values{"source": {minifluxPath}, "compose": {"services: {}"}})
	status(t, response, http.StatusBadRequest, page)
	response, page = harness.post("/new", url.Values{"source": {minifluxPath}, "name": {"Bad Name"}})
	status(t, response, http.StatusBadRequest, page)
}

// TestFilesOnly: a pasted compose and the person's own .env, with "just the
// files", skip the review and the sandbox; their real secret is kept and a
// placeholder is generated.
func TestFilesOnly(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	compose := "services:\n  app:\n    image: docker.io/library/nextcloud:31\n    ports: [\"8080:80\"]\n    environment:\n" +
		"      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      NEXTCLOUD_ADMIN_PASSWORD: ${NEXTCLOUD_ADMIN_PASSWORD}\n"
	env := "POSTGRES_PASSWORD=MyOwnDbPassword-7c2f\nNEXTCLOUD_ADMIN_PASSWORD=changeme\n"
	response, page := harness.post("/new", url.Values{"compose": {compose}, "env": {env}, "name": {"mine"}, "engine": {pipeline.EngineRules}, "test": {skipTest}})
	status(t, response, http.StatusSeeOther, page)
	job := harness.waitForJob(response.Header.Get("Location"))
	if job.Failure != "" || job.Next != "/projects/mine/files" {
		t.Fatalf("files only: %q %q", job.Failure, job.Next)
	}
	response, page = harness.get(job.Next)
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "Not tested") || !strings.Contains(page, "test it from the Review page") {
		t.Fatalf("files page after files only:\n%s", page)
	}
	stored, err := pipeline.Load(context.Background(), harness.server.workspace(), "mine")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Values["POSTGRES_PASSWORD"] != "MyOwnDbPassword-7c2f" || stored.Values["NEXTCLOUD_ADMIN_PASSWORD"] == "changeme" {
		t.Fatalf("stored values: own %q, placeholder %q", stored.Values["POSTGRES_PASSWORD"], stored.Values["NEXTCLOUD_ADMIN_PASSWORD"])
	}
}

func TestSettings(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.get("/settings")
	status(t, response, http.StatusOK, page)
	profile := sentProfile()
	response, page = harness.post("/settings/profile", profile)
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, savedNotice) || !strings.Contains(page, `value="newt"`) {
		t.Fatal("the settings page after a save does not say it was saved or show the networks")
	}
	if networks := harness.server.config().Profile.Networks; len(networks) != 2 || !networks[0].Default || !networks[1].Default {
		t.Fatalf("both networks were not saved as defaults: %+v", networks)
	}
	profile.Set("name-2", "cloudflared")
	profile.Set("kind-2", "wormhole")
	response, page = harness.post("/settings/profile", profile)
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, `value="cloudflared"`) || len(harness.server.config().Profile.Networks) != 2 {
		t.Fatal("a failed save lost what was typed, or saved part of it")
	}
	profile = sentProfile()
	profile.Set("puid", "x")
	response, page = harness.post("/settings/profile", profile)
	status(t, response, http.StatusBadRequest, page)
	response, page = harness.post("/settings/engine", url.Values{"engine": {"rules"}, "registries": {"docker.io/library/*\nghcr.io/*"}})
	status(t, response, http.StatusSeeOther, page)
	if registries := harness.server.config().Registries.Allow; len(registries) != 2 {
		t.Fatalf("trusted images: %v", registries)
	}
	response, page = harness.post("/settings/account", url.Values{"current": {"wrong"}, "password": {"another long password"}, "confirm": {"another long password"}})
	status(t, response, http.StatusBadRequest, page)
	response, page = harness.post("/settings/account", url.Values{"current": {testPassword}, "password": {"another long password"}, "confirm": {"another long password"}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.post("/settings/nowhere", nil)
	status(t, response, http.StatusNotFound, page)
}

// sentProfile is the Environment profile page as a browser sends it: two
// networks new projects join, and the default storage and defaults.
func sentProfile() url.Values {
	return url.Values{"name-0": {"npm_default"}, "kind-0": {"proxy"}, "provider-0": {"npm"}, "default-0": {"1"},
		"name-1": {"newt"}, "kind-1": {"tunnel"}, "provider-1": {"pangolin"}, "default-1": {"1"},
		"style": {"volumes"}, "layout": {"{base}/{project}/{volume}"}, "owner": {"1000:1000"}, "save_dir": {"{base}/{project}"},
		"puid": {"1000"}, "pgid": {"1000"}, "restart": {"unless-stopped"}, "log_max_size": {"10m"}, "log_max_file": {"3"},
		"memory_limit": {"512m"}, "cpus": {"1"}, "pids_limit": {"256"}}
}

func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

// TestRunSteps follows the sandbox's verdict rules: a crash fails its
// service and skips the log scan; a healthy service passes.
func TestRunSteps(t *testing.T) {
	verdict := sandbox.Verdict{Result: sandbox.ResultFail, Reason: sandbox.ReasonCrash, Services: []sandbox.ServiceState{
		{Service: "db", State: "running", Health: healthHealthy},
		{Service: "app", State: containerExited, ExitCode: 1},
	}}
	states := map[string]string{}
	for _, step := range runSteps(verdict) {
		states[step.Name] = step.State
	}
	want := map[string]string{"db healthy": stateDone, "app exited with code 1": pipeline.StateFailed, "Scan the logs": pipeline.StateSkipped, "Tear down": stateDone}
	for name, state := range want {
		if states[name] != state {
			t.Fatalf("%s is %q, want %q (all: %v)", name, states[name], state, states)
		}
	}
}

// TestRunStepsUnreachable: an app that every check passed but the probe
// could not reach fails the probe step, and the page points at the app.
func TestRunStepsUnreachable(t *testing.T) {
	said := "nothing answers on app:8080 from the sandbox network; is it listening on 127.0.0.1 or on another port?"
	verdict := sandbox.Verdict{Result: sandbox.ResultFail, Reason: sandbox.ReasonUnreachable, Detail: said,
		Services: []sandbox.ServiceState{{Service: "app", State: "running", Health: healthHealthy}, {Service: "db", State: "running", Health: healthHealthy}},
		Reach:    &sandbox.Reach{Service: "app", Checked: true, Said: said}}
	states := map[string]string{}
	for _, step := range runSteps(verdict) {
		states[step.Name] = step.State
	}
	want := map[string]string{"app healthy": stateDone, "Scan the logs": stateDone, "Reach app from the network": pipeline.StateFailed}
	for name, state := range want {
		if states[name] != state {
			t.Fatalf("%s is %q, want %q (all: %v)", name, states[name], state, states)
		}
	}
	if failing, found := failingState(verdict); !found || failing.Service != "app" {
		t.Fatalf("the page points at %q", failing.Service)
	}
}

// TestNetworksOnReview: a new project starts with the profile's default
// networks ticked, and the Review can join any number of them.
func TestNetworksOnReview(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	profile := sentProfile()
	profile.Del("default-1")
	profile.Set("domain-1", " https://Newt.Example.com/ ")
	response, page := harness.post("/settings/profile", profile)
	status(t, response, http.StatusSeeOther, page)
	if network, _ := harness.server.config().Profile.Network("newt"); network.Domain != "newt.example.com" {
		t.Fatalf("the domain was not stored as a domain: %q", network.Domain)
	}
	response, page = harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	harness.waitForJob(response.Header.Get("Location"))
	response, page = harness.get("/projects/miniflux/review")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `name="network" value="npm_default" checked`) || strings.Contains(page, `name="network" value="newt" checked`) {
		t.Fatal("the profile's default network is not the one ticked")
	}
	if !strings.Contains(page, `href="/projects/miniflux/delete"`) {
		t.Fatal("the sidebar offers no way to delete the project")
	}
	response, page = harness.post("/projects/miniflux/review", url.Values{"network": {"npm_default", "newt"}, "expose": {"local"}, "action": {"apply"}})
	status(t, response, http.StatusSeeOther, page)
	stored, err := pipeline.Load(context.Background(), harness.server.workspace(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	compose := stored.Generation.ComposeYAML
	if !strings.Contains(compose, "- npm_default") || !strings.Contains(compose, "- newt") || !strings.Contains(compose, `"127.0.0.1:`) {
		t.Fatalf("the networks and the port chosen on the Review were not rendered:\n%s", compose)
	}
	if !strings.Contains(compose, "route miniflux.newt.example.com to") {
		t.Fatalf("the tunnel's domain is not in compose.yaml:\n%s", compose)
	}
	response, page = harness.get("/projects/miniflux/review")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `name="network" value="newt" checked`) || !strings.Contains(page, `name="expose" value="local" checked`) {
		t.Fatal("the Review does not show what was chosen")
	}
}

// TestFailedRunExplainsItself: a run that fails says why, and the reason
// stays reachable from the Review after the plan changes.
func TestFailedRunExplainsItself(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	harness.waitForJob(response.Header.Get("Location"))
	response, page = harness.post("/projects/miniflux/review", url.Values{"trust": {"docker.io/miniflux/*"}})
	status(t, response, http.StatusSeeOther, page)
	stored, err := pipeline.Load(context.Background(), harness.server.workspace(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	image := "    image: " + stored.Plan.Services[0].Image.String() + "\n"
	edited := strings.Replace(stored.Generation.ComposeYAML, image, image+"    entrypoint: "+crashMark+"\n", 1)
	response, page = harness.post("/projects/miniflux/compose", url.Values{"compose": {edited}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.post("/projects/miniflux/validate", nil)
	status(t, response, http.StatusSeeOther, page)
	run := harness.waitForJob(response.Header.Get("Location"))
	response, page = harness.get(response.Header.Get("Location"))
	status(t, response, http.StatusOK, page)
	if run.Next != "" || !strings.Contains(page, "Why it failed") || !strings.Contains(page, failureHints[sandbox.ReasonCrash]) ||
		!strings.Contains(page, "cannot open the database") {
		t.Fatal("the failed run does not say why it failed")
	}
	response, page = harness.post("/projects/miniflux/review", url.Values{"registry": {"docker.io/miniflux/*"}, "value:ADMIN_USERNAME": {"boss"}})
	status(t, response, http.StatusSeeOther, page)
	response, page = harness.get("/projects/miniflux/review")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "The last test failed with FAIL (crash)") || !strings.Contains(page, "changed the plan since") {
		t.Fatal("the Review does not say the last test failed")
	}
	response, page = harness.get("/projects/miniflux/validate")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, olderRunNotice) || !strings.Contains(page, "cannot open the database") {
		t.Fatal("the last run is not shown after the plan changed")
	}
}

// TestSplitPaste takes .env lines pasted around a compose file out of it,
// but not a KEY=value line inside the YAML.
func TestSplitPaste(t *testing.T) {
	compose, env := splitPaste("TZ=Europe/London\n\nservices:\n  app:\n    environment:\n      - KEY=value\n    command: |\n      FOO=bar\n# mine\nSECRET=abc")
	if compose != "services:\n  app:\n    environment:\n      - KEY=value\n    command: |\n      FOO=bar" || env != "TZ=Europe/London\n\n# mine\nSECRET=abc" {
		t.Fatalf("compose %q\nenv %q", compose, env)
	}
}

// TestHomeStarts: the Home box starts from a link or a folder, a suggested
// app, or a pasted compose file with its .env, and lists recent projects.
func TestHomeStarts(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	response, page := harness.get("/")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "get you started") || !strings.Contains(page, `name="pick" value="github.com/miniflux/v2"`) {
		t.Fatal("Home lacks the box or the apps to try")
	}
	response, page = harness.post("/start", url.Values{"ask": {""}})
	status(t, response, http.StatusBadRequest, page)
	response, page = harness.post("/start", url.Values{"pick": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	if job := harness.waitForJob(response.Header.Get("Location")); job.Failure != "" || !strings.HasPrefix(job.Next, "/projects/miniflux/review") {
		t.Fatalf("a suggested app did not start: %q", job.Failure)
	}
	pasted := "services:\n  web:\n    image: docker.io/library/ghost:5\n    ports:\n      - \"2368:2368\"\nTZ=Europe/London\n"
	response, page = harness.post("/start", url.Values{"ask": {pasted}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	if job := harness.waitForJob(response.Header.Get("Location")); job.Failure != "" {
		t.Fatalf("a pasted compose file did not start: %q", job.Failure)
	}
	response, page = harness.get("/")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "Recent") || !strings.Contains(page, "needs a test") || !strings.Contains(page, `href="/projects/ghost/review"`) {
		t.Fatal("Home does not list the recent projects, named after their image, and what they need")
	}
}

// TestAdoptFoundNetwork: Add on a network found on the Docker host saves it
// with the rest of the page, as the default when there is none.
func TestAdoptFoundNetwork(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	profile := sentProfile()
	profile.Del("default-0")
	profile.Del("default-1")
	profile.Set("adopt", "npm_proxy|proxy|npm")
	response, page := harness.post("/settings/profile", profile)
	status(t, response, http.StatusSeeOther, page)
	networks := harness.server.config().Profile.Networks
	if len(networks) != 3 || networks[2].Name != "npm_proxy" || networks[2].Provider != "npm" || !networks[2].Default {
		t.Fatalf("the found network was not added as the default: %+v", networks)
	}
}

// TestModelConnections: a setup nobody saved is kept when another is used,
// each connection keeps its own key, a paid service needs one, names stay
// apart, and removing a connection removes its key and audits that.
func TestModelConnections(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	server := harness.server
	unsaved := map[string]string{"llm.provider": "ollama", "llm.base_url": `"http://192.168.1.123:11434"`, "llm.model": `"qwen2.5-coder:7b"`,
		"llm.api_key_ref": `""`, "llm.max_input_tokens": "8000"}
	if err := config.UpdateFile(server.paths().ConfigFile, unsaved); err != nil || server.reload() != nil {
		t.Fatal(err)
	}
	response, page := harness.get("/settings?section=models")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "console.anthropic.com/settings/keys") || !strings.Contains(page, "qwen2.5-coder:7b") {
		t.Fatal("the page does not guide to a key or show the model in use")
	}
	if strings.Contains(page, "required checked") || strings.Contains(page, "data-guide-hint hidden") {
		t.Fatal("the add form starts with a service chosen")
	}
	claude := url.Values{"preset": {"anthropic"}, "name": {"claude-work"}, "base_url": {"https://api.anthropic.com"}, "model": {"claude-opus-5-5"},
		"max_input_tokens": {"100000"}, "api_key": {"test-key-value"}, "action": {actionSaveAndUse}}
	response, page = harness.post("/settings/models", claude)
	status(t, response, http.StatusSeeOther, page)
	llm := server.config().LLM
	key, _, _ := server.workspace().Store.Secret(context.Background(), "llm-claude-work")
	if llm.Provider != config.ProviderAnthropic || llm.APIKeyRef != "keyring:llm-claude-work" || key != "test-key-value" || len(llm.Connections) != 2 {
		t.Fatalf("claude-work not in use with its own key: %+v", llm)
	}
	if kept := llm.Connections[1]; kept.Name != "ollama" || kept.BaseURL != "http://192.168.1.123:11434" {
		t.Fatalf("the setup in use before was not kept: %+v", llm.Connections)
	}
	response, page = harness.get("/settings?section=models&edit=claude-work")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "claude-work (Anthropic · claude-opus-5-5)") || !strings.Contains(page, `value="ollama" required disabled`) {
		t.Fatal("the engine menu does not name the connection in use, or editing lets the service change")
	}
	switched := url.Values{"preset": {"ollama"}, "name": {"claude-work"}, "edit": {"1"}, "base_url": {"http://127.0.0.1:11434"},
		"model": {"qwen2.5-coder:7b"}, "max_input_tokens": {"8000"}, "action": {"save"}}
	response, page = harness.post("/settings/models", switched)
	status(t, response, http.StatusBadRequest, page)
	connections := server.config().LLM.Connections
	saved := connections[slices.IndexFunc(connections, func(connection config.Connection) bool { return connection.Name == "claude-work" })]
	if saved.Provider != config.ProviderAnthropic || !strings.Contains(page, "stays one") {
		t.Fatalf("an edit turned a Claude connection into Ollama: %+v", saved)
	}
	response, page = harness.post("/settings/models", claude)
	status(t, response, http.StatusBadRequest, page)
	claude.Set("name", "claude-two")
	claude.Del("api_key")
	response, page = harness.post("/settings/models", claude)
	status(t, response, http.StatusBadRequest, page)
	if !strings.Contains(page, "needs an API key") {
		t.Fatal("a paid service was saved without a key")
	}
	response, page = harness.post("/settings/models", url.Values{"use": {"ollama"}})
	status(t, response, http.StatusSeeOther, page)
	if llm := server.config().LLM; llm.Provider != config.ProviderOllama || llm.BaseURL != "http://192.168.1.123:11434" {
		t.Fatalf("switching back did not restore the NUC: %+v", llm)
	}
	response, page = harness.post("/settings/models", url.Values{"remove": {"claude-work"}})
	status(t, response, http.StatusSeeOther, page)
	if _, found, _ := server.workspace().Store.Secret(context.Background(), "llm-claude-work"); found || len(server.config().LLM.Connections) != 1 {
		t.Fatal("the removed connection or its key is still there")
	}
	latest, err := server.workspace().Store.AuditEntries(context.Background(), "", 1)
	if err != nil || len(latest) != 1 || latest[0].Action != store.ActionDeleteSecret || latest[0].Details["name"] != "llm-claude-work" {
		t.Fatalf("removing the key was not audited: %+v %v", latest, err)
	}
}

// TestSideBySideEditing: the editor's check answers in JSON, a save the gate
// refuses answers 422 and stores nothing, a valid one adds a version made by
// hand that keeps the secrets behind their placeholders, and revealing
// secrets writes their names, never their values, to the audit log.
func TestSideBySideEditing(t *testing.T) {
	harness := newHarness(t)
	harness.signUp()
	ctx := context.Background()
	response, page := harness.post("/new", url.Values{"source": {minifluxPath}, "engine": {pipeline.EngineRules}})
	status(t, response, http.StatusSeeOther, page)
	harness.waitForJob(response.Header.Get("Location"))
	response, page = harness.post("/projects/miniflux/review", url.Values{"trust": {"docker.io/miniflux/*"}})
	status(t, response, http.StatusSeeOther, page)
	stored, err := pipeline.Load(ctx, harness.server.workspace(), "miniflux")
	if err != nil {
		t.Fatal(err)
	}
	env, err := stored.EnvFile(webMask)
	if err != nil {
		t.Fatal(err)
	}
	compose := stored.Generation.ComposeYAML
	response, page = harness.post("/projects/miniflux/files/check", url.Values{fileCompose: {compose}, fileEnv: {string(env)}})
	status(t, response, http.StatusOK, page)
	var check pipeline.EditCheck
	if err := json.Unmarshal([]byte(page), &check); err != nil || !check.Parses || check.RulesPassed != check.Rules {
		t.Fatalf("the check of the saved files: %v %s", err, page)
	}
	privileged := strings.Replace(compose, "restart: unless-stopped", "restart: unless-stopped\n    privileged: true", 1)
	response, page = harness.post("/projects/miniflux/files/save", url.Values{fileCompose: {privileged}, fileEnv: {string(env)}})
	status(t, response, http.StatusUnprocessableEntity, page)
	edited := strings.Replace(compose, "restart: unless-stopped", "restart: always", 1)
	response, page = harness.post("/projects/miniflux/files/save", url.Values{fileCompose: {edited}, fileEnv: {string(env)}})
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, `"next":"/projects/miniflux/files"`) {
		t.Fatalf("the save does not lead back to the Result page: %s", page)
	}
	saved, err := pipeline.Load(ctx, harness.server.workspace(), "miniflux")
	if err != nil || saved.Generation.ComposeYAML != edited || saved.Generation.Engine != pipeline.EngineManual {
		t.Fatalf("the edit is not the latest version: %v", err)
	}
	if saved.Values["ADMIN_PASSWORD"] == "" || saved.Values["ADMIN_PASSWORD"] != stored.Values["ADMIN_PASSWORD"] {
		t.Fatal("the secret behind its placeholder changed")
	}
	response, page = harness.get("/projects/miniflux/files")
	status(t, response, http.StatusOK, page)
	if !strings.Contains(page, "Edited by you") || !strings.Contains(page, `id="side-editor"`) {
		t.Fatal("the Result page lacks the version or the editor")
	}
	harness.remember(page)
	response, page = harness.post("/projects/miniflux/reveal", url.Values{"view": {actionFiles}})
	status(t, response, http.StatusOK, page)
	latest, err := harness.server.workspace().Store.AuditEntries(ctx, saved.Project.ID, 1)
	if err != nil || len(latest) != 1 || latest[0].Action != store.ActionReveal {
		t.Fatalf("revealing was not audited: %+v %v", latest, err)
	}
	details, _ := json.Marshal(latest[0].Details)
	if !strings.Contains(string(details), "ADMIN_PASSWORD") || strings.Contains(string(details), saved.Values["ADMIN_PASSWORD"]) {
		t.Fatalf("the audit entry should name the secrets and hold no value: %s", details)
	}
}

// TestHostHintFixesAListedNetwork: a listed network that says npm while the
// newt tunnel runs on it is offered the tunnel's kind and provider, and
// taking the offer changes the listed network rather than adding one.
func TestHostHintFixesAListedNetwork(t *testing.T) {
	networks := []config.ProfileNetwork{{Name: "newt", Kind: "proxy", Provider: "npm", Default: true}, {Name: "npm", Kind: "proxy"}}
	newt := networkRow{Name: "newt", Kind: "proxy", Provider: "npm"}
	hostHint(&newt, doctor.FoundNetwork{Network: "newt", Kind: "tunnel", Provider: "pangolin", Proxy: "Pangolin (newt)", Container: "newt"})
	if newt.Detected != "Pangolin (newt)" || newt.SuggestLabel != "tunnel · pangolin" {
		t.Fatalf("no offer for the tunnel's network: %+v", newt)
	}
	matching := networkRow{Name: "npm", Kind: "proxy", Provider: "npm"}
	hostHint(&matching, doctor.FoundNetwork{Network: "npm", Kind: "proxy", Provider: "npm", Proxy: "Nginx Proxy Manager", Container: "npm"})
	if matching.Suggest != "" {
		t.Fatalf("an offer for a network already right: %+v", matching)
	}
	adopted := adopt(networks, []string{newt.Suggest})
	if len(adopted) != 2 || adopted[0].Kind != "tunnel" || adopted[0].Provider != "pangolin" || !adopted[0].Default {
		t.Fatalf("taking the offer did not fix the listed network: %+v", adopted)
	}
}

// TestKeepCurrent keeps a setup someone made before another replaces it,
// but not the shipped default nobody chose.
func TestKeepCurrent(t *testing.T) {
	if kept := keepCurrent(config.Default().LLM); len(kept.Connections) != 0 {
		t.Fatalf("the shipped default was kept: %+v", kept.Connections)
	}
	nuc := config.Default().LLM
	nuc.BaseURL, nuc.Model = "http://192.168.1.123:11434", "qwen2.5-coder:7b"
	if kept := keepCurrent(nuc); len(kept.Connections) != 1 || kept.Connections[0].BaseURL != nuc.BaseURL {
		t.Fatalf("a changed setup was not kept: %+v", kept.Connections)
	}
}
