package ir

import (
	"slices"
	"testing"
)

func TestHealthcheckURL(t *testing.T) {
	cases := []struct {
		name  string
		test  []string
		found bool
		host  string
		path  string
	}{
		{"curl", []string{HealthcheckShell, "curl -f http://localhost:8080/api/health || exit 1"}, true, "localhost", "/api/health"},
		{"wget words", []string{HealthcheckCommand, "wget", "-qO-", "http://127.0.0.1:3000/healthz"}, true, "127.0.0.1", "/healthz"},
		{"no path", []string{HealthcheckCommand, "wget", "--spider", "http://localhost"}, true, "localhost", ""},
		{"ends a command", []string{HealthcheckShell, "curl -fs http://localhost/ping;echo"}, true, "localhost", "/ping"},
		{"no URL", []string{HealthcheckCommand, "pg_isready", "-U", "app"}, false, "", ""},
		{"https", []string{HealthcheckShell, "curl -fk https://localhost:8443/health"}, false, "", ""},
		{"variable port", []string{HealthcheckShell, "curl -f http://localhost:$${PORT}/health"}, false, "", ""},
	}
	for _, testCase := range cases {
		asked, found := HealthcheckURL(testCase.test)
		if found != testCase.found {
			t.Errorf("%s: found %v, want %v", testCase.name, found, testCase.found)
			continue
		}
		if found && (asked.Hostname() != testCase.host || asked.Path != testCase.path) {
			t.Errorf("%s: %s, want host %s and path %q", testCase.name, asked, testCase.host, testCase.path)
		}
	}
}

func TestURLHosts(t *testing.T) {
	hosts := URLHosts("curl -f http://user:pass@localhost:8080/ && wget https://[::1]/x ftp://evil.example")
	if !slices.Equal(hosts, []string{"localhost", "[::1]", "evil.example"}) {
		t.Fatalf("hosts %q", hosts)
	}
}
