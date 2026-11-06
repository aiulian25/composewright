package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSubprojectIntakeAndScopeDisclosure(t *testing.T) {
	address := "https://github.com/acme/mono/tree/feature%2Fnew/apps/api"
	request := httptest.NewRequest(http.MethodPost, "/start", nil)
	request.PostForm = url.Values{"ask": {address}}
	if ask := askFrom(request); ask.source != address || ask.problem() != "" {
		t.Fatalf("Home altered the tree link: %+v", ask)
	}
	server := intakeServer(t)
	response := httptest.NewRecorder()
	view := analyseView{SourceRoot: "apps/<api>", SharedFiles: []string{"shared/common.env", "shared/<config>.yaml"}}
	if err := server.render(response, http.StatusOK, "analyse", Page{Data: view}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Selected app directory:", "Included 2 shared deployment files", "shared/common.env", "apps/&lt;api&gt;", "shared/&lt;config&gt;.yaml"} {
		if !strings.Contains(response.Body.String(), text) {
			t.Errorf("missing safe scope disclosure %q", text)
		}
	}
	response = httptest.NewRecorder()
	if err := server.render(response, http.StatusOK, "new", Page{Data: server.newView(address, "heads/feature/new", "", "rules")}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Body.String(), "GitHub tree and blob links") || !strings.Contains(response.Body.String(), `value="heads/feature/new"`) {
		t.Fatal("New does not retain source/ref choices")
	}
}
