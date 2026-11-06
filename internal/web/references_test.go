package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
)

func TestReferenceInspectorUsesSavedIdentityAndNoCredentials(t *testing.T) {
	plan := &ir.IR{EnvFile: []ir.EnvFileEntry{{Key: "PASSWORD", Secret: true, Generate: "random"}, {Key: "URL", Value: "scheme://${PASSWORD}"}, {Key: "LOOP", Value: "${LOOP}"}, {Key: "UNKNOWN_USE", Value: "${MISSING}"}}, Services: []ir.Service{
		{Name: "api", Env: []ir.EnvRef{{Key: "DATABASE_URL", Ref: "URL"}}, DependsOn: []ir.Dependency{{Service: "db"}}},
		{Name: "db", Env: []ir.EnvRef{{Key: "PASSWORD", Ref: "PASSWORD"}}, DependsOn: []ir.Dependency{{Service: "api"}}},
	}, OptionalEnv: []ir.OptionalSetting{{Key: "TZ", Service: "api"}, {Key: "INACTIVE", Service: "not-enabled"}}}
	stored := &pipeline.Stored{Plan: plan, Generation: store.Generation{ID: "saved-reference-version", Number: 7}, Options: pipeline.Options{Edited: true}, Values: map[string]string{"PASSWORD": "private-credential-needle", "URL": "scheme://${PASSWORD}"}}
	references := referenceViewFor(stored)
	if references.Base != stored.Generation.ID || !references.Edited || len(references.Cycles) != 2 {
		t.Fatalf("wrong snapshot/cycles: %+v", references)
	}
	encoded, _ := json.Marshal(references)
	if strings.Contains(string(encoded), "private-credential-needle") || strings.Contains(string(encoded), "scheme://") {
		t.Fatal("reference projection contains values")
	}
	rows := envRows(plan, stored.Values, false)
	view := reviewView{Base: stored.Generation.ID, Project: store.Project{Name: "reference-fixture"}, References: references, Env: rows, EnvGroups: groupEnvRows(rows), Optional: plan.OptionalEnv, OptionalEnv: optionalEnvGroups(plan, plan.OptionalEnv), Edited: true}
	for _, group := range view.OptionalEnv {
		for _, row := range group.Rows {
			if row.Key == "INACTIVE" && (len(row.Usage) != 0 || len(row.Consumers) != 0) {
				t.Fatal("inactive optional service gained a consumer")
			}
		}
	}
	server := intakeServer(t)
	response := httptest.NewRecorder()
	if err := server.render(response, http.StatusOK, "review", Page{Title: "Review", User: "tester", Data: view}); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, text := range []string{`data-reference-base="saved-reference-version"`, "saved version v7", "Used by 2 services", "Feeds derived value", "DATABASE_URL", "Waits for:", "Cycle involving", "remove a circular reference", "is referenced but has no definition", "this inspector describes the saved plan"} {
		if !strings.Contains(body, text) {
			t.Errorf("missing inspector behavior %q", text)
		}
	}
	if strings.Contains(body, "private-credential-needle") {
		t.Fatal("inspector revealed a stored secret")
	}
	ids := map[string]int{}
	for _, match := range regexp.MustCompile(`id="(reference-[a-f0-9]+)"`).FindAllStringSubmatch(body, -1) {
		ids[match[1]]++
	}
	for _, match := range regexp.MustCompile(`href="#(reference-[a-f0-9]+)"`).FindAllStringSubmatch(body, -1) {
		if ids[match[1]] != 1 {
			t.Errorf("reference link %s has %d targets", match[1], ids[match[1]])
		}
	}
	stored.Generation.ID = "new-reference-version"
	if referenceViewFor(stored).Base == references.Base {
		t.Fatal("inspector reused a stale generation identity")
	}
}

func TestReferenceInspectorEscapesNames(t *testing.T) {
	plan := &ir.IR{EnvFile: []ir.EnvFileEntry{{Key: "VALUE"}}, Services: []ir.Service{{Name: `app"><script>dummy()</script>`, Env: []ir.EnvRef{{Key: "VALUE", Ref: "VALUE"}}}}}
	rows := envRows(plan, nil, false)
	server := intakeServer(t)
	response := httptest.NewRecorder()
	view := reviewView{Env: rows, EnvGroups: groupEnvRows(rows), References: referenceViewFor(&pipeline.Stored{Plan: plan})}
	if err := server.render(response, http.StatusOK, "review", Page{Data: view}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "<script>dummy()") {
		t.Fatal("reference name escaped into executable markup")
	}
}
