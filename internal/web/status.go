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
)

var providerNames = map[string]string{
	config.ProviderAnthropic:        "Anthropic",
	config.ProviderOpenAICompatible: "OpenAI-compatible",
	config.ProviderOllama:           "Ollama",
}

// dockerState is what the status bar says about Docker.
type dockerState struct {
	Text string
	Tone string
}

// statusCache keeps the Docker check for a minute, so pages stay fast.
type statusCache struct {
	server  *Server
	mutex   sync.Mutex
	checked time.Time
	state   dockerState
}

func newStatusCache(server *Server) *statusCache {
	return &statusCache{server: server}
}

// forget makes the next page check Docker again, after Settings changed.
func (cache *statusCache) forget() {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.checked = time.Time{}
}

func (cache *statusCache) docker(ctx context.Context) dockerState {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	now := cache.server.options.Now()
	if !cache.checked.IsZero() && now.Sub(cache.checked) < statusRefresh {
		return cache.state
	}
	cache.checked, cache.state = now, cache.server.dockerStatus(ctx)
	return cache.state
}

func (server *Server) dockerStatus(ctx context.Context) dockerState {
	runner, err := sandbox.FromConfig(server.config(), server.paths().ProjectsDir())
	if err != nil {
		return dockerState{Text: "Docker not found", Tone: toneDanger}
	}
	checkCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	environment, err := runner.Info(checkCtx)
	switch {
	case err != nil:
		return dockerState{Text: "Docker unreachable", Tone: toneDanger}
	case environment.Rootless:
		return dockerState{Text: dockerPrefix + environment.EngineVersion + ", rootless", Tone: toneOk}
	}
	return dockerState{Text: dockerPrefix + environment.EngineVersion + ", rootful", Tone: toneWarn}
}
