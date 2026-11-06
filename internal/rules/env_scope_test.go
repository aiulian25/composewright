package rules

import (
	"context"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
)

func TestServiceEnvironmentValuesStayDistinct(t *testing.T) {
	facts := testFacts(t, map[string]string{"compose.yaml": `services:
  api:
    image: example/api:1
    environment: {PORT: "8080", PASSWORD: alpha-secret, SHARED: "${TOKEN:-shared}"}
  worker:
    image: example/worker:1
    environment: {PORT: "9090", PASSWORD: beta-secret, SHARED: "${TOKEN:-shared}"}
`})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "api"}})
	refs := map[string]map[string]string{}
	for _, service := range plan.Services {
		refs[service.Name] = map[string]string{}
		for _, variable := range service.Env {
			refs[service.Name][variable.Key] = variable.Ref
		}
	}
	for _, key := range []string{"PORT", "PASSWORD"} {
		if refs["api"][key] == refs["worker"][key] {
			t.Fatalf("%s still shares %q", key, refs["api"][key])
		}
	}
	for service, want := range map[string]string{"api": "8080", "worker": "9090"} {
		entry, ok := plan.EnvEntry(refs[service]["PORT"])
		if !ok || entry.Value != want {
			t.Fatalf("%s PORT: %+v", service, entry)
		}
		password, ok := plan.EnvEntry(refs[service]["PASSWORD"])
		if !ok || !password.Secret || password.Generate == "" || password.Value != "" {
			t.Fatalf("%s password: %+v", service, password)
		}
	}
	if refs["api"]["SHARED"] != "TOKEN" || refs["worker"]["SHARED"] != "TOKEN" {
		t.Fatal("explicit sharing lost")
	}
}

func TestScopedEnvironmentIdentity(t *testing.T) {
	scopeTestRefs := map[string]string{}
	for _, reversed := range []bool{false, true} {
		first := "  api:\n    image: example/api:1\n    env_file: [api.env]\n    environment: {API_PORT: reserved}\n"
		second := "  worker:\n    image: example/worker:1\n    env_file: [worker.env]\n"
		if reversed {
			first, second = second, first
		}
		facts := testFacts(t, map[string]string{"compose.yaml": "services:\n" + first + second, "api.env": "PORT=8080\n", "worker.env": "PORT=9090\n"})
		plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "api"}})
		for _, service := range plan.Services {
			for _, variable := range service.Env {
				if variable.Key != "PORT" {
					continue
				}
				if variable.Ref == "PORT" || variable.Ref == "API_PORT" {
					t.Fatalf("collision: %+v", variable)
				}
				want := "9090"
				if service.Name == "api" {
					want = "8080"
				}
				entry, _ := plan.EnvEntry(variable.Ref)
				if entry.Value != want {
					t.Fatalf("%s: %+v", service.Name, entry)
				}
				identity := service.Name + "/" + variable.Key
				if old, ok := scopeTestRefs[identity]; ok && old != variable.Ref {
					t.Fatalf("reorder changed %s: %s -> %s", identity, old, variable.Ref)
				}
				scopeTestRefs[identity] = variable.Ref
			}
		}
	}
}

func TestScopedOwnSecretKeepsItsSource(t *testing.T) {
	facts := testFacts(t, map[string]string{"compose.yaml": `services:
  api:
    image: example/api:1
    environment: {PASSWORD: first-literal}
  worker:
    image: example/worker:1
    environment: {PASSWORD: second-literal}
`, ".env": "PASSWORD=person-supplied-secret\n"})
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Kind: fetch.SourceLocal, Owner: "example", Repository: "api"}})
	for _, key := range []string{"API_PASSWORD", "WORKER_PASSWORD"} {
		entry, ok := plan.EnvEntry(key)
		if !ok || entry.Value != "${PASSWORD}" || entry.Generate != "" {
			t.Fatalf("own secret source lost: %+v", entry)
		}
	}
	entry, ok := plan.EnvEntry("PASSWORD")
	if !ok || entry.Value != "" || entry.Generate != "" {
		t.Fatalf("own secret must stay outside IR: %+v", entry)
	}
}
