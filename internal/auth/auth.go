// Package auth holds the security primitives: project API keys, dashboard
// passwords, session tokens, CSRF tokens and login rate limiting.
package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// APIKeyPrefix marks Faultbox keys so secret scanners can spot leaks (D10).
const APIKeyPrefix = "fbx_"

// apiKeyRandomBytes is the entropy behind each key.
const apiKeyRandomBytes = 32

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// apiKeyBodyLen is the fixed base62 length of 32 random bytes:
// ceil(256 / log2(62)) = 43.
const apiKeyBodyLen = 43

// NewAPIKey returns a new plaintext project API key, its SHA-256 hash for
// storage, and a short hint that is safe to display.
func NewAPIKey() (key string, hash []byte, hint string, err error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("generate api key: %w", err)
	}
	body := encodeBase62(buf, apiKeyBodyLen)
	key = APIKeyPrefix + body
	return key, HashAPIKey(key), APIKeyPrefix + "…" + body[len(body)-4:], nil
}

// HashAPIKey returns the lookup hash of a key. A fast hash is fine because
// keys carry 256 bits of randomness and are never chosen by people (D10).
func HashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// LooksLikeAPIKey reports whether s has the shape of a Faultbox key. It lets
// callers reject garbage before touching the database.
func LooksLikeAPIKey(s string) bool {
	body, ok := strings.CutPrefix(s, APIKeyPrefix)
	if !ok || len(body) != apiKeyBodyLen {
		return false
	}
	for i := range len(body) {
		if strings.IndexByte(base62, body[i]) < 0 {
			return false
		}
	}
	return true
}

func encodeBase62(b []byte, width int) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	mod := new(big.Int)
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = base62[mod.Int64()]
	}
	return string(out)
}

// ErrPasswordTooLong is returned for passwords bcrypt cannot hash (72 bytes).
var ErrPasswordTooLong = errors.New("password must be at most 72 bytes")

// HashPassword hashes a dashboard password with bcrypt (D9).
func HashPassword(password string, cost int) (string, error) {
	if len(password) > 72 {
		return "", ErrPasswordTooLong
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword reports whether password matches the bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash is compared against when a login names an unknown user, so the
// response takes as long as for a real user and doesn't reveal which emails exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("faultbox-timing-equaliser"), bcrypt.MinCost)

// EqualiseTiming burns roughly one bcrypt comparison.
func EqualiseTiming(password string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// NewToken returns a random URL-safe token and its SHA-256 hash for storage.
func NewToken() (token string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken hashes a session token for storage and lookup.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Keys are purpose-specific keys derived from FAULTBOX_SECRET (D17).
type Keys struct {
	CSRF       []byte
	Encryption []byte // AES-256-GCM key for secrets at rest (M6).
}

// DeriveKeys derives independent keys from the master secret with HKDF-SHA256.
func DeriveKeys(secret []byte) (Keys, error) {
	derive := func(info string) ([]byte, error) {
		return hkdf.Key(sha256.New, secret, nil, info, 32)
	}
	csrf, err := derive("faultbox/csrf/v1")
	if err != nil {
		return Keys{}, err
	}
	enc, err := derive("faultbox/encryption/v1")
	if err != nil {
		return Keys{}, err
	}
	return Keys{CSRF: csrf, Encryption: enc}, nil
}

// CSRFToken binds a CSRF token to a random per-browser cookie value
// (an HMAC-signed double-submit cookie).
func CSRFToken(key []byte, cookieValue string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(cookieValue))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ValidCSRFToken checks a submitted token in constant time.
func ValidCSRFToken(key []byte, cookieValue, token string) bool {
	if cookieValue == "" || token == "" {
		return false
	}
	want := CSRFToken(key, cookieValue)
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}
