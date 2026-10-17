package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestIsSecretName(t *testing.T) {
	cases := map[string]bool{
		"POSTGRES_PASSWORD": true, "ADMIN_TOKEN": true, "SECRET_KEY_BASE": true, "OPENAI_API_KEY": true,
		"SMTP_PASS": true, "api_key": true, "llm.apikey": true, "JWT_SECRET": true, "DB_PWD": true,
		"KEYCLOAK_URL": false, "MONKEY": false, "BASE_URL": false, "SIGNUPS_ALLOWED": false,
		"PASSENGER_COUNT": false, "TZ": false, "DATABASE_URL": false, "variable": false,
	}
	for name, want := range cases {
		if got := IsSecretName(name); got != want {
			t.Errorf("IsSecretName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct{ input, want string }{
		{"POSTGRES_PASSWORD=secret", "POSTGRES_PASSWORD=" + Mask},
		{"  - ADMIN_PASSWORD=test123", "  - ADMIN_PASSWORD=" + Mask},
		{"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}"},
		{`"api_key": "sk-123456"`, `"api_key": ` + Mask},
		{"DATABASE_URL=postgres://miniflux:secret@db/miniflux", "DATABASE_URL=postgres://miniflux:" + Mask + "@db/miniflux"},
		{"connect to postgres://miniflux:secret@db/miniflux now", "connect to postgres://miniflux:" + Mask + "@db/miniflux now"},
		{"Authorization: Bearer abcdefghijklmnop", "Authorization: Bearer " + Mask},
		{"BASE_URL=http://localhost:8080", "BASE_URL=" + Mask},
		{"RUN_MIGRATIONS=1", "RUN_MIGRATIONS=1"},
		{"level=INFO msg=started listen=0.0.0.0:8080", "level=INFO msg=started listen=0.0.0.0:8080"},
		{"TOKEN=", "TOKEN="},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", privateKeyMask},
	}
	for _, testCase := range cases {
		if got := Text(testCase.input); got != testCase.want {
			t.Errorf("Text(%q)\n got  %q\n want %q", testCase.input, got, testCase.want)
		}
	}
}

func TestValuesLongestFirst(t *testing.T) {
	got := Values("pw=abcdef1234 short=abcd x=ab", []string{"abcd", "abcdef1234", "ab"})
	want := "pw=" + Mask + " short=" + Mask + " x=ab"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHandlerMasksSecrets(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(NewHandler(slog.NewTextHandler(&buffer, nil)))
	logger.With(slog.String("token", "ghp_123456789")).Info("fetch",
		slog.String("password", "hunter2"),
		slog.String("stderr", "ADMIN_PASSWORD=hunter3"),
		slog.Any("error", errors.New("dial postgres://u:hunter4@db")),
		slog.Group("request", slog.String("api_key", "sk-hunter5"), slog.String("host", "api.github.com")),
	)
	output := buffer.String()
	for _, leaked := range []string{"ghp_123456789", "hunter2", "hunter3", "hunter4", "hunter5"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("secret %q leaked: %s", leaked, output)
		}
	}
	if !strings.Contains(output, "api.github.com") {
		t.Fatalf("non-secret attribute lost: %s", output)
	}
}

// TestMaskerNumbersSecrets: equal secrets get the same mask wherever they
// are, others another one, and a short one the bare mask.
func TestMaskerNumbersSecrets(t *testing.T) {
	masker := NewMasker()
	got := []string{
		masker.Value("DB_PASSWORD", "hunter22"),
		masker.Value("DATABASE_URL", "postgres://app:hunter22@db/app"),
		masker.Value("SECRET_KEY", "other-secret"),
		masker.Value("DB_PASSWORD", "${DB_PASSWORD:-hunter22}"),
		masker.Value("API_KEY", "abc"),
		masker.Value("DB_PASSWORD", "<redacted-1>"),
	}
	want := []string{"<redacted-1>", "postgres://app:<redacted-1>@db/app", "<redacted-2>", "${DB_PASSWORD:-<redacted-1>}", Mask, "<redacted-1>"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if text := Text(`ROCKET_TLS={certs="/path/to/certs.pem",key="/path/to/key.pem"}`); strings.Contains(text, Mask) {
		t.Fatalf("a file path was masked: %s", text)
	}
	if text := Text("dial postgres://u:" + got[0] + "@db"); text != "dial postgres://u:<redacted-1>@db" {
		t.Fatalf("a numbered mask was masked again: %s", text)
	}
}

// TestMaskerMasksAFallbackSecret: under a key that does not sound secret, a
// fallback for a secret variable is masked as the variable's default is, in
// the - and the :- form.
func TestMaskerMasksAFallbackSecret(t *testing.T) {
	masker := NewMasker()
	defaultMask := masker.Value("DB_PASSWORD", "hunter22secret")
	for _, form := range []string{"-", ":-"} {
		got := masker.Value("APP_DATABASE", "${DB_PASSWORD"+form+"hunter22secret}")
		if want := "${DB_PASSWORD" + form + defaultMask + "}"; got != want {
			t.Errorf("%s form: got %q, want %q", form, got, want)
		}
	}
}
