// Package redact removes secret values from text before it is logged, stored
// or sent to a language model.
package redact

import (
	"regexp"
	"slices"
	"strings"
)

const (
	Mask             = "<redacted>"
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
)

// IsSecretName reports whether a variable or attribute name holds a secret.
func IsSecretName(name string) bool {
	return hasToken(name, secretWords)
}

// Text redacts secret-looking assignments, URL passwords, bearer tokens and
// private key blocks. References such as ${POSTGRES_PASSWORD} are kept because
// they carry no secret material.
func Text(text string) string {
	text = privateKey.ReplaceAllString(text, privateKeyMask)
	text = userInfo.ReplaceAllString(text, "${1}:"+Mask+"@")
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
	if value == "" || strings.HasPrefix(value, "$") {
		return value
	}
	if IsSecretName(name) {
		return Mask
	}
	return Text(value)
}

func redactAssignment(match string) string {
	parts := assignment.FindStringSubmatch(match)
	name, separator, value := parts[1], parts[2], parts[3]
	if !hasToken(name, secretWords) && !hasToken(name, textOnlyWords) {
		return match
	}
	unquoted := strings.Trim(value, `"'`)
	if unquoted == "" || unquoted == Mask || strings.HasPrefix(unquoted, "$") || strings.Contains(unquoted, Mask) {
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
