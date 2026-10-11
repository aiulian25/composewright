// Package runas lets the container image start as root just long enough to
// make its data directory its own and join the Docker socket's group, then
// run as an unprivileged user. Docker creates a bind-mount folder that does
// not exist yet as root with mode 0755, which that user could neither use
// nor fix.
package runas

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// Variable names the uid:gid the image runs as. Only the image sets it,
	// so the binary behaves as before everywhere else.
	Variable              = "COMPOSEWRIGHT_RUN_AS"
	dataDirectoryVariable = "COMPOSEWRIGHT_DATA_DIR"
	dockerHostVariable    = "DOCKER_HOST"
	uidMapPath            = "/proc/self/uid_map"
	// identityMap is uid_map when every uid is itself: root here is root on
	// the host.
	identityMap          = "0 0 4294967295"
	targetSeparator      = ":"
	rootID               = 0
	unknownOwner         = -1
	privateDirectoryMode = 0o700
	groupOtherBits       = 0o077

	capabilitiesNeeded = "composewright starts as root only to make its data folder its own and join the Docker socket's group, then runs as uid %d; " +
		"that needs the CHOWN, SETGID and SETUID capabilities, which cap_drop: [ALL] removes. " +
		"Add cap_add: [CHOWN, SETGID, SETUID] to the service (docker run: --cap-add CHOWN --cap-add SETGID --cap-add SETUID)"
	foreignFolder = "%s belongs to uid %d and is not an empty folder, so composewright will not take it over: " +
		"mount an empty folder or a new volume there, or give this one to uid %d on the host with: sudo chown -R %d:%d <folder>"
)

// action is what happens to the data directory before the switch.
type action int

const (
	keep action = iota
	take
	refuse
)

// decide takes over an empty data directory, such as one Docker just
// created, and never one that holds someone else's files.
func decide(owner, target int, empty bool) action {
	if owner == target {
		return keep
	}
	if empty {
		return take
	}
	return refuse
}

func parseTarget(value string) (uid, gid int, err error) {
	user, group, found := strings.Cut(value, targetSeparator)
	uid, userErr := strconv.Atoi(user)
	gid, groupErr := strconv.Atoi(group)
	// Root is refused: the capabilities kept for the start would stay.
	valid := found && userErr == nil && groupErr == nil && uid > rootID && gid > rootID
	if !valid {
		return 0, 0, fmt.Errorf("%s=%q: want a uid:gid other than root's, such as 1000:1000", Variable, value)
	}
	return uid, gid, nil
}

// identityMapped reports whether uid_map maps every uid to itself. In a user
// namespace, as under rootless Docker, root here is already an unprivileged
// user on the host, the one who owns the folders it mounts.
func identityMapped(uidMap string) bool {
	return strings.Join(strings.Fields(uidMap), " ") == identityMap
}

// groupsFor keeps the groups the container was given, without root's, and
// adds the Docker socket's group, which can itself be root's (Synology).
func groupsFor(inherited []int, socketGroup int, socketFound bool) []int {
	groups := []int{}
	for _, group := range inherited {
		if group != rootID && !slices.Contains(groups, group) {
			groups = append(groups, group)
		}
	}
	if socketFound && !slices.Contains(groups, socketGroup) {
		groups = append(groups, socketGroup)
	}
	return groups
}
