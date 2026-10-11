package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const (
	statusNoSocket     = "Docker socket not mounted"
	statusNoAccess     = "No access to the Docker socket"
	statusNotAnswering = "Docker not answering"
	statusUnreachable  = "Docker unreachable"

	fixMount = "There is no Docker socket at %s inside the container. Mount the host's socket: add " +
		"- /var/run/docker.sock:%s under the service's volumes (with docker run: -v /var/run/docker.sock:%s)."
	fixInstall         = "There is no Docker socket at %s: install Docker Engine and start it, or point docker.host at your daemon."
	fixAccessContainer = "composewright cannot open %s, which belongs to group %d. Start the image as it ships, as root with " +
		"cap_add: [CHOWN, SETGID, SETUID], so that it joins the group by itself, or add group_add: [\"%d\"]."
	fixAccessHost   = "composewright cannot open %s, which belongs to group %d: add your user to that group (usually docker) and sign in again."
	fixNotAnswering = "%s is there, but no Docker daemon answers on it: check that Docker is running."
	fixRemote       = "Nothing answers at %s: check that the daemon or socket proxy there is running and reachable."
)

// containerMarkers are the files Docker and Podman put in every container.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// Unreachable says why the Docker daemon did not answer and what to change,
// for the status bar, Home and doctor.
type Unreachable struct {
	Status string
	Fix    string
}

// Diagnose looks at the socket a host names to tell a socket that is not
// there from one that cannot be opened and from one nothing answers on. A
// host that is not a Unix socket, such as tcp://, is only named.
func Diagnose(host string) Unreachable {
	socket := SocketPath(host)
	if socket == "" {
		return Unreachable{Status: statusUnreachable, Fix: fmt.Sprintf(fixRemote, host)}
	}
	info, err := os.Stat(socket)
	if errors.Is(err, fs.ErrNotExist) {
		return Unreachable{Status: statusNoSocket, Fix: missingSocketFix(socket)}
	}
	if err == nil && !canOpen(socket) {
		return Unreachable{Status: statusNoAccess, Fix: noAccessFix(socket, groupOf(info))}
	}
	return Unreachable{Status: statusNotAnswering, Fix: fmt.Sprintf(fixNotAnswering, socket)}
}

func missingSocketFix(socket string) string {
	if inContainer() {
		return fmt.Sprintf(fixMount, socket, socket, socket)
	}
	return fmt.Sprintf(fixInstall, socket)
}

func noAccessFix(socket string, group int) string {
	if inContainer() {
		return fmt.Sprintf(fixAccessContainer, socket, group, group)
	}
	return fmt.Sprintf(fixAccessHost, socket, group)
}

func inContainer() bool {
	for _, marker := range containerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}
