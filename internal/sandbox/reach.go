package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/ir"
)

const (
	// probeService runs the reachability probe. It has a profile of its own,
	// so `up` never starts it, and `run` puts it on the sandbox network.
	probeService = "composewright-probe"
	// probeImage is an official image, which P1 always trusts, pinned by digest.
	probeImage       = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	probeUser        = "65534:65534"
	probeMemoryBytes = 32 << 20
	probeCPUs        = 0.25
	probePIDs        = 16
	// probeGrace is how long an app whose healthcheck already passed may
	// still refuse connections from the network.
	probeGrace       = 10 * time.Second
	probeInterval    = 2 * time.Second
	probeTimeout     = 5 * time.Second
	defaultProbePath = "/"
	httpScheme       = "http://"
	httpAnswer       = "HTTP/"
	refusedAnswer    = "Connection refused"
	timedOutAnswer   = "download timed out"
	unresolvedAnswer = "bad address"
	notRun           = "skipped"
)

// probeScript asks the URL in $1 with busybox wget every $3 seconds while
// nothing answers, until $2 seconds have passed, each try waiting at most
// $4 seconds, and prints what wget said the last time. A status line is an
// answer even when wget then follows a redirect to the app's public address,
// which the sandbox network refuses.
const probeScript = `deadline=$(($(date +%s) + $2))
while :; do
	said=$(wget -q -S -O /dev/null -T "$4" "$1" 2>&1)
	case $said in *"` + httpAnswer + `"*) break ;; *"` + refusedAnswer + `"*|*"` + timedOutAnswer + `"*) ;; *) break ;; esac
	[ "$(date +%s)" -lt "$deadline" ] || break
	sleep "$3"
done
printf '%s\n' "$said"`

var (
	probeLimits = Limits{MemoryBytes: probeMemoryBytes, CPUs: probeCPUs, PIDs: probePIDs}
	statusLine  = regexp.MustCompile(`HTTP/[0-9.]+ ([0-9]{3})`)
)

// Reach is what the reachability probe found for the app's service, in
// words; Checked is false when it did not run.
type Reach struct {
	Service string `json:"service"`
	Checked bool   `json:"checked"`
	Said    string `json:"said"`
}

// reach asks the app for its probe path from another container on the
// sandbox network once every service is up, the way a proxy reaches it.
// Nothing answering there fails the run: the app listens on 127.0.0.1
// inside its container, or on another port. A cancelled run is an error,
// never a pass.
func (sandbox *Sandbox) reach(ctx context.Context, directory, name string, input Input, verdict Verdict, output *liveOutput) (Verdict, error) {
	probe := input.Probe
	if probe.Service == "" || probe.Port == 0 {
		return verdict, nil
	}
	verdict.Reach = &Reach{Service: probe.Service, Said: notRun}
	if !verdict.Passed() {
		return verdict, nil
	}
	url := probeURL(probe)
	window := sandbox.probeWindow(verdict.Services, probe.Service)
	output.say("asking %s from another container on the sandbox network, for up to %s while nothing answers", url, window)
	runCtx, cancel := context.WithTimeout(ctx, window+pullAllowance)
	defer cancel()
	said, stderr, err := sandbox.compose(runCtx, directory, nil, name, "run", "--rm", "-T", probeService, "sh", "-c", probeScript, probeService,
		url, seconds(window), seconds(probeInterval), seconds(probeTimeout))
	if ctx.Err() != nil {
		return verdict, ctx.Err()
	}
	if err != nil {
		verdict.Reach.Said = "not checked: the probe did not run: " + lastLine(scrub(err.Error()+"\n"+string(stderr), input.Secrets))
		output.say("%s", verdict.Reach.Said)
		return verdict, nil
	}
	reach, failed := readReach(probe, url, scrub(string(said), input.Secrets))
	verdict.Reach = &reach
	output.say("%s", reach.Said)
	if failed {
		return fail(verdict, ReasonUnreachable, reach.Said), nil
	}
	return verdict, nil
}

// probeWindow is how long the app may refuse connections: briefly when its
// healthcheck passed, else as long as the run waits for health.
func (sandbox *Sandbox) probeWindow(states []ServiceState, service string) time.Duration {
	healthy := slices.ContainsFunc(states, func(state ServiceState) bool { return state.Service == service && state.Health == healthHealthy })
	if healthy {
		return probeGrace
	}
	return sandbox.timeout
}

// readReach reads what wget said the last time. Any HTTP status is an
// answer unless the probe expects particular ones. A refused or timed-out
// connection means nothing listens there. A name that does not resolve
// means the service has no address of its own (it shares another
// container's network), so there is nothing to check.
func readReach(probe ir.Probe, url, said string) (Reach, bool) {
	address := probe.Service + ":" + strconv.Itoa(probe.Port)
	reach := Reach{Service: probe.Service, Checked: true}
	status := 0
	if match := statusLine.FindStringSubmatch(said); match != nil {
		status, _ = strconv.Atoi(match[1])
	}
	refused := strings.Contains(said, refusedAnswer) || strings.Contains(said, timedOutAnswer)
	switch {
	case status > 0:
		reach.Said = fmt.Sprintf("%s answered %d", url, status)
	case refused:
		reach.Said = fmt.Sprintf("nothing answers on %s from the sandbox network; is it listening on 127.0.0.1 or on another port?", address)
		return reach, true
	case strings.Contains(said, unresolvedAnswer):
		reach.Checked, reach.Said = false, fmt.Sprintf("not checked: %s has no address of its own on the sandbox network", probe.Service)
		return reach, false
	default:
		reach.Said = fmt.Sprintf("%s took the connection but sent no HTTP answer (%s)", address, lastLine(said))
	}
	unexpected := len(probe.Expect) > 0 && !slices.Contains(probe.Expect, status)
	if unexpected {
		reach.Said += fmt.Sprintf("; the plan expects one of %v", probe.Expect)
	}
	return reach, unexpected
}

func probeURL(probe ir.Probe) string {
	path := probe.Path
	if path == "" {
		path = defaultProbePath
	}
	return httpScheme + probe.Service + ":" + strconv.Itoa(probe.Port) + path
}

func seconds(duration time.Duration) string {
	return strconv.Itoa(int(duration.Seconds()))
}
