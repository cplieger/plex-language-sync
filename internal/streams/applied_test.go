package streams

import (
	"encoding/json"
	"testing"
)

func TestAppliedJSONRoundTrip(t *testing.T) {
	t.Run("household seed with subtitle", func(t *testing.T) {
		orig := Applied{
			Basis:      Basis{From: "bob", Version: 1700000000, Source: SourceHousehold},
			At:         1700000100,
			PartID:     5001,
			AudioID:    9001,
			SubtitleID: 9003,
		}
		raw, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"basis":{"from":"bob","v":1700000000,"src":2},"at":1700000100,"part":5001,"audio":9001,"subtitle":9003}`
		if string(raw) != want {
			t.Errorf("marshal = %s, want %s", raw, want)
		}
		var got Applied
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != orig {
			t.Errorf("round trip = %+v, want %+v", got, orig)
		}
	})

	t.Run("intent without subtitle omits from, subtitle and subtitleUnknown", func(t *testing.T) {
		orig := Applied{
			Basis:   Basis{Version: 1700000000, Source: SourceIntent},
			At:      1700000100,
			PartID:  5001,
			AudioID: 9001,
		}
		raw, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"basis":{"v":1700000000,"src":1},"at":1700000100,"part":5001,"audio":9001}`
		if string(raw) != want {
			t.Errorf("marshal = %s, want %s", raw, want)
		}
		var got Applied
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != orig {
			t.Errorf("round trip = %+v, want %+v", got, orig)
		}
	})

	t.Run("subtitle not read back after an audio write", func(t *testing.T) {
		orig := Applied{At: 1700000100, PartID: 5001, AudioID: 9001, SubtitleUnknown: true}
		raw, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"basis":{"v":0,"src":0},"at":1700000100,"part":5001,"audio":9001,"subtitleUnknown":true}`
		if string(raw) != want {
			t.Errorf("marshal = %s, want %s", raw, want)
		}
		var got Applied
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != orig {
			t.Errorf("round trip = %+v, want %+v", got, orig)
		}
	})

	t.Run("profile version keeps a negative value", func(t *testing.T) {
		orig := Applied{
			Basis:   Basis{Version: -1234567890123456789, Source: SourceProfile},
			PartID:  5001,
			AudioID: 9001,
		}
		raw, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got Applied
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != orig {
			t.Errorf("round trip = %+v, want %+v", got, orig)
		}
	})
}

func TestSourceValuesStartAtOne(t *testing.T) {
	// The zero Source marks a basis that was never set; the persisted
	// values must not shift.
	if SourceIntent != 1 || SourceHousehold != 2 || SourceProfile != 3 {
		t.Errorf("Source values = %d, %d, %d, want 1, 2, 3", SourceIntent, SourceHousehold, SourceProfile)
	}
}
