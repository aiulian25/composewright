package doctor

import (
	"context"
	"slices"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/sandbox"
)

// fewestToCount is how many apps must do something for it to count as the
// host's habit: one app is a choice, not a habit.
const fewestToCount = 2

// Habit is what most of the Docker host's apps do, with how many do it.
type Habit struct {
	Value string
	Apps  int
}

// Habits are how the apps already running on the Docker host are set up,
// for a new one to follow: the time zone they set, and the proxy or tunnel
// network most of them are reached through.
type Habits struct {
	TimeZone Habit
	Network  FoundNetwork
	// NetworkApps counts the apps on Network, its proxy left out.
	NetworkApps int
}

// HostHabits reads the habits off the running containers. Of each
// container only its networks, labels and TZ value are read.
func HostHabits(ctx context.Context, docker *sandbox.Sandbox) (Habits, error) {
	zones, err := docker.TimeZones(ctx)
	if err != nil {
		return Habits{}, err
	}
	host, err := ProxyNetworks(ctx, docker, config.Profile{})
	if err != nil {
		return Habits{}, err
	}
	containers, err := docker.Containers(ctx)
	if err != nil {
		return Habits{}, err
	}
	habits := Habits{TimeZone: commonest(zones)}
	habits.Network, habits.NetworkApps = busiestProxyNetwork(host.Owners, containers)
	return habits, nil
}

// commonest is the value most often given, the first by name on a tie,
// when enough apps give it to be a habit.
func commonest(values []string) Habit {
	counts := map[string]int{}
	for _, value := range values {
		counts[value]++
	}
	var best Habit
	for value, apps := range counts {
		better := apps > best.Apps || (apps == best.Apps && value < best.Value)
		if better {
			best = Habit{Value: value, Apps: apps}
		}
	}
	if best.Apps < fewestToCount {
		return Habit{}
	}
	return best
}

// busiestProxyNetwork is the network of a proxy or tunnel that most apps
// share, counting containers other than the proxy and sandbox runs.
func busiestProxyNetwork(owners map[string]FoundNetwork, containers []sandbox.Container) (FoundNetwork, int) {
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	slices.Sort(names)
	var best FoundNetwork
	bestApps := 0
	for _, name := range names {
		proxy := owners[name]
		apps := 0
		for _, container := range containers {
			isApp := container.Name != proxy.Container && container.Labels[sandbox.LabelProject] == ""
			if isApp && slices.Contains(container.Networks, name) {
				apps++
			}
		}
		if apps > bestApps {
			best, bestApps = proxy, apps
		}
	}
	if bestApps < fewestToCount {
		return FoundNetwork{}, 0
	}
	return best, bestApps
}

// LookupHabits reads the habits of the Docker host the configuration names.
func LookupHabits(ctx context.Context, configuration config.Config, projectsDir string) (Habits, error) {
	docker, err := sandbox.FromConfig(configuration, projectsDir)
	if err != nil {
		return Habits{}, err
	}
	return HostHabits(ctx, docker)
}
