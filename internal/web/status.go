package web

import (
	"context"
	"sync"
	"time"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	statusRefresh = time.Minute
	statusTimeout = 3 * time.Second
	dockerPrefix  = "Docker "
	dockerMissing = "Docker not found"
	cliMissingFix = "The docker CLI was not found: install Docker Engine with the compose plugin, or set docker.cli_path."
)

var providerNames = map[string]string{
	config.ProviderAnthropic:        "Anthropic",
	config.ProviderOpenAICompatible: "OpenAI-compatible",
	config.ProviderOllama:           "Ollama",
}

// dockerState is what the status bar says about Docker, and when Docker
// cannot be used, what to change.
type dockerState struct {
	Text string
	Tone string
	Fix  string
}

// statusCache keeps the Docker check for a minute, so pages stay fast. The
// check runs outside the lock: a slow daemon holds up only the pages that
// check, never the ones behind them, at the cost of an occasional duplicate.
type statusCache struct {
	now     func() time.Time
	check   func(context.Context) dockerState
	mutex   sync.Mutex
	checked time.Time
	state   dockerState
	// forgets counts forget calls, so a check that began before one does
	// not store a state from the old settings.
	forgets uint64
}

func newStatusCache(server *Server) *statusCache {
	return &statusCache{now: server.options.Now, check: server.dockerStatus}
}

// forget makes the next page check Docker again, after Settings changed.
func (cache *statusCache) forget() {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.checked = time.Time{}
	cache.forgets++
}

func (cache *statusCache) docker(ctx context.Context) dockerState {
	cache.mutex.Lock()
	now, checked, state, forgets := cache.now(), cache.checked, cache.state, cache.forgets
	cache.mutex.Unlock()
	fresh := !checked.IsZero() && now.Sub(checked) < statusRefresh
	if fresh {
		return state
	}
	state = cache.check(ctx)
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	// A page closed before Docker answered says nothing about Docker.
	forgotten, pageClosed := cache.forgets != forgets, ctx.Err() != nil
	if forgotten || pageClosed {
		return state
	}
	cache.checked, cache.state = now, state
	return state
}

func (server *Server) dockerStatus(ctx context.Context) dockerState {
	runner, err := sandbox.FromConfig(server.config(), server.paths().ProjectsDir())
	if err != nil {
		return dockerState{Text: dockerMissing, Tone: toneDanger, Fix: cliMissingFix}
	}
	checkCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	environment, err := runner.Info(checkCtx)
	switch {
	case err != nil:
		problem := sandbox.Diagnose(sandbox.DockerHost(server.config().Docker.Host))
		return dockerState{Text: problem.Status, Tone: toneDanger, Fix: problem.Fix}
	case environment.Rootless:
		return dockerState{Text: dockerPrefix + environment.EngineVersion + ", rootless", Tone: toneOk}
	}
	return dockerState{Text: dockerPrefix + environment.EngineVersion + ", rootful", Tone: toneWarn}
}
