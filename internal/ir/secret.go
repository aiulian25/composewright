package ir

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/redact"
)

// authoritySeparator follows the scheme of a URL that names a host or a
// local path.
const authoritySeparator = "://"

var (
	// settingSuffix ends names that describe a secret instead of holding
	// one: how long it lives, how long it is, where it is kept.
	settingSuffix = regexp.MustCompile(`_(EXPIRES|EXPIRY|EXPIRATION|TTL|LIFETIME|TIMEOUT|AGE|ENABLED|DISABLED|REQUIRED|LENGTH|SIZE|FILE|PATH|DIR|ALGORITHM|ID|TYPE|NAME|HEADER|ISSUER|AUDIENCE)$`)
	booleanValue  = regexp.MustCompile(`^(?i:true|false|yes|no|on|off)$`)
	numberValue   = regexp.MustCompile(`^\d+(\.\d+)?[smhdw]?$`)
	passwordName  = regexp.MustCompile(`(^|_)(PASSWORD|PASSWD|PASS|PWD)(_|$)`)
	pathValue     = regexp.MustCompile(`^\.{0,2}/`)
	// placeholderText marks a value someone left for others to replace.
	placeholderText = regexp.MustCompile(`(?i)(change[_\- ]?(me|this|it)|your[_\- ]|example|placeholder|replace[_\- ]?me|xxx|todo|fixme|^<.*>$|^\.\.\.$|^\*+$)`)
	// weakDefaults are values that are never a real secret.
	weakDefaults = []string{"password", "passwd", "secret", "admin", "pass", "test", "default", "root", "toor", "postgres", "mysql", "redis",
		"123456", "12345678", "qwerty", "letmein"}
)

// IsSecretSetting reports whether a .env entry holds secret material. The
// name decides, unless the name or the value shows a setting about a
// secret: JWT_ACCESS_TOKEN_EXPIRES, CORS_SUPPORTS_CREDENTIALS=true, or a
// number or a bare address in anything but a password, as
// MAILER_DSN=smtp://127.0.0.1. Such values are kept as written; a secret is
// replaced by a generated one.
func IsSecretSetting(name, value string) bool {
	upper := strings.ToUpper(name)
	describesSecret := settingSuffix.MatchString(upper)
	plainUnlessPassword := numberValue.MatchString(value) || isBareAddress(value)
	settingValue := booleanValue.MatchString(value) || (plainUnlessPassword && !passwordName.MatchString(upper))
	return redact.IsSecretName(name) && !describesSecret && !settingValue
}

// isBareAddress reports whether value is a URL with no place for a
// credential: a host alone, as smtp://127.0.0.1:25, or a local path, as
// sqlite:///data/app.db, with no user, query or fragment. A path after a
// host can carry a token, as a webhook does, and base64:/Zm9v is no URL.
func isBareAddress(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || !strings.HasPrefix(value, parsed.Scheme+authoritySeparator) {
		return false
	}
	bare := parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && !parsed.ForceQuery
	hostOnly := parsed.Host != "" && strings.Trim(parsed.Path, "/") == ""
	localPath := parsed.Host == "" && strings.HasPrefix(parsed.Path, "/")
	return bare && (hostOnly || localPath)
}

// IsSettingValue reports whether value cannot be secret material, whatever
// name holds it: a switch such as true, or a number or a path under a name
// that describes a secret, as JWT_EXPIRES=3600 or
// DB_PASSWORD_FILE=/run/secrets/db.
func IsSettingValue(name, value string) bool {
	describesSecret := settingSuffix.MatchString(strings.ToUpper(name))
	return booleanValue.MatchString(value) || (describesSecret && (numberValue.MatchString(value) || pathValue.MatchString(value)))
}

// IsPlaceholderSecret reports whether a secret someone wrote is a stand-in
// or a well-known default rather than a real secret: changeme,
// your-password-here, <secret>, admin, 123456. Such values are generated
// instead of kept.
func IsPlaceholderSecret(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || placeholderText.MatchString(trimmed) || slices.Contains(weakDefaults, strings.ToLower(trimmed))
}
