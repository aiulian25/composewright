package ir

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	// urlHost is the host of a URL with any scheme, after any user info.
	urlHost = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(?:[^@/\s"']*@)?(\[[^\]]*\]|[^/:\s"'?#]*)`)
	// httpURL is a whole http URL in a healthcheck, up to a space, a quote
	// or a shell operator.
	httpURL = regexp.MustCompile(`(?i)\bhttp://[^\s"'|;&()<>\x60]+`)
)

// LoopbackHosts are the names a healthcheck uses for its own container.
var LoopbackHosts = []string{"localhost", "127.0.0.1", "0.0.0.0", "::1", "::"}

// URLHosts are the hosts of every URL in a healthcheck command, as written.
func URLHosts(test string) []string {
	var hosts []string
	for _, match := range urlHost.FindAllStringSubmatch(test, -1) {
		hosts = append(hosts, match[1])
	}
	return hosts
}

// HealthcheckURL is the first http URL a healthcheck test asks, as curl,
// wget or a script would be given it.
func HealthcheckURL(test []string) (*url.URL, bool) {
	match := httpURL.FindString(strings.Join(test, " "))
	if match == "" {
		return nil, false
	}
	parsed, err := url.Parse(match)
	return parsed, err == nil
}
