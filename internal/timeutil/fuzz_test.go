package timeutil

import "testing"

func FuzzParseHHMM(f *testing.F) {
	f.Add("00:00")
	f.Add("23:59")
	f.Add("12:30")
	f.Add("")
	f.Add("99:99")
	f.Add("not-time")

	f.Fuzz(func(t *testing.T, s string) {
		hour, minute, ok := ParseHHMM(s)
		if ok {
			if hour < 0 || hour > 23 {
				t.Fatalf("ok=true but hour=%d out of [0,23]", hour)
			}
			if minute < 0 || minute > 59 {
				t.Fatalf("ok=true but minute=%d out of [0,59]", minute)
			}
		}
	})
}
