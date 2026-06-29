package cache

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encryption constants for HKDF key derivation. These are fixed by the
// on-disk schema migration — changing them invalidates existing ciphertext
// (self-heals on next plex.tv token refresh, but offline-restart breaks).
var (
	hkdfSalt = []byte("plex-language-sync-token-encryption")
	hkdfInfo = []byte("user-tokens-v1")
)

// encPrefix is prepended to every encrypted value so the read path can
// distinguish ciphertext from legacy plaintext without trial-decryption
// heuristics. The prefix is chosen to be invalid as a Plex token (which
// are alphanumeric) so false positives are impossible.
const encPrefix = "enc:"

// aesGCMNonceSize is the standard 12-byte nonce for AES-GCM.
const aesGCMNonceSize = 12

// DeriveKey produces a 32-byte AES-256 key from the admin PLEX_TOKEN
// using HKDF-SHA256 with a fixed salt and info string. The output is
// deterministic for a given token — no plex.tv round-trip is needed for
// decryption on restart.
func DeriveKey(plexToken string) ([]byte, error) {
	if plexToken == "" {
		return nil, errors.New("cache/crypto: empty plex token")
	}
	key, err := hkdf.Key(sha256.New, []byte(plexToken), hkdfSalt, string(hkdfInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("cache/crypto: HKDF derive: %w", err)
	}
	return key, nil
}

// EncryptToken encrypts a plaintext token using AES-256-GCM with a random
// 12-byte nonce. Returns "enc:" + base64url(nonce || ciphertext).
func EncryptToken(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("cache/crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cache/crypto: new GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("cache/crypto: random nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.RawURLEncoding.EncodeToString(ciphertext)
	return encPrefix + encoded, nil
}

// DecryptToken reverses EncryptToken. If the value does not carry the
// "enc:" prefix (legacy plaintext), it is returned unchanged — this
// enables transparent migration of pre-encryption cache files.
func DecryptToken(key []byte, value string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil // plaintext pass-through (migration path)
	}

	raw, err := base64.RawURLEncoding.DecodeString(value[len(encPrefix):])
	if err != nil {
		return "", fmt.Errorf("cache/crypto: base64 decode: %w", err)
	}
	if len(raw) < aesGCMNonceSize+1 {
		return "", errors.New("cache/crypto: ciphertext too short")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("cache/crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cache/crypto: new GCM: %w", err)
	}

	nonce := raw[:gcm.NonceSize()]
	ciphertext := raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("cache/crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// IsEncrypted reports whether a stored value carries the encryption
// prefix, indicating it was produced by EncryptToken.
func IsEncrypted(value string) bool {
	return len(value) > len(encPrefix) && value[:len(encPrefix)] == encPrefix
}
