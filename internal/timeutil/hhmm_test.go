package timeutil

import "testing"

func TestParseHHMM(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input  string
		hour   int
		minute int
		ok     bool
	}{
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{"09:05", 9, 5, true},
		{"02:30", 2, 30, true},
		{"", 0, 0, false},
		{"abc", 0, 0, false},
		{"24:00", 0, 0, false},
		{"23:60", 0, 0, false},
		{"abc:30", 0, 0, false},
		{"09:ab", 0, 0, false},
		{"0230", 0, 0, false},
		{"-1:00", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			h, m, ok := ParseHHMM(tt.input)
			if ok != tt.ok {
				t.Fatalf("ParseHHMM(%q) ok=%v, want %v", tt.input, ok, tt.ok)
			}
			if !ok {
				return
			}
			if h != tt.hour || m != tt.minute {
				t.Errorf("ParseHHMM(%q) = (%d, %d), want (%d, %d)",
					tt.input, h, m, tt.hour, tt.minute)
			}
		})
	}
}
