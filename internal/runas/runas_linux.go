//go:build linux

package runas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/aiulian25/composewright/internal/sandbox"
)

// Prepare runs first. As root with Variable set, it makes the data directory
// the target user's, takes the Docker socket's group and becomes that user,
// which leaves no capabilities; otherwise it does nothing.
func Prepare() error {
	value, set := os.LookupEnv(Variable)
	if !set || os.Geteuid() != rootID {
		return nil
	}
	uid, gid, err := parseTarget(value)
	if err != nil {
		return err
	}
	if uidMap, err := os.ReadFile(uidMapPath); err == nil && !identityMapped(string(uidMap)) {
		uid, gid = rootID, rootID
	}
	directory := os.Getenv(dataDirectoryVariable)
	if err := claim(directory, uid, gid); err != nil {
		return err
	}
	if uid != rootID {
		if err := become(uid, gid); err != nil {
			return err
		}
	}
	return tighten(directory)
}

func claim(directory string, uid, gid int) error {
	if directory == "" {
		return nil
	}
	info, err := os.Lstat(directory) //nolint:gosec // G703: the image's own COMPOSEWRIGHT_DATA_DIR, not request input
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return err
	}
	owner := ownerOf(info)
	entries, readErr := os.ReadDir(directory)
	empty := readErr == nil && len(entries) == 0
	switch decide(owner, uid, empty) {
	case keep:
		return nil
	case refuse:
		return fmt.Errorf(foreignFolder, directory, owner, uid, uid, gid)
	}
	return withCapabilityHint(os.Chown(directory, uid, gid), uid) //nolint:gosec // G703: the same data directory
}

func become(uid, gid int) error {
	inherited, err := os.Getgroups()
	if err != nil {
		return err
	}
	socketGroup, socketFound := dockerSocketGroup()
	if err := syscall.Setgroups(groupsFor(inherited, socketGroup, socketFound)); err != nil {
		return withCapabilityHint(err, uid)
	}
	if err := syscall.Setgid(gid); err != nil {
		return withCapabilityHint(err, uid)
	}
	return withCapabilityHint(syscall.Setuid(uid), uid)
}

func dockerSocketGroup() (int, bool) {
	path := sandbox.SocketPath(os.Getenv(dockerHostVariable))
	if path == "" {
		return 0, false
	}
	info, err := os.Stat(path) //nolint:gosec // G703: the socket DOCKER_HOST names, set by whoever runs the container
	if err != nil {
		return 0, false
	}
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(status.Gid), true
}

// tighten closes the data directory to group and others once it is ours,
// as composewright requires.
func tighten(directory string) error {
	if directory == "" {
		return nil
	}
	info, err := os.Lstat(directory) //nolint:gosec // G703: the image's own COMPOSEWRIGHT_DATA_DIR, not request input
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ours := info.IsDir() && ownerOf(info) == os.Geteuid()
	open := info.Mode().Perm()&groupOtherBits != 0
	if !ours || !open {
		return nil
	}
	return os.Chmod(directory, privateDirectoryMode) //nolint:gosec // G703: the same data directory
}

func ownerOf(info fs.FileInfo) int {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return unknownOwner
	}
	return int(status.Uid)
}

func withCapabilityHint(err error, uid int) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf(capabilitiesNeeded+": %w", uid, err)
	}
	return err
}
