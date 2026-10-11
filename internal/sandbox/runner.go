// Package sandbox runs a rendered stack in a throw-away Compose project and
// judges it. It is the only package that starts processes.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	// stdoutLimit and stderrLimit cap what a docker command's output keeps:
	// its last bytes, where a failing service explains itself.
	stdoutLimit        = 1 << 20
	stderrLimit        = 256 << 10
	terminationGrace   = 10 * time.Second
	dockerHostVariable = "DOCKER_HOST"
	defaultSocket      = "/var/run/docker.sock"
	unixScheme         = "unix://"
	unknownGroup       = -1
)

// passedEnvironment is all the docker CLI sees of ours. Anything else, such
// as a POSTGRES_PASSWORD in the user's shell, would override .env values
// during Compose interpolation.
var passedEnvironment = []string{
	"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", dockerHostVariable, "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
}

// Runner runs one docker CLI invocation in directory and returns its output.
// When live is not nil, it also receives the output as it is written.
type Runner func(ctx context.Context, directory string, live io.Writer, arguments ...string) (stdout, stderr []byte, err error)

// ExecRunner runs the docker CLI found at path with a minimal environment,
// in its own process group so a cancelled run never leaves children behind.
func ExecRunner(path, dockerHost string) Runner {
	environment := []string{"COMPOSE_MENU=0", "COMPOSE_ANSI=never"}
	configuredHost := dockerHost != ""
	for _, name := range passedEnvironment {
		value, found := os.LookupEnv(name)
		replaced := name == dockerHostVariable && configuredHost
		if !found || replaced {
			continue
		}
		environment = append(environment, name+"="+value)
	}
	if configuredHost {
		environment = append(environment, dockerHostVariable+"="+dockerHost)
	}
	return func(ctx context.Context, directory string, live io.Writer, arguments ...string) ([]byte, []byte, error) {
		command := exec.CommandContext(ctx, path, arguments...)
		command.Dir, command.Env = directory, environment
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
		command.WaitDelay = terminationGrace
		stdout := &limitedBuffer{limit: stdoutLimit}
		stderr := &limitedBuffer{limit: stderrLimit}
		command.Stdout, command.Stderr = stdout, stderr
		if live != nil {
			command.Stdout, command.Stderr = io.MultiWriter(stdout, live), io.MultiWriter(stderr, live)
		}
		err := command.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
}

// FindDocker resolves the docker CLI: the configured path, else PATH.
func FindDocker(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", errors.New("the docker CLI was not found on PATH; install Docker or set docker.cli_path")
	}
	return path, nil
}

// limitedBuffer keeps the last limit bytes and drops what came before, so a
// chatty container cannot exhaust memory and the end of a log, which
// explains a failure, is what remains.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) >= buffer.limit {
		buffer.Reset()
		buffer.Buffer.Write(data[len(data)-buffer.limit:])
		return len(data), nil
	}
	if overflow := buffer.Len() + len(data) - buffer.limit; overflow > 0 {
		buffer.Next(overflow)
	}
	buffer.Buffer.Write(data)
	return len(data), nil
}
