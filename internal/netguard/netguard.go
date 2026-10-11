// Package netguard is the only place that builds HTTP clients and dialers.
// Every request names a purpose; the purpose's host allowlist is checked
// before sending, and the dialer refuses loopback, private, link-local,
// CGNAT and other internal addresses unless the exact host:port was
// configured as a private target.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

type Purpose string

const (
	LLM      Purpose = "llm"
	SCM      Purpose = "scm"
	Registry Purpose = "registry"
	Release  Purpose = "release"
)

const (
	dialTimeout         = 10 * time.Second
	tlsTimeout          = 10 * time.Second
	headerTimeout       = 30 * time.Second
	modelHeaderTimeout  = 10 * time.Minute
	idleTimeout         = 90 * time.Second
	maxIdleConnections  = 16
	maxRedirects        = 3
	httpsScheme         = "https"
	httpScheme          = "http"
	defaultHTTPSPort    = "443"
	defaultHTTPPort     = "80"
	environmentProxy    = "HTTPS_PROXY"
	environmentNoProxy  = "NO_PROXY"
	noProxyEverything   = "*"
	userAgentHeader     = "User-Agent"
	userAgentPrefix     = "composewright/"
	responseLimitSuffix = "response exceeds the size limit"
)

var (
	ErrHostNotAllowed  = errors.New("host is not on the allowlist for this purpose")
	ErrAddressBlocked  = errors.New("address is internal and not an allowed private target")
	ErrSchemeForbidden = errors.New("only https is allowed, except for configured private targets")
	ErrTooLarge        = errors.New(responseLimitSuffix)
)

// blockedPrefixes complement netip's predicates with ranges that are not
// public: CGNAT, benchmarking, reserved, documentation, NAT64, 6to4, Teredo.
var blockedPrefixes = mustPrefixes("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32",
	"2001:db8::/32", "2002::/16")

// Policy says which hosts each purpose may reach. Hosts are exact names;
// PrivateTargets are host:port pairs exempt from the internal-address check.
type Policy struct {
	Hosts          map[Purpose][]string
	PrivateTargets []string
	Version        string
}

type Guard struct {
	policy Policy
	proxy  *url.URL
	bypass []string
}

// New reads HTTPS_PROXY and NO_PROXY once. A configured proxy is trusted:
// the guard still enforces host allowlists, while the proxy resolves names.
func New(policy Policy) (*Guard, error) {
	guard := &Guard{policy: policy}
	for purpose, hosts := range policy.Hosts {
		lowered := make([]string, len(hosts))
		for index, host := range hosts {
			lowered[index] = strings.ToLower(host)
		}
		guard.policy.Hosts[purpose] = lowered
	}
	proxy := firstEnvironment(environmentProxy)
	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("%s %q is not a URL", environmentProxy, proxy)
		}
		guard.proxy = parsed
	}
	guard.bypass = strings.Split(firstEnvironment(environmentNoProxy), ",")
	return guard, nil
}

// Client returns an HTTP client for one purpose. Redirects must stay on the
// purpose's allowlist and on https.
func (guard *Guard) Client(purpose Purpose) *http.Client {
	return &http.Client{
		Transport: guard.Transport(purpose),
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return guard.allow(purpose, request.URL)
		},
	}
}

// Transport returns a RoundTripper for libraries that take one, such as
// go-containerregistry.
func (guard *Guard) Transport(purpose Purpose) http.RoundTripper {
	timeout := headerTimeout
	if purpose == LLM {
		timeout = modelHeaderTimeout
	}
	base := &http.Transport{
		Proxy:                 guard.proxyFor,
		DialContext:           guard.dial,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       idleTimeout,
		MaxIdleConns:          maxIdleConnections,
		ForceAttemptHTTP2:     true,
	}
	return roundTripper{guard: guard, purpose: purpose, base: base}
}

type roundTripper struct {
	guard   *Guard
	purpose Purpose
	base    *http.Transport
}

func (transport roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := transport.guard.allow(transport.purpose, request.URL); err != nil {
		return nil, err
	}
	if request.Header.Get(userAgentHeader) == "" {
		request = request.Clone(request.Context())
		request.Header.Set(userAgentHeader, userAgentPrefix+transport.guard.policy.Version)
	}
	return transport.base.RoundTrip(request)
}

func (guard *Guard) allow(purpose Purpose, target *url.URL) error {
	host := strings.ToLower(target.Hostname())
	if !slices.Contains(guard.policy.Hosts[purpose], host) {
		return fmt.Errorf("%s (%s): %w", host, purpose, ErrHostNotAllowed)
	}
	if target.Scheme == httpsScheme {
		return nil
	}
	if target.Scheme == httpScheme && guard.privateTarget(hostPort(target)) {
		return nil
	}
	return fmt.Errorf("%s: %w", target.Redacted(), ErrSchemeForbidden)
}

func (guard *Guard) proxyFor(request *http.Request) (*url.URL, error) {
	if guard.proxy == nil || request.URL.Scheme != httpsScheme || guard.bypassed(request.URL.Hostname()) {
		return nil, nil
	}
	return guard.proxy, nil
}

func (guard *Guard) bypassed(host string) bool {
	return slices.ContainsFunc(guard.bypass, func(entry string) bool {
		entry = strings.TrimPrefix(strings.TrimSpace(entry), ".")
		return entry == noProxyEverything || (entry != "" && (host == entry || strings.HasSuffix(host, "."+entry)))
	})
}

// dial connects directly, refusing internal addresses after DNS resolution
// (so a public name that resolves to 127.0.0.1 is refused too), except for
// the configured proxy and configured private targets.
func (guard *Guard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	trusted := guard.privateTarget(address) || (guard.proxy != nil && address == hostPort(guard.proxy))
	dialer := &net.Dialer{Timeout: dialTimeout}
	if !trusted {
		dialer.Control = func(_, resolved string, _ syscall.RawConn) error { return checkAddress(resolved) }
	}
	return dialer.DialContext(ctx, network, address)
}

func (guard *Guard) privateTarget(address string) bool {
	return slices.Contains(guard.policy.PrivateTargets, strings.ToLower(address))
}

func checkAddress(address string) error {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", address, ErrAddressBlocked)
	}
	ip := endpoint.Addr()
	if Internal(ip) {
		return fmt.Errorf("%s: %w", address, ErrAddressBlocked)
	}
	return nil
}

// Internal reports addresses that must never be dialled without an explicit
// private target: loopback, RFC 1918, link-local, multicast, CGNAT,
// reserved and documentation ranges, and IPv4 addresses hidden in IPv6.
func Internal(ip netip.Addr) bool {
	if ip.Is4In6() || ip.Zone() != "" {
		return true
	}
	special := ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast()
	return special || slices.ContainsFunc(blockedPrefixes, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
}

// ReadBody reads a response body up to limit bytes; the caller closes it.
func ReadBody(response *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	return body, nil
}

func hostPort(target *url.URL) string {
	port := target.Port()
	if port == "" && target.Scheme == httpScheme {
		port = defaultHTTPPort
	}
	if port == "" {
		port = defaultHTTPSPort
	}
	return strings.ToLower(net.JoinHostPort(target.Hostname(), port))
}

func firstEnvironment(name string) string {
	return firstNonEmpty(os.Getenv(name), os.Getenv(strings.ToLower(name)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func mustPrefixes(texts ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(texts))
	for index, text := range texts {
		prefixes[index] = netip.MustParsePrefix(text)
	}
	return prefixes
}
