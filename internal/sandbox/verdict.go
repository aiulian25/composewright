package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/docker/go-units"
)

const (
	ResultPass = "PASS"
	ResultFail = "FAIL"

	ReasonCrash     = "crash"
	ReasonUnhealthy = "unhealthy"
	ReasonTimeout   = "timeout"
	ReasonLogs      = "logs"
	ReasonStart     = "start"
	// ReasonUnreachable: every service is up, but the app does not answer
	// from another container on the sandbox network.
	ReasonUnreachable = "unreachable"

	stateExited      = "exited"
	stateDead        = "dead"
	healthUnhealthy  = "unhealthy"
	healthHealthy    = "healthy"
	healthStarting   = "starting"
	timeoutMessage   = "application not healthy after"
	refusedPattern   = "connection refused"
	refusedRepeated  = 3
	maxMatches       = 20
	maxMatchLength   = 300
	clipMarker       = "…"
	tailPerService   = 8 << 10
	logPrefixPattern = `^(\S+?)(-\d+)?\s+\|\s?`
	peakPrefix       = "peak "
	limitJoiner      = " of "
	cpuFormat        = ", %.0f%% CPU"
)

// fatalPatterns are the log lines that fail a run even when every
// container stays up; connection refused fails only when repeated.
var fatalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`panic:`),
	regexp.MustCompile(`FATAL`),
	regexp.MustCompile(`(?i)permission denied`),
	regexp.MustCompile(`(?i)address already in use`),
	regexp.MustCompile(`(?i)no such file`),
}

// benignPatterns are known startup messages that match a fatal pattern
// without meaning failure.
var benignPatterns = []*regexp.Regexp{
	regexp.MustCompile(`FATAL:\s+the database system is (starting up|not yet accepting connections)`),
}

// nonErrorLevel marks a line the application itself logged below error
// level; a fatal pattern inside it (for example an INFO line saying an
// optional file is missing) does not fail the run.
var nonErrorLevel = regexp.MustCompile(`(?i)(\blevel=(info|debug|notice|warn|warning)\b|"level"\s*:\s*"(info|debug|notice|warn|warning)"|\[(info|debug|notice|warn|warning)\]|\b(INFO|DEBUG|NOTICE|WARN|WARNING)\b[:\]])`)

// ansiSequence matches the colour codes many containers write to their logs.
var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

var logPrefix = regexp.MustCompile(logPrefixPattern)

type ServiceState struct {
	Service    string      `json:"service"`
	Name       string      `json:"name"`
	State      string      `json:"state"`
	Health     string      `json:"health"`
	ExitCode   int         `json:"exit_code"`
	Publishers []Publisher `json:"publishers,omitempty"`
	// HealthOutput is what the last healthcheck of a container that never
	// became healthy printed, secrets redacted: why it failed, when the
	// app's own logs do not say.
	HealthOutput string `json:"health_output,omitempty"`
	// PeakMemoryBytes and PeakCPUPercent are the most the service used
	// while the run sampled it; zero when it was never measured.
	PeakMemoryBytes int64   `json:"peak_memory_bytes,omitempty"`
	PeakCPUPercent  float64 `json:"peak_cpu_percent,omitempty"`
	// MemoryLimitBytes is the mem_limit the tested compose.yaml gives the
	// service, before the sandbox caps it.
	MemoryLimitBytes int64 `json:"memory_limit_bytes,omitempty"`
}

// Peak is the service's highest use, such as "peak 612MiB of 1GiB, 12%
// CPU", or empty when it was never measured.
func (state ServiceState) Peak() string {
	if state.PeakMemoryBytes == 0 {
		return ""
	}
	text := peakPrefix + units.BytesSize(float64(state.PeakMemoryBytes))
	if state.MemoryLimitBytes > 0 {
		text += limitJoiner + units.BytesSize(float64(state.MemoryLimitBytes))
	}
	return text + fmt.Sprintf(cpuFormat, state.PeakCPUPercent)
}

// Publisher is one published port as Docker reports it.
type Publisher struct {
	URL           string `json:"URL"`
	TargetPort    int    `json:"TargetPort"`
	PublishedPort int    `json:"PublishedPort"`
	Protocol      string `json:"Protocol"`
}

// Environment describes where the run happened; limits are only real when
// the daemon can enforce them (rootless Docker needs cgroup v2 delegation).
type Environment struct {
	Tier           string   `json:"tier"`
	Rootless       bool     `json:"rootless"`
	CgroupVersion  string   `json:"cgroup_version"`
	CgroupDriver   string   `json:"cgroup_driver"`
	LimitsEnforced bool     `json:"limits_enforced"`
	EngineVersion  string   `json:"engine_version,omitempty"`
	Runtimes       []string `json:"runtimes,omitempty"`
}

// MissingRuntime is ErrRunscMissing when the tier needs a runtime the
// daemon does not offer.
func (environment Environment) MissingRuntime() error {
	runtime := Runtime(environment.Tier)
	if runtime == "" || slices.Contains(environment.Runtimes, runtime) {
		return nil
	}
	return ErrRunscMissing
}

// probesEarly reports whether the engine runs healthchecks at a faster
// start_interval during the start period (Docker Engine 25 and later).
func (environment Environment) probesEarly() bool {
	major, _, _ := strings.Cut(environment.EngineVersion, ".")
	number, err := strconv.Atoi(major)
	return err == nil && number >= startIntervalEngine
}

type Verdict struct {
	Result      string            `json:"result"`
	Reason      string            `json:"reason,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	Project     string            `json:"compose_project"`
	Iteration   int               `json:"iteration"`
	Duration    time.Duration     `json:"duration_ns"`
	Services    []ServiceState    `json:"services"`
	LogMatches  []string          `json:"log_matches,omitempty"`
	Sandbox     Environment       `json:"sandbox"`
	Reach       *Reach            `json:"reach,omitempty"`
	LogTails    map[string]string `json:"-"`
	Logs        string            `json:"-"`
	StartOutput string            `json:"-"`
}

func (verdict Verdict) Passed() bool {
	return verdict.Result == ResultPass
}

// judge applies the verdict rules in order: a crashed container, an
// unhealthy one, the wait timing out, the stack never starting, a fatal log
// line; otherwise the run passes. completedOK names one-shot init services
// that may exit 0; benign lines are ones the person said are normal for
// this app.
func judge(states []ServiceState, upErr error, upOutput, logs string, completedOK map[string]bool, benign []*regexp.Regexp) Verdict {
	verdict := Verdict{Result: ResultPass, Services: states}
	for _, state := range states {
		exited := state.State == stateExited || state.State == stateDead
		finishedInit := state.ExitCode == 0 && completedOK[state.Service]
		if exited && !finishedInit {
			return fail(verdict, ReasonCrash, fmt.Sprintf("%s exited with code %d", state.Service, state.ExitCode))
		}
	}
	for _, state := range states {
		if state.Health == healthUnhealthy {
			return fail(verdict, ReasonUnhealthy, state.Service+" is unhealthy")
		}
	}
	if upErr != nil && strings.Contains(upOutput, timeoutMessage) {
		return fail(verdict, ReasonTimeout, lastLine(upOutput))
	}
	if upErr != nil {
		return fail(verdict, ReasonStart, lastLine(upOutput))
	}
	if matches := fatalLines(logs, benign); len(matches) > 0 {
		verdict = fail(verdict, ReasonLogs, matches[0])
		verdict.LogMatches = matches
	}
	return verdict
}

func fail(verdict Verdict, reason, detail string) Verdict {
	verdict.Result, verdict.Reason, verdict.Detail = ResultFail, reason, detail
	return verdict
}

func fatalLines(logs string, benign []*regexp.Regexp) []string {
	var matches []string
	refused := 0
	for _, line := range strings.Split(logs, "\n") {
		if matchesAny(LogMessage(line), benign) {
			continue
		}
		if strings.Contains(strings.ToLower(line), refusedPattern) {
			refused++
			if refused == refusedRepeated {
				matches = append(matches, clip(line))
			}
		}
		if isFatal(line) && len(matches) < maxMatches {
			matches = append(matches, clip(line))
		}
	}
	return matches
}

func isFatal(line string) bool {
	if nonErrorLevel.MatchString(line) {
		return false
	}
	return !matchesAny(line, benignPatterns) && matchesAny(line, fatalPatterns)
}

func matchesAny(line string, patterns []*regexp.Regexp) bool {
	return slices.ContainsFunc(patterns, func(pattern *regexp.Regexp) bool { return pattern.MatchString(line) })
}

// compileBenign reads each of a project's ignored log patterns as a
// regular expression, or as literal text when it is not a valid one. They
// match a line's text without its container name and timestamp.
func compileBenign(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			expression, err = regexp.Compile(regexp.QuoteMeta(pattern))
		}
		if err != nil {
			continue
		}
		compiled = append(compiled, expression)
	}
	return compiled
}

// LogMessage is a kept log line without its container prefix, timestamp
// and clipping mark: the text an app printed.
func LogMessage(line string) string {
	line = strings.TrimSuffix(strings.TrimSpace(line), clipMarker)
	if _, _, _, message, ok := splitLogLine(line); ok {
		return strings.TrimSpace(message)
	}
	if prefix := logPrefix.FindString(line); prefix != "" {
		line = line[len(prefix):]
	}
	return strings.TrimSpace(line)
}

// parseStates reads `docker compose ps -a --format json`, which prints JSON
// Lines (Compose v5) or a JSON array (older versions).
func parseStates(output []byte) ([]ServiceState, error) {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var raw []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, err
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(trimmed))
		for scanner.Scan() {
			if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
				raw = append(raw, append(json.RawMessage(nil), line...))
			}
		}
	}
	states := make([]ServiceState, 0, len(raw))
	for _, item := range raw {
		var container struct {
			Service, Name, State, Health string
			ExitCode                     int
			Publishers                   []Publisher
		}
		if err := json.Unmarshal(item, &container); err != nil {
			return nil, fmt.Errorf("unexpected docker compose ps output: %w", err)
		}
		states = append(states, ServiceState{Service: container.Service, Name: container.Name, State: container.State,
			Health: container.Health, ExitCode: container.ExitCode, Publishers: container.Publishers})
	}
	sort.Slice(states, func(left, right int) bool { return states[left].Service < states[right].Service })
	return states, nil
}

// tails keeps the last few kilobytes of each service's log lines for the
// repair prompt and the user.
func tails(logs string) map[string]string {
	byService := map[string][]string{}
	for _, line := range strings.Split(logs, "\n") {
		match := logPrefix.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		byService[match[1]] = append(byService[match[1]], line)
	}
	result := map[string]string{}
	for service, lines := range byService {
		text := strings.Join(lines, "\n")
		if len(text) > tailPerService {
			text = text[len(text)-tailPerService:]
		}
		result[service] = text
	}
	return result
}

// lastHealthOutput reads `docker inspect --format '{{json .State.Health}}'`
// for what the latest check printed.
func lastHealthOutput(inspected []byte, secrets []string) string {
	var health struct {
		Log []struct {
			Output string
		}
	}
	if json.Unmarshal(inspected, &health) != nil || len(health.Log) == 0 {
		return ""
	}
	return clip(scrub(strings.TrimSpace(health.Log[len(health.Log)-1].Output), secrets))
}

func clip(line string) string {
	line = strings.TrimSpace(line)
	if len(line) <= maxMatchLength {
		return line
	}
	cut := maxMatchLength
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + clipMarker
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return clip(lines[len(lines)-1])
}
