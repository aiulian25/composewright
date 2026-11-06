package extract

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestActualDotenvIsNotOverwritten(t *testing.T) {
	directory := t.TempDir()
	for name, content := range map[string]string{"compose.yaml": "services:\n  app:\n    image: example/app:${APP_TAG}\n", ".env": "APP_TAG=1\n", ".env.example": "APP_TAG=2\n", "other.env": "APP_TAG=3\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := Extract(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.ComposeFiles) != 1 || facts.ComposeFiles[0].Services[0].Image != "example/app:1" {
		t.Fatalf("wrong image: %+v", facts.ComposeFiles)
	}
}

func TestMatchingEnvSampleKeepsFileIdentity(t *testing.T) {
	samples := []EnvSample{{Path: "config.env.example"}, {Path: ".env.example"}, {Path: ".env"}}
	sample, found := MatchingEnvSample(samples, ".env")
	if !found || sample.Path != ".env" {
		t.Fatalf("empty actual file replaced: %+v", sample)
	}
	sample, found = MatchingEnvSample(samples, "config.env")
	if !found || sample.Path != "config.env.example" {
		t.Fatalf("example missing: %+v", sample)
	}
	if _, found := MatchingEnvSample([]EnvSample{{Path: ".env.production"}}, ".env"); found {
		t.Fatal("production file inferred as an example")
	}
	if _, found = MatchingEnvSample(samples, "other.env"); found {
		t.Fatal("unrelated file selected")
	}
}
