package cli

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/netguard"
)

const (
	linkdingCommit = "27b7303baf41bb28babc610ac8eaa486e1ddfab5"
	// modelHost cannot resolve, so the model call is recorded by the proxy
	// and then fails, and the rules finish the plan.
	modelHost = "llm.composewright.invalid"
	// modelBudget fits linkding's prompt (about 16k tokens): under the default
	// llm.max_input_tokens gen skips the model, and the proxy sees no call.
	modelBudget = "100000"
)

// recordingProxy is an HTTPS CONNECT proxy that remembers every host:port it
// was asked to reach and tunnels the connection unchanged.
type recordingProxy struct {
	mutex sync.Mutex
	hosts map[string]int
}

func (proxy *recordingProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodConnect {
		http.Error(writer, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	proxy.mutex.Lock()
	proxy.hosts[request.Host]++
	proxy.mutex.Unlock()
	upstream, err := net.Dial("tcp", request.Host)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, _ := writer.(http.Hijacker)
	client, _, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { io.Copy(upstream, client); upstream.Close() }()
	go func() { io.Copy(client, upstream); client.Close() }()
}

func (proxy *recordingProxy) contacted() []string {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	hosts := make([]string, 0, len(proxy.hosts))
	for host := range proxy.hosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// TestEgressStaysOnTheAllowlist runs gen, with the model engine, against a
// real GitHub repository through a recording proxy and checks every
// contacted host against the allowlist the config produces.
// It needs network access: COMPOSEWRIGHT_NETWORK_TESTS=1 make egress.
func TestEgressStaysOnTheAllowlist(t *testing.T) {
	if os.Getenv("COMPOSEWRIGHT_NETWORK_TESTS") == "" {
		t.Skip("set COMPOSEWRIGHT_NETWORK_TESTS=1 to run tests that reach the internet")
	}
	proxy := &recordingProxy{hosts: map[string]int{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: proxy}
	go server.Serve(listener)
	defer server.Close()
	environment := isolate(t)
	t.Setenv("HTTPS_PROXY", "http://"+listener.Addr().String())
	t.Setenv("NO_PROXY", "")
	run(t, environment, "init")
	t.Setenv("COMPOSEWRIGHT_TEST_LLM_KEY", "sk-not-a-real-key")
	for key, value := range map[string]string{"llm.provider": config.ProviderAnthropic, "llm.base_url": "https://" + modelHost,
		"llm.api_key_ref": "env:COMPOSEWRIGHT_TEST_LLM_KEY", "llm.max_input_tokens": modelBudget} {
		if result := run(t, environment, "config", "set", key, value); result.code != exitOK {
			t.Fatalf("config set %s: %+v", key, result)
		}
	}
	result := run(t, environment, "gen", "--engine", "model", "--no-validate", "--ref", linkdingCommit, "https://github.com/sissbruecker/linkding")
	if result.code != exitOK || !strings.Contains(result.stdout, "the rules' alone") {
		t.Fatalf("gen through the proxy failed or did not fall back to the rules: %+v", result)
	}
	loaded, err := config.Load(config.Flags{ConfigFile: filepath.Join(environment, "config.yaml"), DataDir: filepath.Join(environment, "data")})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := netguard.PolicyFromConfig(loaded.Config, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	for _, hosts := range policy.Hosts {
		for _, host := range hosts {
			allowed = append(allowed, net.JoinHostPort(host, "443"))
		}
	}
	contacted := proxy.contacted()
	t.Logf("contacted: %v", contacted)
	for _, host := range contacted {
		if !slices.Contains(allowed, host) {
			t.Errorf("contacted %s, which is not on the allowlist", host)
		}
	}
	for _, required := range []string{"api.github.com:443", "codeload.github.com:443", modelHost + ":443"} {
		if !slices.Contains(contacted, required) {
			t.Errorf("%s was not contacted; the proxy did not see the fetch", required)
		}
	}
	report := filepath.Join(environment, "egress.txt")
	_ = os.WriteFile(report, []byte(strings.Join(contacted, "\n")+"\n"), 0o600)
}
