package secrets

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
)

func newTestKey(t *testing.T) Key {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSealRoundTrip(t *testing.T) {
	sealer := NewSealer(newTestKey(t))
	aad := ProjectAAD("0123456789abcdef0123456789abcdef", "POSTGRES_PASSWORD")
	sealed, err := sealer.Seal([]byte("s3cret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed.Nonce) != 24 || sealed.KeyID != sealer.CurrentKeyID() || bytes.Contains(sealed.Ciphertext, []byte("s3cret")) {
		t.Fatalf("unexpected sealed value %+v", sealed)
	}
	plaintext, err := sealer.Open(sealed, aad)
	if err != nil || string(plaintext) != "s3cret" {
		t.Fatalf("open = %q, %v", plaintext, err)
	}
	again, _ := sealer.Seal([]byte("s3cret"), aad)
	if bytes.Equal(again.Nonce, sealed.Nonce) {
		t.Fatal("nonce reused")
	}
}

func TestOpenRejectsAADMismatchAndTampering(t *testing.T) {
	sealer := NewSealer(newTestKey(t))
	project := "0123456789abcdef0123456789abcdef"
	aad := ProjectAAD(project, "POSTGRES_PASSWORD")
	sealed, err := sealer.Seal([]byte("s3cret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	mismatches := map[string][]byte{
		"other project":  ProjectAAD("fedcba9876543210fedcba9876543210", "POSTGRES_PASSWORD"),
		"other variable": ProjectAAD(project, "ADMIN_PASSWORD"),
		"other version":  []byte(project + "POSTGRES_PASSWORD" + "v2"),
		"settings row":   SettingsAAD("POSTGRES_PASSWORD"),
	}
	for name, other := range mismatches {
		if _, err := sealer.Open(sealed, other); err == nil {
			t.Errorf("%s: opened with the wrong AAD", name)
		}
	}
	flipped := sealed
	flipped.Ciphertext = bytes.Clone(sealed.Ciphertext)
	flipped.Ciphertext[0] ^= 0x01
	if _, err := sealer.Open(flipped, aad); err == nil {
		t.Error("flipped ciphertext opened")
	}
	truncated := sealed
	truncated.Ciphertext = sealed.Ciphertext[:len(sealed.Ciphertext)-1]
	if _, err := sealer.Open(truncated, aad); err == nil {
		t.Error("truncated ciphertext opened")
	}
	foreign := NewSealer(newTestKey(t))
	if _, err := foreign.Open(sealed, aad); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("foreign key: got %v, want ErrUnknownKey", err)
	}
}

func TestResealMovesValuesToTheNewKey(t *testing.T) {
	old := newTestKey(t)
	aad := SettingsAAD("llm")
	sealed, err := NewSealer(old).Seal([]byte("sk-test"), aad)
	if err != nil {
		t.Fatal(err)
	}
	next := newTestKey(t)
	rotated := NewSealer(next, old)
	if plaintext, err := rotated.Open(sealed, aad); err != nil || string(plaintext) != "sk-test" {
		t.Fatalf("retired key not used: %q %v", plaintext, err)
	}
	moved, err := rotated.Reseal(sealed, aad, next)
	if err != nil || moved.KeyID != next.ID {
		t.Fatalf("reseal: %+v %v", moved, err)
	}
	if plaintext, err := NewSealer(next).Open(moved, aad); err != nil || string(plaintext) != "sk-test" {
		t.Fatalf("resealed value unreadable without the old key: %q %v", plaintext, err)
	}
}

func TestGenerate(t *testing.T) {
	patterns := map[string]*regexp.Regexp{
		GeneratePassword24: regexp.MustCompile(`^[A-Za-z0-9]{24}$`),
		GenerateHex32:      regexp.MustCompile(`^[0-9a-f]{64}$`),
		GenerateBase64:     regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`),
		GenerateUUID:       regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`),
		GenerateLaravelKey: regexp.MustCompile(`^base64:[A-Za-z0-9+/]{43}=$`),
	}
	const samples = 10000
	for kind, pattern := range patterns {
		seen := map[string]bool{}
		for range samples {
			value, err := Generate(kind)
			if err != nil {
				t.Fatal(err)
			}
			if !pattern.MatchString(value) {
				t.Fatalf("%s produced %q", kind, value)
			}
			if seen[value] {
				t.Fatalf("%s repeated %q", kind, value)
			}
			seen[value] = true
		}
	}
	if _, err := Generate("password6"); err == nil {
		t.Fatal("unknown generator accepted")
	}
}
