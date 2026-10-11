package sandbox

import (
	"cmp"
	"os"

	"github.com/aiulian25/composewright/internal/config"
)

// DockerHost is where the docker CLI connects: docker.host, else
// DOCKER_HOST; empty leaves it to the docker context.
func DockerHost(configured string) string {
	return cmp.Or(configured, os.Getenv(dockerHostVariable))
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
