// Package atomicfile writes files so readers never see partial content and a
// file never exists, even briefly, with looser permissions than requested.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Write replaces path with data via a temporary file and a rename.
func Write(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	name := temporary.Name()
	if err := fill(temporary, data, mode); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	if err := os.Rename(name, path); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	return syncDirectory(filepath.Dir(path))
}

// CreateExclusive writes a new file and fails if path already exists.
func CreateExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// A file cut short, by a full disk for example, would block the next
	// attempt, which refuses an existing file.
	if err := fill(file, data, mode); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return syncDirectory(filepath.Dir(path))
}

// syncDirectory makes a new or renamed entry in directory survive a crash.
// Some file systems cannot sync a directory and answer EINVAL; the entry
// stands there anyway.
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := handle.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return errors.Join(err, handle.Close())
	}
	return handle.Close()
}

func fill(file *os.File, data []byte, mode os.FileMode) error {
	if err := file.Chmod(mode); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}
