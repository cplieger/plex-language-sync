package streams

import (
	"strconv"
	"testing"
)

// FuzzFlexIntUnmarshal drives FlexInt.UnmarshalJSON with arbitrary bytes
// (FlexInt decodes untrusted Plex JSON). Beyond "must not panic", it pins
// FlexInt's reason to exist with a metamorphic invariant: whatever integer a
// successful decode produced, presenting that same integer in the OTHER wire
// shape — bare number vs quoted numeric string — must decode to the identical
// value. A regression in either branch breaks the dual-shape equivalence.
func FuzzFlexIntUnmarshal(f *testing.F) {
	f.Add([]byte("1"))
	f.Add([]byte("0"))
	f.Add([]byte("-3"))
	f.Add([]byte(`"1"`))
	f.Add([]byte(`"-3"`))
	f.Add([]byte(`""`))
	f.Add([]byte("null"))
	f.Add([]byte("true"))
	f.Add([]byte("false"))
	f.Add([]byte("1.0"))
	f.Add([]byte(`"not-a-number"`))
	f.Add([]byte(`"abc`))
	f.Add([]byte(`"\q"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var got FlexInt
		if err := got.UnmarshalJSON(data); err != nil {
			return // malformed input: the only contract is "must not panic"
		}
		// On any successful decode, the same integer in bare and quoted form
		// must both round-trip back to it (the dual-shape contract).
		bare := strconv.Itoa(int(got))
		quoted := strconv.Quote(bare)

		var fromBare, fromQuoted FlexInt
		if err := fromBare.UnmarshalJSON([]byte(bare)); err != nil {
			t.Fatalf("re-decoding bare %s failed: %v", bare, err)
		}
		if err := fromQuoted.UnmarshalJSON([]byte(quoted)); err != nil {
			t.Fatalf("re-decoding quoted %s failed: %v", quoted, err)
		}
		if fromBare != got || fromQuoted != got {
			t.Errorf("dual-shape mismatch for %d: bare=%d, quoted=%d", got, fromBare, fromQuoted)
		}
	})
}
