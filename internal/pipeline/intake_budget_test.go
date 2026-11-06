package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntakeBudgetDoesNotCancelCaller(t *testing.T) {
	dependencies := testDependencies(t, &scriptedDocker{})
	dependencies.Config.Network.IntakeTimeout = time.Nanosecond
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte("services:\n  app:\n    image: docker.io/library/nginx:alpine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Generate(ctx, dependencies, Options{Source: directory, Name: "intake-budget", StopAfter: StageFacts})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("intake did not respect deadline: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("intake cancelled its caller")
	}
}
