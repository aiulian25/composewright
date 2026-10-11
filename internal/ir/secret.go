package ir

import (
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/redact"
)

var (
	// settingSuffix ends names that describe a secret instead of holding
	// one: how long it lives, how long it is, where it is kept.
	settingSuffix = regexp.MustCompile(`_(EXPIRES|EXPIRY|EXPIRATION|TTL|LIFETIME|TIMEOUT|AGE|ENABLED|DISABLED|REQUIRED|LENGTH|SIZE|FILE|PATH|DIR|ALGORITHM|ID|TYPE|NAME|HEADER|ISSUER|AUDIENCE)$`)
	booleanValue  = regexp.MustCompile(`^(?i:true|false|yes|no|on|off)$`)
	numberValue   = regexp.MustCompile(`^\d+(\.\d+)?[smhdw]?$`)
	passwordName  = regexp.MustCompile(`(^|_)(PASSWORD|PASSWD|PASS|PWD)(_|$)`)
	// placeholderText marks a value someone left for others to replace.
	placeholderText = regexp.MustCompile(`(?i)(change[_\- ]?(me|this|it)|your[_\- ]|example|placeholder|replace[_\- ]?me|xxx|todo|fixme|^<.*>$|^\.\.\.$|^\*+$)`)
	// weakDefaults are values that are never a real secret.
	weakDefaults = []string{"password", "passwd", "secret", "admin", "pass", "test", "default", "root", "toor", "postgres", "mysql", "redis",
		"123456", "12345678", "qwerty", "letmein"}
)

// IsSecretSetting reports whether a .env entry holds secret material. The
// name decides, unless the name or the value shows a setting about a
// secret: JWT_ACCESS_TOKEN_EXPIRES, CORS_SUPPORTS_CREDENTIALS=true, or a
// number in anything but a password. Such values are kept as written; a
// secret is replaced by a generated one.
func IsSecretSetting(name, value string) bool {
	upper := strings.ToUpper(name)
	describesSecret := settingSuffix.MatchString(upper)
	settingValue := booleanValue.MatchString(value) || (numberValue.MatchString(value) && !passwordName.MatchString(upper))
	return redact.IsSecretName(name) && !describesSecret && !settingValue
}

// IsPlaceholderSecret reports whether a secret someone wrote is a stand-in
// or a well-known default rather than a real secret: changeme,
// your-password-here, <secret>, admin, 123456. Such values are generated
// instead of kept.
func IsPlaceholderSecret(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || placeholderText.MatchString(trimmed) || slices.Contains(weakDefaults, strings.ToLower(trimmed))
}
