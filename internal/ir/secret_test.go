package ir

import "testing"

func TestIsSecretSetting(t *testing.T) {
	cases := []struct {
		name, value string
		secret      bool
	}{
		{"DB_PASSWORD", "", true},
		{"SECRET_KEY", "change-me", true},
		{"JWT_SECRET_KEY", "", true},
		{"API_TOKEN", "abc123def", true},
		{"VNC_PASSWORD", "123456", true},
		{"JWT_ACCESS_TOKEN_EXPIRES", "3600", false},
		{"JWT_REFRESH_TOKEN_EXPIRES", "", false},
		{"CORS_SUPPORTS_CREDENTIALS", "true", false},
		{"SESSION_TOKEN_TTL", "15m", false},
		{"API_KEY_HEADER", "X-Api-Key", false},
		{"POSTGRES_PASSWORD_FILE", "/run/secrets/db", false},
		{"TOKEN_LIFETIME_HOURS", "24", false},
		{"APP_URL", "http://localhost", false},
		{"MAILER_DSN", "smtp://127.0.0.1", false},
		{"MAILER_DSN", "smtp://mail:25/", false},
		{"DATABASE_DSN", "sqlite:///data/app.db", false},
		{"MAILER_DSN", "smtp://user:hunter2@mail.example.com:587", true},
		{"MAILER_DSN", "smtp://mail.example.com?api_key=abc123", true},
		{"SENTRY_DSN", "https://publickey@o1.ingest.sentry.io/2", true},
		{"DB_PASSWORD", "Tr0ub4dor://x9", true},
		{"APP_KEY", "base64:/Zm9vYmFyYmF6cXV4", true},
		{"DB_PASSWORD", "Tr0ub4dor:/x9", true},
		{"NOTIFY_SECRET", "discord://123456/abcdefTOKEN", true},
		{"SLACK_WEBHOOK_TOKEN", "https://hooks.slack.com/services/T/B/XXXX", true},
	}
	for _, testCase := range cases {
		if got := IsSecretSetting(testCase.name, testCase.value); got != testCase.secret {
			t.Errorf("IsSecretSetting(%q, %q) = %v, want %v", testCase.name, testCase.value, got, testCase.secret)
		}
	}
}

func TestIsPlaceholderSecret(t *testing.T) {
	cases := map[string]bool{
		"": true, "changeme": true, "CHANGE_ME": true, "change-this-secret": true, "your_password": true, "your-secret-here": true,
		"<password>": true, "<redacted>": true, "...": true, "****": true, "admin": true, "Password": true, "123456": true, "xxx-xxx": true,
		"MyOwnDbPassword-7c2f": false, "Yourself2024!": false, "k3x9-Qp7w-ZZ81": false, "correct horse battery staple": false,
	}
	for value, want := range cases {
		if got := IsPlaceholderSecret(value); got != want {
			t.Errorf("IsPlaceholderSecret(%q) = %v, want %v", value, got, want)
		}
	}
}
