package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	SourceSandbox = "sandbox"
	SourceCompose = "compose"

	defaultFollowInterval = 2 * time.Second
	// followOverlap re-reads a little before the oldest cursor so a line a
	// container wrote while the previous poll was reading is not missed.
	followOverlap = 5 * time.Second
	logTimeLayout = time.RFC3339Nano
)

// Line is one line of live output from a run, already redacted.
type Line struct {
	Time   time.Time `json:"time"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
}

// liveOutput sends a run's progress and container output to an observer.
// With no observer every method does nothing.
type liveOutput struct {
	emit    func(Line)
	secrets []string
	now     func() time.Time

	mutex    sync.Mutex
	cursors  map[string]time.Time
	boundary map[string]map[string]bool
}

func newLiveOutput(emit func(Line), secrets []string, now func() time.Time) *liveOutput {
	return &liveOutput{emit: emit, secrets: secrets, now: now, cursors: map[string]time.Time{}, boundary: map[string]map[string]bool{}}
}

func (output *liveOutput) send(source, text string) {
	if output.emit == nil {
		return
	}
	output.emit(Line{Time: output.now(), Source: source, Text: scrub(text, output.secrets)})
}

func (output *liveOutput) say(format string, arguments ...any) {
	output.send(SourceSandbox, fmt.Sprintf(format, arguments...))
}

func (output *liveOutput) removed(name string, err error) {
	if err != nil {
		output.say("could not remove project %s; the janitor retries before the next run: %v", name, err)
		return
	}
	output.say("removed project %s with its containers, networks and volumes; the .env copy is deleted", name)
}

// writer turns a command's output into lines from source.
func (output *liveOutput) writer(source string) io.Writer {
	if output.emit == nil {
		return nil
	}
	return &lineWriter{send: func(text string) { output.send(source, text) }}
}

// logs sends the lines of `compose logs -t` output that were not sent yet.
// Each container keeps its own cursor, because one container's lines are
// in time order while lines of different containers are not.
func (output *liveOutput) logs(text string) {
	if output.emit == nil {
		return
	}
	output.mutex.Lock()
	defer output.mutex.Unlock()
	for _, raw := range strings.Split(text, "\n") {
		container, service, stamp, message, ok := splitLogLine(raw)
		if !ok || !output.fresh(container, stamp, raw) {
			continue
		}
		output.emit(Line{Time: stamp, Source: service, Text: scrub(message, output.secrets)})
	}
}

// fresh records a line and reports whether it is newer than what this
// container already sent. Lines sharing the cursor's timestamp are told
// apart by their text, because --since includes that instant again.
func (output *liveOutput) fresh(container string, stamp time.Time, raw string) bool {
	cursor, seen := output.cursors[container]
	switch {
	case seen && stamp.Before(cursor):
		return false
	case seen && stamp.Equal(cursor) && output.boundary[container][raw]:
		return false
	case !seen || stamp.After(cursor):
		output.cursors[container] = stamp
		output.boundary[container] = map[string]bool{}
	}
	output.boundary[container][raw] = true
	return true
}

// since is where the next poll starts: a little before the oldest cursor.
func (output *liveOutput) since(started time.Time) time.Time {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	oldest := time.Time{}
	for _, cursor := range output.cursors {
		if oldest.IsZero() || cursor.Before(oldest) {
			oldest = cursor
		}
	}
	start := oldest.Add(-followOverlap)
	if oldest.IsZero() || start.Before(started) {
		return started
	}
	return start
}

// follow polls the logs while `up --wait` runs; `compose logs --follow`
// started before the containers exist prints nothing. The returned stop
// waits for the poller to finish.
func (output *liveOutput) follow(ctx context.Context, sandbox *Sandbox, directory, name string, started time.Time) (stop func()) {
	if output.emit == nil {
		return func() {}
	}
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(sandbox.followInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
			since := output.since(started).UTC().Format(logTimeLayout)
			text, _, err := sandbox.compose(pollCtx, directory, nil, name, "logs", "--no-color", "-t", "--since", since)
			if err == nil {
				output.logs(string(text))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// ParseLogs turns kept `compose logs -t` output back into lines, in time
// order across containers.
func ParseLogs(text string) []Line {
	lines := []Line{}
	for _, raw := range strings.Split(text, "\n") {
		_, service, stamp, message, ok := splitLogLine(raw)
		if ok {
			lines = append(lines, Line{Time: stamp, Source: service, Text: message})
		}
	}
	slices.SortStableFunc(lines, func(left, right Line) int { return left.Time.Compare(right.Time) })
	return lines
}

// splitLogLine reads "db-1  | 2026-10-10T08:15:02.123456789Z message".
func splitLogLine(raw string) (container, service string, stamp time.Time, message string, ok bool) {
	match := logPrefix.FindStringSubmatch(raw)
	if match == nil {
		return "", "", time.Time{}, "", false
	}
	rest := raw[len(match[0]):]
	text, message, _ := strings.Cut(rest, " ")
	stamp, err := time.Parse(logTimeLayout, text)
	if err != nil {
		return "", "", time.Time{}, "", false
	}
	return match[1] + match[2], match[1], stamp, message, true
}

func verdictLine(verdict Verdict) string {
	if verdict.Passed() {
		return fmt.Sprintf("PASS in %s: every service is running or healthy", verdict.Duration.Round(time.Second))
	}
	return fmt.Sprintf("FAIL (%s) in %s: %s", verdict.Reason, verdict.Duration.Round(time.Second), verdict.Detail)
}

// lineWriter collects writes into lines. stdout and stderr of one command
// write from two goroutines, so it locks.
type lineWriter struct {
	mutex   sync.Mutex
	pending bytes.Buffer
	send    func(string)
}

func (writer *lineWriter) Write(data []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	writer.pending.Write(data)
	for {
		line, err := writer.pending.ReadString('\n')
		if err != nil {
			writer.pending.WriteString(line)
			return len(data), nil
		}
		if text := strings.TrimSpace(line); text != "" {
			writer.send(text)
		}
	}
}
