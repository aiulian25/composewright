package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// leasesDir holds one lease per run beside the projects; no project's
	// folder starts with a dot.
	leasesDir     = ".sandbox-leases"
	leasesDirMode = 0o700
	leaseFileMode = 0o600
)

// takeLease marks the run name as alive before it writes a file or asks
// Docker for anything: the run holds a lock on its lease until it ends, so
// the janitor of any process leaves it alone, and the lock goes with a
// process that dies.
func (sandbox *Sandbox) takeLease(name string) (*os.File, error) {
	if err := os.MkdirAll(sandbox.leases(), leasesDirMode); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(sandbox.leasePath(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, leaseFileMode)
	if err != nil {
		return nil, fmt.Errorf("lease for sandbox %s: %w", name, err)
	}
	if err := lockLease(file); err != nil {
		return nil, errors.Join(err, file.Close(), os.Remove(file.Name()))
	}
	return file, nil
}

// endLease lets the run go: once everything it owned is gone the lease is
// removed, else it is only unlocked, so a janitor finds the run and retries.
func endLease(file *os.File, removed bool) error {
	if !removed {
		return file.Close()
	}
	return errors.Join(os.Remove(file.Name()), file.Close())
}

// abandoned reports whether the run name has a lease no live run holds.
func (sandbox *Sandbox) abandoned(name string) bool {
	file, err := os.OpenFile(sandbox.leasePath(name), os.O_RDWR, leaseFileMode)
	if err != nil {
		return false
	}
	free := lockLease(file) == nil
	closeErr := file.Close()
	return free && closeErr == nil
}

// leased reports whether the run name has a lease at all.
func (sandbox *Sandbox) leased(name string) bool {
	_, err := os.Stat(sandbox.leasePath(name))
	return !errors.Is(err, fs.ErrNotExist)
}

// leaseNames are the runs that have a lease.
func (sandbox *Sandbox) leaseNames() []string {
	entries, _ := os.ReadDir(sandbox.leases())
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), projectPrefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func (sandbox *Sandbox) leases() string {
	return filepath.Join(sandbox.root, leasesDir)
}

func (sandbox *Sandbox) leasePath(name string) string {
	return filepath.Join(sandbox.leases(), name)
}

// lockLease takes the lease's lock without waiting; it fails while another
// open file, in this process or any other, holds it.
func lockLease(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // G115: a file descriptor fits an int
}
