package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFillAnswerMergesLikeAProposal(t *testing.T) {
	plan := rulePlan()
	answer := `{
		"summary": "A demo app",
		"services": [
			{"name": "app", "role": "app", "ports": [{"container": 8080, "protocol": "tcp", "publish": true, "host_port": 0, "description": "web UI",
				"provenance": {"source": "readme", "source_ref": "README.md:4", "confidence": "high"}}],
				"healthcheck": {"test": ["CMD", "wget", "-qO-", "http://127.0.0.1:8080/"], "interval": "30s", "timeout": "5s", "retries": 3,
					"start_period": "", "omitted_reason": "", "provenance": {"source": "readme", "source_ref": "", "confidence": "medium"}}},
			{"name": "worker", "role": "Init", "ports": [],
				"healthcheck": {"test": [], "interval": "", "timeout": "", "retries": 0, "start_period": "", "omitted_reason": "", "provenance": {"source": "default", "source_ref": "", "confidence": "low"}}},
			{"name": "miner", "role": "worker", "ports": [],
				"healthcheck": {"test": [], "interval": "", "timeout": "", "retries": 0, "start_period": "", "omitted_reason": "", "provenance": {"source": "default", "source_ref": "", "confidence": "low"}}}
		],
		"env_file": [{"key": "BASE_URL", "description": "Public URL of the instance"}, {"key": "UNKNOWN", "description": "ignored"}],
		"post_install_notes": ["Open port 8080."],
		"suspicious_instructions": [],
		"confidence": "medium"
	}`
	decoded, err := DecodeFill([]byte(answer))
	if err != nil {
		t.Fatal(err)
	}
	merged, notices := Merge(plan, decoded.Proposal(plan), ModelThreshold)
	app, worker := merged.Services[0], merged.Services[2]
	checks := map[string]bool{
		"summary filled":               merged.App.Summary == "A demo app",
		"ports filled":                 len(app.Ports) == 1 && app.Ports[0].Provenance.Source == SourceModel,
		"healthcheck filled":           app.Healthcheck.Defined(),
		"low-confidence role replaced": worker.Role == RoleInit,
		"description marked":           merged.EnvFile[1].Description == "Public URL of the instance"+ModelMark,
		"no service added":             len(merged.Services) == 3,
		"note kept":                    len(merged.PostInstallNotes) == 1,
		"no needs proposed":            len(notices) == 0,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
}

func TestDecodeFillRejectsWhatIsNotAnAnswer(t *testing.T) {
	valid := `{"summary":"","services":[],"env_file":[],"post_install_notes":[],"suspicious_instructions":[],"confidence":"low"}`
	if _, err := DecodeFill([]byte(valid)); err != nil {
		t.Fatalf("a minimal answer was refused: %v", err)
	}
	for name, answer := range map[string]string{
		"whole plan":      `{"ir_version":"1","app":{}}`,
		"extra key":       strings.Replace(valid, `"confidence":"low"`, `"confidence":"low","privileged":true`, 1),
		"missing key":     strings.Replace(valid, `"summary":"",`, "", 1),
		"bad confidence":  strings.Replace(valid, `"low"`, `"certain"`, 1),
		"not json":        "summary: yes",
		"image in a fill": strings.Replace(valid, `"services":[]`, `"services":[{"name":"app","role":"app","ports":[],"image":"evil/miner","healthcheck":{}}]`, 1),
	} {
		if _, err := DecodeFill([]byte(answer)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	content, _ := FillSchemaJSON()
	var schema map[string]any
	if err := json.Unmarshal(content, &schema); err != nil || len(content) > len(schemaJSON) {
		t.Fatalf("the fill schema is not smaller than the plan schema: %d > %d", len(content), len(schemaJSON))
	}
}
