// Package workspace opens the local state (config, data directory, master key
// and database) and refuses to run when any of it is readable by others.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiulian25/composewright/internal/atomicfile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/secrets"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	retiredDirName = "retired"
	nextKeySuffix  = ".next"
	keyExtension   = ".key"
)

var ErrNotInitialised = errors.New("composewright is not initialised")

type Workspace struct {
	Config config.Config
	Paths  config.Paths
	Key    secrets.Key
	Store  *store.Store
	Actor  string
}

type InitResult struct {
	Paths         config.Paths
	KeyID         string
	KeyCreated    bool
	ConfigCreated bool
}

// Init creates whatever is missing and validates whatever already exists.
func Init(ctx context.Context, loaded *config.Loaded) (InitResult, error) {
	paths := loaded.Paths
	result := InitResult{Paths: paths}
	if err := ensurePrivateDir(paths.DataDir); err != nil {
		return result, err
	}
	key, created, err := ensureKey(paths)
	if err != nil {
		return result, err
	}
	result.KeyID, result.KeyCreated = key.ID, created
	if !loaded.FileExists {
		if err := writeDefaultConfig(paths.ConfigFile); err != nil {
			return result, err
		}
		result.ConfigCreated = true
	}
	workspace, err := Open(ctx, loaded)
	if err != nil {
		return result, err
	}
	defer workspace.Close()
	if !created {
		return result, nil
	}
	return result, workspace.Store.Audit(ctx, workspace.Actor, store.ActionInit, "", map[string]any{"key_id": key.ID})
}

// Open loads the state every command needs and enforces the permission
// rules: private data directory, private key directory, 0600 key files.
func Open(ctx context.Context, loaded *config.Loaded) (*Workspace, error) {
	paths := loaded.Paths
	if err := finishInterruptedSwap(paths.KeyFile); err != nil {
		return nil, err
	}
	if _, err := os.Stat(paths.KeyFile); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no key at %s; run `composewright init`", ErrNotInitialised, paths.KeyFile)
	}
	if err := secrets.CheckPrivateDir(paths.DataDir); err != nil {
		return nil, err
	}
	if paths.DefaultKeyFile() {
		if err := secrets.CheckPrivateDir(paths.KeysDir()); err != nil {
			return nil, err
		}
	}
	key, err := secrets.LoadKeyFile(paths.KeyFile)
	if err != nil {
		return nil, err
	}
	retired, err := loadRetiredKeys(paths.KeyFile)
	if err != nil {
		return nil, err
	}
	database, err := store.Open(ctx, paths.Database(), paths.BackupsDir(), secrets.NewSealer(key, retired...))
	if err != nil {
		return nil, err
	}
	if err := database.RecordKey(ctx, key.ID); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return &Workspace{Config: loaded.Config, Paths: paths, Key: key, Store: database, Actor: actor()}, nil
}

func (workspace *Workspace) Close() error {
	return workspace.Store.Close()
}

// RotationResult describes a completed key rotation.
type RotationResult struct {
	OldKeyID string `json:"old_key_id"`
	NewKeyID string `json:"new_key_id"`
	Resealed int    `json:"resealed"`
	Retired  string `json:"retired_key_file"`
}

// RotateKey writes a new key next to the current one, re-seals every value
// in one transaction, then swaps the files. The old key is kept under
// keys/retired/ until `keys purge`, so an older backup stays restorable.
// An interrupted rotation is resumed with the key it left behind.
func (workspace *Workspace) RotateKey(ctx context.Context) (RotationResult, error) {
	keyFile := workspace.Paths.KeyFile
	nextFile := keyFile + nextKeySuffix
	next, created, err := nextKey(nextFile)
	if errors.Is(err, fs.ErrPermission) {
		return RotationResult{}, fmt.Errorf("write the new key (is %s writable?): %w", filepath.Dir(keyFile), err)
	}
	if err != nil {
		return RotationResult{}, err
	}
	resealed, err := workspace.Store.ResealAll(ctx, next)
	// Only a key made by this call may go: one left by an interrupted
	// rotation can already seal every value.
	if err != nil && created {
		return RotationResult{}, errors.Join(err, os.Remove(nextFile))
	}
	if err != nil {
		return RotationResult{}, err
	}
	retiredDir := filepath.Join(filepath.Dir(keyFile), retiredDirName)
	if err := os.MkdirAll(retiredDir, secrets.KeyDirMode); err != nil {
		return RotationResult{}, err
	}
	retiredFile := filepath.Join(retiredDir, workspace.Key.ID+keyExtension)
	if err := os.Rename(keyFile, retiredFile); err != nil {
		return RotationResult{}, err
	}
	if err := os.Rename(nextFile, keyFile); err != nil {
		return RotationResult{}, err
	}
	result := RotationResult{OldKeyID: workspace.Key.ID, NewKeyID: next.ID, Resealed: resealed, Retired: retiredFile}
	workspace.Key = next
	return result, workspace.Store.Audit(ctx, workspace.Actor, store.ActionRotate, "",
		map[string]any{"old_key_id": result.OldKeyID, "new_key_id": result.NewKeyID, "resealed": resealed})
}

type KeyStatus struct {
	CurrentKeyID string         `json:"current_key_id"`
	KeyFile      string         `json:"key_file"`
	Retired      []string       `json:"retired_key_ids"`
	Usage        map[string]int `json:"sealed_values_by_key"`
	Pending      bool           `json:"rotation_incomplete"`
}

func (workspace *Workspace) KeyStatus(ctx context.Context) (KeyStatus, error) {
	usage, err := workspace.Store.KeyUsage(ctx)
	if err != nil {
		return KeyStatus{}, err
	}
	retired, err := loadRetiredKeys(workspace.Paths.KeyFile)
	if err != nil {
		return KeyStatus{}, err
	}
	status := KeyStatus{CurrentKeyID: workspace.Key.ID, KeyFile: workspace.Paths.KeyFile, Usage: usage}
	for _, key := range retired {
		status.Retired = append(status.Retired, key.ID)
	}
	_, err = os.Stat(workspace.Paths.KeyFile + nextKeySuffix)
	status.Pending = err == nil
	return status, nil
}

// PurgeRetiredKeys deletes retired key files once no stored value needs them.
func (workspace *Workspace) PurgeRetiredKeys(ctx context.Context) ([]string, error) {
	usage, err := workspace.Store.KeyUsage(ctx)
	if err != nil {
		return nil, err
	}
	retired, err := retiredKeyFiles(workspace.Paths.KeyFile)
	if err != nil {
		return nil, err
	}
	var purged []string
	for _, file := range retired {
		keyID := filepath.Base(file[:len(file)-len(keyExtension)])
		if usage[keyID] > 0 {
			return purged, fmt.Errorf("retired key %s still protects %d values; run `composewright keys rotate` first", keyID, usage[keyID])
		}
		if err := os.Remove(file); err != nil {
			return purged, err
		}
		purged = append(purged, keyID)
	}
	return purged, workspace.Store.Audit(ctx, workspace.Actor, store.ActionPurgeKeys, "", map[string]any{"purged": purged})
}

// loadRetiredKeys returns keys under keys/retired/ plus a pending .next key
// left behind by an interrupted rotation, so no sealed value becomes
// unreadable.
func loadRetiredKeys(keyFile string) ([]secrets.Key, error) {
	files, err := retiredKeyFiles(keyFile)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(keyFile + nextKeySuffix); err == nil {
		files = append(files, keyFile+nextKeySuffix)
	}
	keys := make([]secrets.Key, 0, len(files))
	for _, file := range files {
		key, err := secrets.LoadKeyFile(file)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// nextKey is the key a rotation re-seals with: the one an interrupted
// rotation left behind, since values may already be sealed with it, or a
// new one. created says which.
func nextKey(nextFile string) (key secrets.Key, created bool, err error) {
	key, err = secrets.LoadKeyFile(nextFile)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return secrets.Key{}, false, err
	}
	if key, err = secrets.NewKey(); err != nil {
		return secrets.Key{}, false, err
	}
	return key, true, secrets.WriteKeyFile(nextFile, key)
}

// finishInterruptedSwap completes a rotation that stopped between its two
// renames: the old key is retired already, and every value is sealed with
// the next one, because ResealAll committed before the first rename.
func finishInterruptedSwap(keyFile string) error {
	if _, err := os.Stat(keyFile); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	nextFile := keyFile + nextKeySuffix
	_, err := os.Stat(nextFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Rename(nextFile, keyFile)
}

func retiredKeyFiles(keyFile string) ([]string, error) {
	return filepath.Glob(filepath.Join(filepath.Dir(keyFile), retiredDirName, "*"+keyExtension))
}

func actor() string {
	name := os.Getenv("USER")
	if current, err := user.Current(); err == nil {
		name = current.Username
	}
	return "cli:" + name + " (uid " + strconv.Itoa(os.Geteuid()) + ")"
}

func ensurePrivateDir(path string) error {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, config.DirMode); err != nil {
			return err
		}
		return os.Chmod(path, config.DirMode)
	}
	if err != nil {
		return err
	}
	return secrets.CheckPrivateDir(path)
}

func ensureKey(paths config.Paths) (secrets.Key, bool, error) {
	if paths.DefaultKeyFile() {
		if err := ensurePrivateDir(paths.KeysDir()); err != nil {
			return secrets.Key{}, false, err
		}
	}
	if err := finishInterruptedSwap(paths.KeyFile); err != nil {
		return secrets.Key{}, false, err
	}
	key, err := secrets.LoadKeyFile(paths.KeyFile)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return secrets.Key{}, false, err
	}
	key, err = secrets.NewKey()
	if err != nil {
		return secrets.Key{}, false, err
	}
	return key, true, secrets.WriteKeyFile(paths.KeyFile, key)
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), config.DirMode); err != nil {
		return err
	}
	return atomicfile.CreateExclusive(path, config.DefaultTemplate, config.FileMode)
}

// ResolveSecret reads a secret reference from the config: keyring:<name>
// is a sealed setting in the database, env:<VARIABLE> an environment
// variable. An empty reference resolves to an empty value.
func (workspace *Workspace) ResolveSecret(ctx context.Context, reference string) (string, error) {
	if name, found := strings.CutPrefix(reference, config.SecretRefKeyring); found {
		value, _, err := workspace.Store.Secret(ctx, name)
		return value, err
	}
	return ResolveEnvironmentSecret(reference), nil
}

// ResolveEnvironmentSecret resolves env:<VARIABLE> references, the only
// kind usable before the workspace is initialised.
func ResolveEnvironmentSecret(reference string) string {
	name, found := strings.CutPrefix(reference, config.SecretRefEnv)
	if !found {
		return ""
	}
	return os.Getenv(name)
}
