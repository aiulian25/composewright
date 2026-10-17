package sandbox

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-units"
)

const (
	// statsTimeout bounds one `docker stats --no-stream`, which itself waits
	// about a second to measure CPU; a slow daemon holds up a run by no
	// more than this.
	statsTimeout   = 10 * time.Second
	statsEveryPoll = 2
	usageSeparator = "/"
	percentSign    = "%"
)

// Usage is what one service used: memory in bytes and CPU in percent of
// one core, as docker stats reports them.
type Usage struct {
	MemoryBytes int64
	CPUPercent  float64
}

// higher is the larger of each figure; two containers of one service, or
// two samples of one container, report their highest.
func (usage Usage) higher(other Usage) Usage {
	return Usage{MemoryBytes: max(usage.MemoryBytes, other.MemoryBytes), CPUPercent: max(usage.CPUPercent, other.CPUPercent)}
}

// peaks keeps the highest usage seen per service across samples.
type peaks struct {
	mutex   sync.Mutex
	highest map[string]Usage
}

func newPeaks() *peaks {
	return &peaks{highest: map[string]Usage{}}
}

func (kept *peaks) add(sample map[string]Usage) {
	kept.mutex.Lock()
	defer kept.mutex.Unlock()
	for service, usage := range sample {
		kept.highest[service] = kept.highest[service].higher(usage)
	}
}

// record writes the peaks onto the services they were measured for.
func (kept *peaks) record(states []ServiceState) {
	kept.mutex.Lock()
	defer kept.mutex.Unlock()
	for index, state := range states {
		usage := kept.highest[state.Service]
		states[index].PeakMemoryBytes, states[index].PeakCPUPercent = usage.MemoryBytes, usage.CPUPercent
	}
}

// sample measures the project's containers once and keeps the result.
// Sampling is best effort: any failure only means no figure.
func (sandbox *Sandbox) sample(ctx context.Context, directory, name string, kept *peaks) {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	listed, _, err := sandbox.compose(ctx, directory, nil, name, "ps", "-a", "--format", "json")
	if err != nil {
		return
	}
	states, err := parseStates(listed)
	if err != nil || len(states) == 0 {
		return
	}
	services := make(map[string]string, len(states))
	arguments := []string{"stats", "--no-stream", "--format", "{{json .}}"}
	for _, state := range states {
		services[state.Name] = state.Service
		arguments = append(arguments, state.Name)
	}
	output, _, err := sandbox.run(ctx, "", nil, arguments...)
	if err != nil {
		return
	}
	kept.add(parseStats(output, services))
}

// sampling samples every other follow interval while `up --wait` runs.
// The returned stop waits for the sampler to finish.
func (sandbox *Sandbox) sampling(ctx context.Context, directory, name string, kept *peaks) (stop func()) {
	sampleCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(statsEveryPoll * sandbox.followInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				sandbox.sample(sampleCtx, directory, name, kept)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// parseStats reads `docker stats --no-stream --format '{{json .}}'`, one
// container a line, into usage per service; services maps container names
// to their services. A line it cannot read is left out.
func parseStats(output []byte, services map[string]string) map[string]Usage {
	usage := map[string]Usage{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		var stats struct{ Name, MemUsage, CPUPerc string }
		if json.Unmarshal([]byte(line), &stats) != nil {
			continue
		}
		service, known := services[stats.Name]
		used, _, _ := strings.Cut(stats.MemUsage, usageSeparator)
		memory, memoryErr := units.RAMInBytes(strings.TrimSpace(used))
		cpu, cpuErr := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(stats.CPUPerc), percentSign), 64)
		if !known || memoryErr != nil || cpuErr != nil {
			continue
		}
		usage[service] = usage[service].higher(Usage{MemoryBytes: memory, CPUPercent: cpu})
	}
	return usage
}
