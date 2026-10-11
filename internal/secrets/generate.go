package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
)

const (
	GeneratePassword24 = "password24"
	GenerateHex32      = "hex32"
	GenerateBase64     = "base64_32"
	GenerateUUID       = "uuid"
	// GenerateLaravelKey is the APP_KEY form `php artisan key:generate` writes.
	GenerateLaravelKey = "laravel_key"

	laravelKeyPrefix = "base64:"
	passwordLength   = 24
	randomBytes      = 32
	uuidBytes        = 16
	// Alphanumeric only: many images break on $, #, quotes or spaces in .env.
	passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	uuidVersion4     = 0x40
	uuidVersionMask  = 0x0f
	uuidVariantRFC   = 0x80
	uuidVariantMask  = 0x3f
)

// Generate creates a secret of the given kind from crypto/rand.
func Generate(kind string) (string, error) {
	switch kind {
	case GeneratePassword24:
		return password(passwordLength)
	case GenerateHex32:
		buffer, err := random(randomBytes)
		return hex.EncodeToString(buffer), err
	case GenerateBase64:
		buffer, err := random(randomBytes)
		return base64.RawURLEncoding.EncodeToString(buffer), err
	case GenerateUUID:
		return uuid()
	case GenerateLaravelKey:
		buffer, err := random(randomBytes)
		return laravelKeyPrefix + base64.StdEncoding.EncodeToString(buffer), err
	}
	return "", fmt.Errorf("unknown secret generator %q", kind)
}

func random(size int) ([]byte, error) {
	buffer := make([]byte, size)
	_, err := rand.Read(buffer)
	return buffer, err
}

// password draws each character uniformly; rand.Int rejects samples that
// would bias the distribution.
func password(length int) (string, error) {
	alphabetSize := big.NewInt(int64(len(passwordAlphabet)))
	result := make([]byte, length)
	for index := range result {
		choice, err := rand.Int(rand.Reader, alphabetSize)
		if err != nil {
			return "", err
		}
		result[index] = passwordAlphabet[choice.Int64()]
	}
	return string(result), nil
}

func uuid() (string, error) {
	buffer, err := random(uuidBytes)
	if err != nil {
		return "", err
	}
	buffer[6] = buffer[6]&uuidVersionMask | uuidVersion4
	buffer[8] = buffer[8]&uuidVariantMask | uuidVariantRFC
	encoded := hex.EncodeToString(buffer)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
