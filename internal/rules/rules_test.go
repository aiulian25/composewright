package rules

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/corpus"
	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
)

var update = flag.Bool("update", false, "rewrite testdata/corpus/*/expected.ir.json")

// TestCorpusPlans is the S4 acceptance test: rules alone produce a plan for
// every corpus repository that matches the schema and gets the verdict
// corpus.tsv expects, which for most is passing the policy gate.
func TestCorpusPlans(t *testing.T) {
	entries, err := corpus.Load("../../testdata/corpus")
	if err != nil || len(entries) < 5 {
		t.Fatalf("corpus: %d entries, %v", len(entries), err)
	}
	defaults := config.Default()
	for _, entry := range entries {
		t.Run(entry.Slug, func(t *testing.T) {
			facts, err := extract.Extract(context.Background(), entry.Directory)
			if err != nil {
				t.Fatal(err)
			}
			plan := Build(context.Background(), Input{Facts: facts, Source: entry.Source})
			if err := ir.ValidateSchema(plan); err != nil {
				t.Fatalf("plan does not match the schema: %v", err)
			}
			report := policy.Evaluate(policy.Input{Plan: plan, Profile: defaults.Profile, AllowRegistries: defaults.Registries.Allow,
				ExtraRegistries: entry.AllowRegistries, RepositoryOwner: entry.Source.Owner})
			if blocking := corpus.Blocking(report); !slices.Equal(blocking, entry.Verdict) {
				t.Fatalf("rules-only plan blocked by %v, corpus.tsv expects %v: %+v", blocking, entry.Verdict, report.Findings)
			}
			if app := plan.AppService(); entry.Passes() && (app == nil || app.Image.Registry == "") {
				t.Fatal("no app service with an image")
			}
			assertGolden(t, filepath.Join(filepath.Dir(entry.Directory), "expected.ir.json"), plan)
		})
	}
}

func assertGolden(t *testing.T, golden string, plan *ir.IR) {
	t.Helper()
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if *update {
		if err := os.WriteFile(golden, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/rules -update)", err)
	}
	if string(want) != string(encoded) {
		t.Fatalf("plan changed for %s; inspect the diff and rerun with -update", golden)
	}
}

// TestNamedSecretsKeepTheirOwnValue: upstream reads each password by its own
// name, so an equal placeholder fallback is no reason to share one secret;
// a literal written with the same value still refers to the first.
func TestNamedSecretsKeepTheirOwnValue(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": `services:
  app:
    image: example/app:1.0
    environment:
      DATABASE_URL: postgres://app:${POSTGRES_PASSWORD:-snapotter}@db/app
      REDIS_URL: redis://:${REDIS_PASSWORD:-snapotter}@cache:6379
      DB_PASS: snapotter
    depends_on: [db, cache]
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-snapotter}
  cache:
    image: redis:8
    command: redis-server --requirepass ${REDIS_PASSWORD:-snapotter}
`})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	values := map[string]ir.EnvFileEntry{}
	for _, entry := range plan.EnvFile {
		values[entry.Key] = entry
	}
	for _, key := range []string{"POSTGRES_PASSWORD", "REDIS_PASSWORD"} {
		if values[key].Generate == "" || values[key].Value != "" {
			t.Errorf("%s = %+v, want a secret of its own", key, values[key])
		}
	}
	if values["DB_PASS"].Value != "${POSTGRES_PASSWORD}" {
		t.Errorf("literal not linked to the named secret: %+v", values["DB_PASS"])
	}
}

func TestSecretsAreGeneratedAndLinked(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": `services:
  app:
    image: example/app:1.0
    environment:
      DATABASE_URL: postgres://app:secret@db/app
      APP_DB_PASSWORD: secret
      ADMIN_PASSWORD: hunter22
      CACHE_URL: redis://:other-secret@cache.example.com:6379
    depends_on: [db]
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: secret
`})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	values := map[string]ir.EnvFileEntry{}
	for _, entry := range plan.EnvFile {
		values[entry.Key] = entry
	}
	if values["APP_DB_PASSWORD"].Generate != ir.GenerateHex32 || values["APP_DB_PASSWORD"].Value != "" {
		t.Fatalf("first secret not generated: %+v", values["APP_DB_PASSWORD"])
	}
	if values["POSTGRES_PASSWORD"].Value != "${APP_DB_PASSWORD}" {
		t.Fatalf("same literal not linked: %+v", values["POSTGRES_PASSWORD"])
	}
	if values["DATABASE_URL"].Value != "postgres://app:${APP_DB_PASSWORD}@db/app" || !values["DATABASE_URL"].Secret {
		t.Fatalf("DSN not rewritten: %+v", values["DATABASE_URL"])
	}
	if values["ADMIN_PASSWORD"].Generate != ir.GeneratePassword24 {
		t.Fatalf("admin password generator %q", values["ADMIN_PASSWORD"].Generate)
	}
	if values["CACHE_URL"].Value != "redis://:${CACHE_URL_PASSWORD}@cache.example.com:6379" || !values["CACHE_URL_PASSWORD"].Required {
		t.Fatalf("external DSN not turned into a question: %+v %+v", values["CACHE_URL"], values["CACHE_URL_PASSWORD"])
	}
	encoded, _ := json.Marshal(plan)
	for _, leaked := range []string{"hunter22", ":secret@", "other-secret"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("upstream sample secret %q kept in the plan", leaked)
		}
	}
	app, _ := plan.Service("app")
	if app.DependsOn[0].Condition != ir.ConditionHealthy {
		t.Fatalf("app does not wait for a healthy database: %+v", app.DependsOn)
	}
	database, _ := plan.Service("db")
	if database.Role != ir.RoleDB || !database.Healthcheck.Defined() || len(database.Volumes) != 1 {
		t.Fatalf("sidecar rules not applied: %+v", database)
	}
}

// TestAllInOneImage follows an image that bundles its own Postgres and
// Redis: the passwords inside its connection strings and the app's keys are
// generated, the mail password is left to the user, settings named like
// secrets keep their values, and the two data folders stay two volumes.
func TestAllInOneImage(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": `services:
  cargo:
    image: ghcr.io/example/cargo:latest
    environment:
      DATABASE_URL: postgresql://cargo:${DB_PASSWORD}@127.0.0.1:5432/cargo
      REDIS_URL: redis://:${REDIS_PASSWORD}@127.0.0.1:6379/0
      SECRET_KEY: ${SECRET_KEY}
      JWT_SECRET_KEY: ${JWT_SECRET_KEY}
      JWT_ACCESS_TOKEN_EXPIRES: ${JWT_ACCESS_TOKEN_EXPIRES:-3600}
      CORS_SUPPORTS_CREDENTIALS: ${CORS_SUPPORTS_CREDENTIALS:-true}
      MAIL_PASSWORD: ${MAIL_PASSWORD}
      OLLAMA_BASE_URL: ${OLLAMA_BASE_URL:-http://localhost:11434}
      CARGO_BASE_URL: ${CARGO_BASE_URL:-http://localhost}
    ports: ["5000:5000"]
    volumes:
      - ./volumes/pgdata:/var/lib/postgresql/data
      - ./volumes/redis:/data
      - ./volumes/geoip:/app/volumes/geoip:ro
`, ".env.example": "SECRET_KEY=\nJWT_SECRET_KEY=\nDB_PASSWORD=\nREDIS_PASSWORD=\nMAIL_PASSWORD=\n"})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "cargo"}})
	entries := map[string]ir.EnvFileEntry{}
	for _, entry := range plan.EnvFile {
		entries[entry.Key] = entry
	}
	for _, generated := range []string{"DB_PASSWORD", "REDIS_PASSWORD", "SECRET_KEY", "JWT_SECRET_KEY"} {
		if entry := entries[generated]; !entry.Secret || entry.Generate == "" {
			t.Errorf("%s is not generated: %+v", generated, entry)
		}
	}
	if entry := entries["MAIL_PASSWORD"]; !entry.Secret || entry.Generate != "" || entry.Value != "" {
		t.Errorf("an outside credential was invented: %+v", entry)
	}
	settings := map[string]string{"JWT_ACCESS_TOKEN_EXPIRES": "3600", "CORS_SUPPORTS_CREDENTIALS": "true",
		"OLLAMA_BASE_URL": "http://localhost:11434", "CARGO_BASE_URL": "http://localhost:5000"}
	for key, value := range settings {
		if entry := entries[key]; entry.Secret || entry.Generate != "" || entry.Value != value {
			t.Errorf("setting %s was treated as a secret: %+v", key, entry)
		}
	}
	service, _ := plan.Service("cargo")
	names := map[string]string{}
	for _, volume := range service.Volumes {
		names[volume.ContainerPath] = volume.Name
	}
	if names["/var/lib/postgresql/data"] != "pgdata" || names["/data"] != "redis" || names["/app/volumes/geoip"] != "geoip" {
		t.Errorf("volumes: %v", names)
	}
	if !strings.Contains(strings.Join(service.Notes, "\n"), "read-only") {
		t.Errorf("the read-only mount is not explained: %q", service.Notes)
	}
}

// TestVolumesKeepOnePurpose: services that bind the same folder share its
// volume, and different folders with the same name never do.
func TestVolumesKeepOnePurpose(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": `services:
  app:
    image: example/app:1.0
    volumes: ["./uploads:/app/uploads", "./a/data:/data", "cache:/cache"]
  worker:
    image: example/worker:1.0
    volumes: ["./uploads:/srv/uploads", "./b/data:/data", "/scratch"]
  helper:
    image: example/helper:1.0
    volumes: ["./cache:/var/cache", "cache:/shared-cache"]
volumes:
  cache: {}
`})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	mounted := map[string]string{}
	for _, service := range plan.Services {
		for _, volume := range service.Volumes {
			mounted[service.Name+":"+volume.ContainerPath] = volume.Name
		}
	}
	want := map[string]string{"app:/app/uploads": "uploads", "worker:/srv/uploads": "uploads", "app:/data": "data", "worker:/data": "worker-data",
		"app:/cache": "cache", "helper:/shared-cache": "cache", "helper:/var/cache": "helper-cache", "worker:/scratch": "scratch"}
	for mount, name := range want {
		if mounted[mount] != name {
			t.Errorf("%s mounts %q, want %q (all: %v)", mount, mounted[mount], name, mounted)
		}
	}
}

// TestServiceNamesAreLowerCased: Compose allows capitals in a service name,
// as in snapotter-hq/SnapOtter, and the plan does not; the rules lower-case
// the name and every reference so the plan is not blocked for it.
func TestServiceNamesAreLowerCased(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": `services:
  SnapOtter:
    image: example/app:1.0
    ports: ["1349:1349"]
    depends_on:
      SnapDB:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://otter:otter-password@SnapDB:5432/otter
  SnapDB:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: otter-password
    healthcheck:
      test: ["CMD", "pg_isready"]
`})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	app, found := plan.Service("snapotter")
	if !found || len(app.DependsOn) != 1 || app.DependsOn[0].Service != "snapdb" || plan.Probe.Service != "snapotter" {
		t.Fatalf("names were not lower-cased everywhere: %+v", plan.Services)
	}
	defaults := config.Default()
	report := policy.Evaluate(policy.Input{Plan: plan, Profile: defaults.Profile, AllowRegistries: defaults.Registries.Allow, RepositoryOwner: "example"})
	for _, finding := range report.Findings {
		if strings.HasSuffix(finding.Path, ".name") {
			t.Errorf("a lower-cased name is still refused: %+v", finding)
		}
	}
}

func TestChooseComposeSkipsDevelopmentAndAddonFiles(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker/docker-compose-dev.yml":             "services:\n  app:\n    image: example/app:dev\n",
		"docker/compose/docker-compose.db.yml":      "services:\n  db:\n    image: postgres:17\n",
		"docker/compose/docker-compose.traefik.yml": "services:\n  app:\n    image: example/app:1\n  proxy:\n    image: traefik:v3\n",
		"docker/compose/docker-compose.yml":         "services:\n  app:\n    image: example/app:1\n",
		"scripts/test-env/compose.yml":              "services:\n  app:\n    image: example/app:test\n",
	})
	chosen := chooseCompose(facts, []string{"app", "example"})
	if chosen == nil || chosen.Path != "docker/compose/docker-compose.yml" {
		t.Fatalf("chose %+v", chosen)
	}
	// perber/leafwiki has no deployment compose file, only experiments: the
	// README's docker run is the plan then.
	experiments := testFacts(t, map[string]string{
		"hacks/proxy-auth/with-logout.yml": "services:\n  dex:\n    image: dexidp/dex:v2\n  app:\n    image: example/app:1\n",
		"e2e-proxy/docker-compose.yml":     "services:\n  app:\n    image: example/app:1\n",
	})
	if chosen := chooseCompose(experiments, []string{"app", "example"}); chosen != nil {
		t.Fatalf("chose the experiment %s", chosen.Path)
	}
	// Nor a file that mounts the repository (development), a staging file, a
	// proxy example, or one that only builds another program in the repository.
	skipped := testFacts(t, map[string]string{
		"docker-compose.yml":                      "services:\n  app:\n    build: .\n    image: app-dev\n    volumes:\n      - .:/app\n",
		"deploy/docker-compose.staging.yml":       "services:\n  app:\n    image: example/app:1\n",
		"examples/docker-compose-caddy-proxy.yml": "services:\n  app:\n    image: example/app:1\n  caddy:\n    image: caddy:2\n",
		"apps/workshop/docker-compose.yml":        "services:\n  workshop:\n    build: ../..\n",
	})
	if chosen := chooseCompose(skipped, []string{"app", "example"}); chosen != nil {
		t.Fatalf("chose %s", chosen.Path)
	}
}

// TestLocalBuildTagUsesThePublishedImage: mealie:dev only names a local
// build, so the plan uses the image the project documents, a stable tag
// before a nightly one.
func TestLocalBuildTagUsesThePublishedImage(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker/docker-compose.yml": "services:\n  mealie:\n    image: mealie:dev\n    build:\n      context: ../\n    ports:\n      - 9091:9000\n",
		"README.md":                 "# Mealie\n\nNightly: ghcr.io/mealie-recipes/mealie:nightly\n\nStable: ghcr.io/mealie-recipes/mealie:latest\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "mealie-recipes", Repository: "mealie"}})
	if image := plan.AppService().Image; image.Reference() != "ghcr.io/mealie-recipes/mealie" || image.Tag != "latest" {
		t.Fatalf("image %+v", image)
	}
}

// TestDocumentationFallbackFindsTheAppsImage: pi-hole's only compose file is
// a proxy example; its README names pihole/pihole, outside the owner's
// namespace and spelled without the hyphen, which becomes the plan's image.
func TestDocumentationFallbackFindsTheAppsImage(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"examples/docker-compose-caddy-proxy.yml": "services:\n  pihole:\n    image: pihole/pihole:latest\n  caddy:\n    image: caddy:latest\n",
		"README.md": "# Pi-hole\n\n```yaml\nservices:\n  pihole:\n    image: pihole/pihole:2026.09.0\n```\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "pi-hole", Repository: "docker-pi-hole"}})
	if image := plan.AppService().Image; len(plan.Services) != 1 || image.Reference() != "docker.io/pihole/pihole" || image.Tag != "2026.09.0" {
		t.Fatalf("services %+v", plan.Services)
	}
}

// TestListenAddressesOpenInAContainer: a sample written for the binary
// listens on loopback, which nothing outside a container reaches, so the
// app's own listen address becomes 0.0.0.0; another service's stays.
func TestListenAddressesOpenInAContainer(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"README.md":    "# leafwiki\n\n```\ndocker run -p 8080:8080 ghcr.io/perber/leafwiki:latest\n```\n",
		".env.example": "LEAFWIKI_HOST=127.0.0.1\nLEAFWIKI_PORT=8080\nBIND_ADDRESS=localhost:8080\nDB_HOST=127.0.0.1\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "perber", Repository: "leafwiki"}})
	want := map[string]string{"LEAFWIKI_HOST": "0.0.0.0", "BIND_ADDRESS": "0.0.0.0:8080", "DB_HOST": "127.0.0.1"}
	for key, value := range want {
		if entry, found := plan.EnvEntry(key); !found || entry.Value != value {
			t.Errorf("%s = %+v, want %q", key, entry, value)
		}
	}
}

func TestDocumentationFallbackPrefersTheOwnersNamespace(t *testing.T) {
	facts := testFacts(t, map[string]string{"README.md": "# vw\n\nAn example password manager server you can host yourself.\n\n" +
		"```\ndocker run -d -e DOMAIN=https://vw.domain.tld -v /vw-data/:/data/ -p 127.0.0.1:8000:80 vaultwarden/server:latest\n```\n\n" +
		"Also on ghcr.io/dani-garcia/vaultwarden\n"})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "dani-garcia", Repository: "vaultwarden"}})
	app := plan.AppService()
	if app.Image.Reference() != "ghcr.io/dani-garcia/vaultwarden" || app.Image.Tag != "latest" {
		t.Fatalf("image %+v", app.Image)
	}
	if len(app.Ports) != 1 || app.Ports[0].Container != 80 || app.Ports[0].HostPort != 8000 || app.Volumes[0].ContainerPath != "/data" {
		t.Fatalf("docker run settings lost: %+v", app)
	}
	domain, _ := plan.EnvEntry("DOMAIN")
	if domain.Value != "http://localhost:8000" {
		t.Fatalf("placeholder domain kept: %q", domain.Value)
	}
	if plan.App.Name != "vaultwarden" || plan.Confidence != ir.ConfidenceMedium {
		t.Fatalf("app %+v confidence %s", plan.App, plan.Confidence)
	}
}

// TestEnvFileSharesAppSettings: a service that loads .env, documented by
// example.env, gets the variables another service's environment uses, such
// as DB_PASSWORD, so both read one generated value; not those that only
// configure the compose file, such as UPLOAD_LOCATION and IMMICH_VERSION.
func TestEnvFileSharesAppSettings(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker/docker-compose.yml": "services:\n  server:\n    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-release}\n    env_file: [.env]\n" +
			"    volumes:\n      - ${UPLOAD_LOCATION}:/data\n  database:\n    image: postgres:16\n    environment:\n      POSTGRES_PASSWORD: ${DB_PASSWORD}\n",
		"docker/example.env": "UPLOAD_LOCATION=./library\nIMMICH_VERSION=release\nDB_PASSWORD=postgres\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "immich-app", Repository: "immich"}})
	server, _ := plan.Service("server")
	reads := map[string]string{}
	for _, variable := range server.Env {
		reads[variable.Key] = variable.Ref
	}
	_, compose := reads["UPLOAD_LOCATION"]
	_, tag := reads["IMMICH_VERSION"]
	if reads["DB_PASSWORD"] != "DB_PASSWORD" || compose || tag {
		t.Fatalf("server reads %v", reads)
	}
}

// TestPostgresDataPathFollowsTheVersion: from 18 the image refuses a volume
// at /var/lib/postgresql/data, so it is mounted at /var/lib/postgresql; 17
// keeps the old path, and so does a service that sets PGDATA itself.
func TestPostgresDataPathFollowsTheVersion(t *testing.T) {
	cases := map[string]struct{ database, want string }{
		"latest moves":        {"image: postgres:latest\n", "/var/lib/postgresql"},
		"pgvector pg18 moves": {"image: pgvector/pgvector:0.8.1-pg18\n", "/var/lib/postgresql"},
		"17 stays":            {"image: postgres:17-alpine\n", "/var/lib/postgresql/data"},
		"PGDATA stays":        {"image: postgres:18\n    environment:\n      PGDATA: /var/lib/postgresql/data/pgdata\n", "/var/lib/postgresql/data"},
	}
	for name, testCase := range cases {
		compose := "services:\n  app:\n    image: example/app:1\n  db:\n    " + testCase.database + "    volumes:\n      - db:/var/lib/postgresql/data\nvolumes:\n  db: {}\n"
		plan := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"compose.yaml": compose}),
			Source: &fetch.Source{Owner: "example", Repository: "app"}})
		database, _ := plan.Service("db")
		if len(database.Volumes) != 1 || database.Volumes[0].ContainerPath != testCase.want {
			t.Errorf("%s: volumes %+v, want one at %s", name, database.Volumes, testCase.want)
		}
	}
}

// TestMonorepoServiceGetsItsOwnDockerfile: in a monorepo each service takes
// the facts of the Dockerfile in the folder named after it, so the server
// does not inherit the machine-learning image's healthcheck, and the
// catalogue gives immich's images the memory they need.
func TestMonorepoServiceGetsItsOwnDockerfile(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker/docker-compose.yml":   "services:\n  immich-server:\n    image: ghcr.io/immich-app/immich-server:v3\n    ports:\n      - 2283:2283\n",
		"machine-learning/Dockerfile": "FROM python:3.13\nHEALTHCHECK CMD python3 healthcheck.py\n",
		"server/Dockerfile":           "FROM node:24\nHEALTHCHECK CMD immich-healthcheck\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "immich-app", Repository: "immich"}})
	server := plan.AppService()
	if strings.Join(server.Healthcheck.Test, " ") != "CMD-SHELL immich-healthcheck" || server.Resources.MemoryLimit != "2g" {
		t.Fatalf("healthcheck %v, memory %q", server.Healthcheck.Test, server.Resources.MemoryLimit)
	}
}

// TestAppThatNeedsADatabaseGetsOne: an app whose documented settings leave
// its database host empty gets the database they name, by prefix or port;
// a host with nothing naming the database, or one the app may go without,
// adds nothing.
func TestAppThatNeedsADatabaseGetsOne(t *testing.T) {
	run := func(settings string) map[string]string {
		return map[string]string{"README.md": "# wiki\n\n```\ndocker run -p 6875:80 " + settings + " ghcr.io/example/wiki:1.0\n```\n"}
	}
	cases := []struct {
		name, want string
		files      map[string]string
	}{
		{"port names MariaDB", "docker.io/library/mariadb:11.8", run("-e DB_HOST= -e DB_PORT=3306 -e DB_USERNAME= -e DB_PASSWORD= -e DB_DATABASE=")},
		{"prefix names Postgres", "docker.io/library/postgres:18", run("-e POSTGRES_HOST= -e POSTGRES_USER= -e POSTGRES_PASSWORD= -e POSTGRES_DB=")},
		{"nothing names the database", "", run("-e DB_HOST= -e DB_PASSWORD=")},
		{"optional host", "", map[string]string{"README.md": "# wiki\n\n```\ndocker run -p 6875:80 ghcr.io/example/wiki:1.0\n```\n", ".env.example": "DB_HOST=\nDB_PORT=3306\n"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := Build(context.Background(), Input{Facts: testFacts(t, test.files), Source: &fetch.Source{Owner: "example", Repository: "wiki"}})
			databases := slices.DeleteFunc(slices.Clone(plan.Services), func(service ir.Service) bool { return service.Role != ir.RoleDB })
			if test.want == "" {
				if len(databases) != 0 {
					t.Fatalf("added %+v", databases)
				}
				return
			}
			if len(databases) != 1 || databases[0].Image.Reference()+":"+databases[0].Image.Tag != test.want {
				t.Fatalf("databases %+v", databases)
			}
			database, app := databases[0], plan.AppService()
			if !database.Healthcheck.Defined() || len(database.Volumes) != 1 || app.DependsOn[0] != (ir.Dependency{Service: database.Name, Condition: ir.ConditionHealthy}) {
				t.Fatalf("database %+v, app depends on %+v", database, app.DependsOn)
			}
			for _, variable := range database.Env {
				if _, found := plan.EnvEntry(variable.Ref); !found {
					t.Fatalf("%s refers to missing %s", variable.Key, variable.Ref)
				}
			}
		})
	}
}

func TestDatabaseSettingsReachTheAddedDatabase(t *testing.T) {
	facts := testFacts(t, map[string]string{"README.md": "# BookStack\n\n```\ndocker run -p 6875:80 -e APP_KEY= -e DB_HOST= -e DB_PORT=3306 -e DB_USERNAME= " +
		"-e DB_PASSWORD= -e DB_DATABASE= lscr.io/linuxserver/bookstack:latest\n```\n"})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "linuxserver", Repository: "docker-bookstack"}})
	want := map[string]ir.EnvFileEntry{
		"DB_HOST": {Value: "mariadb"}, "DB_USERNAME": {Value: "bookstack"}, "DB_DATABASE": {Value: "bookstack"},
		"DB_PASSWORD": {Generate: ir.GenerateHex32}, "MARIADB_ROOT_PASSWORD": {Generate: ir.GenerateHex32}, "APP_KEY": {Generate: ir.GenerateLaravelKey},
	}
	for key, expected := range want {
		entry, found := plan.EnvEntry(key)
		if !found || entry.Value != expected.Value || entry.Generate != expected.Generate {
			t.Errorf("%s = %+v, want value %q generated %q", key, entry, expected.Value, expected.Generate)
		}
	}
	database, _ := plan.Service("mariadb")
	refs := map[string]string{}
	for _, variable := range database.Env {
		refs[variable.Key] = variable.Ref
	}
	if refs["MARIADB_USER"] != "DB_USERNAME" || refs["MARIADB_PASSWORD"] != "DB_PASSWORD" || refs["MARIADB_DATABASE"] != "DB_DATABASE" {
		t.Fatalf("database settings %v", refs)
	}
}

// TestComposeWithOnlyAProxyDeploysIt: a folder whose compose file runs only
// nginx deploys nginx, while a file with only a database is still an add-on
// to merge into another file, so nothing is planned from it.
func TestComposeWithOnlyAProxyDeploysIt(t *testing.T) {
	cases := []struct{ name, compose, want string }{
		{"web server", "services:\n  web:\n    image: nginx:1.27\n    ports: [\"8080:80\"]\n", "docker.io/library/nginx:1.27"},
		{"database", "services:\n  db:\n    image: postgres:18\n", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			facts := testFacts(t, map[string]string{"compose.yaml": test.compose})
			plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Repository: "site"}})
			app := plan.AppService()
			got := ""
			if app != nil && app.Image.Repository != "" {
				got = app.Image.Reference() + ":" + app.Image.Tag
			}
			if got != test.want {
				t.Fatalf("app image %q, want %q; services %+v", got, test.want, plan.Services)
			}
		})
	}
}

func TestParsePortMapping(t *testing.T) {
	cases := map[string]ir.Port{
		"8080":                {Container: 8080, Protocol: "tcp", Publish: true},
		"8000:80":             {Container: 80, Protocol: "tcp", Publish: true, HostPort: 8000},
		"127.0.0.1:53:53/udp": {Container: 53, Protocol: "udp", Publish: true},
	}
	for input, want := range cases {
		got, ok := parsePortMapping(input)
		if !ok || got != want {
			t.Errorf("parsePortMapping(%q) = %+v, want %+v", input, got, want)
		}
	}
	if HostPort(ir.Port{Container: 80}) != 8080 || HostPort(ir.Port{Container: 3001}) != 3001 || HostPort(ir.Port{Container: 80, HostPort: 9000}) != 9000 {
		t.Fatal("HostPort")
	}
}

// testFacts writes files into a temporary repository and extracts them.
func testFacts(t *testing.T, files map[string]string) *extract.Facts {
	t.Helper()
	directory := t.TempDir()
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := extract.Extract(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// TestProbePortIsTCP: the probe asks over HTTP, so it takes the first
// published TCP port, else the first TCP one, and never a UDP port.
func TestProbePortIsTCP(t *testing.T) {
	cases := []struct {
		name  string
		ports []ir.Port
		want  int
	}{
		{"udp first", []ir.Port{{Container: 51820, Protocol: ir.ProtocolUDP, Publish: true}, {Container: 51821, Protocol: ir.ProtocolTCP, Publish: true}}, 51821},
		{"published wins", []ir.Port{{Container: 8080, Protocol: ir.ProtocolTCP}, {Container: 9090, Protocol: ir.ProtocolTCP, Publish: true}}, 9090},
		{"none published", []ir.Port{{Container: 8080, Protocol: ir.ProtocolTCP}}, 8080},
		{"udp only", []ir.Port{{Container: 53, Protocol: ir.ProtocolUDP, Publish: true}}, 0},
	}
	for _, testCase := range cases {
		port, _ := probePort(&ir.Service{Ports: testCase.ports})
		if port != testCase.want {
			t.Errorf("%s: probe port %d, want %d", testCase.name, port, testCase.want)
		}
	}
}

// TestDockerSocketBecomesANeed: a socket upstream mounts, in compose or in the
// README's docker run, becomes one docker_socket request per service with
// where the app expects it, and never a volume.
func TestDockerSocketBecomesANeed(t *testing.T) {
	sources := map[string]map[string]string{
		"compose": {"docker-compose.yml": "services:\n  ui:\n    image: example/ui:1.0\n" +
			"    volumes: [\"/var/run/docker.sock:/var/run/docker.sock:ro\", \"/run/docker.sock:/var/run/docker.sock\", \"data:/data\"]\nvolumes:\n  data: {}\n"},
		"readme": {"README.md": "# UI\n\n```sh\ndocker run -d -p 8080:8080 -v /var/run/docker.sock:/var/run/docker.sock example/ui:1.0\n```\n"},
	}
	for name, files := range sources {
		plan := Build(context.Background(), Input{Facts: testFacts(t, files), Source: &fetch.Source{Owner: "example", Repository: "ui"}})
		if len(plan.Needs) != 1 {
			t.Fatalf("%s: needs %+v", name, plan.Needs)
		}
		need := plan.Needs[0]
		if need.Capability != ir.NeedDockerSocket || need.Service != plan.AppService().Name || need.Detail != ir.DockerSocket {
			t.Fatalf("%s: need %+v", name, need)
		}
		for _, volume := range plan.AppService().Volumes {
			if strings.Contains(volume.ContainerPath, "docker.sock") {
				t.Fatalf("%s: the socket became a volume: %+v", name, volume)
			}
		}
	}
}

// TestOptionalSettingsComeFromSamplesAndDocs: what a .env sample or a README
// or docs table documents and the plan leaves unset becomes an optional
// setting of the app, with the documented default, never an example secret,
// and never a variable for tests or one the plan already sets.
func TestOptionalSettingsComeFromSamplesAndDocs(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: example/app:1.0\n    environment:\n      APP_URL: ${APP_URL}\n",
		".env.example":       "APP_URL=http://localhost:8080\n# Log level, one of debug, info, warn\n# APP_LOG_LEVEL=info\n# APP_ADMIN_TOKEN=changeme-example\n",
		"README.md": "# App\n\n| Variable | Default | Description |\n|---|---|---|\n| `APP_CACHE_TTL` | `5m` | How long pages are cached |\n" +
			"| `APP_PROXY` | not set | An outbound proxy |\n| `APP_ORIGIN` | (empty) | Allowed origins |\n| `APP_SOCKET` | the mounted socket | Where Docker answers |\n| `AVI` | | A video format |\n| `APP_SILENT` | | |\n",
		"tests/README.md": "| Variable | Description |\n|---|---|\n| `APP_TEST_SEED` | Seed for the tests |\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	settings := map[string]ir.OptionalSetting{}
	for _, setting := range plan.OptionalEnv {
		settings[setting.Key] = setting
	}
	want := map[string]string{"APP_LOG_LEVEL": "info", "APP_ADMIN_TOKEN": "", "APP_CACHE_TTL": "5m", "APP_PROXY": "", "APP_ORIGIN": "", "APP_SOCKET": ""}
	if len(settings) != len(want) {
		t.Fatalf("optional settings %v, want %v", settings, want)
	}
	for key, value := range want {
		setting, found := settings[key]
		if !found || setting.Default != value || setting.Service != plan.AppService().Name {
			t.Errorf("%s: %+v, want default %q for the app", key, setting, value)
		}
	}
	if !strings.Contains(settings["APP_SOCKET"].Description, "Default: the mounted socket") || settings["APP_LOG_LEVEL"].Description == "" {
		t.Errorf("descriptions: %q, %q", settings["APP_SOCKET"].Description, settings["APP_LOG_LEVEL"].Description)
	}
}

// TestOptionalDefaultsComeFromTheImage: when this repository builds the
// app's image, a setting its Dockerfile sets shows the container's value,
// not a sample's path for running without Docker, which would lie outside
// every volume.
func TestOptionalDefaultsComeFromTheImage(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: example/app:1.0\n",
		"Dockerfile":         "FROM alpine:3\nENV FILES_STORAGE_PATH=/data/files\nENV LOG_DIR=${DATA_DIR}/logs\n",
		".env.example":       "FILES_STORAGE_PATH=./data/files\nLOG_DIR=./data/logs\nAPP_THEME=light\n",
	})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "app"}})
	settings := map[string]string{}
	for _, setting := range plan.OptionalEnv {
		settings[setting.Key] = setting.Default
	}
	want := map[string]string{"FILES_STORAGE_PATH": "/data/files", "LOG_DIR": "", "APP_THEME": "light"}
	for key, value := range want {
		if got, found := settings[key]; !found || got != value {
			t.Errorf("%s default %q (listed %v), want %q", key, got, found, value)
		}
	}
}
