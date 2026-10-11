package fetch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitHub(t *testing.T) {
	valid := map[string][2]string{
		"https://github.com/miniflux/v2":                 {"miniflux", "v2"},
		"https://github.com/dani-garcia/vaultwarden.git": {"dani-garcia", "vaultwarden"},
		"https://www.github.com/louislam/uptime-kuma/":   {"louislam", "uptime-kuma"},
		"git@github.com:sissbruecker/linkding.git":       {"sissbruecker", "linkding"},
		"github.com/miniflux/v2":                         {"miniflux", "v2"},
	}
	for input, want := range valid {
		owner, repository, ok := ParseGitHub(input)
		if !ok || owner != want[0] || repository != want[1] {
			t.Errorf("ParseGitHub(%q) = %q %q %v", input, owner, repository, ok)
		}
	}
	for _, input := range []string{"http://github.com/a/b", "https://github.com/a", "https://github.com/a/b/tree/main",
		"https://evil.com/github.com/a/b", "https://github.com.evil.com/a/b", "https://github.com/a/..", "file:///etc", "evilgithub.com/a/b", "ftp://github.com/a/b"} {
		if _, _, ok := ParseGitHub(input); ok {
			t.Errorf("ParseGitHub(%q) accepted", input)
		}
	}
}

func TestLocalReadsOriginRemote(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\turl = https://github.com/wrong/one\n[remote \"origin\"]\n\turl = git@github.com:miniflux/v2.git\n"
	if err := os.WriteFile(filepath.Join(directory, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := Local(directory, "")
	if err != nil {
		t.Fatal(err)
	}
	if source.Owner != "miniflux" || source.Repository != "v2" || source.URL != "https://github.com/miniflux/v2" {
		t.Fatalf("source %+v", source)
	}
	overridden, err := Local(directory, "https://github.com/louislam/uptime-kuma")
	if err != nil || overridden.Owner != "louislam" {
		t.Fatalf("--upstream ignored: %+v %v", overridden, err)
	}
}

func TestGitHubStatusMessages(t *testing.T) {
	cases := map[int]string{404: "not found", 403: "rate limit", 500: "unexpected status"}
	for status, want := range cases {
		response := &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{}}
		if status == 403 {
			response.Header.Set(rateLimitRemaining, "0")
		}
		if err := githubStatus(response, "o", "r"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: %v", status, err)
		}
	}
	if err := githubStatus(&http.Response{StatusCode: 200}, "o", "r"); err != nil {
		t.Fatal(err)
	}
}

// TestReleaseTag reads a release's tag, and leaves anything that is not a
// usable ref to the default branch.
func TestReleaseTag(t *testing.T) {
	cases := map[string]string{`{"tag_name":"v2.2.0","name":"2.2.0"}`: "v2.2.0", `{"tag_name":"../../etc"}`: "", `{"name":"untagged"}`: ""}
	for body, want := range cases {
		if got, err := releaseTag([]byte(body)); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", body, got, err, want)
		}
	}
	if _, err := releaseTag([]byte("not json")); err == nil {
		t.Fatal("a broken answer was read as a release")
	}
}

func TestGitHubRejectsHostileRefs(t *testing.T) {
	for _, ref := range []string{"../../etc", "main?x=1", "a b", "refs/../../x", strings.Repeat("a", 300)} {
		if _, err := GitHub(context.Background(), nil, "", "o", "r", ref); err == nil || !strings.Contains(err.Error(), "ref") {
			t.Errorf("ref %q accepted: %v", ref, err)
		}
	}
}
