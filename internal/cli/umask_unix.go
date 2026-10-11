//go:build unix

package cli

import "syscall"

const privateUmask = 0o077

// restrictUmask makes every file and directory the process creates private
// by default, including those SQLite and child processes create.
func restrictUmask() {
	syscall.Umask(privateUmask)
}
