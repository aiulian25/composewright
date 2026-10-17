package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/sandbox"
)

const (
	// containerListTimeout bounds asking Docker which host ports running
	// containers bind: the answer only adds a warning.
	containerListTimeout = 3 * time.Second
	publishedByProject   = "published by project %s (v%d)"
	boundByContainer     = "bound by running container %s on this Docker host"
	portKeySeparator     = ":"
	portChoiceSeparator  = "="
	// portChoiceProblem is shown on the Review page and by gen alike.
	portChoiceProblem = "the port %q must be SERVICE:CONTAINER=HOST, as app:8080=8081"
	unmatchedPortNote = "the host port chosen for %s was not set: the plan has no such port"
	chosenPortSource  = "chosen host port"
)

// chosenPort marks a host port a person chose, kept in Options.Ports.
var chosenPort = ir.Provenance{Source: ir.SourceUser, SourceRef: chosenPortSource, Confidence: ir.ConfidenceHigh}

// ErrUnknownPort means a host port was chosen for a port the plan lacks.
var ErrUnknownPort = errors.New("the plan has no such port")

// PortChoice is the text that chooses host for a service's container port,
// as app:8080=8081; ParsePortChoice reads it back.
func PortChoice(service string, container, host int) string {
	return portKey(service, container) + portChoiceSeparator + strconv.Itoa(host)
}

// ParsePortChoice reads SERVICE:CONTAINER=HOST into the key Options.Ports
// uses and the host port.
func ParsePortChoice(text string) (key string, host int, err error) {
	key, hostText, found := strings.Cut(text, portChoiceSeparator)
	service, containerText, keyed := strings.Cut(key, portKeySeparator)
	container, containerErr := strconv.Atoi(containerText)
	host, hostErr := strconv.Atoi(hostText)
	valid := found && keyed && service != "" && containerErr == nil && hostErr == nil && host >= 1 && host <= composefile.MaxPort
	if !valid {
		return "", 0, fmt.Errorf(portChoiceProblem, text)
	}
	return portKey(service, container), host, nil
}

func portKey(service string, container int) string {
	return service + portKeySeparator + strconv.Itoa(container)
}

// chosenPorts are the kept choices with the new ones over them.
func chosenPorts(kept, chosen map[string]int) map[string]int {
	if len(chosen) == 0 {
		return kept
	}
	ports := map[string]int{}
	maps.Copy(ports, kept)
	maps.Copy(ports, chosen)
	return ports
}

// setHostPorts publishes each chosen container port on its chosen host
// port, and returns the choices that match no port of the plan.
func setHostPorts(plan *ir.IR, ports map[string]int, provenance ir.Provenance) []string {
	var unmatched []string
	for key, host := range ports {
		if !setHostPort(plan, key, host, provenance) {
			unmatched = append(unmatched, key)
		}
	}
	slices.Sort(unmatched)
	return unmatched
}

func setHostPort(plan *ir.IR, key string, host int, provenance ir.Provenance) bool {
	matched := false
	for serviceIndex := range plan.Services {
		service := &plan.Services[serviceIndex]
		for portIndex := range service.Ports {
			port := &service.Ports[portIndex]
			if portKey(service.Name, port.Container) == key {
				port.HostPort, port.Provenance, matched = host, provenance, true
			}
		}
	}
	return matched
}

// takenPorts are the host ports other projects' latest versions publish
// and running containers outside the project bind, each with what takes
// it. Both are best effort: a store or a Docker daemon that cannot answer
// leaves out a warning, never blocks a plan.
func takenPorts(ctx context.Context, dependencies Dependencies, name string) map[int][]string {
	taken := map[int][]string{}
	for _, published := range otherProjectsPorts(ctx, dependencies, name) {
		taken[published.port] = append(taken[published.port], published.description)
	}
	for _, container := range runningContainers(ctx, dependencies.Sandbox, name) {
		for _, port := range container.Ports {
			taken[port] = append(taken[port], fmt.Sprintf(boundByContainer, container.Name))
		}
	}
	return taken
}

type takenPort struct {
	port        int
	description string
}

func otherProjectsPorts(ctx context.Context, dependencies Dependencies, name string) []takenPort {
	if dependencies.Workspace == nil {
		return nil
	}
	published, err := dependencies.Workspace.Store.PublishedPorts(ctx)
	if err != nil {
		return nil
	}
	var taken []takenPort
	for _, port := range published {
		if port.Project != name {
			taken = append(taken, takenPort{port: port.Port, description: fmt.Sprintf(publishedByProject, port.Project, port.Version)})
		}
	}
	return taken
}

// runningContainers are the running containers that are neither the
// project's own, deployed as a Compose project of its name, nor a sandbox.
func runningContainers(ctx context.Context, runner *sandbox.Sandbox, name string) []sandbox.Container {
	if runner == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, containerListTimeout)
	defer cancel()
	containers, err := runner.Containers(ctx)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(containers, func(container sandbox.Container) bool {
		_, isSandbox := container.Labels[sandbox.LabelProject]
		return isSandbox || container.Labels[sandbox.LabelComposeProject] == name
	})
}
