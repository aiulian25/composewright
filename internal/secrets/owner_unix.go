//go:build unix

package secrets

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func checkOwner(path string, info fs.FileInfo) error {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(status.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by the current user (uid %d)", path, status.Uid, os.Geteuid())
	}
	return nil
}
