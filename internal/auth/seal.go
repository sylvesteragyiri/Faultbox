package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// sealVersion prefixes sealed values so the format can change later, for
// example when faultbox rotate-secret re-encrypts them (D17).
const sealVersion byte = 1

// ErrUnseal means a sealed value is corrupt or was sealed with another key.
var ErrUnseal = errors.New("cannot decrypt stored secret: it is corrupt or FAULTBOX_SECRET changed")

// Seal encrypts plaintext with AES-256-GCM under key (Keys.Encryption).
// The result is version || nonce || ciphertext+tag.
func Seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(append(out, sealVersion), nonce...)
	return gcm.Seal(out, nonce, plaintext, []byte{sealVersion}), nil
}

// Unseal reverses Seal.
func Unseal(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < 1+n+gcm.Overhead() || sealed[0] != sealVersion {
		return nil, ErrUnseal
	}
	plain, err := gcm.Open(nil, sealed[1:1+n], sealed[1+n:], []byte{sealVersion})
	if err != nil {
		return nil, ErrUnseal
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
