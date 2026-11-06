package web

import (
	"context"
	"html"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/workspace"
	assets "github.com/aiulian25/composewright/web"
)

// Test intake handlers directly through HTTP recorders: no TCP listener,
// Docker daemon, background jobs or model endpoint is required.
func intakeServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	loaded := &config.Loaded{Config: config.Default(), Paths: config.Paths{DataDir: filepath.Join(root, "data"), ConfigFile: filepath.Join(root, "config.yaml"), KeyFile: filepath.Join(root, "data", "master.key")}}
	loaded.Config.LLM.Provider = config.ProviderNone
	if _, err := workspace.Init(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	opened, err := workspace.Open(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	pages, err := assets.Templates(functions())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{loaded: loaded, options: Options{Loaded: loaded, Workspace: opened}, pages: pages}
}

func textareaValue(t *testing.T, body, name string) string {
	t.Helper()
	match := regexp.MustCompile(`(?s)<textarea[^>]*name="` + name + `"[^>]*>(.*?)</textarea>`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("missing textarea %s", name)
	}
	// HTML textarea parsing discards one newline immediately after the start tag.
	return strings.TrimPrefix(html.UnescapeString(match[1]), "\n")
}

func TestIntakeDraftSurvivesErrors(t *testing.T) {
	server := intakeServer(t)
	compose := "\nservices:\n  api:\n    image: example/app:1\n# </textarea><script>alert('dummy')</script>\n"
	env := "PASSWORD='dummy<secret>&\nsecond-line'\n"
	for _, files := range [][]*multipart.FileHeader{nil, {{Filename: "compose.yaml"}}} {
		name := "INVALID!"
		if len(files) > 0 {
			name = "valid"
		}
		form := url.Values{"compose": {compose}, "env": {env}, "name": {name}, "engine": {"rules"}, "test": {"0"}}
		request := httptest.NewRequest(http.MethodPost, "/new", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if len(files) > 0 {
			request.MultipartForm = &multipart.Form{File: map[string][]*multipart.FileHeader{"files": files}}
		}
		response := httptest.NewRecorder()
		if err := server.startGeneration(response, request, nil); err != nil {
			t.Fatal(err)
		}
		if response.Code != 400 {
			t.Fatalf("response: %d %v", response.Code, response.Header())
		}
		body := response.Body.String()
		if textareaValue(t, body, "compose") != compose || textareaValue(t, body, "env") != env {
			t.Fatal("draft changed")
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("draft may be cached")
		}
		if strings.Contains(body, "<script>alert") || response.Header().Get("Location") != "" {
			t.Fatal("draft escaped into markup or redirect")
		}
		if len(files) > 0 && !strings.Contains(body, "Choose your files again") {
			t.Fatal("file reselection not explained")
		}
	}
}

func TestIntakeSourceRulesAndExclusiveWrites(t *testing.T) {
	cases := []struct {
		ask     generationRequest
		invalid bool
	}{
		{generationRequest{source: "https://github.com/example/app", env: "A=1"}, true},
		{generationRequest{env: "A=1"}, true},
		{generationRequest{files: []*multipart.FileHeader{{Filename: ".env"}}}, true},
		{generationRequest{compose: "services: {}", env: "A=1"}, false},
		{generationRequest{env: "A=1", files: []*multipart.FileHeader{{Filename: "README.md"}}}, false},
		{generationRequest{files: []*multipart.FileHeader{{Filename: "compose.yaml"}, {Filename: "compose.yaml"}}}, true},
	}
	for _, tc := range cases {
		if got := tc.ask.problem() != ""; got != tc.invalid {
			t.Errorf("invalid=%v want %v", got, tc.invalid)
		}
	}
	server := intakeServer(t)
	if _, err := server.saveUploads([]*multipart.FileHeader{{Filename: "compose.yaml"}}, "pasted", ""); err == nil {
		t.Fatal("duplicate write allowed")
	}
	directory, err := server.saveUploads(nil, "services: {}", "A=1\n")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	content, err := os.ReadFile(filepath.Join(directory, ".env"))
	if err != nil || string(content) != "A=1\n" {
		t.Fatal("successful intake changed file")
	}
}

func TestIntakeHomeAndLaunchErrorsKeepDrafts(t *testing.T) {
	server := intakeServer(t)
	raw := "\nPASSWORD='dummy\n</textarea><script>no</script>'\n"
	form := url.Values{"ask": {raw}, "engine": {"rules"}}
	request := httptest.NewRequest(http.MethodPost, "/start", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.MultipartForm = &multipart.Form{File: map[string][]*multipart.FileHeader{"files": {{Filename: "compose.yaml"}}}}
	response := httptest.NewRecorder()
	if err := server.start(response, request, nil); err != nil {
		t.Fatal(err)
	}
	if response.Code != 400 || response.Header().Get("Cache-Control") != "no-store" || textareaValue(t, response.Body.String(), "ask") != raw {
		t.Fatal("Home lost draft")
	}
	form = url.Values{"name": {"valid"}, "compose": {"services: {}"}, "env": {"PASSWORD=dummy\n"}}
	request = httptest.NewRequest(http.MethodPost, "/new", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.MultipartForm = &multipart.Form{File: map[string][]*multipart.FileHeader{"files": {{Filename: "README.md"}}}}
	response = httptest.NewRecorder()
	// A header without backing content fails while staging, before any job.
	if err := server.startGeneration(response, request, nil); err != nil {
		t.Fatal(err)
	}
	if response.Code != 500 || textareaValue(t, response.Body.String(), "env") != "PASSWORD=dummy\n" {
		t.Fatal("staging failure lost draft")
	}
	entries, err := os.ReadDir(server.uploadsRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("failed staging left files behind")
	}
}
