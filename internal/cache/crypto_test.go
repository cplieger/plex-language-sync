package cache

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- DeriveKey ---

func TestDeriveKeyDeterministic(t *testing.T) {
	t.Parallel()
	k1, err := DeriveKey("my-plex-token-abc123")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	k2, err := DeriveKey("my-plex-token-abc123")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	if string(k1) != string(k2) {
		t.Error("DeriveKey is not deterministic for the same input")
	}
	if len(k1) != 32 {
		t.Errorf("DeriveKey() len = %d, want 32", len(k1))
	}
}

func TestDeriveKeyDifferentTokensDifferentKeys(t *testing.T) {
	t.Parallel()
	k1, _ := DeriveKey("token-A")
	k2, _ := DeriveKey("token-B")
	if string(k1) == string(k2) {
		t.Error("different tokens should produce different keys")
	}
}

func TestDeriveKeyEmptyTokenErrors(t *testing.T) {
	t.Parallel()
	_, err := DeriveKey("")
	if err == nil {
		t.Error("DeriveKey(\"\") should return an error")
	}
}

// --- EncryptToken / DecryptToken round-trip ---

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()
	key, _ := DeriveKey("test-token")
	original := "xyzzy-user-token-12345"

	ct, err := EncryptToken(key, original)
	if err != nil {
		t.Fatalf("EncryptToken() error = %v", err)
	}
	if ct == original {
		t.Error("ciphertext should differ from plaintext")
	}
	if !IsEncrypted(ct) {
		t.Error("ciphertext should be detected as encrypted")
	}

	plain, err := DecryptToken(key, ct)
	if err != nil {
		t.Fatalf("DecryptToken() error = %v", err)
	}
	if plain != original {
		t.Errorf("DecryptToken() = %q, want %q", plain, original)
	}
}

func TestEncryptTokenProducesUniqueNonces(t *testing.T) {
	t.Parallel()
	key, _ := DeriveKey("test-token")
	ct1, _ := EncryptToken(key, "same-value")
	ct2, _ := EncryptToken(key, "same-value")
	if ct1 == ct2 {
		t.Error("two encryptions of the same value should produce different ciphertext (random nonce)")
	}
}

func TestDifferentKeysProduceDifferentCiphertext(t *testing.T) {
	t.Parallel()
	key1, _ := DeriveKey("token-A")
	key2, _ := DeriveKey("token-B")
	ct1, _ := EncryptToken(key1, "user-token-value")
	ct2, _ := EncryptToken(key2, "user-token-value")
	if ct1 == ct2 {
		t.Error("different keys should produce different ciphertext")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	t.Parallel()
	key1, _ := DeriveKey("token-A")
	key2, _ := DeriveKey("token-B")
	ct, _ := EncryptToken(key1, "secret")
	_, err := DecryptToken(key2, ct)
	if err == nil {
		t.Error("decryption with wrong key should fail")
	}
}

func TestDecryptCorruptedCiphertextFails(t *testing.T) {
	t.Parallel()
	key, _ := DeriveKey("test-token")
	ct, _ := EncryptToken(key, "value")
	// Corrupt the ciphertext by flipping a character.
	corrupted := ct[:len(ct)-2] + "XX"
	_, err := DecryptToken(key, corrupted)
	if err == nil {
		t.Error("decryption of corrupted ciphertext should fail")
	}
}

// --- DecryptToken backward-compat: plaintext pass-through ---

func TestDecryptPlaintextPassThrough(t *testing.T) {
	t.Parallel()
	key, _ := DeriveKey("test-token")
	plainToken := "abcdef123456-plex-token"

	result, err := DecryptToken(key, plainToken)
	if err != nil {
		t.Fatalf("DecryptToken(plaintext) error = %v", err)
	}
	if result != plainToken {
		t.Errorf("DecryptToken(plaintext) = %q, want %q (pass-through)", result, plainToken)
	}
}

// --- IsEncrypted ---

func TestIsEncrypted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  bool
	}{
		{"enc:AAAA", true},
		{"enc:", false},      // prefix only, no data
		{"plaintext", false}, // normal token
		{"", false},          // empty
		{"ENC:data", false},  // wrong case
		{"enc:x", true},      // minimal encrypted value
	}
	for _, tc := range cases {
		if got := IsEncrypted(tc.input); got != tc.want {
			t.Errorf("IsEncrypted(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- Integration: SaveTo encrypts, LoadFrom decrypts ---

func TestSaveToEncryptsUserTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	key, _ := DeriveKey("admin-token")
	c := New()
	c.SetEncryptionKey(key)
	c.SetUserTokens(map[string]string{
		"user1": "secret-token-1",
		"user2": "secret-token-2",
	})

	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	// Read raw JSON and verify tokens are NOT plaintext.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var ondisk Data
	if err := json.Unmarshal(raw, &ondisk); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for uid, val := range ondisk.UserTokens {
		if !IsEncrypted(val) {
			t.Errorf("on-disk user_tokens[%s] is plaintext: %q", uid, val)
		}
		if strings.Contains(val, "secret-token") {
			t.Errorf("on-disk user_tokens[%s] contains plaintext substring", uid)
		}
	}

	// Verify in-memory state is still plaintext.
	tokens := c.UserTokens()
	if tokens["user1"] != "secret-token-1" {
		t.Errorf("in-memory token[user1] = %q, want secret-token-1", tokens["user1"])
	}
}

func TestLoadFromDecryptsUserTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	key, _ := DeriveKey("admin-token")

	// Save encrypted cache.
	orig := New()
	orig.SetEncryptionKey(key)
	orig.SetUserTokens(map[string]string{"u1": "tok1"})
	if err := orig.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	// Load into a new cache with the same key.
	loaded := New()
	loaded.SetEncryptionKey(key)
	if err := loaded.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	tokens := loaded.UserTokens()
	if tokens["u1"] != "tok1" {
		t.Errorf("LoadFrom decrypted token = %q, want tok1", tokens["u1"])
	}
}

func TestLoadFromPlaintextCacheMigrates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Write a pre-encryption plaintext cache file.
	plainData := Data{
		ProcessedEpisodes: map[string]int64{},
		LanguageProfiles:  map[string]map[string]string{},
		UserTokens:        map[string]string{"u1": "plain-token-abc"},
		LastSchedulerRun:  0,
	}
	raw, _ := json.MarshalIndent(&plainData, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Load with encryption key — plaintext should pass through.
	key, _ := DeriveKey("admin-token")
	c := New()
	c.SetEncryptionKey(key)
	if err := c.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	tokens := c.UserTokens()
	if tokens["u1"] != "plain-token-abc" {
		t.Errorf("plaintext migration: token = %q, want plain-token-abc", tokens["u1"])
	}

	// Next SaveTo should encrypt it.
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}
	raw2, _ := os.ReadFile(path)
	var ondisk Data
	if err := json.Unmarshal(raw2, &ondisk); err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(ondisk.UserTokens["u1"]) {
		t.Error("after save, on-disk token should be encrypted")
	}
}

func TestSaveToWithoutKeyStoresPlaintext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// No encryption key set — tokens remain plaintext on disk.
	c := New()
	c.SetUserTokens(map[string]string{"u1": "my-plain-token"})
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}
	raw, _ := os.ReadFile(path)
	var ondisk Data
	if err := json.Unmarshal(raw, &ondisk); err != nil {
		t.Fatal(err)
	}
	if ondisk.UserTokens["u1"] != "my-plain-token" {
		t.Errorf("without key, on-disk token = %q, want my-plain-token", ondisk.UserTokens["u1"])
	}
}

// --- DecryptToken ciphertext-length boundary ---

// TestDecryptTokenCiphertextLengthBoundary pins the minimum-length guard in
// DecryptToken. A decoded payload must be at least nonce-size + 1 byte (a
// 12-byte nonce plus at least one byte of ciphertext) before DecryptToken
// hands it to AES-GCM:
//
//   - 12 bytes is one short of the minimum, so DecryptToken rejects it with a
//     "too short" error before touching the cipher.
//   - 13 bytes clears the guard, so DecryptToken proceeds to AES-GCM, which
//     fails authentication on the bogus input and reports a "decrypt" error.
//
// Each case asserts the exact error class, so widening or narrowing the guard
// by one byte flips one case's error and fails the test.
func TestDecryptTokenCiphertextLengthBoundary(t *testing.T) {
	t.Parallel()
	// DeriveKey yields a valid 32-byte AES-256 key so aes.NewCipher succeeds
	// for the case that clears the length guard and reaches gcm.Open.
	key, err := DeriveKey("test-token")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}

	tests := []struct {
		name    string
		wantMsg string
		notMsg  string
		rawLen  int
	}{
		{
			name: "twelve bytes is too short", rawLen: 12,
			wantMsg: "too short", notMsg: "decrypt",
		},
		{
			name: "thirteen bytes reaches decrypt", rawLen: 13,
			wantMsg: "decrypt", notMsg: "too short",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// encPrefix marks the value as ciphertext so DecryptToken does not
			// take the plaintext pass-through path. RawURLEncoding round-trips
			// byte counts, so the decoded length equals rawLen exactly.
			value := encPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, tc.rawLen))

			_, err := DecryptToken(key, value)
			if err == nil {
				t.Fatalf("DecryptToken(rawLen=%d) error = nil, want error containing %q",
					tc.rawLen, tc.wantMsg)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("DecryptToken(rawLen=%d) error = %q, want substring %q",
					tc.rawLen, msg, tc.wantMsg)
			}
			if strings.Contains(msg, tc.notMsg) {
				t.Errorf("DecryptToken(rawLen=%d) error = %q, must NOT contain %q",
					tc.rawLen, msg, tc.notMsg)
			}
		})
	}
}
