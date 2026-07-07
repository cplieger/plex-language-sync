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
					{"id": 3, "streamType": 2, "languageCode": "jpn", "selected": true},
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

	if ep.GrandparentTitle != "Breaking Bad" {
		t.Errorf("GrandparentTitle = %q, want %q", ep.GrandparentTitle, "Breaking Bad")
	}
	if got := ep.EpisodeNum(); got != 5 {
		t.Errorf("EpisodeNum() = %d, want 5 (bare-number FlexInt in situ)", got)
	}
	if got := ep.SeasonNum(); got != 2 {
		t.Errorf("SeasonNum() = %d, want 2 (quoted-string FlexInt in situ)", got)
	}
	if got := int(ep.LibrarySectionID); got != 3 {
		t.Errorf("LibrarySectionID = %d, want 3 (quoted-string FlexInt in situ)", got)
	}
	if got := FirstPartID(&ep); got != 42 {
		t.Errorf("FirstPartID() = %d, want 42 (Media[0].Part[0].id)", got)
	}

	audio, sub := Selected(&ep)
	if audio == nil || audio.ID != 3 {
		t.Errorf("Selected() audio = %v, want stream ID=3", audio)
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
