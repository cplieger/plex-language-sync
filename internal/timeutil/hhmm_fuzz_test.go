package timeutil

import (
	"fmt"
	"testing"
)

// FuzzParseHHMM pins two invariants over arbitrary input strings:
//
//   - bounded output: a successful parse always yields an in-range hour
//     [0,23] and minute [0,59];
//   - canonical round-trip: re-rendering an accepted (hour, minute) as
//     "HH:MM" and parsing that again reproduces the same values, so the
//     accept decision and the extracted values stay consistent (catches
//     e.g. a transposed hour/minute that bounds alone would not).
func FuzzParseHHMM(f *testing.F) {
	f.Add("00:00")
	f.Add("23:59")
	f.Add("12:30")
	f.Add("9:5") // non-canonical but accepted; exercises the round-trip
	f.Add("")
	f.Add("99:99")
	f.Add("not-time")

	f.Fuzz(func(t *testing.T, s string) {
		hour, minute, ok := ParseHHMM(s)
		if !ok {
			return
		}
		if hour < 0 || hour > 23 {
			t.Fatalf("ParseHHMM(%q) ok=true but hour=%d out of [0,23]", s, hour)
		}
		if minute < 0 || minute > 59 {
			t.Fatalf("ParseHHMM(%q) ok=true but minute=%d out of [0,59]", s, minute)
		}
		canonical := fmt.Sprintf("%02d:%02d", hour, minute)
		h2, m2, ok2 := ParseHHMM(canonical)
		if !ok2 || h2 != hour || m2 != minute {
			t.Fatalf("ParseHHMM(%q)=(%d,%d,true); canonical %q re-parsed to (%d,%d,%v), want (%d,%d,true)",
				s, hour, minute, canonical, h2, m2, ok2, hour, minute)
		}
	})
}
