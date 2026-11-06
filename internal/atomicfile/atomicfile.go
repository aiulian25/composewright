// Package atomicfile writes files so readers never see partial content and a
// file never exists, even briefly, with looser permissions than requested.
package atomicfile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

const (
	// temporaryFlags create a temporary file that never existed before.
	temporaryFlags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	// temporaryMarker names a temporary file beside the one it replaces.
	temporaryMarker = ".tmp-"
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

// WriteIn replaces name, a slash-separated path inside root, with data via a
// temporary file and a rename, as Write does. Every step resolves inside
// root, so a symbolic link in a folder on the way can never lead the file
// outside it: the write is refused instead.
func WriteIn(root *os.Root, name string, data []byte, mode os.FileMode) error {
	temporaryName, err := StageIn(root, name, data, mode)
	if err != nil {
		return err
	}
	if err := root.Rename(temporaryName, name); err != nil {
		return errors.Join(err, root.Remove(temporaryName))
	}
	return SyncIn(root, name)
}

// StageIn writes data, synced and with its mode, to a new temporary file
// beside name inside root and returns that file's name, for the caller to
// rename or link into place, or to remove.
func StageIn(root *os.Root, name string, data []byte, mode os.FileMode) (string, error) {
	folder, base := path.Split(name)
	temporaryName := folder + "." + base + temporaryMarker + rand.Text()
	temporary, err := root.OpenFile(temporaryName, temporaryFlags, mode)
	if err != nil {
		return "", fmt.Errorf("create temporary file for %s: %w", name, err)
	}
	if err := fill(temporary, data, mode); err != nil {
		return "", errors.Join(err, root.Remove(temporaryName))
	}
	return temporaryName, nil
}

// SyncIn makes the entries of the folder holding name, inside root,
// survive a crash.
func SyncIn(root *os.Root, name string) error {
	folder, _ := path.Split(name)
	handle, err := root.Open(path.Clean(folder + "."))
	if err != nil {
		return err
	}
	return syncHandle(handle)
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
	return syncHandle(handle)
}

func syncHandle(handle *os.File) error {
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
