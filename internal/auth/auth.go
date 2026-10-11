// Package auth hashes the web UI password with Argon2id, issues session and
// CSRF tokens, and slows down repeated failed logins.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters from OWASP: 19 MiB, 2 passes, 1 lane.
const (
	argonMemoryKiB = 19 * 1024
	argonPasses    = 2
	argonLanes     = 1
	argonKeyBytes  = 32
	saltBytes      = 16
	tokenBytes     = 16
	csrfBytes      = 32
	phcPrefix      = "$argon2id$v=19$"
	phcFields      = 6

	MinimumPasswordLength = 12
	MaximumPasswordLength = 512
)

var (
	ErrPasswordTooShort = fmt.Errorf("a password needs at least %d characters", MinimumPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("a password has at most %d characters", MaximumPasswordLength)
	ErrMalformedHash    = errors.New("the stored password hash is not an Argon2id PHC string")
)

var encoding = base64.RawStdEncoding

// CheckPassword applies the password policy: length only, as NIST SP 800-63B
// recommends, with an upper bound that keeps hashing cheap to request.
func CheckPassword(password string) error {
	length := utf8.RuneCountInString(password)
	switch {
	case length < MinimumPasswordLength:
		return ErrPasswordTooShort
	case length > MaximumPasswordLength:
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword returns a PHC string with a fresh random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonPasses, argonMemoryKiB, argonLanes, argonKeyBytes)
	return fmt.Sprintf("%sm=%d,t=%d,p=%d$%s$%s", phcPrefix, argonMemoryKiB, argonPasses, argonLanes,
		encoding.EncodeToString(salt), encoding.EncodeToString(key)), nil
}

// VerifyPassword recomputes the hash with the parameters stored in the PHC
// string, so hashes made with older parameters keep working.
func VerifyPassword(encoded, password string) (bool, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != phcFields || !strings.HasPrefix(encoded, phcPrefix) {
		return false, ErrMalformedHash
	}
	var memory, passes uint32
	var lanes uint8
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &memory, &passes, &lanes); err != nil {
		return false, ErrMalformedHash
	}
	salt, saltErr := encoding.DecodeString(fields[4])
	want, keyErr := encoding.DecodeString(fields[5])
	if saltErr != nil || keyErr != nil || len(want) != argonKeyBytes {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, lanes, argonKeyBytes)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewToken returns a session token for the cookie and the hash to store.
func NewToken() (token, hash string, err error) {
	value := make([]byte, tokenBytes)
	if _, err := rand.Read(value); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(value)
	return token, TokenHash(token), nil
}

// TokenHash is what the database keeps instead of the token.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewCSRF returns a synchronizer token for state-changing forms.
func NewCSRF() (string, error) {
	value := make([]byte, csrfBytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// SameToken compares two tokens in constant time.
func SameToken(left, right string) bool {
	return left != "" && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
