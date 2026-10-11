package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "master.key")
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(path, key); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != key.ID || string(loaded.material) != string(key.material) {
		t.Fatal("loaded key differs from written key")
	}
	if err := WriteKeyFile(path, key); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second write: got %v, want ErrKeyExists", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != KeyFileMode {
		t.Fatalf("mode %o, want %o", info.Mode().Perm(), KeyFileMode)
	}
}

func TestLoadKeyFileRefusesLoosePermissions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "master.key")
	key, _ := NewKey()
	if err := WriteKeyFile(path, key); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		mode    os.FileMode
		refused bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o644, true}, {0o604, true}, {0o660, true}}
	for _, testCase := range cases {
		if err := os.Chmod(path, testCase.mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(path)
		if (err != nil) != testCase.refused {
			t.Errorf("mode %04o: err=%v, refused=%v", testCase.mode, err, testCase.refused)
		}
		if testCase.refused && !strings.Contains(err.Error(), "chmod 0600") {
			t.Errorf("mode %04o: error lacks the fix: %v", testCase.mode, err)
		}
	}
}

func TestLoadKeyFileRefusesSymlinkAndGarbage(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "real.key")
	key, _ := NewKey()
	if err := WriteKeyFile(target, key); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink accepted: %v", err)
	}
	garbage := filepath.Join(directory, "garbage.key")
	if err := os.WriteFile(garbage, []byte("not a key\n"), KeyFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(garbage); err == nil {
		t.Fatal("garbage key accepted")
	}
	short := filepath.Join(directory, "short.key")
	if err := os.WriteFile(short, []byte(keyHeader+"AAAA\n"), KeyFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(short); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestCheckPrivateDir(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(directory, KeyDirMode); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(directory); err != nil {
		t.Fatalf("0700 refused: %v", err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(directory); err == nil {
		t.Fatal("0755 accepted")
	}
}
