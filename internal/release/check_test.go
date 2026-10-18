package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aiulian25/composewright/internal/netguard"
)

// serverGuard lets the release purpose reach a local test server only.
func serverGuard(t *testing.T, address string) *netguard.Guard {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := netguard.New(netguard.Policy{Hosts: map[netguard.Purpose][]string{netguard.Release: {target.Hostname()}}, PrivateTargets: []string{target.Host}})
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func TestFetch(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
		want   string
		fails  bool
	}{
		"published":    {status: http.StatusOK, body: `{"tag_name":"v0.2.0","html_url":"https://example.test/v0.2.0","published_at":"2026-10-12T08:00:00Z"}`, want: "v0.2.0"},
		"none yet":     {status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		"no tag":       {status: http.StatusOK, body: `{}`, fails: true},
		"rate limited": {status: http.StatusForbidden, body: `{"message":"rate limit"}`, fails: true},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept") != acceptHeader {
					t.Errorf("Accept %q", request.Header.Get("Accept"))
				}
				writer.WriteHeader(answer.status)
				_, _ = writer.Write([]byte(answer.body))
			}))
			defer server.Close()
			latest, err := Fetch(context.Background(), serverGuard(t, server.URL), server.URL)
			if (err != nil) != answer.fails || latest.Tag != answer.want {
				t.Fatalf("got %+v, %v", latest, err)
			}
			if answer.want != "" && (latest.URL == "" || latest.Published.IsZero()) {
				t.Fatalf("link or date missing: %+v", latest)
			}
		})
	}
}
