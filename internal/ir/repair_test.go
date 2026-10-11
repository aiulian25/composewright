package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRepairAnswerMergesLikeARepair: an answer in the repair schema changes
// what a repair may change and nothing else, and round-trips through its
// schema.
func TestRepairAnswerMergesLikeARepair(t *testing.T) {
	plan := rulePlan()
	app := plan.Services[0]
	model := Provenance{Source: SourceModel, Confidence: ConfidenceMedium}
	repair := RepairAnswer{Confidence: ConfidenceMedium, PostInstallNotes: []string{"The app needs its database first."},
		Services: []ServiceRepair{{Name: "app", Command: []string{"serve", "--migrate"}, Entrypoint: []string{}, Ports: []Port{},
			Env:       []EnvRef{{Key: "MODE", Literal: "debug", Provenance: model}},
			DependsOn: []Dependency{{Service: "db", Condition: ConditionHealthy}}, Healthcheck: app.Healthcheck}},
		EnvFile: []EnvFileEntry{{Key: "BASE_URL", Value: "http://localhost:8080", Provenance: model},
			{Key: "LOG_LEVEL", Value: "debug", Provenance: model}}}
	encoded, err := json.Marshal(repair)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRepair(encoded)
	if err != nil {
		t.Fatalf("an answer built from the Go types does not match its schema: %v", err)
	}
	merged, notices := MergeRepair(plan, decoded.Proposal(plan))
	repaired, _ := merged.Service("app")
	baseURL, _ := merged.EnvEntry("BASE_URL")
	_, added := merged.EnvEntry("LOG_LEVEL")
	checks := map[string]bool{
		"command changed":       strings.Join(repaired.Command, " ") == "serve --migrate",
		"image kept":            repaired.Image.Reference() == app.Image.Reference(),
		"secret reference kept": len(repaired.Env) == 2 && repaired.Env[1].Key == "DB_PASSWORD",
		"waits for the db":      len(repaired.DependsOn) == 1 && repaired.DependsOn[0].Service == "db",
		"value changed":         baseURL.Value == "http://localhost:8080",
		"variable added":        added,
		"other services kept":   len(merged.Services) == 3 && merged.Services[2].Healthcheck.Defined(),
		"note added":            len(merged.PostInstallNotes) == 1,
		"nothing refused":       len(notices) == 0,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
}

func TestDecodeRepairRejectsWhatIsNotARepair(t *testing.T) {
	valid := `{"services":[],"env_file":[],"post_install_notes":[],"confidence":"low"}`
	if _, err := DecodeRepair([]byte(valid)); err != nil {
		t.Fatalf("an empty repair was refused: %v", err)
	}
	for name, answer := range map[string]string{
		"whole plan":        `{"ir_version":"1","app":{}}`,
		"extra key":         strings.Replace(valid, `"confidence":"low"`, `"confidence":"low","needs":[]`, 1),
		"image in a repair": strings.Replace(valid, `"services":[]`, `"services":[{"name":"app","image":"evil/miner"}]`, 1),
	} {
		if _, err := DecodeRepair([]byte(answer)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	content, _ := RepairSchemaJSON()
	if len(content) >= len(schemaJSON) {
		t.Fatalf("the repair schema is not smaller than the plan schema: %d >= %d", len(content), len(schemaJSON))
	}
}
