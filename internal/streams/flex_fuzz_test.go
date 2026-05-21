package streams

import "testing"

func FuzzFlexIntUnmarshal(f *testing.F) {
	f.Add([]byte("1"))
	f.Add([]byte("0"))
	f.Add([]byte(`"1"`))
	f.Add([]byte("null"))
	f.Add([]byte("true"))
	f.Add([]byte("false"))
	f.Add([]byte("1.0"))
	f.Add([]byte(`"not-a-number"`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var v FlexInt
		_ = v.UnmarshalJSON(data) // must not panic
	})
}
