package streams

import (
	"encoding/json"
	"testing"
)

// TestFlexInt_UnmarshalJSON exercises the three valid input shapes
// (bare number, quoted numeric string, null) plus the zero-valued
// edge cases (absent field, empty quoted string) and a representative
// malformed input. Mirrors the matrix the pre-flex json.Number
// callers implicitly relied on via their strconv.Atoi fallback.
func TestFlexInt_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	type wrap struct {
		N FlexInt `json:"n"`
	}

	tests := []struct {
		name    string
		input   string
		want    FlexInt
		wantErr bool
	}{
		{"bare number", `{"n":14}`, 14, false},
		{"quoted number", `{"n":"14"}`, 14, false},
		{"bare zero", `{"n":0}`, 0, false},
		{"quoted zero", `{"n":"0"}`, 0, false},
		{"negative bare", `{"n":-3}`, -3, false},
		{"negative quoted", `{"n":"-3"}`, -3, false},
		{"null", `{"n":null}`, 0, false},
		{"absent", `{}`, 0, false},
		{"empty quoted", `{"n":""}`, 0, false},
		{"malformed quoted", `{"n":"abc"}`, 0, true},
		{"float bare", `{"n":1.5}`, 0, true},
		{"object", `{"n":{}}`, 0, true},
		{"array", `{"n":[1]}`, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got wrap
			err := json.Unmarshal([]byte(tt.input), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) err = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) err = %v, want nil", tt.input, err)
			}
			if got.N != tt.want {
				t.Errorf("Unmarshal(%s).N = %d, want %d", tt.input, got.N, tt.want)
			}
		})
	}
}

// TestFlexInt_ErrorPrefix guards inviolate item 5 (log-grep compat):
// flexInt parse failures must NOT emit the "invalid rating key"
// phrasing that plex.RatingKey.Validate owns. Conflating the two
// would break Loki alerts keyed on rating-key validation errors.
func TestFlexInt_ErrorPrefix(t *testing.T) {
	t.Parallel()
	var v FlexInt
	err := v.UnmarshalJSON([]byte(`"nope"`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if msg := err.Error(); !containsPrefix(msg, "flexint:") {
		t.Errorf("error %q must start with 'flexint:' prefix (must not reuse rating-key phrasing)", msg)
	}
}

func containsPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestFlexInt_UnmarshalJSON_EmptyInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
	}{
		{"nil slice", nil},
		{"empty slice", []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := FlexInt(7)
			if err := f.UnmarshalJSON(tt.data); err != nil {
				t.Fatalf("UnmarshalJSON(%q) err = %v, want nil", tt.data, err)
			}
			if f != 0 {
				t.Errorf("UnmarshalJSON(%q) = %d, want 0", tt.data, f)
			}
		})
	}
}

// TestFlexInt_UnmarshalJSON_MalformedQuotedString covers the quoted-string
// decode-failure branch: input that begins with a quote but is not a valid
// JSON string (unterminated, or an invalid escape) must surface a
// "flexint:"-prefixed error and never panic. This is distinct from a valid
// JSON string that merely isn't numeric ("abc"), which fails later at the
// strconv.Atoi step.
func TestFlexInt_UnmarshalJSON_MalformedQuotedString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{"unterminated string", `"abc`},
		{"invalid escape", `"\q"`},
		{"lone escaped quote", `"\"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f FlexInt
			err := f.UnmarshalJSON([]byte(tt.data))
			if err == nil {
				t.Fatalf("UnmarshalJSON(%q) err = nil, want error", tt.data)
			}
			if !containsPrefix(err.Error(), "flexint:") {
				t.Errorf("UnmarshalJSON(%q) err = %q, want 'flexint:' prefix", tt.data, err.Error())
			}
		})
	}
}
