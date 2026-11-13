package streams

import (
	"encoding/json"
	"testing"
)

// TestEpisode_JSONDecodeContract decodes a representative Plex
// /library/metadata payload straight into the wire structs and pins the
// contract the rest of the package relies on: the inviolate JSON struct
// tags, the polymorphic Media->Part->Stream nesting, and FlexInt decoding
// both wire shapes (bare 5, quoted "2") in situ. The existing FlexInt
// tests use a standalone wrap struct, so no test currently proves the tags
// on Episode/Media/Part/Stream map the real Plex field names or that
// FlexInt works when embedded in Episode.
//
// One stream id arrives QUOTED. That is /status/sessions' shape for
// Media/Part/Stream ids, and it is what Stream embedding plexapi.Stream
// buys: the field is plexapi.FlexInt, so a quoted id decodes instead of
// failing the whole MediaContainer the way the app's former `ID int` did.
//
// The payload deliberately carries fields the structs do NOT declare
// (librarySectionID, the Media id) — real Plex sends them, and the
// non-strict decoder must ignore them rather than fail.
func TestEpisode_JSONDecodeContract(t *testing.T) {
	t.Parallel()

	const payload = `{
		"ratingKey": "12345",
		"grandparentTitle": "Breaking Bad",
		"type": "episode",
		"index": 5,
		"parentIndex": "2",
		"librarySectionID": "3",
		"Media": [{
			"id": 900,
			"Part": [{
				"id": 42,
				"Stream": [
					{"id": 1, "streamType": 1, "selected": true},
					{"id": 2, "streamType": 2, "languageCode": "eng", "selected": false},
					{"id": "3", "streamType": 2, "languageCode": "jpn", "selected": true},
					{"id": 4, "streamType": 3, "languageCode": "eng", "selected": true},
					{"id": 5, "streamType": 3, "languageCode": "jpn", "selected": false}
				]
			}]
		}]
	}`

	var ep Episode
	if err := json.Unmarshal([]byte(payload), &ep); err != nil {
		t.Fatalf("json.Unmarshal(Episode) err = %v, want nil", err)
	}

	if ep.GrandparentTitle.Raw() != "Breaking Bad" {
		t.Errorf("GrandparentTitle = %q, want %q", ep.GrandparentTitle.Raw(), "Breaking Bad")
	}
	if got := ep.Num(); got != 5 {
		t.Errorf("Num() = %d, want 5 (bare-number FlexInt in situ)", got)
	}
	if got := ep.SeasonNum(); got != 2 {
		t.Errorf("SeasonNum() = %d, want 2 (quoted-string FlexInt in situ)", got)
	}
	if got := FirstPartID(&ep); got != 42 {
		t.Errorf("FirstPartID() = %d, want 42 (Media[0].Part[0].id)", got)
	}

	sel := Selected(&ep)
	audio, sub := sel.Audio, sel.Subtitle
	if audio == nil || audio.ID != 3 {
		t.Errorf("Selected() audio = %v, want stream ID=3 (quoted id \"3\" on the wire)", audio)
	}
	if got := ID(audio); got != 3 {
		t.Errorf("ID(Selected().Audio) = %d, want 3: the persisted dedup key narrows the wire FlexInt back to int", got)
	}
	if sub == nil || sub.ID != 4 {
		t.Errorf("Selected() subtitle = %v, want stream ID=4", sub)
	}
	if got := len(Audio(&ep)); got != 2 {
		t.Errorf("len(Audio()) = %d, want 2", got)
	}
	if got := len(Subtitle(&ep)); got != 2 {
		t.Errorf("len(Subtitle()) = %d, want 2", got)
	}
}

// TestDecodeStreamDefault pins Plex's library-read shape for `default`:
// the key is sent only when true, and one part can mark several
// subtitles default, so an absent key must decode false.
func TestDecodeStreamDefault(t *testing.T) {
	t.Parallel()

	const payload = `{
		"ratingKey": "1001",
		"Media": [{"Part": [{"id": 5001, "Stream": [
			{"id": 9001, "streamType": 2, "languageCode": "jpn", "default": true, "selected": true},
			{"id": 9002, "streamType": 2, "languageCode": "eng"},
			{"id": 9003, "streamType": 3, "languageCode": "eng", "default": true},
			{"id": 9004, "streamType": 3, "languageCode": "eng", "default": true},
			{"id": 9005, "streamType": 3, "languageCode": "fre"}
		]}]}]
	}`

	var ep Episode
	if err := json.Unmarshal([]byte(payload), &ep); err != nil {
		t.Fatalf("json.Unmarshal(Episode) err = %v, want nil", err)
	}
	if len(ep.Media) != 1 || len(ep.Media[0].Part) != 1 {
		t.Fatalf("decoded Media = %+v, want one media with one part", ep.Media)
	}
	got := ep.Media[0].Part[0].Stream
	want := map[int]bool{9001: true, 9002: false, 9003: true, 9004: true, 9005: false}
	if len(got) != len(want) {
		t.Fatalf("decoded %d streams, want %d", len(got), len(want))
	}
	for i := range got {
		id := int(got[i].ID)
		if got[i].Default != want[id] {
			t.Errorf("stream %d Default = %v, want %v", id, got[i].Default, want[id])
		}
	}
}

// TestDecodeLastViewedAt pins the per-token view time on an episode
// list: Plex sends it as a number on library reads, a quoted id-style
// string is tolerated, and an episode the reading user never finished
// carries no key.
func TestDecodeLastViewedAt(t *testing.T) {
	t.Parallel()

	const payload = `[
		{"ratingKey": "1001", "lastViewedAt": 1767225600},
		{"ratingKey": "1002", "lastViewedAt": "1767312000"},
		{"ratingKey": "1003"}
	]`

	var eps []Episode
	if err := json.Unmarshal([]byte(payload), &eps); err != nil {
		t.Fatalf("json.Unmarshal([]Episode) err = %v, want nil", err)
	}
	want := map[string]FlexInt{"1001": 1767225600, "1002": 1767312000, "1003": 0}
	if len(eps) != len(want) {
		t.Fatalf("decoded %d episodes, want %d", len(eps), len(want))
	}
	for i := range eps {
		if got := eps[i].LastViewedAt; got != want[eps[i].RatingKey] {
			t.Errorf("episode %s LastViewedAt = %d, want %d", eps[i].RatingKey, got, want[eps[i].RatingKey])
		}
	}
}
