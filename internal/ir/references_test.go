package ir

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReferencesTraceSharedAndSeparateValues(t *testing.T) {
	plan := &IR{EnvFile: []EnvFileEntry{{Key: "PASSWORD", Secret: true, Generate: "random"}, {Key: "URL", Value: "postgres://${PASSWORD}@db"}, {Key: "API_MODE", Value: "same"}, {Key: "DB_MODE", Value: "same"}}, Services: []Service{
		{Name: "api", Env: []EnvRef{{Key: "DATABASE_URL", Ref: "URL"}, {Key: "MODE", Ref: "API_MODE"}}, DependsOn: []Dependency{{Service: "db"}}},
		{Name: "db", Env: []EnvRef{{Key: "POSTGRES_PASSWORD", Ref: "PASSWORD"}, {Key: "MODE", Ref: "DB_MODE"}}},
		{Name: "worker", DependsOn: []Dependency{{Service: "api"}}},
	}}
	before, _ := json.Marshal(plan)
	graph := plan.References()
	names := func(key string) []string {
		var names []string
		for _, node := range graph.Consumers(key) {
			names = append(names, node.Name)
		}
		return names
	}
	for key, want := range map[string][]string{"PASSWORD": {"api", "db"}, "URL": {"api"}, "API_MODE": {"api"}, "DB_MODE": {"db"}} {
		if got := names(key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s consumers: %v want %v", key, got, want)
		}
	}
	if !hasReferenceEdge(graph, "PASSWORD", "URL", ReferenceDerived, "") || !hasReferenceEdge(graph, "URL", "api", ReferenceConsumer, "DATABASE_URL") || !hasReferenceEdge(graph, "db", "api", ReferenceStartup, "") {
		t.Fatal("reference chain or startup edge missing")
	}
	if len(graph.Cycles) != 0 {
		t.Fatalf("false cycle: %+v", graph.Cycles)
	}
	after, _ := json.Marshal(plan)
	if string(before) != string(after) {
		t.Fatal("inspecting references mutated plan")
	}
	repeat, _ := json.Marshal(plan.References())
	first, _ := json.Marshal(graph)
	if string(first) != string(repeat) {
		t.Fatal("graph is nondeterministic")
	}
}

func hasReferenceEdge(graph ReferenceGraph, from, to, kind, field string) bool {
	sourceKind, targetKind := ReferenceEnv, ReferenceService
	if kind == ReferenceDerived {
		targetKind = ReferenceEnv
	}
	if kind == ReferenceStartup {
		sourceKind = ReferenceService
	}
	for _, edge := range graph.Edges {
		if edge.From == ReferenceID(sourceKind, from) && edge.To == ReferenceID(targetKind, to) && edge.Kind == kind && edge.Field == field {
			return true
		}
	}
	return false
}

func TestReferencesCyclesMissingAndNamespace(t *testing.T) {
	plan := &IR{EnvFile: []EnvFileEntry{{Key: "A", Value: "${B}"}, {Key: "B", Value: "${A}"}, {Key: "SELF", Value: "${SELF}"}}, Services: []Service{
		{Name: "A", Env: []EnvRef{{Key: "VALUE", Ref: "UNKNOWN"}}, DependsOn: []Dependency{{Service: "B"}, {Service: "absent"}}},
		{Name: "B", DependsOn: []Dependency{{Service: "A"}}},
	}}
	graph := plan.References()
	if len(graph.Cycles) != 3 {
		t.Fatalf("expected variable, self and service cycles: %+v", graph.Cycles)
	}
	members := map[string]int{}
	for _, cycle := range graph.Cycles {
		for _, node := range cycle {
			members[node.Kind+":"+node.Name]++
		}
	}
	for _, name := range []string{"env:A", "env:B", "env:SELF", "service:A", "service:B"} {
		if members[name] != 1 {
			t.Errorf("cycle member %s: %d", name, members[name])
		}
	}
	missing := map[string]bool{}
	for _, node := range graph.Nodes {
		if node.Missing {
			missing[node.Name] = true
		}
	}
	if !missing["UNKNOWN"] || !missing["absent"] {
		t.Fatal("missing definitions not reported")
	}
	if ReferenceID(ReferenceEnv, "A") == ReferenceID(ReferenceService, "A") {
		t.Fatal("variable and service anchors collide")
	}
}

func TestReferencesContainNamesOnlyAndRespectEscapes(t *testing.T) {
	plan := &IR{App: App{Summary: "private-summary-marker"}, EnvFile: []EnvFileEntry{{Key: "PASSWORD", Value: "private-password-marker", Secret: true}, {Key: "URL", Value: "scheme://${PASSWORD:-private-fallback-marker}"}, {Key: "BARE", Value: "$IGNORED"}}, Services: []Service{{Name: "app", Env: []EnvRef{{Key: "URL", Ref: "URL"}, {Key: "LITERAL", Literal: "private-literal-marker"}}, Command: []string{"echo $RUNTIME $${ESCAPED} ${COMMAND:-private-command-marker}"}, Healthcheck: Healthcheck{Test: []string{"echo ${HEALTH}"}}}}}
	graph := plan.References()
	encoded, _ := json.Marshal(graph)
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "scheme://") {
		t.Fatalf("graph retained a source value: %s", encoded)
	}
	for _, name := range []string{"RUNTIME", "COMMAND", "HEALTH"} {
		if !hasReferenceEdge(graph, name, "app", ReferenceConsumer, map[string]string{"RUNTIME": "command", "COMMAND": "command", "HEALTH": "healthcheck"}[name]) {
			t.Errorf("missing reference %s", name)
		}
	}
	for _, node := range graph.Nodes {
		if node.Name == "IGNORED" || node.Name == "ESCAPED" {
			t.Errorf("literal/escaped value treated as reference: %s", node.Name)
		}
	}
}

func TestReferencesFileSecretsAndOptionalPassthrough(t *testing.T) {
	plan := &IR{EnvFile: []EnvFileEntry{{Key: "PASSWORD", Secret: true, Generate: "random", FileEnvKey: "PASSWORD_FILE"}}, Services: []Service{{Name: "app", Env: []EnvRef{{Key: "PASSWORD", Ref: "PASSWORD"}}}, {Name: "db"}}, OptionalEnv: []OptionalSetting{{Key: "TZ", Service: "app"}, {Key: "TZ", Service: "db"}, {Key: "PASSWORD", Service: "db"}, {Key: "DISABLED", Service: "absent"}}}
	graph := plan.References()
	if !hasReferenceEdge(graph, "PASSWORD", "app", ReferenceConsumer, "PASSWORD_FILE") || !hasReferenceEdge(graph, "TZ", "app", ReferenceConsumer, "TZ") {
		t.Fatal("file secret or optional passthrough missing")
	}
	if hasReferenceEdge(graph, "TZ", "db", ReferenceConsumer, "TZ") || hasReferenceEdge(graph, "PASSWORD", "db", ReferenceConsumer, "PASSWORD") {
		t.Fatal("suppressed optional setting became a consumer")
	}
	for _, node := range graph.Nodes {
		if node.Name == "TZ" && node.Missing {
			t.Fatal("optional setting marked missing")
		}
		if node.Name == "DISABLED" {
			t.Fatal("disabled service became a consumer")
		}
	}
}
