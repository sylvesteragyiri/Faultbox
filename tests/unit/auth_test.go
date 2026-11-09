package unit

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
)

func TestNewAPIKeyFormat(t *testing.T) {
	t.Parallel()

	key, hash, hint, err := auth.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, auth.APIKeyPrefix) || len(key) != len(auth.APIKeyPrefix)+43 {
		t.Fatalf("key %q has the wrong shape", key)
	}
	if !auth.LooksLikeAPIKey(key) {
		t.Fatalf("LooksLikeAPIKey(%q) = false", key)
	}
	if !bytes.Equal(hash, auth.HashAPIKey(key)) {
		t.Fatal("returned hash does not match HashAPIKey")
	}
	if want := auth.APIKeyPrefix + "…" + key[len(key)-4:]; hint != want {
		t.Fatalf("hint = %q, want %q", hint, want)
	}
	if strings.Contains(hint, key[len(auth.APIKeyPrefix):len(key)-4]) {
		t.Fatal("hint leaks the key body")
	}
}

func TestNewAPIKeyIsUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 200 {
		key, _, _, err := auth.NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("duplicate key %q", key)
		}
		seen[key] = true
	}
}

func TestLooksLikeAPIKeyRejectsGarbage(t *testing.T) {
	t.Parallel()

	valid, _, _, _ := auth.NewAPIKey()
	for _, s := range []string{
		"",
		"fbx_",
		"sk_live_" + valid[4:],
		valid[:len(valid)-1],
		valid + "x",
		valid[:10] + "!" + valid[11:],
	} {
		if auth.LooksLikeAPIKey(s) {
			t.Errorf("LooksLikeAPIKey(%q) = true, want false", s)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("correct-horse-battery", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(hash, "correct-horse-battery") {
		t.Fatal("correct password rejected")
	}
	if auth.CheckPassword(hash, "correct-horse-batterY") {
		t.Fatal("wrong password accepted")
	}
	if _, err := auth.HashPassword(strings.Repeat("a", 73), bcrypt.MinCost); err == nil {
		t.Fatal("73-byte password accepted; bcrypt would silently truncate it")
	}
}

func TestCSRFToken(t *testing.T) {
	t.Parallel()

	key := []byte("0123456789abcdef0123456789abcdef")
	token := auth.CSRFToken(key, "cookie-a")

	if !auth.ValidCSRFToken(key, "cookie-a", token) {
		t.Fatal("valid token rejected")
	}
	if auth.ValidCSRFToken(key, "cookie-b", token) {
		t.Fatal("token accepted for a different cookie")
	}
	if auth.ValidCSRFToken([]byte("another-key-another-key-another!!"), "cookie-a", token) {
		t.Fatal("token accepted under a different key")
	}
	if auth.ValidCSRFToken(key, "", "") || auth.ValidCSRFToken(key, "cookie-a", "") {
		t.Fatal("empty token accepted")
	}
}

func TestDeriveKeys(t *testing.T) {
	t.Parallel()

	secret := []byte("a-master-secret-of-at-least-32-bytes!")
	a, err := auth.DeriveKeys(secret)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := auth.DeriveKeys(secret)
	if !bytes.Equal(a.CSRF, b.CSRF) || !bytes.Equal(a.Encryption, b.Encryption) {
		t.Fatal("derivation is not deterministic")
	}
	if bytes.Equal(a.CSRF, a.Encryption) {
		t.Fatal("purpose keys must differ")
	}
	if len(a.CSRF) != 32 || len(a.Encryption) != 32 {
		t.Fatal("keys must be 32 bytes")
	}
	other, _ := auth.DeriveKeys([]byte("a-different-secret-of-32-bytes-or-more"))
	if bytes.Equal(a.CSRF, other.CSRF) {
		t.Fatal("different secrets produced the same key")
	}
}

func TestSessionTokens(t *testing.T) {
	t.Parallel()

	tok, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("token %q is too short", tok)
	}
	if !bytes.Equal(hash, auth.HashToken(tok)) {
		t.Fatal("hash mismatch")
	}
	tok2, _, _ := auth.NewToken()
	if tok == tok2 {
		t.Fatal("tokens repeat")
	}
}

func TestFailureLimiter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := auth.NewFailureLimiter(3, 15*time.Minute)
	l.SetClock(func() time.Time { return now })

	for i := range 3 {
		if l.Blocked("ip:1.2.3.4") {
			t.Fatalf("blocked after %d failures, limit is 3", i)
		}
		l.Fail("ip:1.2.3.4")
	}
	if !l.Blocked("ip:1.2.3.4") {
		t.Fatal("not blocked after 3 failures")
	}
	if l.Blocked("ip:5.6.7.8") {
		t.Fatal("unrelated key blocked")
	}

	now = now.Add(15 * time.Minute)
	if l.Blocked("ip:1.2.3.4") {
		t.Fatal("still blocked after the window passed")
	}

	l.Fail("email:a@b.c")
	l.Fail("email:a@b.c")
	l.Fail("email:a@b.c")
	l.Reset("email:a@b.c")
	if l.Blocked("email:a@b.c") {
		t.Fatal("still blocked after Reset")
	}
}
