package cache

import "testing"

// FuzzDecryptToken exercises the untrusted-input boundary: DecryptToken is
// fed user_tokens values straight from cache.json, which an attacker with
// access to the /config volume can tamper with. It asserts two invariants
// over arbitrary input: (1) DecryptToken never panics (crash-safety on the
// token-at-rest boundary), and (2) any value that is NOT enc:-prefixed is
// returned verbatim with no error - the migration pass-through contract
// LoadFrom relies on. AES-GCM authentication guarantees an enc:-prefixed
// forgery cannot decrypt to a usable token, so a tampered ciphertext can
// only error here.
func FuzzDecryptToken(f *testing.F) {
	f.Add("enc:AAAA")
	f.Add("plaintext-token-abc123")
	f.Add("enc:")
	f.Add("")
	key, err := DeriveKey("admin-token")
	if err != nil {
		f.Fatalf("DeriveKey() error = %v", err)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := DecryptToken(key, value)
		if !IsEncrypted(value) && (err != nil || got != value) {
			t.Errorf("DecryptToken(non-enc %q) = (%q, %v), want (%q, nil)", value, got, err, value)
		}
	})
}
