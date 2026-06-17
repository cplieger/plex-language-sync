package cache

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gk_plex_language_sync_u1_key32 returns a fixed, valid 32-byte AES-256 key.
// Used so aes.NewCipher succeeds for cases that pass the length guard and
// reach gcm.Open.
func gk_plex_language_sync_u1_key32() []byte {
	return []byte("0123456789abcdef0123456789abcdef") // 32 bytes
}

// TestGkPlexLanguageSyncU1_SaveToEncKeyNoTokensWritesNull pins the
// CONDITIONALS_BOUNDARY mutant at cache.go:146 — the `len(c.data.UserTokens) > 0`
// guard on the on-disk token-encryption block.
//
// given a zero-value Cache (UserTokens is a nil map) with an encryption key set
// when SaveTo serializes it
// then the encryption block must NOT run, leaving UserTokens as the nil map,
//
//	which marshals to JSON null.
//
// The mutant changes `> 0` to `>= 0`. Since len is always >= 0, the mutated
// guard fires even on the empty map, replacing the nil map with a freshly
// allocated non-nil empty map that marshals to {} instead of null. Asserting
// the exact serialized form is legitimate here: the package doc declares the
// on-disk JSON schema an inviolate read-forward/write-back contract.
func TestGkPlexLanguageSyncU1_SaveToEncKeyNoTokensWritesNull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Zero-value Cache: every map (including UserTokens) is nil. Setting an
	// encryption key makes the guard's first clause (c.encKey != nil) true, so
	// the outcome hinges entirely on the len(...) > 0 comparator.
	var c Cache
	c.SetEncryptionKey(gk_plex_language_sync_u1_key32())

	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	got := string(fields["user_tokens"])
	if got != "null" {
		t.Errorf("SaveTo(encKey set, nil UserTokens): user_tokens = %s, want null "+
			"(a `>= 0` mutation would allocate an empty map -> {})", got)
	}
}

// TestGkPlexLanguageSyncU1_DecryptTokenLengthBoundary pins both mutants on
// crypto.go:83 — `if len(raw) < aesGCMNonceSize+1` where aesGCMNonceSize == 12,
// so the threshold is 13:
//
//   - CONDITIONALS_BOUNDARY (col 14): `<` -> `<=`. Distinguished at len == 13:
//     original `13 < 13` is false (proceeds to gcm.Open -> "decrypt" error);
//     mutated `13 <= 13` is true ("ciphertext too short").
//   - ARITHMETIC_BASE (col 31): `+1` -> `-1`, lowering the threshold to 11.
//     Distinguished at len == 12: original `12 < 13` is true ("ciphertext too
//     short"); mutated `12 < 11` is false (proceeds to gcm.Open -> "decrypt").
//
// Each case asserts the exact error class the original produces; applying
// either mutation flips one case's error from "too short" to "decrypt" or
// vice versa.
func TestGkPlexLanguageSyncU1_DecryptTokenLengthBoundary(t *testing.T) {
	t.Parallel()
	key := gk_plex_language_sync_u1_key32()

	tests := []struct {
		name    string
		rawLen  int
		wantMsg string // substring the error MUST contain
		notMsg  string // substring the error MUST NOT contain
	}{
		{
			// Below the threshold: original returns "ciphertext too short"
			// before slicing. ARITHMETIC_BASE (threshold -> 11) makes 12 < 11
			// false, proceeding to gcm.Open on an empty ciphertext -> "decrypt".
			name: "twelve bytes is too short", rawLen: 12,
			wantMsg: "too short", notMsg: "decrypt",
		},
		{
			// Exactly the threshold: original `13 < 13` is false, so it
			// proceeds to gcm.Open (12-byte nonce + 1-byte ciphertext) ->
			// authentication failure wrapped as "decrypt". CONDITIONALS_BOUNDARY
			// (`<=`) makes `13 <= 13` true -> "ciphertext too short".
			name: "thirteen bytes reaches decrypt", rawLen: 13,
			wantMsg: "decrypt", notMsg: "too short",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// encPrefix marks the value as ciphertext so DecryptToken does not
			// take the plaintext pass-through path. The decoded byte length
			// equals rawLen exactly (RawURLEncoding round-trips byte counts).
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
