package sandbox

import (
	"cmp"
	"context"
	"os"
	"strings"

	"github.com/aiulian25/composewright/internal/config"
)

// DockerHost is where the docker CLI connects: docker.host, else
// DOCKER_HOST; empty leaves it to the docker context.
func DockerHost(configured string) string {
	return cmp.Or(configured, os.Getenv(dockerHostVariable))
}

// SocketPath is the Unix socket a Docker host names: the path of a unix://
// host, or Docker's default socket when no host is set. Other hosts, such as
// tcp://, have none.
func SocketPath(host string) string {
	if host == "" {
		return defaultSocket
	}
	path, isUnix := strings.CutPrefix(host, unixScheme)
	if !isUnix {
		return ""
	}
	return path
}

// Sweep runs the janitor of the sandbox the configuration describes: what
// runs that ended without cleaning up left is removed, a live run's kept.
func Sweep(ctx context.Context, configuration config.Config, projectsDir string) ([]string, error) {
	sandbox, err := FromConfig(configuration, projectsDir)
	if err != nil {
		return nil, err
	}
	return sandbox.Janitor(ctx)
}

// FromConfig builds the sandbox the configuration describes: its docker CLI,
// daemon, limits, timeout and tier. Runs live under projectsDir.
func FromConfig(configuration config.Config, projectsDir string) (*Sandbox, error) {
	docker, err := FindDocker(configuration.Docker.CLIPath)
	if err != nil {
		return nil, err
	}
	validation := configuration.Docker.Validate
	limits, err := ParseLimits(validation.MemoryLimit, validation.CPUs, validation.PIDsLimit)
	if err != nil {
		return nil, err
	}
	runner := ExecRunner(docker, configuration.Docker.Host)
	sandbox := New(runner, limits, validation.Timeout, configuration.Docker.SandboxTier, projectsDir)
	sandbox.platform = configuration.Profile.Platform
	return sandbox, nil
}
