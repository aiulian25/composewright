// Package redact removes secret values from text before it is logged, stored
// or sent to a language model.
package redact

import (
	"encoding/json"
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
	// escapedDollar is how Compose and a shell write a literal $, and
	// literalMarker what stands for it while references are taken out.
	escapedDollar = "$$"
	literalMarker = "\x00"
	errorOperator = "?"
	// referenceJoiners join references, as in ${HOST}:${PORT}/${PATH}.
	referenceJoiners = ":/@.,-_"
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
	// expansion is one innermost $NAME or ${NAME}, or ${NAME} with an
	// operator such as :- and what follows it: a default or an alternative
	// value is a value too, and an inner expansion is matched first.
	expansion = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-=?+])([^{}$]*))?\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	// referencedName names each variable a value expands.
	referencedName = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
	masked         = regexp.MustCompile(maskPattern)
	wholeMask      = regexp.MustCompile(`^` + maskPattern + `$`)
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
	if value == "" || IsMask(value) {
		return value
	}
	if IsSecretName(name) {
		return masker.secret(value)
	}
	value = masker.maskDefaults(value, namesSecret(value))
	if OnlyReferences(value) {
		return value
	}
	return masker.Text(value)
}

// Secret redacts the value of a secret, whatever its name: one that only
// refers to other variables keeps the references, with every default and
// alternative value masked, and any other is masked whole.
func Secret(value string) string {
	return (*Masker)(nil).secret(value)
}

func (masker *Masker) secret(value string) string {
	if value == "" || IsMask(value) {
		return value
	}
	value = masker.maskDefaults(value, true)
	if OnlyReferences(value) {
		return value
	}
	return masker.mask(value)
}

// OnlyReferences reports whether value is nothing but variable expansions,
// such as ${DB_HOST}:${DB_PORT:-5432}, with no text between them; an
// escaped $$ is literal text.
func OnlyReferences(value string) bool {
	rest := strings.ReplaceAll(value, escapedDollar, literalMarker)
	for {
		reduced := expansion.ReplaceAllString(rest, "")
		if reduced == rest {
			return strings.TrimFunc(reduced, isReferenceJoiner) == ""
		}
		rest = reduced
	}
}

// isReferenceJoiner is punctuation that joins references without carrying
// a value of its own, as in ${HOST}:${PORT} or ${USER}@${HOST}.
func isReferenceJoiner(character rune) bool {
	return strings.ContainsRune(referenceJoiners, character)
}

// namesSecret reports whether any variable value expands sounds secret, so
// every default in it, however nested, stands in for that secret.
func namesSecret(value string) bool {
	return slices.ContainsFunc(referencedName.FindAllStringSubmatch(value, -1), func(match []string) bool { return IsSecretName(match[1]) })
}

// maskDefaults masks the default and alternative values in value's
// expansions that stand in for a secret: every one when secret, else those
// of a variable whose name sounds secret. A ${NAME:?message} keeps its
// message, which Compose shows instead of a value.
func (masker *Masker) maskDefaults(value string, secret bool) string {
	return expansion.ReplaceAllStringFunc(value, func(match string) string {
		parts := expansion.FindStringSubmatch(match)
		name, operator, fallback := parts[1], parts[2], parts[3]
		standsIn := secret || IsSecretName(name)
		if !standsIn || fallback == "" || IsMask(fallback) || strings.HasSuffix(operator, errorOperator) {
			return match
		}
		return "${" + name + operator + masker.mask(fallback) + "}"
	})
}

func redactAssignment(match string) string {
	parts := assignment.FindStringSubmatch(match)
	name, separator, value := parts[1], parts[2], parts[3]
	if !hasToken(name, secretWords) && !hasToken(name, textOnlyWords) {
		return match
	}
	unquoted := strings.Trim(value, `"'`)
	if unquoted == "" || hasMask(unquoted) || filePath.MatchString(unquoted) {
		return match
	}
	if OnlyReferences(unquoted) {
		return name + separator + (*Masker)(nil).maskDefaults(value, true)
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

// Strings runs Text over every string of value, a JSON-encodable struct,
// whichever field holds it, and returns the redacted copy.
func Strings[T any](value *T) (*T, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(textTree(tree))
	if err != nil {
		return nil, err
	}
	var redacted T
	return &redacted, json.Unmarshal(encoded, &redacted)
}

func textTree(node any) any {
	switch value := node.(type) {
	case string:
		return Text(value)
	case []any:
		for index := range value {
			value[index] = textTree(value[index])
		}
	case map[string]any:
		for key := range value {
			value[key] = textTree(value[key])
		}
	}
	return node
}
