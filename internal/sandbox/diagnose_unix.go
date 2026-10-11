//go:build unix

package sandbox

import (
	"io/fs"
	"syscall"
)

// accessReadWrite is access(2)'s R_OK|W_OK: what talking to a socket needs.
const accessReadWrite = 0x4 | 0x2

func canOpen(socket string) bool {
	return syscall.Access(socket, accessReadWrite) == nil
}

func groupOf(info fs.FileInfo) int {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return unknownGroup
	}
	return int(status.Gid)
}
