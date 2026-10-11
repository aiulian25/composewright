package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q does not carry the OWASP parameters", hash)
	}
	again, _ := HashPassword("correct horse battery staple")
	if again == hash {
		t.Fatal("two hashes of one password share a salt")
	}
	for password, want := range map[string]bool{"correct horse battery staple": true, "correct horse battery stapl": false, "": false} {
		if got, err := VerifyPassword(hash, password); err != nil || got != want {
			t.Errorf("verify %q = %v, %v", password, got, err)
		}
	}
	for _, malformed := range []string{"", "$2b$10$abcdefghijklmnopqrstuu", "$argon2id$v=19$m=x,t=2,p=1$c2FsdA$aGFzaA", strings.Replace(hash, "$argon2id", "$argon2i", 1)} {
		if _, err := VerifyPassword(malformed, "x"); err == nil {
			t.Errorf("malformed hash %q accepted", malformed)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := map[string]error{"short": ErrPasswordTooShort, "twelve chars": nil, strings.Repeat("é", 512): nil, strings.Repeat("a", 513): ErrPasswordTooLong}
	for password, want := range cases {
		if got := CheckPassword(password); !errors.Is(got, want) {
			t.Errorf("CheckPassword(%d runes) = %v, want %v", len([]rune(password)), got, want)
		}
	}
}

func TestTokens(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil || len(token) != 22 || hash != TokenHash(token) || len(hash) != 64 || strings.Contains(hash, token) {
		t.Fatalf("token %q hash %q err %v", token, hash, err)
	}
	other, _, _ := NewToken()
	csrf, _ := NewCSRF()
	if other == token || len(csrf) != 43 || !SameToken(csrf, csrf) || SameToken(csrf, token) || SameToken("", "") {
		t.Fatal("tokens are not unique or comparison is wrong")
	}
}

func TestThrottle(t *testing.T) {
	throttle := NewThrottle()
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for attempt := range 3 {
		if wait := throttle.Wait("client", start); wait != 0 {
			t.Fatalf("attempt %d had to wait %s", attempt, wait)
		}
		throttle.Fail("client", start)
	}
	if wait := throttle.Wait("client", start); wait != time.Second {
		t.Fatalf("after three failures wait %s, want 1s", wait)
	}
	throttle.Fail("client", start.Add(time.Second))
	if wait := throttle.Wait("client", start.Add(time.Second)); wait != 2*time.Second {
		t.Fatalf("after four failures wait %s, want 2s", wait)
	}
	if wait := throttle.Wait("other", start); wait != 0 {
		t.Fatal("another client was throttled")
	}
	for range 40 {
		throttle.Fail("client", start.Add(2*time.Second))
	}
	if wait := throttle.Wait("client", start.Add(2*time.Second)); wait != longestDelay {
		t.Fatalf("wait %s, want the cap %s", wait, longestDelay)
	}
	if wait := throttle.Wait("client", start.Add(2*time.Hour)); wait != 0 {
		t.Fatal("an hour without failures did not reset the count")
	}
	throttle.Succeed("client")
	if wait := throttle.Wait("client", start.Add(2*time.Second)); wait != 0 {
		t.Fatal("a success did not reset the count")
	}
}
