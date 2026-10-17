package sandbox

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJudgeSkipsBenignLines(t *testing.T) {
	states := []ServiceState{{Service: "app", State: "running", Health: healthHealthy}}
	logs := "app-1  | 2026-10-10T08:15:02Z FATAL: optional plugin missing\n"
	if verdict := judge(states, nil, "", logs, nil, nil); verdict.Reason != ReasonLogs || len(verdict.LogMatches) != 1 {
		t.Fatalf("without a pattern: %+v", verdict)
	}
	benign := []*regexp.Regexp{regexp.MustCompile(regexp.QuoteMeta("FATAL: optional plugin missing"))}
	if verdict := judge(states, nil, "", logs, nil, benign); !verdict.Passed() {
		t.Fatalf("a benign line failed the run: %+v", verdict)
	}
	other := logs + "app-1  | 2026-10-10T08:15:03Z panic: boom\n"
	if verdict := judge(states, nil, "", other, nil, benign); verdict.Reason != ReasonLogs || verdict.Detail != "app-1  | 2026-10-10T08:15:03Z panic: boom" {
		t.Fatalf("another fatal line passed: %+v", verdict)
	}
}

func TestValidateIgnoresTheProjectsBenignPatterns(t *testing.T) {
	logs := "app-1  | FATAL: config (default) not found\napp-1  | FATAL: optional plugin missing\n"
	for name, patterns := range map[string][]string{
		"quoted and regular expression": {regexp.QuoteMeta("FATAL: config (default) not found"), `optional plugin \w+`},
		"invalid expression is literal": {"FATAL: config (default", "FATAL: optional plugin missing"},
		"anchored on the line's text":   {`^FATAL: config \(default\)`, `^FATAL: optional`},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeDocker{ps: healthyApp, logs: logs}
			verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte(""),
				Benign: patterns})
			if err != nil {
				t.Fatal(err)
			}
			if !verdict.Passed() {
				t.Fatalf("verdict %s/%s: %v", verdict.Result, verdict.Reason, verdict.LogMatches)
			}
		})
	}
}

func TestLogMessage(t *testing.T) {
	for line, want := range map[string]string{
		"app-1  | 2026-10-10T08:15:02.123456789Z FATAL: optional plugin missing": "FATAL: optional plugin missing",
		"db  | FATAL: no stamp":                         "FATAL: no stamp",
		"app-1  | 2026-10-10T08:15:02Z " + "long line…": "long line",
		"plain text": "plain text",
	} {
		if got := LogMessage(line); got != want {
			t.Errorf("LogMessage(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestBenignPatternsSkipInvalidTextAndContainerNames(t *testing.T) {
	if compiled := compileBenign([]string{"\xff(", ""}); len(compiled) != 0 {
		t.Fatalf("compiled %v", compiled)
	}
	states := []ServiceState{{Service: "app", State: "running", Health: healthHealthy}}
	logs := "app-1  | 2026-10-10T08:15:02Z panic: boom\n"
	if verdict := judge(states, nil, "", logs, nil, compileBenign([]string{"app"})); verdict.Passed() {
		t.Fatal("a pattern matched the container name and hid a panic")
	}
}

func TestClipKeepsWholeCharacters(t *testing.T) {
	line := strings.Repeat("a", maxMatchLength-1) + "é tail"
	clipped := clip(line)
	if !utf8.ValidString(clipped) || !strings.HasSuffix(clipped, clipMarker) {
		t.Fatalf("clip(%q) = %q", line, clipped)
	}
	if message := LogMessage(clipped); !strings.HasPrefix(line, message) {
		t.Fatalf("LogMessage(%q) = %q is not the start of the line", clipped, message)
	}
}
