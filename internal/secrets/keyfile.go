// Package secrets owns the key-encryption key, authenticated sealing of
// secret values and the generators that create them.
package secrets

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aiulian25/composewright/internal/atomicfile"
)

const (
	keyHeader      = "composewright-kek-v1:"
	keySize        = 32
	keyIDBytes     = 8
	KeyFileMode    = 0o600
	KeyDirMode     = 0o700
	groupOtherBits = 0o077
)

var ErrKeyExists = errors.New("key file already exists")

type Key struct {
	ID       string
	material []byte
}

func NewKey() (Key, error) {
	material := make([]byte, keySize)
	if _, err := rand.Read(material); err != nil {
		return Key{}, err
	}
	return keyFromMaterial(material), nil
}

func keyFromMaterial(material []byte) Key {
	digest := sha256.Sum256(material)
	return Key{ID: hex.EncodeToString(digest[:keyIDBytes]), material: material}
}

// WriteKeyFile creates path with mode 0600 and refuses to overwrite anything.
func WriteKeyFile(path string, key Key) error {
	if err := os.MkdirAll(filepath.Dir(path), KeyDirMode); err != nil {
		return err
	}
	encoded := keyHeader + base64.StdEncoding.EncodeToString(key.material) + "\n"
	err := atomicfile.CreateExclusive(path, []byte(encoded), KeyFileMode)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", path, ErrKeyExists)
	}
	return err
}

// LoadKeyFile reads the key and refuses to use it when anyone but the owner
// could read it, when it is a symlink, or when another user owns it.
func LoadKeyFile(path string) (Key, error) {
	if err := checkPrivate(path, false); err != nil {
		return Key{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err
	}
	encoded, found := bytes.CutPrefix(bytes.TrimSpace(content), []byte(keyHeader))
	if !found {
		return Key{}, fmt.Errorf("%s is not a composewright key file (missing %q header)", path, keyHeader)
	}
	material, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil || len(material) != keySize {
		return Key{}, fmt.Errorf("%s does not contain a %d-byte key", path, keySize)
	}
	return keyFromMaterial(material), nil
}

// CheckPrivateDir verifies a directory holding secrets is not reachable by
// group or others.
func CheckPrivateDir(path string) error {
	return checkPrivate(path, true)
}

func checkPrivate(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("%s must be a directory", path)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	if info.Mode().Perm()&groupOtherBits != 0 {
		return fmt.Errorf("%s has mode %04o, which group or others can access; run: chmod %04o %s",
			path, info.Mode().Perm(), privateMode(directory), path)
	}
	return checkOwner(path, info)
}

func privateMode(directory bool) os.FileMode {
	if directory {
		return KeyDirMode
	}
	return KeyFileMode
}
