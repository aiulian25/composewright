package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

const privateMode = 0o600

func readFile(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content), info.Mode().Perm()
}

// TestWriteReplacesContentAndMode: the new content arrives whole, with the
// mode asked for, and no temporary file is left beside it.
func TestWriteReplacesContentAndMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), privateMode); err != nil {
		t.Fatal(err)
	}
	if content, mode := readFile(t, path); content != "new" || mode != privateMode {
		t.Fatalf("content %q mode %o, want new and %o", content, mode, privateMode)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the file: %v", len(entries), err)
	}
}

// TestCreateExclusiveRefusesAnExistingFile: an existing file is reported
// with fs.ErrExist and left as it was.
func TestCreateExclusiveRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("kept"), privateMode); err != nil {
		t.Fatal(err)
	}
	if err := CreateExclusive(path, []byte("other"), privateMode); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing file: %v, want fs.ErrExist", err)
	}
	if content, _ := readFile(t, path); content != "kept" {
		t.Fatalf("the existing file was changed to %q", content)
	}
}

// TestCreateExclusiveWritesTheMode: a new file gets its content and the mode
// asked for, whatever the umask.
func TestCreateExclusiveWritesTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := CreateExclusive(path, []byte("key"), privateMode); err != nil {
		t.Fatal(err)
	}
	if content, mode := readFile(t, path); content != "key" || mode != privateMode {
		t.Fatalf("content %q mode %o, want key and %o", content, mode, privateMode)
	}
}

// TestCreateExclusiveRemovesAFileCutShort: when writing fails part way, as
// on a full disk, nothing is left behind to block the next attempt. A file
// size limit makes the write fail; Go ignores the SIGXFSZ it raises.
func TestCreateExclusiveRemovesAFileCutShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	const allowedBytes = 4
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: allowedBytes, Max: limit.Max}); err != nil {
		t.Skipf("cannot lower the file size limit: %v", err)
	}
	err := CreateExclusive(path, []byte("a key longer than the limit"), privateMode)
	if restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("a write past the file size limit succeeded")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a file cut short was left behind: %v", statErr)
	}
	if err := CreateExclusive(path, []byte("key"), privateMode); err != nil {
		t.Fatalf("the next attempt is blocked: %v", err)
	}
}
