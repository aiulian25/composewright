package web

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/pipeline"
)

func TestJobBudgetReleasesSlotAndKeepsResult(t *testing.T) {
	registry := newJobs(time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.timeout = 20 * time.Millisecond
	retained := &pipeline.Result{}
	currentJob, err := registry.start(jobGenerate, "bounded", "bounded", func(ctx context.Context, _ *job) (string, *pipeline.Result, error) {
		<-ctx.Done()
		return "", retained, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { registry.wait.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		registry.shutdown(time.Second)
		t.Fatal("job remained active beyond its budget")
	}
	state := currentJob.snapshot()
	if !currentJob.isFinished() || !strings.Contains(state.Failure, "time budget") || currentJob.result != retained {
		t.Fatal("timeout lost completed work or stayed running")
	}
	if _, running := registry.running("bounded"); running {
		t.Fatal("slot not released")
	}
	if _, err = registry.start(jobGenerate, "next", "next", func(context.Context, *job) (string, *pipeline.Result, error) { return "", nil, nil }); err != nil {
		t.Fatal(err)
	}
	registry.wait.Wait()
}
