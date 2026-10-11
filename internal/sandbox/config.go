package sandbox

import "github.com/aiulian25/composewright/internal/config"

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
