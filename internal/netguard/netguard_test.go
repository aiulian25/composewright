package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
)

func TestInternalAddresses(t *testing.T) {
	internal := []string{"127.0.0.1", "127.8.9.10", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.10",
		"169.254.169.254", "fe80::1", "100.64.0.1", "100.127.255.254", "198.18.0.1", "198.19.255.255", "240.0.0.1",
		"255.255.255.255", "0.0.0.0", "0.1.2.3", "fc00::1", "fd12:3456::1", "::ffff:127.0.0.1", "::ffff:8.8.8.8",
		"224.0.0.1", "ff02::1", "64:ff9b::7f00:1", "2002:7f00:1::1", "2001:0:4136:e378::1", "192.0.2.1", "2001:db8::1", "::"}
	for _, text := range internal {
		if !Internal(netip.MustParseAddr(text)) {
			t.Errorf("%s treated as public", text)
		}
	}
	for _, text := range []string{"8.8.8.8", "140.82.112.3", "1.1.1.1", "2606:4700:4700::1111", "2a00:1450:4001:80b::200e"} {
		if Internal(netip.MustParseAddr(text)) {
			t.Errorf("%s treated as internal", text)
		}
	}
}

func testGuard(t *testing.T, policy Policy) *Guard {
	t.Helper()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	guard, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func get(ctx context.Context, guard *Guard, purpose Purpose, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := guard.Client(purpose).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = ReadBody(response, 1024)
	return err
}

func TestLoopbackIsRefusedUnlessItIsAPrivateTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { io.WriteString(writer, "ok") }))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	guard := testGuard(t, Policy{Hosts: map[Purpose][]string{LLM: {"127.0.0.1"}}})
	if err := get(context.Background(), guard, LLM, server.URL); !errors.Is(err, ErrSchemeForbidden) {
		t.Fatalf("plain http to loopback: %v", err)
	}
	allowed := testGuard(t, Policy{Hosts: map[Purpose][]string{LLM: {"127.0.0.1"}}, PrivateTargets: []string{target.Host}})
	if err := get(context.Background(), allowed, LLM, server.URL); err != nil {
		t.Fatalf("configured private target refused: %v", err)
	}
	if err := get(context.Background(), allowed, SCM, server.URL); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("another purpose reached the private target: %v", err)
	}
}

// TestDialRefusesInternalAddressesBehindPublicNames proves the check runs on
// the resolved address: a TLS server on 127.0.0.1 reached through an
// allowlisted name is refused at dial time.
func TestDialRefusesInternalAddressesBehindPublicNames(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { io.WriteString(writer, "ok") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	guard := testGuard(t, Policy{Hosts: map[Purpose][]string{SCM: {"localhost"}}})
	err := get(context.Background(), guard, SCM, "https://localhost:"+port+"/")
	if !errors.Is(err, ErrAddressBlocked) {
		t.Fatalf("dial to a name resolving to loopback: %v", err)
	}
}

func TestRedirectsStayOnTheAllowlist(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		io.WriteString(writer, "ok")
	}))
	defer redirector.Close()
	target, _ := url.Parse(redirector.URL)
	guard := testGuard(t, Policy{Hosts: map[Purpose][]string{SCM: {"127.0.0.1"}}, PrivateTargets: []string{target.Host}})
	err := get(context.Background(), guard, SCM, redirector.URL+"/start")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("redirect to the metadata service: %v", err)
	}
}

func TestReadBodyLimit(t *testing.T) {
	response := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 20)))}
	if _, err := ReadBody(response, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized body: %v", err)
	}
}

func TestPolicyFromConfig(t *testing.T) {
	configuration := config.Default()
	policy, err := PolicyFromConfig(configuration, []string{"registry.example.com:5000/team/*"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(policy.Hosts[LLM], "127.0.0.1") || !contains(policy.PrivateTargets, "127.0.0.1:11434") {
		t.Fatalf("ollama base url not allowed: %+v", policy)
	}
	for _, host := range []string{"auth.docker.io", "registry-1.docker.io", "ghcr.io", "quay.io", "registry.example.com"} {
		if !contains(policy.Hosts[Registry], host) {
			t.Errorf("registry host %s missing: %v", host, policy.Hosts[Registry])
		}
	}
	if !contains(policy.Hosts[SCM], "codeload.github.com") || len(policy.Hosts[Release]) != 1 {
		t.Fatalf("scm/release hosts: %+v", policy.Hosts)
	}
	configuration.LLM.Provider, configuration.LLM.BaseURL = config.ProviderAnthropic, "https://api.anthropic.com"
	public, err := PolicyFromConfig(configuration, nil, "test")
	if err != nil || !contains(public.Hosts[LLM], "api.anthropic.com") || contains(public.PrivateTargets, "api.anthropic.com:443") {
		t.Fatalf("a public model host must be allowed but stay under the internal-address check: %+v %v", public, err)
	}
	configuration.LLM.Provider = config.ProviderNone
	none, _ := PolicyFromConfig(configuration, nil, "test")
	if len(none.Hosts[LLM]) != 0 || contains(none.PrivateTargets, "127.0.0.1:11434") {
		t.Fatalf("provider none still allows the model host: %+v", none)
	}
}
