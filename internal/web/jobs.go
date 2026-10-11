package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aiulian25/composewright/internal/extract"
	"github.com/aiulian25/composewright/internal/fetch"
	"github.com/aiulian25/composewright/internal/pipeline"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	jobGenerate = "generate"
	jobValidate = "validate"

	eventStep   = "step"
	eventFacts  = "facts"
	eventLine   = "line"
	eventNotice = "notice"
	eventDone   = "done"

	maxRunningJobs = 2
	maxJobLines    = 5000
	jobRetention   = time.Hour
	jobIDBytes     = 16
	sseHeartbeat   = 15 * time.Second
	// heartbeatEvent is a named event, not a comment, so the page sees it
	// and can tell a quiet run from a dead connection (app.js).
	heartbeatEvent = "event: ping\ndata: {}\n\n"
	eventStream    = "text/event-stream"
	lastEventID    = "Last-Event-ID"
)

var errStopped = errors.New("stopped by you")

// event is one thing a job reports; the browser receives it as JSON over
// Server-Sent Events, and a reloaded page is rendered from the same list.
type event struct {
	Kind   string         `json:"kind"`
	Step   *pipeline.Step `json:"step,omitempty"`
	Line   *sandbox.Line  `json:"line,omitempty"`
	Text   string         `json:"text,omitempty"`
	Next   string         `json:"next,omitempty"`
	Failed bool           `json:"failed,omitempty"`
}

type timing struct {
	started time.Time
	ended   time.Time
}

// job is a generation or a validation running outside the request that
// started it.
type job struct {
	ID      string
	Kind    string
	Title   string
	Started time.Time

	mutex    sync.Mutex
	project  string
	events   []event
	changed  chan struct{}
	finished bool
	ended    time.Time
	failure  string
	next     string
	steps    map[string]pipeline.Step
	timings  map[string]timing
	facts    *extract.Facts
	source   *fetch.Source
	notices  []string
	lines    []sandbox.Line
	dropped  int
	result   *pipeline.Result
	cancel   context.CancelFunc
	now      func() time.Time
}

// snapshot is a job's state at one moment, for rendering a page.
type snapshot struct {
	ID       string
	Events   int
	Kind     string
	Title    string
	Project  string
	Started  time.Time
	Finished bool
	Ended    time.Time
	Failure  string
	Next     string
	Steps    map[string]pipeline.Step
	Timings  map[string]timing
	Facts    *extract.Facts
	Source   *fetch.Source
	Notices  []string
	Lines    []sandbox.Line
	Dropped  int
	Result   *pipeline.Result
}

func (current *job) snapshot() snapshot {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	steps := make(map[string]pipeline.Step, len(current.steps))
	for name, step := range current.steps {
		steps[name] = step
	}
	timings := make(map[string]timing, len(current.timings))
	for name, value := range current.timings {
		timings[name] = value
	}
	return snapshot{ID: current.ID, Events: len(current.events), Kind: current.Kind, Title: current.Title, Project: current.project, Started: current.Started,
		Finished: current.finished, Ended: current.ended, Failure: current.failure, Next: current.next, Steps: steps, Timings: timings,
		Facts: current.facts, Source: current.source, Notices: append([]string(nil), current.notices...),
		Lines: append([]sandbox.Line(nil), current.lines...), Dropped: current.dropped, Result: current.result}
}

// add appends an event and wakes every stream waiting for one.
func (current *job) add(item event) {
	current.events = append(current.events, item)
	close(current.changed)
	current.changed = make(chan struct{})
}

func (current *job) observer() pipeline.Observer {
	return pipeline.Observer{
		Step: func(step pipeline.Step) {
			current.mutex.Lock()
			defer current.mutex.Unlock()
			now := current.now()
			record := current.timings[step.Name]
			if step.State == pipeline.StateRunning {
				record.started = now
			} else {
				record.ended = now
			}
			current.timings[step.Name], current.steps[step.Name] = record, step
			current.add(event{Kind: eventStep, Step: &step})
		},
		Facts: func(facts *extract.Facts, source *fetch.Source) {
			current.mutex.Lock()
			defer current.mutex.Unlock()
			current.facts, current.source = facts, source
			current.add(event{Kind: eventFacts})
		},
		Line: func(line sandbox.Line) {
			current.mutex.Lock()
			defer current.mutex.Unlock()
			if len(current.lines) >= maxJobLines {
				current.dropped++
				return
			}
			current.lines = append(current.lines, line)
			current.add(event{Kind: eventLine, Line: &line})
		},
	}
}

func (current *job) notice(text string) {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	current.notices = append(current.notices, text)
	current.add(event{Kind: eventNotice, Text: text})
}

func (current *job) finish(next string, result *pipeline.Result, err error) {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	current.finished, current.ended, current.next, current.result = true, current.now(), next, result
	if result != nil {
		current.project = result.Project.Name
		current.notices = append(current.notices, result.Notices...)
	}
	if err != nil {
		current.failure = err.Error()
	}
	current.add(event{Kind: eventDone, Next: next, Failed: err != nil, Text: current.failure})
}

// since returns the events from index on, the channel that closes on the
// next one, and whether the job is over.
func (current *job) since(index int) ([]event, <-chan struct{}, bool) {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	index = min(max(index, 0), len(current.events))
	return append([]event(nil), current.events[index:]...), current.changed, current.finished
}

// jobs runs at most two at a time, one per project.
type jobs struct {
	mutex  sync.Mutex
	byID   map[string]*job
	now    func() time.Time
	wait   sync.WaitGroup
	logger *slog.Logger
}

func newJobs(now func() time.Time, logger *slog.Logger) *jobs {
	return &jobs{byID: map[string]*job{}, now: now, logger: logger}
}

// work is what a job does; it returns where the browser goes next.
type work func(ctx context.Context, current *job) (next string, result *pipeline.Result, err error)

func (registry *jobs) start(kind, title, project string, run work) (*job, error) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	registry.forget()
	running := 0
	for _, existing := range registry.byID {
		if existing.isFinished() {
			continue
		}
		running++
		if project != "" && existing.projectName() == project {
			return nil, conflict("%s is already running. Wait for it to finish or stop it first.", project)
		}
	}
	if running >= maxRunningJobs {
		return nil, conflict("Two runs are already going. Wait for one of them to finish.")
	}
	identifier := make([]byte, jobIDBytes)
	if _, err := rand.Read(identifier); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	current := &job{ID: hex.EncodeToString(identifier), Kind: kind, Title: title, Started: registry.now(), project: project,
		changed: make(chan struct{}), steps: map[string]pipeline.Step{}, timings: map[string]timing{}, cancel: cancel, now: registry.now}
	registry.byID[current.ID] = current
	registry.wait.Add(1)
	go func() {
		defer registry.wait.Done()
		defer cancel()
		next, result, err := run(ctx, current)
		if errors.Is(ctx.Err(), context.Canceled) && err != nil {
			err = errStopped
		}
		current.finish(next, result, err)
		registry.report(current)
	}()
	return current, nil
}

// report writes the outcome of a run, and every notice it raised, to the
// server log, so a model that fell back can be diagnosed later.
func (registry *jobs) report(current *job) {
	finished := current.snapshot()
	for _, notice := range finished.Notices {
		registry.logger.Info("run notice", "job", finished.ID, "kind", finished.Kind, "project", finished.Project, "notice", notice)
	}
	if finished.Failure != "" {
		registry.logger.Warn("run failed", "job", finished.ID, "kind", finished.Kind, "project", finished.Project, "error", finished.Failure)
		return
	}
	registry.logger.Info("run finished", "job", finished.ID, "kind", finished.Kind, "project", finished.Project,
		"duration", finished.Ended.Sub(finished.Started).Round(time.Second).String())
}

func (current *job) isFinished() bool {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	return current.finished
}

func (current *job) projectName() string {
	current.mutex.Lock()
	defer current.mutex.Unlock()
	return current.project
}

// forget drops finished jobs older than an hour; their results are in the
// database and their logs in the project folder.
func (registry *jobs) forget() {
	cutoff := registry.now().Add(-jobRetention)
	for identifier, existing := range registry.byID {
		existing.mutex.Lock()
		stale := existing.finished && existing.ended.Before(cutoff)
		existing.mutex.Unlock()
		if stale {
			delete(registry.byID, identifier)
		}
	}
}

func (registry *jobs) get(identifier string) (*job, bool) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	current, found := registry.byID[identifier]
	return current, found
}

// running returns the unfinished job of a project, if any.
func (registry *jobs) running(project string) (*job, bool) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	for _, existing := range registry.byID {
		if !existing.isFinished() && existing.projectName() == project {
			return existing, true
		}
	}
	return nil, false
}

// shutdown stops every job and waits for their cleanup.
func (registry *jobs) shutdown(grace time.Duration) {
	registry.mutex.Lock()
	for _, existing := range registry.byID {
		existing.cancel()
	}
	registry.mutex.Unlock()
	finished := make(chan struct{})
	go func() {
		registry.wait.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(grace):
	}
}

func (server *Server) job(request *http.Request) (*job, error) {
	current, found := server.jobs.get(request.PathValue("id"))
	if !found {
		return nil, statusError{status: http.StatusNotFound, message: "This run is no longer in memory; its results are on the project page."}
	}
	return current, nil
}

// jobEvents streams a job's events from ?from= (the count the page was
// rendered with); a reconnecting browser sends the last id it saw and
// continues after it.
func (server *Server) jobEvents(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	current, err := server.job(request)
	if err != nil {
		return err
	}
	from, _ := strconv.Atoi(request.URL.Query().Get("from"))
	if resumed, err := strconv.Atoi(request.Header.Get(lastEventID)); err == nil {
		from = resumed + 1
	}
	controller := http.NewResponseController(writer)
	header := writer.Header()
	header.Set(contentType, eventStream)
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		events, changed, finished := current.since(from)
		for offset, item := range events {
			encoded, err := json.Marshal(item)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", from+offset, encoded); err != nil { //nolint:gosec // G705: JSON in a text/event-stream, never HTML
				return nil
			}
		}
		from += len(events)
		if err := controller.Flush(); err != nil {
			return nil
		}
		if finished {
			return nil
		}
		select {
		case <-changed:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(writer, heartbeatEvent); err != nil {
				return nil
			}
		case <-request.Context().Done():
			return nil
		}
	}
}

func (server *Server) cancelJob(writer http.ResponseWriter, request *http.Request, _ *visit) error {
	current, err := server.job(request)
	if err != nil {
		return err
	}
	current.cancel()
	return redirect(writer, request, "/jobs/"+current.ID)
}

// validating reports a project whose sandbox run is going, for the status
// bar.
func (registry *jobs) validating() (string, bool) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	for _, existing := range registry.byID {
		if !existing.isFinished() && existing.Kind == jobValidate {
			return existing.projectName(), true
		}
	}
	return "", false
}
