package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	tailPerService   = 8 << 10
	logPrefixPattern = `^(\S+?)(-\d+)?\s+\|\s?`
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
	Tier           string `json:"tier"`
	Rootless       bool   `json:"rootless"`
	CgroupVersion  string `json:"cgroup_version"`
	CgroupDriver   string `json:"cgroup_driver"`
	LimitsEnforced bool   `json:"limits_enforced"`
	EngineVersion  string `json:"engine_version,omitempty"`
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
// that may exit 0.
func judge(states []ServiceState, upErr error, upOutput, logs string, completedOK map[string]bool) Verdict {
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
	if matches := fatalLines(logs); len(matches) > 0 {
		verdict = fail(verdict, ReasonLogs, matches[0])
		verdict.LogMatches = matches
	}
	return verdict
}

func fail(verdict Verdict, reason, detail string) Verdict {
	verdict.Result, verdict.Reason, verdict.Detail = ResultFail, reason, detail
	return verdict
}

func fatalLines(logs string) []string {
	var matches []string
	refused := 0
	for _, line := range strings.Split(logs, "\n") {
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
	for _, benign := range benignPatterns {
		if benign.MatchString(line) {
			return false
		}
	}
	for _, pattern := range fatalPatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
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
	if len(line) > maxMatchLength {
		return line[:maxMatchLength] + "…"
	}
	return line
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return clip(lines[len(lines)-1])
}
