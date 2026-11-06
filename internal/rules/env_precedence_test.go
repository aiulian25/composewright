package rules

import (
	"context"
	"testing"

	"github.com/aiulian25/composewright/internal/fetch"
)

func TestActualEnvAndOrderedFilesWin(t *testing.T) {
	facts := testFacts(t, map[string]string{
		"compose.yaml": `services:
  api:
    image: example/api:${APP_TAG:-fallback}
    env_file: [base.env, final.env]
    environment:
      EXPLICIT: ""
      ACTUAL: ${ACTUAL:-fallback}
      EMPTY_DEFAULT: ${EMPTY:-fallback}
      MISSING: ${UNRELATED}
`,
		".env":              "APP_TAG=actual\nACTUAL=real\nEMPTY=\n",
		".env.example":      "APP_TAG=example\nACTUAL=sample\n",
		"unrelated.env":     "APP_TAG=wrong\nUNRELATED=wrong\n",
		"base.env":          "VALUE=first\nEXPLICIT=wrong\nEMPTY=first\n",
		"final.env":         "VALUE=second\nVALUE=last\nEMPTY=\n",
		"final.env.example": "VALUE=example\n",
	})
	if len(facts.ComposeFiles) != 1 || facts.ComposeFiles[0].Services[0].Image != "example/api:actual" {
		t.Fatalf("image: %+v", facts.ComposeFiles)
	}
	plan := Build(context.Background(), Input{Facts: facts, Source: &fetch.Source{Owner: "example", Repository: "api"}})
	for key, want := range map[string]string{"VALUE": "last", "EXPLICIT": "", "ACTUAL": "real", "EMPTY_DEFAULT": "fallback", "MISSING": ""} {
		service, _ := plan.Service("api")
		ref := ""
		for _, env := range service.Env {
			if env.Key == key {
				ref = env.Ref
			}
		}
		entry, ok := plan.EnvEntry(ref)

		if !ok || entry.Value != want {
			t.Errorf("%s: %+v want %q", key, entry, want)
		}
		if key == "VALUE" && entry.Provenance.SourceRef != "final.env:2" {
			t.Errorf("winning source: %+v", entry.Provenance)
		}
	}
}

func TestComposeValueSourcePrecedence(t *testing.T) {
	facts := testFacts(t, map[string]string{"compose.yaml": "services:\n  api:\n    image: example/api:1\n", ".env": "EMPTY=\n", ".env.example": "EMPTY=example\n"})
	b := builder{facts: facts}
	for _, tc := range []struct{ op, want string }{{":-", "fallback"}, {"-", ""}, {"", ""}} {
		value, _ := b.composeValue(&facts.ComposeFiles[0], "EMPTY", tc.op, "fallback", provenance("compose", facts.ComposeFiles[0].Opening(), "high"))
		if value != tc.want {
			t.Errorf("%s: %q want %q", tc.op, value, tc.want)
		}
	}
}
