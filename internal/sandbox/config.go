package sandbox

import (
	"cmp"
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
	return New(runner, limits, validation.Timeout, configuration.Docker.SandboxTier, projectsDir), nil
}
