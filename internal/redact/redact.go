// Package redact removes secret values from text before it is logged, stored
// or sent to a language model.
package redact

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	Mask         = "<redacted>"
	numberedMask = "<redacted-%d>"
	// maskPattern matches Mask and a Masker's numbered masks.
	maskPattern      = `<redacted(?:-\d+)?>`
	privateKeyMask   = "<redacted private key>"
	minimumKnownSize = 4
)

// secretWords are matched as whole tokens of a variable name split on _ . -
// so KEYCLOAK_URL or MONKEY are not treated as secrets.
var secretWords = []string{
	"PASSWORD", "PASSWD", "PASS", "PWD", "SECRET", "SECRETS", "TOKEN", "TOKENS",
	"KEY", "KEYS", "APIKEY", "SALT", "CREDENTIAL", "CREDENTIALS", "PRIVATE", "DSN",
}

// textOnlyWords are also redacted inside free text (*_URL values may carry
// credentials) but do not make a variable a generated secret.
var textOnlyWords = []string{"URL", "URI"}

var (
	assignment = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.-]*)("?\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`)
	userInfo   = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+):([^/\s@]+)@`)
	bearer     = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]{8,}`)
	privateKey = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	// fallbackReference is ${NAME:-fallback}: the fallback is a value too.
	fallbackReference = regexp.MustCompile(`^(\$\{([A-Za-z_][A-Za-z0-9_]*):?-)(.+)\}$`)
	masked            = regexp.MustCompile(maskPattern)
	wholeMask         = regexp.MustCompile(`^` + maskPattern + `$`)
	// filePath is a path to a file, such as key="/path/to/key.pem": an
	// example, never secret material, which has no dot in base64.
	filePath = regexp.MustCompile(`^\.{0,2}/[\w./-]*\.\w+$`)
)

// Masker masks secrets as Value and Text do, but gives each distinct secret
// its own numbered mask, the same wherever it appears, so secrets that were
// equal still are once masked. A nil Masker masks every secret with Mask.
type Masker struct {
	masks map[string]string
}

func NewMasker() *Masker {
	return &Masker{masks: map[string]string{}}
}

// mask numbers secrets in the order first seen. One shorter than
// minimumKnownSize is not worth telling apart and gets Mask.
func (masker *Masker) mask(secret string) string {
	if masker == nil || len(secret) < minimumKnownSize {
		return Mask
	}
	if _, found := masker.masks[secret]; !found {
		masker.masks[secret] = fmt.Sprintf(numberedMask, len(masker.masks)+1)
	}
	return masker.masks[secret]
}

// IsMask reports whether value is a mask and nothing else.
func IsMask(value string) bool {
	return wholeMask.MatchString(value)
}

// hasMask reports whether text holds a mask.
func hasMask(text string) bool {
	return masked.MatchString(text)
}

// IsSecretName reports whether a variable or attribute name holds a secret.
func IsSecretName(name string) bool {
	return hasToken(name, secretWords)
}

// Text redacts secret-looking assignments, URL passwords, bearer tokens and
// private key blocks. References such as ${POSTGRES_PASSWORD} are kept because
// they carry no secret material.
func Text(text string) string {
	return (*Masker)(nil).Text(text)
}

// Text redacts as the function Text does, numbering URL passwords.
func (masker *Masker) Text(text string) string {
	text = privateKey.ReplaceAllString(text, privateKeyMask)
	text = userInfo.ReplaceAllStringFunc(text, func(match string) string {
		parts := userInfo.FindStringSubmatch(match)
		if IsMask(parts[2]) {
			return match
		}
		return parts[1] + ":" + masker.mask(parts[2]) + "@"
	})
	text = bearer.ReplaceAllString(text, "${1}"+Mask)
	return assignment.ReplaceAllStringFunc(text, redactAssignment)
}

// Values replaces every occurrence of known secret values, longest first so a
// value that contains another is removed whole.
func Values(text string, secrets []string) string {
	ordered := slices.Clone(secrets)
	slices.SortFunc(ordered, func(left, right string) int { return len(right) - len(left) })
	for _, secret := range ordered {
		if len(secret) < minimumKnownSize {
			continue
		}
		text = strings.ReplaceAll(text, secret, Mask)
	}
	return text
}

// Value redacts one variable's value: a secret-named variable loses its value
// unless it only refers to another variable, and any other value has
// credentials removed from its text.
func Value(name, value string) string {
	return (*Masker)(nil).Value(name, value)
}

// Value redacts as the function Value does, numbering the secrets.
func (masker *Masker) Value(name, value string) string {
	if parts := fallbackReference.FindStringSubmatch(value); parts != nil && (IsSecretName(name) || IsSecretName(parts[2])) {
		// The fallback is masked as the secret it stands in for, whichever
		// of the two names sounds secret.
		secretName := name
		if !IsSecretName(name) {
			secretName = parts[2]
		}
		return parts[1] + masker.Value(secretName, parts[3]) + "}"
	}
	if value == "" || strings.HasPrefix(value, "$") || IsMask(value) {
		return value
	}
	if IsSecretName(name) {
		return masker.mask(value)
	}
	return masker.Text(value)
}

func redactAssignment(match string) string {
	parts := assignment.FindStringSubmatch(match)
	name, separator, value := parts[1], parts[2], parts[3]
	if !hasToken(name, secretWords) && !hasToken(name, textOnlyWords) {
		return match
	}
	unquoted := strings.Trim(value, `"'`)
	if unquoted == "" || strings.HasPrefix(unquoted, "$") || hasMask(unquoted) || filePath.MatchString(unquoted) {
		return match
	}
	return name + separator + Mask
}

func hasToken(name string, words []string) bool {
	tokens := strings.FieldsFunc(strings.ToUpper(name), func(character rune) bool {
		return character == '_' || character == '.' || character == '-'
	})
	for _, token := range tokens {
		if slices.Contains(words, token) {
			return true
		}
	}
	return false
}
