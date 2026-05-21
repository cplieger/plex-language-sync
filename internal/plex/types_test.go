package plex

import "testing"

func TestRatingKey_StringRoundTrip(t *testing.T) {
	if RatingKey("42").String() != "42" {
		t.Errorf("RatingKey(42).String() = %q, want 42", RatingKey("42").String())
	}
	if RatingKey("").String() != "" {
		t.Errorf("empty RatingKey.String() = %q, want empty", RatingKey("").String())
	}
}

func TestRatingKey_Validate(t *testing.T) {
	tests := []struct {
		name    string
		key     RatingKey
		wantErr bool
	}{
		{"valid numeric", "12345", false},
		{"valid single digit", "0", false},
		{"empty", "", true},
		{"non-numeric", "abc", true},
		{"mixed", "12a", true},
		{"negative accepted as numeric", "-1", false}, // strconv.Atoi accepts negatives
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tt.key)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tt.key, err)
			}
		})
	}
}
