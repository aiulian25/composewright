package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/store"
)

func TestEnvServiceGroups(t *testing.T) {
	plan := &ir.IR{EnvFile: []ir.EnvFileEntry{{Key: "API_MODE"}, {Key: "DB_MODE"}, {Key: "URL", Value: "${PASSWORD}"}, {Key: "PASSWORD", Value: "${URL}"}, {Key: "GLOBAL"}}, Services: []ir.Service{
		{Name: "api", Env: []ir.EnvRef{{Key: "MODE", Ref: "API_MODE"}, {Key: "DATABASE_URL", Ref: "URL"}}},
		{Name: "db", Env: []ir.EnvRef{{Key: "MODE", Ref: "DB_MODE"}, {Key: "PASSWORD", Ref: "PASSWORD"}}},
	}}
	rows := envRows(plan, map[string]string{}, false)
	want := map[string]string{"API_MODE": "Service: api", "DB_MODE": "Service: db", "PASSWORD": "Shared: api, db", "URL": "Shared: api, db", "GLOBAL": "Project settings"}
	seen := map[string]int{}
	for _, group := range groupEnvRows(rows) {
		for _, row := range group.Rows {
			seen[row.Key]++
			if row.Group != want[row.Key] || group.Label != row.Group {
				t.Fatalf("wrong group for %s: %s", row.Key, row.Group)
			}
		}
	}
	for key := range want {
		if seen[key] != 1 {
			t.Fatalf("%s appears %d times", key, seen[key])
		}
	}
}

// This recorder renders the real page and can also supply a file fixture for
// browser checks without a listener, credentials, Docker or a running server.
func TestEnvReviewFallback(t *testing.T) {
	provenance := ir.Provenance{Source: ir.SourceCompose, Confidence: ir.ConfidenceHigh, SourceRef: "compose.yaml:3"}
	entries := []ir.EnvFileEntry{
		{Key: "API_URL", Required: true, Description: "Endpoint address", Provenance: provenance},
		{Key: "API_MODE", Description: "Mode <check>", Provenance: ir.Provenance{Source: ir.SourceCompose, Confidence: ir.ConfidenceLow}},
		{Key: "DB_PASSWORD", Secret: true, Required: true, Provenance: provenance},
		{Key: "MISSING_TOKEN", Secret: true, Required: true, Provenance: provenance},
		{Key: "GENERATED", Secret: true, Generate: "random", Provenance: provenance},
		{Key: "GLOBAL", Provenance: provenance},
	}
	plan := &ir.IR{EnvFile: entries, Services: []ir.Service{
		{Name: "api", Env: []ir.EnvRef{{Ref: "API_URL"}, {Ref: "API_MODE"}, {Ref: "DB_PASSWORD"}, {Ref: "MISSING_TOKEN"}}},
		{Name: "db", Env: []ir.EnvRef{{Ref: "DB_PASSWORD"}, {Ref: "GENERATED"}}},
	}}
	rows := envRows(plan, map[string]string{"API_MODE": "auto", "DB_PASSWORD": "dummy-private-needle", "GENERATED": "dummy-generated-needle", "GLOBAL": "en_GB"}, true)
	optional := []ir.OptionalSetting{{Key: "TZ", Service: "api", Default: "UTC", Description: "Timezone"}, {Key: "METRICS", Service: "db", Default: "false", Description: "Metrics endpoint"}, {Key: "MAIL_TOKEN", Service: "api", Description: "Mailbox access"}}
	plan.OptionalEnv = optional
	view := reviewView{References: referenceViewFor(&pipeline.Stored{Plan: plan}), Project: store.Project{Name: "filter-fixture"}, Env: rows, EnvGroups: groupEnvRows(rows), Optional: optional, OptionalEnv: optionalEnvGroups(plan, optional), Variables: len(rows), Revealed: true}
	server := intakeServer(t)
	response := httptest.NewRecorder()
	if err := server.render(response, http.StatusOK, "review", Page{Title: "Review", User: "tester", Data: view}); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	if strings.Count(body, "data-env-row ") != 9 || strings.Count(body, `name="secret:DB_PASSWORD"`) != 1 {
		t.Fatal("fallback loses or duplicates settings")
	}
	for _, text := range []string{"Service: api", "Service: db", "Shared: api, db", "Project settings", `data-env-tools hidden`, `Mode &lt;check&gt;`} {
		if !strings.Contains(body, text) {
			t.Fatalf("missing %s", text)
		}
	}
	if strings.Contains(body, `data-env-search="dummy-`) {
		t.Fatal("secret value in searchable metadata")
	}
	if file := os.Getenv("COMPOSEWRIGHT_REVIEW_FIXTURE"); file != "" {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
