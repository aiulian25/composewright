package rules

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/ir"
)

// profileLine puts a service behind a profile; noProfileLine takes its
// place, so every other line keeps its number.
const (
	profileLine   = "    profiles: [extra]\n"
	noProfileLine = "    # no profile\n"
)

var notesSource = &fetch.Source{Owner: "example", Repository: "notes"}

// TestProfileServicesAreOptional: a service upstream starts only with a
// profile is listed, not built, until it is switched on; it never takes the
// app's place or rules its compose file out.
func TestProfileServicesAreOptional(t *testing.T) {
	const app = "  app:\n    image: example/notes:1.0\n    ports: [\"8080:8080\"]\n"
	const tts = "  tts:\n    image: example/notes-tts:1.0\n    profiles: [tts]\n    environment:\n      VOICE: ${TTS_VOICE:-alto}\n"
	plan := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": "services:\n" + tts + app}), Source: notesSource})
	if err := ir.ValidateSchema(plan); err != nil {
		t.Fatal(err)
	}
	want := []ir.OptionalService{{Name: "tts", Image: "example/notes-tts:1.0", Profiles: []string{"tts"}, DependsOn: []string{}}}
	if len(plan.Services) != 1 || plan.AppService().Name != "app" || !slices.EqualFunc(plan.OptionalServices, want, sameOptional) {
		t.Fatalf("services %+v, optional %+v", plan.Services, plan.OptionalServices)
	}
	if _, found := plan.EnvEntry("TTS_VOICE"); found {
		t.Fatalf("a switched-off service brought its variable: %+v", plan.EnvFile)
	}
	enabled := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": "services:\n" + tts + app}),
		Source: notesSource, EnabledOptional: []string{"TTS"}})
	service, found := enabled.Service("tts")
	if !found || enabled.AppService().Name != "app" || !slices.Contains(service.Notes, "optional: upstream profile tts") {
		t.Fatalf("switched on: %+v", enabled.Services)
	}
	const development = "  dev:\n    image: example/notes:dev\n    profiles: [dev]\n    volumes: [\".:/src\"]\n"
	mounted := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": "services:\n" + app + development}), Source: notesSource})
	if len(mounted.Services) != 1 || mounted.Services[0].Provenance.Source != ir.SourceCompose {
		t.Fatalf("an optional development service ruled the compose file out: %+v", mounted.Services)
	}
}

// TestSwitchedOnIsOrdinary: the plan with optional services switched on is
// the plan of the same file with them as ordinary services, so passwords
// shared with them, generated for them and volumes they mount come out as
// the rules make them for any service.
func TestSwitchedOnIsOrdinary(t *testing.T) {
	const app = "  app:\n    image: example/notes:1.0\n    ports: [\"8080:8080\"]\n    volumes: [\"./data:/app/data\"]\n"
	for name, services := range map[string]string{
		"a database shares the app's password": app + "    environment:\n      DB_PASSWORD: supersecretchangeme\n" +
			"  db:\n    image: docker.io/library/postgres:16\n" + profileLine + "    environment:\n      POSTGRES_PASSWORD: supersecretchangeme\n",
		"a database generates the password the app reads": app + "    environment:\n      DB_PASSWORD: ${DB_PASSWORD}\n" +
			"  db:\n    image: docker.io/library/postgres:16\n" + profileLine + "    environment:\n      POSTGRES_PASSWORD: ${DB_PASSWORD}\n",
		"volumes stay apart": app + "  tts:\n    image: example/tts:1.0\n" + profileLine + "    volumes: [\"/data\", \"./models:/models\", \"data:/cache\"]\n",
		"a dependency comes along": app + "  indexer:\n    image: example/indexer:1.0\n" + profileLine + "    depends_on: [db]\n" +
			"    environment:\n      INDEXER_DB_PASSWORD: supersecretchangeme\n" +
			"  db:\n    image: docker.io/library/postgres:16\n" + profileLine + "    environment:\n      POSTGRES_PASSWORD: supersecretchangeme\n",
	} {
		t.Run(name, func(t *testing.T) {
			optional := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": "services:\n" + services}),
				Source: notesSource, EnabledOptional: []string{"db", "tts", "indexer"}})
			ordinary := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": "services:\n" + strings.ReplaceAll(services, profileLine, noProfileLine)}),
				Source: notesSource})
			if len(optional.OptionalServices) == 0 || len(optional.Services) != len(ordinary.Services) {
				t.Fatalf("optional %+v, services %+v", optional.OptionalServices, optional.Services)
			}
			optional.OptionalServices = ordinary.OptionalServices
			for index := range optional.Services {
				service := &optional.Services[index]
				service.Notes = slices.DeleteFunc(service.Notes, func(note string) bool { return strings.HasPrefix(note, "optional: upstream profile") })
			}
			if got, want := encodedPlan(t, optional), encodedPlan(t, ordinary); got != want {
				t.Fatalf("switched on:\n%s\nas ordinary services:\n%s", got, want)
			}
		})
	}
}

// TestEnabledBringsDependencies: switching on an optional service switches
// on the optional services it depends on, names match in any case and come
// back as the plan spells them, and a name the plan does not offer is
// refused.
func TestEnabledBringsDependencies(t *testing.T) {
	const compose = "services:\n  app:\n    image: example/notes:1.0\n    ports: [\"8080:8080\"]\n" +
		"  Grafana:\n    image: example/grafana:1.0\n    profiles: [monitoring]\n    depends_on: [prometheus, app]\n" +
		"  prometheus:\n    image: example/prometheus:1.0\n    profiles: [monitoring]\n    depends_on: [exporter]\n" +
		"  exporter:\n    image: example/exporter:1.0\n    profiles: [monitoring]\n"
	plan := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": compose}), Source: notesSource})
	enabled, err := Enabled(plan.OptionalServices, []string{"GRAFANA"})
	if err != nil || !slices.Equal(enabled, []string{"grafana", "prometheus", "exporter"}) {
		t.Fatalf("switched on %v: %v", enabled, err)
	}
	if grafana := plan.OptionalServices[0]; !slices.Equal(grafana.DependsOn, []string{"prometheus"}) {
		t.Fatalf("grafana depends on %v", grafana.DependsOn)
	}
	// Listed before what needs it, a dependency of a dependency takes a
	// second pass to find.
	reversed := slices.Clone(plan.OptionalServices)
	slices.Reverse(reversed)
	if enabled, err := Enabled(reversed, []string{"grafana"}); err != nil || !slices.Equal(enabled, []string{"exporter", "prometheus", "grafana"}) {
		t.Fatalf("switched on %v: %v", enabled, err)
	}
	if _, err := Enabled(plan.OptionalServices, []string{"gpu"}); !errors.Is(err, ErrNotOptional) {
		t.Fatalf("unknown optional service: %v", err)
	}
}

func sameOptional(left, right ir.OptionalService) bool {
	return left.Name == right.Name && left.Image == right.Image && slices.Equal(left.Profiles, right.Profiles) && slices.Equal(left.DependsOn, right.DependsOn)
}

func encodedPlan(t *testing.T, plan *ir.IR) string {
	t.Helper()
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestNoDependencyOnAServiceThatIsOff: Compose refuses a stack whose app
// depends on a service it does not define, so the app waits for an optional
// cache only while the cache is on.
func TestNoDependencyOnAServiceThatIsOff(t *testing.T) {
	facts := testFacts(t, map[string]string{"docker-compose.yml": "services:\n  app:\n    image: example/notes:1.0\n    ports: [\"8080:8080\"]\n" +
		"    depends_on:\n      cache:\n        condition: service_started\n        required: false\n" +
		"  cache:\n    image: docker.io/library/redis:7\n" + profileLine})
	waitsForCache := func(plan *ir.IR) bool {
		app, _ := plan.Service("app")
		return slices.ContainsFunc(app.DependsOn, func(dependency ir.Dependency) bool { return dependency.Service == "cache" })
	}
	off := Build(context.Background(), Input{Facts: facts, Source: notesSource})
	if waitsForCache(off) {
		t.Fatalf("with the cache off the app still depends on it: %+v", off.Services)
	}
	app, _ := off.Service("app")
	if !slices.ContainsFunc(app.Notes, func(note string) bool { return strings.Contains(note, "cache, an optional service that is off") }) {
		t.Errorf("no note says the dependency was dropped: %v", app.Notes)
	}
	if on := Build(context.Background(), Input{Facts: facts, Source: notesSource, EnabledOptional: []string{"cache"}}); !waitsForCache(on) {
		t.Fatalf("with the cache on the app no longer waits for it: %+v", on.Services)
	}
}

// TestSwitchingOnKeepsTheSecretOwner: a password an always-on database
// shares with a switched-on service listed before it stays the database's,
// so switching the service on never moves a password already deployed.
func TestSwitchingOnKeepsTheSecretOwner(t *testing.T) {
	const password = "Xk9pLm2qRt7v"
	compose := "services:\n  backup:\n    image: example/backup:1.0\n" + profileLine + "    environment:\n      BACKUP_DB_PASSWORD: " + password + "\n" +
		"  db:\n    image: docker.io/library/postgres:16\n    environment:\n      POSTGRES_PASSWORD: " + password + "\n" +
		"  app:\n    image: example/notes:1.0\n    ports: [\"8080:8080\"]\n    environment:\n      DB_PASSWORD: " + password + "\n"
	for _, enabled := range [][]string{nil, {"backup"}} {
		plan := Build(context.Background(), Input{Facts: testFacts(t, map[string]string{"docker-compose.yml": compose}), Source: notesSource, EnabledOptional: enabled})
		owner, _ := plan.EnvEntry("POSTGRES_PASSWORD")
		if owner == nil || owner.Generate == "" {
			t.Fatalf("with %v POSTGRES_PASSWORD is %+v", enabled, owner)
		}
		if names := []string{plan.Services[0].Name, plan.Services[1].Name}; len(enabled) > 0 && !slices.Equal(names, []string{"backup", "db"}) {
			t.Fatalf("the services left the file's order: %v", names)
		}
		readers := []string{"DB_PASSWORD"}
		if len(enabled) > 0 {
			readers = append(readers, "BACKUP_DB_PASSWORD")
		}
		for _, key := range readers {
			if entry, _ := plan.EnvEntry(key); entry == nil || entry.Value != "${POSTGRES_PASSWORD}" {
				t.Errorf("with %v %s is %+v", enabled, key, entry)
			}
		}
	}
}
