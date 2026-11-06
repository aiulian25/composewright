package netguard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
)

type budgetTransport func(*http.Request) (*http.Response, error)

func (run budgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return run(request)
}

func TestResponseBudgets(t *testing.T) {
	for _, test := range []struct {
		name                  string
		idle, total, progress time.Duration
		cancel                bool
		want                  error
	}{
		{"headers only", 25 * time.Millisecond, time.Second, 0, false, ErrResponseIdle},
		{"trickle total", 100 * time.Millisecond, 80 * time.Millisecond, 5 * time.Millisecond, false, ErrRequestDeadline},
		{"manual cancel", time.Second, time.Second, 0, true, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, writer := io.Pipe()
			defer writer.Close()
			transport := budgetTransport(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: reader, Header: make(http.Header)}, nil
			})
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://allowed.invalid", nil)
			response, err := boundedRoundTrip(transport, request, LLM, requestBudgets{test.total, test.idle})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			done := make(chan error, 1)
			go func() { _, err := io.ReadAll(response.Body); done <- err }()
			if test.progress > 0 {
				go func() {
					for {
						time.Sleep(test.progress)
						if _, err := writer.Write([]byte("x")); err != nil {
							return
						}
					}
				}()
			}
			if test.cancel {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, test.want) {
					t.Fatalf("got %v want %v", err, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("reader remained blocked")
			}
		})
	}
}

func TestSlowLocalStreamSurvivesProgress(t *testing.T) {
	reader, writer := io.Pipe()
	request, _ := http.NewRequest(http.MethodGet, "http://local.invalid:11434/api/chat", nil)
	guard, err := New(Policy{Hosts: map[Purpose][]string{LLM: {"local.invalid"}}, PrivateTargets: []string{"local.invalid:11434"}, ProviderTimeout: time.Second, ProviderIdleTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	transport := budgetTransport(func(*http.Request) (*http.Response, error) {
		time.Sleep(30 * time.Millisecond) // controlled cold start, no network
		return &http.Response{StatusCode: 200, Body: reader, Header: make(http.Header)}, nil
	})
	if err := guard.allow(LLM, request.URL); err != nil {
		t.Fatal(err)
	}
	response, err := boundedRoundTrip(transport, request, LLM, guard.budgets(LLM))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	go func() {
		defer writer.Close()
		for _, part := range []string{"one", "two", "three"} {
			time.Sleep(20 * time.Millisecond)
			if _, err := io.WriteString(writer, part); err != nil {
				return
			}
		}
	}()
	value, err := io.ReadAll(response.Body)
	if err != nil || string(value) != "onetwothree" {
		t.Fatalf("stream: %q %v", value, err)
	}
	// Retained byte caps still apply to otherwise healthy bodies.
	if _, err = ReadBody(&http.Response{Body: io.NopCloser(strings.NewReader("long"))}, 2); !errors.Is(err, ErrTooLarge) {
		t.Fatal("byte bound lost")
	}
}

func TestRequestBudgetIncludesHeaders(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://allowed.invalid", nil)
	transport := budgetTransport(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	_, err := boundedRoundTrip(transport, request, SCM, requestBudgets{20 * time.Millisecond, time.Second})
	if !errors.Is(err, ErrRequestDeadline) {
		t.Fatalf("headers did not respect total budget: %v", err)
	}
}

func TestBodyBudgetsKeepConnectionsReusable(t *testing.T) {
	guard, err := New(Policy{Hosts: map[Purpose][]string{SCM: {"local.invalid"}}, PrivateTargets: []string{"local.invalid:80"}})
	if err != nil {
		t.Fatal(err)
	}
	transport := guard.base(SCM)
	var count atomic.Int32
	var serving sync.WaitGroup
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		count.Add(1)
		serving.Add(1)
		go func() {
			defer serving.Done()
			defer server.Close()
			reader := bufio.NewReader(server)
			for {
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				request.Body.Close()
				if _, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
	defer func() { transport.CloseIdleConnections(); serving.Wait() }()
	for i := 0; i < 10; i++ {
		response, err := guard.Client(SCM).Get("http://local.invalid/")
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(value) != "ok" {
			t.Fatalf("response %q: %v", value, err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("body cleanup lost keepalive: %d connections", count.Load())
	}
}

func TestConfiguredBudgetsReachClients(t *testing.T) {
	configuration := config.Default()
	configuration.Network.IntakeTimeout = 2 * time.Second
	configuration.Network.ProviderTimeout = 4 * time.Second
	configuration.Network.ProviderIdleTimeout = 3 * time.Second
	policy, err := PolicyFromConfig(configuration, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	guard, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Client(SCM).Timeout != 2*time.Second || guard.Client(LLM).Timeout != 4*time.Second || guard.base(LLM).ResponseHeaderTimeout != 3*time.Second {
		t.Fatal("configuration lost at network boundary")
	}
}
