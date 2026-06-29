package cache

import (
	"testing"

	"pgregory.net/rapid"
)

// TestEncryptDecryptRoundTripPBT is the round-trip property for the
// token-at-rest codec: decrypting an encrypted token returns the exact
// original plaintext for ANY input string. TestEncryptDecryptRoundTrip
// pins a single fixed token; this exercises empty strings, multibyte and
// control runes, long values, and values that themselves begin with the
// "enc:" prefix - the inputs most likely to expose a content-sensitivity
// bug in the base64 / AES-GCM framing.
func TestEncryptDecryptRoundTripPBT(t *testing.T) {
	key, err := DeriveKey("admin-token")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	rapid.Check(t, func(t *rapid.T) {
		plain := rapid.String().Draw(t, "plain")
		ct, err := EncryptToken(key, plain)
		if err != nil {
			t.Fatalf("EncryptToken(%q) error = %v", plain, err)
		}
		if !IsEncrypted(ct) {
			t.Errorf("EncryptToken(%q) = %q, want an enc:-prefixed value", plain, ct)
		}
		got, err := DecryptToken(key, ct)
		if err != nil {
			t.Fatalf("DecryptToken(EncryptToken(%q)) error = %v", plain, err)
		}
		if got != plain {
			t.Errorf("round-trip: DecryptToken(EncryptToken(%q)) = %q, want %q", plain, got, plain)
		}
	})
}
