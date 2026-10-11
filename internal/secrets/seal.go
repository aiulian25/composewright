package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	aadVersion     = "v1"
	settingsPrefix = "settings"
)

var ErrUnknownKey = errors.New("value was sealed with a key that is not available")

// Sealed is one encrypted value as stored: a random 24-byte nonce, the
// ciphertext with its 16-byte tag, and the id of the key that sealed it.
type Sealed struct {
	Nonce      []byte
	Ciphertext []byte
	KeyID      string
}

// Sealer encrypts with the current key and decrypts with whichever key a row
// names, so rows sealed before a rotation stay readable until re-sealed.
type Sealer struct {
	current Key
	keys    map[string]Key
}

func NewSealer(current Key, retired ...Key) *Sealer {
	keys := map[string]Key{current.ID: current}
	for _, key := range retired {
		keys[key.ID] = key
	}
	return &Sealer{current: current, keys: keys}
}

func (sealer *Sealer) CurrentKeyID() string {
	return sealer.current.ID
}

// ProjectAAD binds a value to its project and variable name:
// project_id || key || "v1". Project ids have a fixed length, so the
// concatenation is unambiguous.
func ProjectAAD(projectID, key string) []byte {
	return []byte(projectID + key + aadVersion)
}

// SettingsAAD binds a value that belongs to no project, such as the LLM key.
func SettingsAAD(key string) []byte {
	return []byte(settingsPrefix + key + aadVersion)
}

func (sealer *Sealer) Seal(plaintext, aad []byte) (Sealed, error) {
	return seal(sealer.current, plaintext, aad)
}

func (sealer *Sealer) Open(sealed Sealed, aad []byte) ([]byte, error) {
	key, found := sealer.keys[sealed.KeyID]
	if !found {
		return nil, fmt.Errorf("%w (key id %s)", ErrUnknownKey, sealed.KeyID)
	}
	aead, err := chacha20poly1305.NewX(key.material)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad)
	if err != nil {
		return nil, errors.New("decryption failed: the value was tampered with or belongs to another row")
	}
	return plaintext, nil
}

// Reseal decrypts with whichever key sealed the value and encrypts with
// target; key rotation uses it to move every row to the new key.
func (sealer *Sealer) Reseal(sealed Sealed, aad []byte, target Key) (Sealed, error) {
	plaintext, err := sealer.Open(sealed, aad)
	if err != nil {
		return Sealed{}, err
	}
	return seal(target, plaintext, aad)
}

func seal(key Key, plaintext, aad []byte) (Sealed, error) {
	aead, err := chacha20poly1305.NewX(key.material)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, aad), KeyID: key.ID}, nil
}
