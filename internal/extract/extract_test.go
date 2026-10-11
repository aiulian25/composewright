package extract

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/corpus/*/expected.facts.json")

func TestParseDockerfileKeepsTheFinalStage(t *testing.T) {
	content := `FROM golang:1.26 AS build
ENV CGO_ENABLED=0
EXPOSE 9999
RUN <<EOF
EXPOSE 1234
ENV FAKE=1
EOF
FROM alpine:3.20
ARG VERSION=1.2.3
ENV LISTEN_ADDR=0.0.0.0:8080 \
    RUN_MIGRATIONS="1 2"
ENV LEGACY value with spaces
EXPOSE 8080/tcp 5353/udp ${PORT}
VOLUME ["/data", "/config"]
VOLUME /cache
USER 1000:1000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/usr/bin/app", "-healthcheck"]
ENTRYPOINT ["/usr/bin/app"]
CMD --serve
`
	dockerfile := parseDockerfile("Dockerfile", []byte(content))
	if dockerfile.BaseImage != "alpine:3.20" {
		t.Fatalf("base image %q", dockerfile.BaseImage)
	}
	gotEnv := map[string]string{}
	for _, variable := range dockerfile.Env {
		gotEnv[variable.Key] = variable.Value
	}
	wantEnv := map[string]string{"LISTEN_ADDR": "0.0.0.0:8080", "RUN_MIGRATIONS": "1 2", "LEGACY": "value with spaces"}
	if !reflect.DeepEqual(gotEnv, wantEnv) {
		t.Fatalf("env %v", gotEnv)
	}
	if len(dockerfile.Exposed) != 2 || dockerfile.Exposed[0].Port != 8080 || dockerfile.Exposed[1].Protocol != "udp" {
		t.Fatalf("exposed %+v", dockerfile.Exposed)
	}
	if len(dockerfile.Volumes) != 3 || dockerfile.Volumes[2].Path != "/cache" || dockerfile.User != "1000:1000" {
		t.Fatalf("volumes %+v user %q", dockerfile.Volumes, dockerfile.User)
	}
	health := dockerfile.Healthcheck
	if health == nil || strings.Join(health.Test, " ") != "CMD /usr/bin/app -healthcheck" || health.Interval != "30s" || health.Retries != 3 {
		t.Fatalf("healthcheck %+v", health)
	}
	if health.Location.Line != 17 || dockerfile.Exposed[0].Location.Line != 13 {
		t.Fatalf("line numbers wrong: %+v %+v", health.Location, dockerfile.Exposed[0].Location)
	}
	staged := parseDockerfile("Dockerfile", []byte("FROM base AS build\nEXPOSE 1\nFROM alpine AS release\nEXPOSE 3001\nHEALTHCHECK CMD extra/healthcheck\nFROM release AS rootless\nUSER node\nFROM alpine AS upload-artifact\nENV CI=1\n"))
	if len(staged.Exposed) != 1 || staged.Exposed[0].Port != 3001 || staged.Healthcheck == nil || staged.BaseImage != "alpine" {
		t.Fatalf("release stage not chosen: %+v", staged)
	}
	named := parseDockerfile("Dockerfile", []byte("ARG SOURCE=remote\nFROM alpine AS base\nEXPOSE 53 80\nFROM base AS remote-install\nFROM ${SOURCE}-install AS final\nHEALTHCHECK CMD dig pi.hole\n"))
	if len(named.Exposed) != 2 || named.Healthcheck == nil {
		t.Fatalf("FROM ${SOURCE}-install did not inherit through the ARG default: %+v", named)
	}
	if strings.Join(dockerfile.Command, " ") != "/bin/sh -c --serve" {
		t.Fatalf("shell-form CMD %v", dockerfile.Command)
	}
	disabled := parseDockerfile("Dockerfile", []byte("FROM x\nHEALTHCHECK NONE\n"))
	if disabled.Healthcheck == nil || !disabled.Healthcheck.Disabled {
		t.Fatal("HEALTHCHECK NONE not recorded")
	}
	shell := parseDockerfile("Dockerfile", []byte("FROM x\nHEALTHCHECK CMD wget -qO- http://127.0.0.1/ || exit 1\n"))
	if shell.Healthcheck.Test[0] != "CMD-SHELL" || shell.Healthcheck.Test[1] != "wget -qO- http://127.0.0.1/ || exit 1" {
		t.Fatalf("shell healthcheck %v", shell.Healthcheck.Test)
	}
}

func TestParseEnvSample(t *testing.T) {
	content := `# ##########
# Public URL of the instance
# shown in emails
DOMAIN=https://vw.example.com
export SIGNUPS_ALLOWED="true"
TZ='Europe/London' # local time

## Optional: admin page token
# ADMIN_TOKEN=change-me
NOT A VARIABLE
QUOTED="a # not a comment"
`
	sample := parseEnvSample(".env.template", []byte(content))
	if len(sample.Entries) != 5 {
		t.Fatalf("entries %+v", sample.Entries)
	}
	want := []EnvSampleEntry{
		{Key: "DOMAIN", Value: "https://vw.example.com", Comment: "Public URL of the instance shown in emails"},
		{Key: "SIGNUPS_ALLOWED", Value: "true"},
		{Key: "TZ", Value: "Europe/London"},
		{Key: "ADMIN_TOKEN", Value: "change-me", Comment: "Optional: admin page token", Commented: true},
		{Key: "QUOTED", Value: "a # not a comment"},
	}
	for index, entry := range want {
		got := sample.Entries[index]
		if got.Key != entry.Key || got.Value != entry.Value || got.Comment != entry.Comment || got.Commented != entry.Commented {
			t.Errorf("entry %d = %+v, want %+v", index, got, entry)
		}
	}
	if sample.Entries[0].Location.Line != 4 {
		t.Fatalf("line %d", sample.Entries[0].Location.Line)
	}
}

func TestDockerRunSkipsInlineComments(t *testing.T) {
	content := "```bash\ndocker run -d \\\n  -e QUEUE_CONNECTION= `#optional` \\\n  -p 6875:80 \\\n  lscr.io/linuxserver/bookstack:latest\n```\n"
	scan := scanMarkdown("README.md", []byte(content))
	if len(scan.dockerRuns) != 1 {
		t.Fatalf("docker runs %+v", scan.dockerRuns)
	}
	run := scan.dockerRuns[0]
	if run.Image != "lscr.io/linuxserver/bookstack:latest" || len(run.Ports) != 1 || run.Env[0].Key != "QUEUE_CONNECTION" {
		t.Fatalf("docker run %+v", run)
	}
}

func TestScanMarkdown(t *testing.T) {
	content := "# App\n\n```sh\ndocker run -d --name app \\\n  -e DOMAIN=https://x.example \\\n  -p 127.0.0.1:8000:80 --rm \\\n  -v /vw-data/:/data/ \\\n  vaultwarden/server:latest --flag\n```\n\n" +
		"| Variable | Default | Description |\n|---|---|---|\n| `SIGNUPS_ALLOWED` | `true` | Allow new users |\n| `API` | x | not a variable |\n\n" +
		"- `ADMIN_TOKEN`: token for the /admin page\n\nThe default password is `changeme`.\n\nPull ghcr.io/dani-garcia/vaultwarden:1.32.0 or docker pull vaultwarden/server\n"
	scan := scanMarkdown("README.md", []byte(content))
	if len(scan.dockerRuns) != 1 {
		t.Fatalf("docker runs %+v", scan.dockerRuns)
	}
	run := scan.dockerRuns[0]
	if run.Image != "vaultwarden/server:latest" || run.Ports[0] != "127.0.0.1:8000:80" || run.Volumes[0] != "/vw-data/:/data/" || run.Env[0].Key != "DOMAIN" || run.Location.Line != 4 {
		t.Fatalf("docker run %+v", run)
	}
	names := map[string]Mention{}
	for _, mention := range scan.mentions {
		names[mention.Name] = mention
	}
	if names["SIGNUPS_ALLOWED"].Text != "Allow new users" || names["SIGNUPS_ALLOWED"].Default != "true" {
		t.Fatalf("table row %+v", names["SIGNUPS_ALLOWED"])
	}
	if _, found := names["API"]; found {
		t.Fatal("common word taken for a variable")
	}
	if names["ADMIN_TOKEN"].Text != "token for the /admin page" {
		t.Fatalf("list item %+v", names["ADMIN_TOKEN"])
	}
	if len(scan.notes) != 1 || !strings.Contains(scan.notes[0].Text, "default password") {
		t.Fatalf("notes %+v", scan.notes)
	}
	references := map[string]bool{}
	for _, image := range scan.images {
		references[image.Reference] = true
	}
	for _, want := range []string{"vaultwarden/server:latest", "ghcr.io/dani-garcia/vaultwarden:1.32.0", "vaultwarden/server"} {
		if !references[want] {
			t.Errorf("image %s not found in %v", want, references)
		}
	}
}

func TestExtractComposeKeepsRawValuesAndLines(t *testing.T) {
	content := `services:
  app:
    image: ${APP_IMAGE:-example/app:1.2}
    ports:
      - "${HOST_PORT:-8080}:8080"
    environment:
      - DATABASE_URL=postgres://app:secret@db/app
      - TOKEN=${TOKEN:?set a token}
      - EMPTY
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17
    volumes:
      - ./pgdata:/var/lib/postgresql/data
      - named:/backup
    healthcheck:
      test: ["CMD", "pg_isready"]
      interval: 10s
volumes:
  named:
`
	file, mentions, err := extractCompose(context.Background(), "compose.yaml", []byte(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	app, db := file.Services[0], file.Services[1]
	if app.Image != "example/app:1.2" || app.ImageAt.Line != 3 || app.Ports[0].Published != "8080" || app.Ports[0].Location.Line != 5 {
		t.Fatalf("app %+v", app)
	}
	if app.Environment[0].Raw != "postgres://app:secret@db/app" || app.Environment[1].Raw != "${TOKEN:?set a token}" || app.Environment[2].HasValue {
		t.Fatalf("environment %+v", app.Environment)
	}
	if app.DependsOn[0].Service != "db" || app.DependsOn[0].Condition != "service_healthy" {
		t.Fatalf("depends_on %+v", app.DependsOn)
	}
	if db.Volumes[0].Type != "bind" || db.Volumes[1].Type != "volume" || db.Healthcheck.Interval != "10s" {
		t.Fatalf("db %+v", db)
	}
	required := false
	for _, mention := range mentions {
		required = required || (mention.Name == "TOKEN" && mention.Required)
	}
	if !required {
		t.Fatal("${TOKEN:?} not marked required")
	}
	if _, _, err := extractCompose(context.Background(), "ci.yml", []byte("on: push\njobs: {}\nservices:\n  db:\n    image: postgres\n"), nil); !errors.Is(err, errNotCompose) {
		t.Fatalf("workflow taken for compose: %v", err)
	}
}

// TestExtractLeavesOutUnreadableFiles: a checkout mounted into the container
// may hold files and folders its user cannot read; the rest is still read,
// and the problem names what was left out.
func TestExtractLeavesOutUnreadableFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	directory := t.TempDir()
	files := map[string]string{"compose.yaml": "services:\n  web:\n    image: nginx:1.27\n", ".golangci.yml": "linters: {}\n", "private/README.md": "# private\n"}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".golangci.yml", "private"} {
		if err := os.Chmod(filepath.Join(directory, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(directory, "private"), 0o700) })
	facts, err := Extract(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.ComposeFiles) != 1 || len(facts.Problems) != 1 || facts.Problems[0] != "permission denied, left out: .golangci.yml, private" {
		t.Fatalf("compose files %d, problems %q", len(facts.ComposeFiles), facts.Problems)
	}
}

// TestExtractComposeReadsEmptyKeys: an environment: whose entries are all
// commented out is empty, not a reason to drop the file
// (actualbudget/actual), and line numbers still point at the original.
func TestExtractComposeReadsEmptyKeys(t *testing.T) {
	content := "services:\n  app:\n    image: example/app:1\n    environment:\n      # - PORT=5006\n    ports:\n      - '5006:5006'\n"
	file, _, err := extractCompose(context.Background(), "compose.yaml", []byte(content), nil)
	if err != nil || len(file.Services) != 1 || len(file.Services[0].Ports) != 1 || file.Services[0].Ports[0].Location.Line != 7 {
		t.Fatalf("file %+v, err %v", file, err)
	}
}

// TestComposeDefaultsFollowTheDocument: a variable written with two defaults
// takes the first, on every run (homarr's WORKSHOP_API_URL). Reading a map
// picked either, so the check repeats.
func TestComposeDefaultsFollowTheDocument(t *testing.T) {
	const runs = 20
	content := "services:\n  app:\n    image: example/app:1\n    environment:\n      API: ${API_URL:-http://localhost:8090}\n      WEB: ${API_URL:-http://localhost:3003}\n"
	for range runs {
		_, mentions, err := extractCompose(context.Background(), "compose.yaml", []byte(content), nil)
		if err != nil {
			t.Fatal(err)
		}
		found := slices.IndexFunc(mentions, func(mention Mention) bool { return mention.Name == "API_URL" })
		if found < 0 || mentions[found].Default != "http://localhost:8090" {
			t.Fatalf("API_URL mentions %+v", mentions)
		}
	}
}

func TestRedactedHidesSecretValues(t *testing.T) {
	facts := &Facts{Variables: mergeVariables([]Mention{
		{Name: "POSTGRES_PASSWORD", Kind: KindCompose, Default: "hunter2"},
		{Name: "DATABASE_URL", Kind: KindCompose, Default: "postgres://u:hunter3@db/x"},
		{Name: "BASE_URL", Kind: KindReadme, Default: "http://localhost:8080"},
	})}
	redacted, err := facts.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(redacted)
	if strings.Contains(string(encoded), "hunter2") || strings.Contains(string(encoded), "hunter3") {
		t.Fatalf("secret leaked: %s", encoded)
	}
	original, _ := facts.Variable("DATABASE_URL")
	if !strings.Contains(string(encoded), "postgres://u:") || original.Default != "postgres://u:hunter3@db/x" {
		t.Fatal("redaction changed the original or removed the URL structure")
	}
}

// TestCorpusFacts pins what extraction finds in each corpus snapshot.
func TestCorpusFacts(t *testing.T) {
	directories, _ := filepath.Glob("../../testdata/corpus/*/files")
	if len(directories) < 5 {
		t.Fatalf("corpus incomplete: %v", directories)
	}
	for _, directory := range directories {
		slug := filepath.Base(filepath.Dir(directory))
		t.Run(slug, func(t *testing.T) {
			facts, err := Extract(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.MarshalIndent(facts, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join(filepath.Dir(directory), "expected.facts.json")
			if *update {
				if err := os.WriteFile(golden, append(encoded, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/extract -update)", err)
			}
			if string(want) != string(encoded)+"\n" {
				t.Fatalf("facts for %s changed; inspect and rerun with -update", slug)
			}
		})
	}
}

func FuzzParseEnvSample(f *testing.F) {
	f.Add([]byte("# c\nA=1\n# B=2\nexport C=\"x y\" # z\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, entry := range parseEnvSample(".env", data).Entries {
			if entry.Key == "" || strings.ContainsAny(entry.Key, "= \n") {
				t.Fatalf("invalid key %q", entry.Key)
			}
		}
	})
}

// FuzzExtractCompose: whatever an untrusted compose file holds, reading its
// facts never panics.
func FuzzExtractCompose(f *testing.F) {
	for _, seed := range []string{"../../testdata/corpus/linkding/files/docker-compose.yml", "../../testdata/corpus/uptime-kuma/files/compose.yaml"} {
		content, err := os.ReadFile(seed)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(content)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = extractCompose(context.Background(), "compose.yaml", data, nil)
	})
}

func FuzzParseDockerfile(f *testing.F) {
	f.Add([]byte("FROM a\nENV A=1 B=\"2 3\"\nHEALTHCHECK --interval=1s CMD [\"x\"]\nRUN <<EOF\nx\nEOF\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseDockerfile("Dockerfile", data)
	})
}

func FuzzScanMarkdown(f *testing.F) {
	f.Add([]byte("docker run -e A=1 -p 80:80 img\n| Variable | Description |\n|---|---|\n| `A_B` | x |\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = scanMarkdown("README.md", data)
	})
}
