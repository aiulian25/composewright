package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/store"
	"github.com/aiulian25/composewright/internal/store/storetest"
	"github.com/aiulian25/composewright/internal/workspace"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes the CLI in-process against an isolated HOME and XDG tree.
func run(t *testing.T, environment string, arguments ...string) result {
	t.Helper()
	return runWithInput(t, environment, "", arguments...)
}

func runWithInput(t *testing.T, environment, input string, arguments ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	application := &app{build: BuildInfo{Version: "test", Commit: "abc", Date: "now"}, stdin: strings.NewReader(input), stdout: &stdout, stderr: &stderr}
	code := application.run(append([]string{"--config", filepath.Join(environment, "config.yaml"), "--data-dir", filepath.Join(environment, "data")}, arguments...))
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, variable := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "COMPOSEWRIGHT_CONFIG", "COMPOSEWRIGHT_DATA_DIR", "COMPOSEWRIGHT_MASTER_KEY_FILE"} {
		t.Setenv(variable, "")
	}
	return root
}

func TestInitCreatesPrivateStateAndIsIdempotent(t *testing.T) {
	environment := isolate(t)
	first := run(t, environment, "--json", "init")
	if first.code != exitOK {
		t.Fatalf("init failed: %+v", first)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(first.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report["key_created"] != true || report["config_created"] != true {
		t.Fatalf("unexpected report %v", report)
	}
	assertMode(t, filepath.Join(environment, "data"), 0o700)
	assertMode(t, filepath.Join(environment, "data", "keys"), 0o700)
	assertMode(t, filepath.Join(environment, "data", "keys", "master.key"), 0o600)
	assertMode(t, filepath.Join(environment, "config.yaml"), 0o600)

	second := run(t, environment, "--json", "init")
	if second.code != exitOK || !strings.Contains(second.stdout, `"key_created": false`) || !strings.Contains(second.stdout, report["key_id"].(string)) {
		t.Fatalf("second init changed state: %+v", second)
	}
}

func TestInitRefusesLooseKeyFile(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "init"); result.code != exitOK {
		t.Fatalf("init failed: %+v", result)
	}
	keyFile := filepath.Join(environment, "data", "keys", "master.key")
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	result := run(t, environment, "init")
	if result.code == exitOK || !strings.Contains(result.stderr, "chmod 0600") {
		t.Fatalf("loose key accepted: %+v", result)
	}
}

func TestConfigSetGetShow(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "config", "set", "ui.theme", "light"); result.code != exitOK {
		t.Fatalf("set failed: %+v", result)
	}
	if result := run(t, environment, "config", "get", "ui.theme"); strings.TrimSpace(result.stdout) != "light" {
		t.Fatalf("get returned %+v", result)
	}
	if result := run(t, environment, "config", "set", "ui.theme", "neon"); result.code == exitOK {
		t.Fatal("invalid theme accepted")
	}
	show := run(t, environment, "config", "show")
	if show.code != exitOK || !strings.Contains(show.stdout, "theme: light") || !strings.Contains(show.stdout, "# data dir:") {
		t.Fatalf("show output: %+v", show)
	}
}

// TestConfigSetRepairsAFileThatDoesNotLoad: a value that keeps the config
// from loading can be set right, and a result that does not validate is
// still refused.
func TestConfigSetRepairsAFileThatDoesNotLoad(t *testing.T) {
	environment := isolate(t)
	broken := bytes.Replace(config.DefaultTemplate, []byte("session_ttl: 12h0m0s"), []byte("session_ttl: 0s"), 1)
	if err := os.WriteFile(filepath.Join(environment, "config.yaml"), broken, config.FileMode); err != nil {
		t.Fatal(err)
	}
	if result := run(t, environment, "config", "get", "ui.session_ttl"); result.code == exitOK {
		t.Fatal("a config whose sessions end at once loaded")
	}
	if result := run(t, environment, "config", "set", "ui.session_ttl", "1m"); result.code == exitOK {
		t.Fatal("config set wrote a session lifetime out of range")
	}
	if result := run(t, environment, "config", "set", "ui.session_ttl", "12h"); result.code != exitOK {
		t.Fatalf("config set could not repair the file: %+v", result)
	}
	if result := run(t, environment, "config", "get", "ui.session_ttl"); strings.TrimSpace(result.stdout) != "12h0m0s" {
		t.Fatalf("get after the repair: %+v", result)
	}
}

func TestVersionJSON(t *testing.T) {
	result := run(t, isolate(t), "--json", "version")
	if result.code != exitOK || !strings.Contains(result.stdout, `"version": "test"`) {
		t.Fatalf("version: %+v", result)
	}
}

// TestVersionFlag: --version prints what the version command prints.
func TestVersionFlag(t *testing.T) {
	environment := isolate(t)
	alias, command := run(t, environment, "--version"), run(t, environment, "version")
	if alias.code != exitOK || alias.stdout != command.stdout || !strings.HasPrefix(alias.stdout, "composewright test (commit abc") {
		t.Fatalf("--version: %+v, version: %+v", alias, command)
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	application := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	root := application.rootCommand()
	for _, command := range allCommands(root) {
		if command.Short == "" || command.Long == "" {
			t.Errorf("%q lacks Short or Long help", command.CommandPath())
		}
		if command.Runnable() && command.Example == "" {
			t.Errorf("%q lacks an Example", command.CommandPath())
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s mode %04o, want %04o", path, info.Mode().Perm(), want)
	}
}

func TestSanitizeStripsTerminalControl(t *testing.T) {
	hostile := "ok\x1b[2J\x1b]0;owned\x07 done\r\n"
	if got := sanitize(hostile); got != "ok[2J]0;owned done" {
		t.Fatalf("sanitize = %q", got)
	}
}

func TestDebugPolicyBlocksAdversarialCorpus(t *testing.T) {
	files, err := filepath.Glob("../../testdata/adversarial/*.json")
	if err != nil || len(files) == 0 {
		t.Fatal("no fixtures")
	}
	result := run(t, isolate(t), append([]string{"debug", "policy", "--owner", "miniflux"}, files...)...)
	if result.code != exitBlocked || !strings.Contains(result.stderr, fmt.Sprintf("%d of %d plans blocked", len(files), len(files))) {
		t.Fatalf("debug policy: %+v", result)
	}
}

func openTestWorkspace(t *testing.T, environment string) *workspace.Workspace {
	t.Helper()
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(environment, "config.yaml"), DataDir: filepath.Join(environment, "data")})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := workspace.Open(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

// TestPasswordResetsTheAdministrator: a forgotten password is replaced from
// the CLI, which needs an administrator and a long enough password, ends
// every session and writes the change to the audit log.
func TestPasswordResetsTheAdministrator(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "init"); result.code != exitOK {
		t.Fatalf("init: %+v", result)
	}
	const newPassword = "a long new password"
	if result := runWithInput(t, environment, newPassword+"\n", "password"); result.code == exitOK {
		t.Fatal("a password was set with no administrator")
	}
	ctx := context.Background()
	opened := openTestWorkspace(t, environment)
	hash, err := auth.HashPassword("the first password")
	if err != nil {
		t.Fatal(err)
	}
	administrator, err := opened.Store.CreateFirstUser(ctx, "iulian", hash)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session := store.Session{IDHash: "signed-in-browser", UserID: administrator.ID, CSRF: "token", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := errors.Join(opened.Store.CreateSession(ctx, session), opened.Close()); err != nil {
		t.Fatal(err)
	}
	if result := runWithInput(t, environment, "short\n", "password"); result.code == exitOK {
		t.Fatal("a short password was accepted")
	}
	result := runWithInput(t, environment, newPassword+"\n", "password")
	if result.code != exitOK || !strings.Contains(result.stdout, "iulian") || strings.Contains(result.stdout+result.stderr, newPassword) {
		t.Fatalf("password: %+v", result)
	}
	opened = openTestWorkspace(t, environment)
	defer opened.Close()
	changed, err := opened.Store.UserByName(ctx, "iulian")
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := auth.VerifyPassword(changed.PasswordHash, newPassword); err != nil || !matches {
		t.Fatalf("the new password does not sign in: %v", err)
	}
	if _, err := opened.Store.SessionByHash(ctx, session.IDHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a browser stayed signed in: %v", err)
	}
	latest, err := opened.Store.AuditEntries(ctx, "", 1)
	if err != nil || len(latest) != 1 || latest[0].Action != store.ActionPassword || latest[0].Details["user"] != "iulian" {
		t.Fatalf("the reset was not audited: %+v %v", latest, err)
	}
}

func TestKeysLifecycleAndSecrets(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "init"); result.code != exitOK {
		t.Fatalf("init: %+v", result)
	}
	if result := runWithInput(t, environment, "sk-test-123\n", "config", "set-secret", "llm"); result.code != exitOK || strings.Contains(result.stdout+result.stderr, "sk-test-123") {
		t.Fatalf("set-secret: %+v", result)
	}
	if result := runWithInput(t, environment, "\n", "config", "set-secret", "llm"); result.code == exitOK {
		t.Fatal("empty secret accepted")
	}
	if result := run(t, environment, "config", "set-secret", "Bad Name"); result.code == exitOK {
		t.Fatal("invalid secret name accepted")
	}
	status := run(t, environment, "keys", "status")
	if status.code != exitOK || !strings.Contains(status.stdout, "1 sealed values") {
		t.Fatalf("status: %+v", status)
	}
	if result := run(t, environment, "keys", "rotate"); result.code != exitOK || !strings.Contains(result.stdout, "1 values re-encrypted") {
		t.Fatalf("rotate: %+v", result)
	}
	if result := run(t, environment, "keys", "status"); !strings.Contains(result.stdout, "retired") {
		t.Fatalf("status after rotate: %+v", result)
	}
	if result := run(t, environment, "keys", "purge"); result.code == exitOK {
		t.Fatalf("purge ran without confirmation: %+v", result)
	}
	if result := run(t, environment, "--yes", "keys", "purge"); result.code != exitOK || !strings.Contains(result.stdout, "1 retired keys deleted") {
		t.Fatalf("purge: %+v", result)
	}
}

// TestTrustedProxyValues: --trusted-proxy takes a range, or one address
// standing for itself, and serve refuses anything else before it starts.
func TestTrustedProxyValues(t *testing.T) {
	prefixes, err := proxyPrefixes([]string{"172.18.0.5", "10.0.0.0/24"})
	if err != nil || len(prefixes) != 2 || prefixes[0].String() != "172.18.0.5/32" || prefixes[1].String() != "10.0.0.0/24" {
		t.Fatalf("prefixes %v, error %v", prefixes, err)
	}
	result := run(t, isolate(t), "serve", "--trusted-proxy", "proxy.home.arpa")
	if result.code == 0 || !strings.Contains(result.stderr, `--trusted-proxy "proxy.home.arpa"`) {
		t.Fatalf("serve took a host name as a trusted proxy: code %d, stderr %q", result.code, result.stderr)
	}
}

// TestAuditVerify: audit verify confirms an untouched log, and once a row is
// edited behind composewright's back it names the entry and exits non-zero.
func TestAuditVerify(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "init"); result.code != 0 {
		t.Fatalf("init: %d %s", result.code, result.stderr)
	}
	if result := run(t, environment, "audit", "verify"); result.code != 0 || !strings.Contains(result.stdout, "audit log intact: 1 entry") {
		t.Fatalf("untouched log: %d %q %q", result.code, result.stdout, result.stderr)
	}
	opened := openTestWorkspace(t, environment)
	path := opened.Paths.Database()
	opened.Close()
	storetest.EditAuditActor(t, path, 1)
	if result := run(t, environment, "audit", "verify"); result.code == 0 || !strings.Contains(result.stderr, "audit log entry 1 does not match the chain") {
		t.Fatalf("edited log passed: %d %q %q", result.code, result.stdout, result.stderr)
	}
}

func TestListWithoutProjects(t *testing.T) {
	environment := isolate(t)
	if result := run(t, environment, "list"); result.code == exitOK {
		t.Fatal("list worked before init")
	}
	run(t, environment, "init")
	if result := run(t, environment, "list"); result.code != exitOK || !strings.Contains(result.stdout, "No projects yet") {
		t.Fatalf("list: %+v", result)
	}
}

func TestGenStopAfterFacts(t *testing.T) {
	environment := isolate(t)
	result := run(t, environment, "gen", "--offline", "--stop-after", "facts", "../../testdata/corpus/miniflux/files")
	if result.code != exitOK || !strings.Contains(result.stdout, "contrib/docker-compose/basic.yml:12") || strings.Contains(result.stdout, "secret@db") {
		t.Fatalf("gen facts: %+v", result)
	}
	asJSON := run(t, environment, "--json", "gen", "--offline", "--stop-after", "facts", "../../testdata/corpus/miniflux/files")
	if asJSON.code != exitOK || strings.Contains(asJSON.stdout, "test123") || !strings.Contains(asJSON.stdout, `"compose_files"`) {
		t.Fatalf("gen facts json leaked or failed: code=%d", asJSON.code)
	}
	if result := run(t, environment, "gen", "--offline", "--stop-after", "nonsense", "."); result.code == exitOK {
		t.Fatal("unknown stage accepted")
	}
}

func TestGenStopAfterIR(t *testing.T) {
	environment := isolate(t)
	result := run(t, environment, "gen", "--offline", "--stop-after", "ir", "--upstream", "https://github.com/miniflux/v2", "../../testdata/corpus/miniflux/files")
	if result.code != exitOK || !strings.Contains(result.stdout, "postgres://miniflux:${POSTGRES_PASSWORD}@db/miniflux") ||
		!strings.Contains(result.stdout, "generated password24") || strings.Contains(result.stdout, "test123") {
		t.Fatalf("gen ir: %+v", result)
	}
}

const minifluxFiles = "../../testdata/corpus/miniflux/files"

func generate(t *testing.T, environment string, extra ...string) result {
	t.Helper()
	arguments := append([]string{"gen", "--offline", "--no-llm", "--no-validate", "--upstream", "https://github.com/miniflux/v2"}, extra...)
	return run(t, environment, append(arguments, minifluxFiles)...)
}

func TestGenStoresShowsExportsAndRemoves(t *testing.T) {
	environment := isolate(t)
	if result := generate(t, environment); result.code == exitOK {
		t.Fatal("gen stored a project before init")
	}
	run(t, environment, "init")
	first := generate(t, environment)
	if first.code != exitOK || !strings.Contains(first.stdout, "stored as project miniflux") {
		t.Fatalf("gen: %+v", first)
	}
	shown := run(t, environment, "show", "miniflux", "--file", "env")
	if shown.code != exitOK || !strings.Contains(shown.stdout, "<hidden: composewright show --reveal>") {
		t.Fatalf("show env: %+v", shown)
	}
	revealed := run(t, environment, "show", "miniflux", "--file", "env", "--reveal")
	password := envValue(t, revealed.stdout, "POSTGRES_PASSWORD")
	if len(password) != 64 {
		t.Fatalf("revealed password %q", password)
	}
	generate(t, environment)
	again := run(t, environment, "show", "miniflux", "--file", "env", "--reveal")
	if envValue(t, again.stdout, "POSTGRES_PASSWORD") != password {
		t.Fatal("regenerating rotated a secret that may already be deployed")
	}
	destination := filepath.Join(environment, "export")
	if result := run(t, environment, "export", "miniflux", "--to", destination); result.code != exitOK {
		t.Fatalf("export: %+v", result)
	}
	exported, err := os.ReadFile(filepath.Join(destination, ".env"))
	if err != nil || !strings.Contains(string(exported), password) {
		t.Fatalf("exported .env lacks the secret: %v", err)
	}
	if result := run(t, environment, "export", "miniflux", "--to", destination); result.code == exitOK {
		t.Fatal("export overwrote files without --overwrite")
	}
	templates := filepath.Join(environment, "arcane")
	if result := run(t, environment, "export", "miniflux", "--to", "arcane:"+templates); result.code != exitOK {
		t.Fatalf("arcane export: %+v", result)
	}
	example, _ := os.ReadFile(filepath.Join(templates, "miniflux", ".env.example"))
	if strings.Contains(string(example), password) {
		t.Fatal("an Arcane template contains a secret")
	}
	if result := run(t, environment, "--yes", "rm", "miniflux"); result.code != exitOK {
		t.Fatalf("rm: %+v", result)
	}
	if result := run(t, environment, "show", "miniflux"); result.code == exitOK {
		t.Fatal("project still shown after rm")
	}
}

func TestGenBlocksUngrantedNeedsAndUnknownNetworks(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	blocked := run(t, environment, "gen", "--offline", "--no-llm", "--no-validate", minifluxFiles)
	if blocked.code != exitBlocked || !strings.Contains(blocked.stdout, "outside the registry allowlist") {
		t.Fatalf("image of an unknown owner not blocked: %+v", blocked)
	}
	if result := generate(t, environment, "--expose-via", "traefik"); result.code == exitOK {
		t.Fatal("a network missing from the profile was accepted")
	}
	if result := generate(t, environment, "--network", "traefik"); result.code == exitOK {
		t.Fatal("a network to join that is missing from the profile was accepted")
	}
	if result := generate(t, environment, "--allow-need", "everything"); result.code == exitOK {
		t.Fatal("an unknown capability was accepted")
	}
}

func TestProfileExportImport(t *testing.T) {
	environment := isolate(t)
	run(t, environment, "init")
	file := filepath.Join(environment, "profile.yaml")
	profile := "networks:\n  - {name: npm_default, kind: proxy, provider: npm, default: true}\nstorage:\n  style: bind\n  base_path: /srv/docker\n  layout: \"{base}/{project}/{volume}\"\n  owner: \"1000:1000\"\n  save_dir: \"{base}/{project}\"\ndefaults:\n  tz: Europe/London\n  puid: 1000\n  pgid: 1000\n  restart: unless-stopped\n  domain_suffix: home.arpa\n  log_max_size: 10m\n  log_max_file: 3\n  memory_limit: 512m\n  cpus: 1\n  pids_limit: 256\nregistry_mirror: \"\"\n"
	if err := os.WriteFile(file, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := run(t, environment, "profile", "import", file); result.code != exitOK {
		t.Fatalf("import: %+v", result)
	}
	shown := run(t, environment, "profile", "show")
	if !strings.Contains(shown.stdout, "npm_default") || !strings.Contains(shown.stdout, "domain_suffix: home.arpa") {
		t.Fatalf("profile not imported: %s", shown.stdout)
	}
	result := generate(t, environment)
	if result.code != exitOK {
		t.Fatalf("gen with the imported profile: %+v", result)
	}
	compose := run(t, environment, "show", "miniflux", "--file", "compose")
	if !strings.Contains(compose.stdout, "external: true") || !strings.Contains(compose.stdout, "/srv/docker/miniflux/miniflux-db:/var/lib/postgresql") {
		t.Fatalf("profile not applied:\n%s", compose.stdout)
	}
	if err := os.WriteFile(file, []byte("networks: [{name: x, kind: wormhole}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := run(t, environment, "profile", "import", file); result.code == exitOK {
		t.Fatal("an invalid profile was imported")
	}
}

func envValue(t *testing.T, content, key string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if value, found := strings.CutPrefix(line, key+"="); found {
			return strings.Trim(value, `'"`)
		}
	}
	t.Fatalf("%s not in:\n%s", key, content)
	return ""
}
