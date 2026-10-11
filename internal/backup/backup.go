// Package backup writes and reads a composewright backup: the Settings
// page's choices with the model keys they use, and every project with its
// versions and .env, in one file encrypted with a passphrase. The master
// key never leaves the install, so a backup restores on any install.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/aiulian25/composewright/internal/auth"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	formatVersion = 1
	magic         = "composewright backup 1"
	lineBreak     = "\n"
	headerLines   = 3
	saltBytes     = 16
	keyBytes      = 32
	// The key derivation is fixed for format 1, so a crafted file cannot ask
	// for more memory or time than these.
	kdfPasses    = 3
	kdfMemoryKiB = 64 * 1024
	kdfLanes     = 1
	// maxPayloadBytes caps what a decompressed backup may hold.
	maxPayloadBytes = 256 << 20
	// FileExtension names backup files.
	FileExtension = ".cwbackup"
	// PassphraseName is the stored secret every backup is encrypted with. No
	// backup may carry it as a model key, so a restore cannot replace it.
	PassphraseName = "backup-passphrase"
)

var (
	ErrNotABackup      = errors.New("this file is not a composewright backup")
	ErrWrongPassphrase = errors.New("the passphrase is wrong, or the file was changed")
	errDamaged         = fmt.Errorf("the backup is damaged or holds more than %d MB", maxPayloadBytes>>20)
)

// Archive is what a backup holds. Keys are the stored model API keys the
// settings name, by their keyring name.
type Archive struct {
	Format   int                   `json:"format"`
	Created  time.Time             `json:"created"`
	Version  string                `json:"version"`
	Settings Settings              `json:"settings"`
	Keys     map[string]string     `json:"keys"`
	Projects []store.ProjectRecord `json:"projects"`
}

// Settings are the choices the Settings page saves. The rest of the
// configuration belongs to the install: paths, how Docker is reached, the
// sandbox tier and the command-line options.
type Settings struct {
	Profile        config.Profile `json:"profile"`
	LLM            config.LLM     `json:"llm"`
	Registries     []string       `json:"registries"`
	PrivateTargets []string       `json:"private_targets"`
	Theme          string         `json:"theme"`
}

// Apply puts the Settings choices into configuration.
func (settings Settings) Apply(configuration config.Config) config.Config {
	configuration.Profile, configuration.LLM, configuration.UI.Theme = settings.Profile, settings.LLM, settings.Theme
	configuration.Registries.Allow, configuration.Network.AllowPrivateTargets = settings.Registries, settings.PrivateTargets
	return configuration
}

// Collect gathers an archive from the configuration and the store.
func Collect(ctx context.Context, database *store.Store, configuration config.Config, version string, now time.Time) (Archive, error) {
	archive := Archive{Format: formatVersion, Created: now.UTC(), Version: version, Keys: map[string]string{},
		Settings: Settings{Profile: configuration.Profile, LLM: configuration.LLM, Registries: configuration.Registries.Allow,
			PrivateTargets: configuration.Network.AllowPrivateTargets, Theme: configuration.UI.Theme}}
	for _, name := range keyNames(configuration.LLM) {
		value, found, err := database.Secret(ctx, name)
		if err != nil {
			return Archive{}, err
		}
		if found {
			archive.Keys[name] = value
		}
	}
	var err error
	archive.Projects, err = database.ProjectRecords(ctx)
	return archive, err
}

// keyNames are the stored secrets the model settings refer to.
func keyNames(llm config.LLM) []string {
	references := []string{llm.APIKeyRef}
	for _, connection := range llm.Connections {
		references = append(references, connection.APIKeyRef)
	}
	var names []string
	for _, reference := range references {
		name, stored := strings.CutPrefix(reference, config.SecretRefKeyring)
		if stored && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// Seal encodes, compresses and encrypts an archive with a key derived from
// the passphrase.
func Seal(archive Archive, passphrase string) ([]byte, error) {
	if err := auth.CheckPassword(passphrase); err != nil {
		return nil, err
	}
	var payload bytes.Buffer
	compressor := gzip.NewWriter(&payload)
	if err := json.NewEncoder(compressor).Encode(archive); err != nil {
		return nil, err
	}
	if err := compressor.Close(); err != nil {
		return nil, err
	}
	salt, nonce := make([]byte, saltBytes), make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealer, err := chacha20poly1305.NewX(deriveKey(passphrase, salt))
	if err != nil {
		return nil, err
	}
	head := header(salt, nonce)
	return sealer.Seal([]byte(head), nonce, payload.Bytes(), []byte(head)), nil
}

// Open decrypts a backup and checks what it holds before anything is
// restored from it.
func Open(content []byte, passphrase string) (Archive, error) {
	parts := bytes.SplitN(content, []byte(lineBreak), headerLines+1)
	if len(parts) != headerLines+1 || string(parts[0]) != magic {
		return Archive{}, ErrNotABackup
	}
	salt, saltErr := base64.StdEncoding.DecodeString(string(parts[1]))
	nonce, nonceErr := base64.StdEncoding.DecodeString(string(parts[2]))
	if saltErr != nil || nonceErr != nil || len(salt) != saltBytes || len(nonce) != chacha20poly1305.NonceSizeX {
		return Archive{}, ErrNotABackup
	}
	opener, err := chacha20poly1305.NewX(deriveKey(passphrase, salt))
	if err != nil {
		return Archive{}, err
	}
	payload, err := opener.Open(nil, nonce, parts[headerLines], []byte(header(salt, nonce)))
	if err != nil {
		return Archive{}, ErrWrongPassphrase
	}
	return decode(payload)
}

// Reseal opens a backup with one passphrase and seals it with another.
func Reseal(content []byte, from, to string) ([]byte, error) {
	archive, err := Open(content, from)
	if err != nil {
		return nil, err
	}
	return Seal(archive, to)
}

// header is the readable start of a backup file, which the encryption
// authenticates with the content.
func header(salt, nonce []byte) string {
	return magic + lineBreak + base64.StdEncoding.EncodeToString(salt) + lineBreak + base64.StdEncoding.EncodeToString(nonce) + lineBreak
}

func deriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, kdfPasses, kdfMemoryKiB, kdfLanes, keyBytes)
}

func decode(payload []byte) (Archive, error) {
	decompressor, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return Archive{}, errDamaged
	}
	var archive Archive
	if err := json.NewDecoder(io.LimitReader(decompressor, maxPayloadBytes)).Decode(&archive); err != nil {
		return Archive{}, errDamaged
	}
	return archive, archive.problem()
}

// problem checks what a restore would write: the names that become folders,
// the .env keys, the keys of the model settings, and each version's JSON.
func (archive Archive) problem() error {
	if archive.Format != formatVersion {
		return fmt.Errorf("backup format %d is not supported", archive.Format)
	}
	named := keyNames(archive.Settings.LLM)
	for name := range archive.Keys {
		if name == PassphraseName || !slices.Contains(named, name) {
			return fmt.Errorf("the backup holds a key %q that its settings do not use", name)
		}
	}
	seen := map[string]bool{}
	for _, record := range archive.Projects {
		name := record.Project.Name
		if !store.ValidProjectName(name) || record.Project.Slug != name || seen[name] {
			return fmt.Errorf("the backup has a project %q that cannot be restored", name)
		}
		seen[name] = true
		if err := recordProblem(record); err != nil {
			return fmt.Errorf("project %s: %w", name, err)
		}
	}
	return nil
}

func recordProblem(record store.ProjectRecord) error {
	for _, variable := range record.Variables {
		if !policy.ValidEnvKey(variable.Key) {
			return fmt.Errorf(".env key %q is not a variable name", variable.Key)
		}
	}
	for _, kept := range record.Generations {
		generation := kept.Generation
		documents := []string{generation.IRJSON, generation.OptionsJSON, generation.PolicyJSON, generation.ValidationJSON, generation.FactsJSON}
		invalid := slices.ContainsFunc(documents, func(document string) bool { return document != "" && !json.Valid([]byte(document)) })
		if invalid || generation.IRJSON == "" {
			return fmt.Errorf("version %d does not hold a readable plan", generation.Number)
		}
	}
	return nil
}
