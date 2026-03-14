package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestMatchAudioStream(t *testing.T) {
	tests := []struct {
		name       string
		ref        *plexStream
		candidates []*plexStream
		wantID     int
	}{
		{
			name: "nil ref returns nil",
			ref:  nil,
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng"},
			},
			wantID: 0,
		},
		{
			name: "exact language match single",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "jpn", Codec: "aac"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", Codec: "aac"},
				{ID: 2, StreamType: 2, LanguageCode: "jpn", Codec: "aac"},
			},
			wantID: 2,
		},
		{
			name: "no language match returns nil",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "kor"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng"},
				{ID: 2, StreamType: 2, LanguageCode: "jpn"},
			},
			wantID: 0,
		},
		{
			name: "prefers matching codec",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", Codec: "eac3"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", Codec: "aac"},
				{ID: 2, StreamType: 2, LanguageCode: "eng", Codec: "eac3"},
			},
			wantID: 2,
		},
		{
			name: "prefers matching channel layout",
			ref: &plexStream{
				ID: 10, StreamType: 2, LanguageCode: "eng",
				Codec: "aac", AudioChannelLayout: "5.1(side)",
			},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "stereo"},
				{ID: 2, StreamType: 2, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "5.1(side)"},
			},
			wantID: 2,
		},
		{
			name: "filters out visual impaired when ref is not",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", VisualImpaired: false},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", VisualImpaired: true},
				{ID: 2, StreamType: 2, LanguageCode: "eng", VisualImpaired: false},
			},
			wantID: 2,
		},
		{
			name: "prefers visual impaired when ref is",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", VisualImpaired: true},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", VisualImpaired: false},
				{ID: 2, StreamType: 2, LanguageCode: "eng", VisualImpaired: true},
			},
			wantID: 2,
		},
		{
			name: "filters descriptive tracks",
			ref:  &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", Title: "English"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", Title: "English (Commentary)"},
				{ID: 2, StreamType: 2, LanguageCode: "eng", Title: "English"},
			},
			wantID: 2,
		},
		{
			name: "title match boosts score",
			ref: &plexStream{
				ID: 10, StreamType: 2, LanguageCode: "eng",
				DisplayTitle: "English (EAC3 5.1)",
			},
			candidates: []*plexStream{
				{ID: 1, StreamType: 2, LanguageCode: "eng", DisplayTitle: "English (AAC Stereo)"},
				{ID: 2, StreamType: 2, LanguageCode: "eng", DisplayTitle: "English (EAC3 5.1)"},
			},
			wantID: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchAudioStream(tt.ref, tt.candidates)
			gotID := 0
			if got != nil {
				gotID = got.ID
			}
			if gotID != tt.wantID {
				t.Errorf("matchAudioStream() got ID=%d, want ID=%d", gotID, tt.wantID)
			}
		})
	}
}

func TestMatchSubtitleStream(t *testing.T) {
	tests := []struct {
		name       string
		ref        *plexStream
		refAudio   *plexStream
		candidates []*plexStream
		wantID     int
	}{
		{
			name:     "nil ref and nil audio returns nil",
			ref:      nil,
			refAudio: nil,
			candidates: []*plexStream{
				{ID: 1, StreamType: 3, LanguageCode: "eng"},
			},
			wantID: 0,
		},
		{
			name:     "nil ref matches forced subs in audio language",
			ref:      nil,
			refAudio: &plexStream{ID: 10, StreamType: 2, LanguageCode: "jpn"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 3, LanguageCode: "jpn", Forced: false},
				{ID: 2, StreamType: 3, LanguageCode: "jpn", Forced: true},
				{ID: 3, StreamType: 3, LanguageCode: "eng", Forced: true},
			},
			wantID: 2,
		},
		{
			name:     "exact language match",
			ref:      &plexStream{ID: 10, StreamType: 3, LanguageCode: "eng"},
			refAudio: &plexStream{ID: 20, StreamType: 2, LanguageCode: "jpn"},
			candidates: []*plexStream{
				{ID: 1, StreamType: 3, LanguageCode: "jpn"},
				{ID: 2, StreamType: 3, LanguageCode: "eng"},
			},
			wantID: 2,
		},
		{
			name: "prefers hearing impaired when ref is",
			ref: &plexStream{
				ID: 10, StreamType: 3, LanguageCode: "eng",
				HearingImpaired: true,
			},
			refAudio: nil,
			candidates: []*plexStream{
				{ID: 1, StreamType: 3, LanguageCode: "eng", HearingImpaired: false},
				{ID: 2, StreamType: 3, LanguageCode: "eng", HearingImpaired: true},
			},
			wantID: 2,
		},
		{
			name:     "no match returns nil",
			ref:      &plexStream{ID: 10, StreamType: 3, LanguageCode: "kor"},
			refAudio: nil,
			candidates: []*plexStream{
				{ID: 1, StreamType: 3, LanguageCode: "eng"},
			},
			wantID: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchSubtitleStream(tt.ref, tt.refAudio, tt.candidates)
			gotID := 0
			if got != nil {
				gotID = got.ID
			}
			if gotID != tt.wantID {
				t.Errorf("matchSubtitleStream() got ID=%d, want ID=%d", gotID, tt.wantID)
			}
		})
	}
}

func TestFilterEpisodesAfter(t *testing.T) {
	ref := &plexEpisode{ParentIndex: "2", Index: "5"}
	episodes := []plexEpisode{
		{ParentIndex: "1", Index: "1", RatingKey: "s1e1"},
		{ParentIndex: "2", Index: "3", RatingKey: "s2e3"},
		{ParentIndex: "2", Index: "5", RatingKey: "s2e5"},
		{ParentIndex: "2", Index: "6", RatingKey: "s2e6"},
		{ParentIndex: "3", Index: "1", RatingKey: "s3e1"},
	}

	got := filterEpisodesAfter(episodes, ref)
	if len(got) != 2 {
		t.Fatalf("expected 2 episodes after S02E05, got %d", len(got))
	}
	if got[0].RatingKey != "s2e6" {
		t.Errorf("first episode should be s2e6, got %s", got[0].RatingKey)
	}
	if got[1].RatingKey != "s3e1" {
		t.Errorf("second episode should be s3e1, got %s", got[1].RatingKey)
	}
}

func TestSelectedStreams(t *testing.T) {
	ep := &plexEpisode{
		Media: []plexMedia{{
			Part: []plexPart{{
				Stream: []plexStream{
					{ID: 1, StreamType: 1, Selected: true},
					{ID: 2, StreamType: 2, Selected: false, LanguageCode: "eng"},
					{ID: 3, StreamType: 2, Selected: true, LanguageCode: "jpn"},
					{ID: 4, StreamType: 3, Selected: true, LanguageCode: "eng"},
					{ID: 5, StreamType: 3, Selected: false, LanguageCode: "jpn"},
				},
			}},
		}},
	}

	audio, sub := selectedStreams(ep)
	if audio == nil || audio.ID != 3 {
		t.Errorf("expected audio stream ID=3, got %v", audio)
	}
	if sub == nil || sub.ID != 4 {
		t.Errorf("expected subtitle stream ID=4, got %v", sub)
	}
}

func TestSelectedStreamsEmpty(t *testing.T) {
	ep := &plexEpisode{}
	audio, sub := selectedStreams(ep)
	if audio != nil || sub != nil {
		t.Error("expected nil streams for empty episode")
	}
}

func TestStreamDesc(t *testing.T) {
	if got := streamDesc(nil); got != "none" {
		t.Errorf("streamDesc(nil) = %q, want %q", got, "none")
	}
	s := &plexStream{ID: 1, ExtendedDisplayTitle: "English (EAC3 5.1)"}
	if got := streamDesc(s); got != "English (EAC3 5.1)" {
		t.Errorf("streamDesc() = %q, want %q", got, "English (EAC3 5.1)")
	}
}

func TestContainsDescriptive(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"english", false},
		{"english (commentary)", true},
		{"audio description", true},
		{"descriptive audio", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := containsDescriptive(tt.title); got != tt.want {
			t.Errorf("containsDescriptive(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

func TestEnvBool(t *testing.T) {
	tests := []struct {
		val      string
		fallback bool
		want     bool
	}{
		{"true", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
		{"", true, true},
		{"", false, false},
		{"invalid", true, true},
	}
	for _, tt := range tests {
		t.Setenv("TEST_BOOL", tt.val)
		if got := envBool("TEST_BOOL", tt.fallback); got != tt.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", tt.val, tt.fallback, got, tt.want)
		}
	}
}

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" foo , bar , , baz ")
	if len(got) != 3 || got[0] != "foo" || got[1] != "bar" || got[2] != "baz" {
		t.Errorf("splitTrim() = %v", got)
	}
	if got := splitTrim(""); len(got) != 0 {
		t.Errorf("splitTrim empty = %v", got)
	}
}

func TestParseScheduleTime(t *testing.T) {
	a := &app{cfg: &config{schedulerTime: "14:30"}}
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	got := a.parseScheduleTime(now)
	if got.Hour() != 14 || got.Minute() != 30 {
		t.Errorf("parseScheduleTime(14:30) = %02d:%02d, want 14:30", got.Hour(), got.Minute())
	}
}

func TestFilterEpisodesAfterEmpty(t *testing.T) {
	ref := &plexEpisode{ParentIndex: "1", Index: "1"}
	got := filterEpisodesAfter(nil, ref)
	if len(got) != 0 {
		t.Errorf("expected 0 episodes, got %d", len(got))
	}
}

func TestFilterEpisodesAfterSameEpisode(t *testing.T) {
	ref := &plexEpisode{ParentIndex: "1", Index: "5"}
	episodes := []plexEpisode{
		{ParentIndex: "1", Index: "5", RatingKey: "same"},
	}
	got := filterEpisodesAfter(episodes, ref)
	if len(got) != 0 {
		t.Errorf("expected 0 episodes (same as ref), got %d", len(got))
	}
}

func TestParseSharedServersXML(t *testing.T) {
	input := `<MediaContainer>
  <SharedServer id="12345" username="friend1" userID="67890" accessToken="abc123"/>
  <SharedServer id="12346" username="friend2" userID="67891" accessToken="def456"/>
</MediaContainer>`

	var result sharedServersXML
	if err := xml.Unmarshal([]byte(input), &result); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v", err)
	}
	if len(result.SharedServer) != 2 {
		t.Fatalf("expected 2 shared servers, got %d", len(result.SharedServer))
	}

	s := result.SharedServer[0]
	if s.UserID != "67890" || s.Username != "friend1" || s.AccessToken != "abc123" {
		t.Errorf("first server: got userID=%q username=%q token=%q",
			s.UserID, s.Username, s.AccessToken)
	}

	s = result.SharedServer[1]
	if s.UserID != "67891" || s.Username != "friend2" || s.AccessToken != "def456" {
		t.Errorf("second server: got userID=%q username=%q token=%q",
			s.UserID, s.Username, s.AccessToken)
	}
}

func TestParseSharedServersXMLEmpty(t *testing.T) {
	input := `<MediaContainer></MediaContainer>`
	var result sharedServersXML
	if err := xml.Unmarshal([]byte(input), &result); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v", err)
	}
	if len(result.SharedServer) != 0 {
		t.Errorf("expected 0 shared servers, got %d", len(result.SharedServer))
	}
}

func TestCacheLanguageProfilePerUser(t *testing.T) {
	var c appCache
	c.data.LanguageProfiles = make(map[string]map[string]string)

	// User 1 prefers English subs for Japanese audio.
	c.learnLanguageProfile("1", "jpn", "eng")
	// User 2 prefers no subs for Japanese audio.
	c.learnLanguageProfile("2", "jpn", "")

	lang, ok := c.getSubtitleLangForAudio("1", "jpn")
	if !ok || lang != "eng" {
		t.Errorf("user 1 jpn: got %q, %v; want eng, true", lang, ok)
	}

	lang, ok = c.getSubtitleLangForAudio("2", "jpn")
	if !ok || lang != "" {
		t.Errorf("user 2 jpn: got %q, %v; want empty, true", lang, ok)
	}

	// Unknown user returns false.
	_, ok = c.getSubtitleLangForAudio("999", "jpn")
	if ok {
		t.Error("expected false for unknown user")
	}
}

func TestUserManagerClientForUser(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		baseURL: parsed,
		admin:   userInfo{ID: "1", Name: "admin", Token: "admin-token"},
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "friend", Token: "friend-token"},
		},
		clients: make(map[string]*plexClient),
	}

	adminClient := &plexClient{baseURL: parsed, token: "admin-token"}

	// Admin user returns the admin client.
	got := um.clientForUser("1", adminClient)
	if got != adminClient {
		t.Error("expected admin client for admin userID")
	}

	// Known shared user returns a new client with their token.
	got = um.clientForUser("2", adminClient)
	if got.token != "friend-token" {
		t.Errorf("expected friend-token, got %q", got.token)
	}

	// Unknown user falls back to admin.
	got = um.clientForUser("999", adminClient)
	if got != adminClient {
		t.Error("expected admin client for unknown userID")
	}
}

// --- Tests: scoreAudioStream ---

func TestScoreAudioStream(t *testing.T) {
	tests := []struct {
		name     string
		ref, s   plexStream
		wantMin  int
		wantMore bool
	}{
		{
			name:    "same codec adds 5",
			ref:     plexStream{Codec: "eac3"},
			s:       plexStream{Codec: "eac3"},
			wantMin: 5,
		},
		{
			name:    "different codec adds 0",
			ref:     plexStream{Codec: "eac3"},
			s:       plexStream{Codec: "aac"},
			wantMin: 0,
		},
		{
			name:    "same channel layout adds 3",
			ref:     plexStream{AudioChannelLayout: "5.1(side)"},
			s:       plexStream{AudioChannelLayout: "5.1(side)"},
			wantMin: 3,
		},
		{
			name:    "low channel ref prefers more channels",
			ref:     plexStream{Channels: 2},
			s:       plexStream{Channels: 6},
			wantMin: 2,
		},
		{
			name:    "high channel ref no bonus",
			ref:     plexStream{Channels: 6},
			s:       plexStream{Channels: 2},
			wantMin: 0,
		},
		{
			name:    "matching titles add score",
			ref:     plexStream{Title: "English", DisplayTitle: "English (EAC3 5.1)"},
			s:       plexStream{Title: "English", DisplayTitle: "English (EAC3 5.1)"},
			wantMin: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreAudioStream(&tt.ref, &tt.s)
			if got < tt.wantMin {
				t.Errorf("scoreAudioStream() = %d, want >= %d", got, tt.wantMin)
			}
		})
	}
}

// --- Tests: scoreSubtitleStream ---

func TestScoreSubtitleStream(t *testing.T) {
	t.Run("nil ref returns 0", func(t *testing.T) {
		got := scoreSubtitleStream(nil, &plexStream{})
		if got != 0 {
			t.Errorf("scoreSubtitleStream(nil) = %d, want 0", got)
		}
	})

	t.Run("matching forced and HI adds 6", func(t *testing.T) {
		ref := &plexStream{Forced: true, HearingImpaired: true}
		s := &plexStream{Forced: true, HearingImpaired: true}
		got := scoreSubtitleStream(ref, s)
		if got < 6 {
			t.Errorf("scoreSubtitleStream() = %d, want >= 6", got)
		}
	})

	t.Run("mismatched forced loses 3", func(t *testing.T) {
		ref := &plexStream{Forced: true}
		s := &plexStream{Forced: false}
		got := scoreSubtitleStream(ref, s)
		// forced mismatch: no +3 for forced, but +3 for HI match (both false)
		if got > 4 {
			t.Errorf("scoreSubtitleStream() = %d, want <= 4", got)
		}
	})

	t.Run("matching codec adds 1", func(t *testing.T) {
		ref := &plexStream{Codec: "srt"}
		s := &plexStream{Codec: "srt"}
		// +3 forced match (both false) +3 HI match (both false) +1 codec
		got := scoreSubtitleStream(ref, s)
		if got != 7 {
			t.Errorf("scoreSubtitleStream() = %d, want 7", got)
		}
	})
}

// --- Tests: subtitleMatchCriteria ---

func TestSubtitleMatchCriteria(t *testing.T) {
	t.Run("nil ref nil audio", func(t *testing.T) {
		lang, forced, hi := subtitleMatchCriteria(nil, nil)
		if lang != "" || forced || hi {
			t.Errorf("got (%q, %v, %v), want empty", lang, forced, hi)
		}
	})

	t.Run("nil ref with audio returns forced", func(t *testing.T) {
		audio := &plexStream{LanguageCode: "jpn"}
		lang, forced, hi := subtitleMatchCriteria(nil, audio)
		if lang != "jpn" || !forced || hi {
			t.Errorf("got (%q, %v, %v), want (jpn, true, false)", lang, forced, hi)
		}
	})

	t.Run("ref overrides audio", func(t *testing.T) {
		ref := &plexStream{LanguageCode: "eng", Forced: false, HearingImpaired: true}
		audio := &plexStream{LanguageCode: "jpn"}
		lang, forced, hi := subtitleMatchCriteria(ref, audio)
		if lang != "eng" || forced || !hi {
			t.Errorf("got (%q, %v, %v), want (eng, false, true)", lang, forced, hi)
		}
	})
}

// --- Tests: titleMatchScore ---

func TestTitleMatchScore(t *testing.T) {
	t.Run("all titles match", func(t *testing.T) {
		ref := &plexStream{
			Title: "English", DisplayTitle: "English (EAC3)",
			ExtendedDisplayTitle: "English (EAC3 5.1)",
		}
		s := &plexStream{
			Title: "English", DisplayTitle: "English (EAC3)",
			ExtendedDisplayTitle: "English (EAC3 5.1)",
		}
		got := titleMatchScore(ref, s)
		if got != 15 {
			t.Errorf("titleMatchScore() = %d, want 15", got)
		}
	})

	t.Run("no titles match", func(t *testing.T) {
		ref := &plexStream{Title: "English"}
		s := &plexStream{Title: "Japanese"}
		got := titleMatchScore(ref, s)
		if got != 0 {
			t.Errorf("titleMatchScore() = %d, want 0", got)
		}
	})

	t.Run("empty titles no match", func(t *testing.T) {
		ref := &plexStream{}
		s := &plexStream{}
		got := titleMatchScore(ref, s)
		if got != 0 {
			t.Errorf("titleMatchScore() = %d, want 0", got)
		}
	})
}

// --- Tests: filterByLanguage ---

func TestFilterByLanguage(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, LanguageCode: "eng"},
		{ID: 2, LanguageCode: "jpn"},
		{ID: 3, LanguageCode: "eng"},
	}
	got := filterByLanguage(streams, "eng")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("unexpected IDs: %d, %d", got[0].ID, got[1].ID)
	}

	got = filterByLanguage(streams, "kor")
	if len(got) != 0 {
		t.Errorf("expected 0 for kor, got %d", len(got))
	}
}

// --- Tests: filterByBoolPref ---

func TestFilterByBoolPref(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, Forced: true},
		{ID: 2, Forced: false},
		{ID: 3, Forced: true},
	}

	t.Run("filters to matching", func(t *testing.T) {
		got := filterByBoolPref(streams, true, func(s *plexStream) bool { return s.Forced })
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("returns all when none match", func(t *testing.T) {
		all := []*plexStream{{ID: 1, Forced: false}}
		got := filterByBoolPref(all, true, func(s *plexStream) bool { return s.Forced })
		if len(got) != 1 {
			t.Fatalf("expected 1 (fallback to all), got %d", len(got))
		}
	})
}

// --- Tests: bestByScore ---

func TestBestByScore(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, Channels: 2},
		{ID: 2, Channels: 6},
		{ID: 3, Channels: 4},
	}
	got := bestByScore(streams, func(s *plexStream) int { return s.Channels })
	if got.ID != 2 {
		t.Errorf("expected ID=2 (highest channels), got ID=%d", got.ID)
	}
}

// --- Tests: firstPartID ---

func TestFirstPartID(t *testing.T) {
	t.Run("returns first part ID", func(t *testing.T) {
		ep := &plexEpisode{
			Media: []plexMedia{{Part: []plexPart{{ID: 42}}}},
		}
		if got := firstPartID(ep); got != 42 {
			t.Errorf("firstPartID() = %d, want 42", got)
		}
	})

	t.Run("returns 0 for empty", func(t *testing.T) {
		if got := firstPartID(&plexEpisode{}); got != 0 {
			t.Errorf("firstPartID() = %d, want 0", got)
		}
	})
}

// --- Tests: audioStreams / subtitleStreams ---

func TestAudioStreams(t *testing.T) {
	ep := &plexEpisode{
		Media: []plexMedia{{Part: []plexPart{{Stream: []plexStream{
			{ID: 1, StreamType: 1},
			{ID: 2, StreamType: 2, LanguageCode: "eng"},
			{ID: 3, StreamType: 3, LanguageCode: "eng"},
			{ID: 4, StreamType: 2, LanguageCode: "jpn"},
		}}}}},
	}
	got := audioStreams(ep)
	if len(got) != 2 {
		t.Fatalf("expected 2 audio streams, got %d", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 4 {
		t.Errorf("unexpected IDs: %d, %d", got[0].ID, got[1].ID)
	}
}

func TestSubtitleStreams(t *testing.T) {
	ep := &plexEpisode{
		Media: []plexMedia{{Part: []plexPart{{Stream: []plexStream{
			{ID: 1, StreamType: 2},
			{ID: 2, StreamType: 3, LanguageCode: "eng"},
			{ID: 3, StreamType: 3, LanguageCode: "jpn"},
		}}}}},
	}
	got := subtitleStreams(ep)
	if len(got) != 2 {
		t.Fatalf("expected 2 subtitle streams, got %d", len(got))
	}
}

// --- Tests: plexEpisode methods ---

func TestEpisodeMethods(t *testing.T) {
	ep := &plexEpisode{
		ParentIndex:      "2",
		Index:            "5",
		GrandparentTitle: "Breaking Bad",
	}
	if got := ep.seasonNum(); got != 2 {
		t.Errorf("seasonNum() = %d, want 2", got)
	}
	if got := ep.episodeNum(); got != 5 {
		t.Errorf("episodeNum() = %d, want 5", got)
	}
	if got := ep.shortName(); got != "'Breaking Bad' (S02E05)" {
		t.Errorf("shortName() = %q", got)
	}
}

func TestEpisodeMethodsInvalid(t *testing.T) {
	ep := &plexEpisode{ParentIndex: "abc", Index: "xyz"}
	if got := ep.seasonNum(); got != 0 {
		t.Errorf("seasonNum() = %d, want 0", got)
	}
	if got := ep.episodeNum(); got != 0 {
		t.Errorf("episodeNum() = %d, want 0", got)
	}
}

// --- Tests: cache operations ---

func TestCacheWasRecentlyProcessed(t *testing.T) {
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)

	if c.wasRecentlyProcessed("ep1") {
		t.Error("expected false for unknown key")
	}

	c.markProcessed("ep1")
	if !c.wasRecentlyProcessed("ep1") {
		t.Error("expected true after marking")
	}
}

func TestCachePruneOldEntries(t *testing.T) {
	var c appCache
	c.data.ProcessedEpisodes = map[string]int64{
		"old": time.Now().Add(-48 * time.Hour).Unix(),
		"new": time.Now().Unix(),
	}
	c.pruneOldEntries()
	if _, ok := c.data.ProcessedEpisodes["old"]; ok {
		t.Error("old entry should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["new"]; !ok {
		t.Error("new entry should be kept")
	}
}

func TestCacheLearnLanguageProfileIgnoresEmptyAudio(t *testing.T) {
	var c appCache
	c.data.LanguageProfiles = make(map[string]map[string]string)
	c.learnLanguageProfile("1", "", "eng")
	if len(c.data.LanguageProfiles) != 0 {
		t.Error("should not learn profile with empty audio lang")
	}
}

func TestCacheMarkProcessedAutoprune(t *testing.T) {
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)
	// Fill with >10000 old entries to trigger inline prune.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 10001 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.markProcessed("fresh")
	// After prune, old entries should be gone.
	if len(c.data.ProcessedEpisodes) > 2 {
		t.Errorf("expected pruned map, got %d entries", len(c.data.ProcessedEpisodes))
	}
}

func TestCacheMarkProcessedBoundary10000(t *testing.T) {
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)
	// Fill with exactly 9999 old entries. After inserting "fresh", total = 10000.
	// The threshold is > 10000 (not >=), so prune should NOT fire at exactly 10000.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 9999 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.markProcessed("fresh")
	// 9999 old + 1 fresh = 10000 entries. 10000 > 10000 is false → no prune.
	if len(c.data.ProcessedEpisodes) != 10000 {
		t.Errorf("markProcessed at exactly 10000 entries should NOT prune, got %d entries",
			len(c.data.ProcessedEpisodes))
	}
}

// --- Tests: plexStream methods ---

func TestStreamIsAudioIsSubtitle(t *testing.T) {
	audio := plexStream{StreamType: 2}
	sub := plexStream{StreamType: 3}
	video := plexStream{StreamType: 1}

	if !audio.isAudio() {
		t.Error("expected isAudio() true for StreamType 2")
	}
	if audio.isSubtitle() {
		t.Error("expected isSubtitle() false for StreamType 2")
	}
	if !sub.isSubtitle() {
		t.Error("expected isSubtitle() true for StreamType 3")
	}
	if video.isAudio() || video.isSubtitle() {
		t.Error("video stream should not be audio or subtitle")
	}
}

// --- Tests: titleForMatch ---

func TestTitleForMatch(t *testing.T) {
	tests := []struct {
		name string
		s    plexStream
		want string
	}{
		{"extended first", plexStream{ExtendedDisplayTitle: "ext", DisplayTitle: "disp", Title: "t"}, "ext"},
		{"display second", plexStream{DisplayTitle: "disp", Title: "t"}, "disp"},
		{"title last", plexStream{Title: "t"}, "t"},
		{"empty", plexStream{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.titleForMatch(); got != tt.want {
				t.Errorf("titleForMatch() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: shouldIgnoreLibrary ---

func TestShouldIgnoreLibrary(t *testing.T) {
	a := &app{cfg: &config{ignoreLibraries: []string{"Music", "Photos"}}}
	if !a.shouldIgnoreLibrary("Music") {
		t.Error("expected Music to be ignored")
	}
	if a.shouldIgnoreLibrary("TV Shows") {
		t.Error("expected TV Shows to not be ignored")
	}
	if a.shouldIgnoreLibrary("") {
		t.Error("expected empty string to not be ignored")
	}
}

// --- Tests: envOr ---

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_PLS_ENV", "custom")
	if got := envOr("TEST_PLS_ENV", "default"); got != "custom" {
		t.Errorf("envOr = %q, want custom", got)
	}
	t.Setenv("TEST_PLS_ENV", "")
	if got := envOr("TEST_PLS_ENV", "default"); got != "default" {
		t.Errorf("envOr = %q, want default", got)
	}
}

// --- Tests: loadConfig ---

func TestLoadConfig(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "season")
	t.Setenv("UPDATE_STRATEGY", "all")
	t.Setenv("TRIGGER_ON_PLAY", "true")
	t.Setenv("TRIGGER_ON_SCAN", "false")
	t.Setenv("SCHEDULER_ENABLE", "true")
	t.Setenv("LANGUAGE_PROFILES", "false")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "03:00")
	t.Setenv("IGNORE_LABELS", "SKIP,NOPE")
	t.Setenv("IGNORE_LIBRARIES", "Music,Photos")
	t.Setenv("DEBUG", "false")
	t.Setenv("SKIP_TLS_VERIFICATION", "false")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q", cfg.plexURL)
	}
	if cfg.updateLevel != "season" {
		t.Errorf("updateLevel = %q, want season", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("updateStrategy = %q, want all", cfg.updateStrategy)
	}
	if !cfg.triggerOnPlay {
		t.Error("triggerOnPlay should be true")
	}
	if cfg.triggerOnScan {
		t.Error("triggerOnScan should be false")
	}
	if cfg.languageProfiles {
		t.Error("languageProfiles should be false")
	}
	if cfg.schedulerTime != "03:00" {
		t.Errorf("schedulerTime = %q, want 03:00", cfg.schedulerTime)
	}
	if len(cfg.ignoreLabels) != 2 || cfg.ignoreLabels[0] != "SKIP" {
		t.Errorf("ignoreLabels = %v", cfg.ignoreLabels)
	}
	if len(cfg.ignoreLibraries) != 2 || cfg.ignoreLibraries[0] != "Music" {
		t.Errorf("ignoreLibraries = %v", cfg.ignoreLibraries)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")

	cfg := loadConfig()

	if cfg.updateLevel != "show" {
		t.Errorf("updateLevel = %q, want show", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("updateStrategy = %q, want all", cfg.updateStrategy)
	}
	if !cfg.triggerOnPlay {
		t.Error("triggerOnPlay should default to true")
	}
	if !cfg.triggerOnScan {
		t.Error("triggerOnScan should default to true")
	}
	if len(cfg.ignoreLabels) != 2 {
		t.Errorf("ignoreLabels should default to 2 items, got %v", cfg.ignoreLabels)
	}
}

func TestLoadConfigInvalidUpdateLevel(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "invalid")
	t.Setenv("UPDATE_STRATEGY", "invalid")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "25:99")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()

	if cfg.updateLevel != "show" {
		t.Errorf("invalid updateLevel should default to show, got %q", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("invalid updateStrategy should default to all, got %q", cfg.updateStrategy)
	}
	if cfg.schedulerTime != "02:00" {
		t.Errorf("invalid schedulerTime should default to 02:00, got %q", cfg.schedulerTime)
	}
}

// --- Tests: matchSubtitleStream edge cases ---

func TestMatchSubtitleStreamForcedOnly(t *testing.T) {
	ref := (*plexStream)(nil)
	refAudio := &plexStream{ID: 10, StreamType: 2, LanguageCode: "jpn"}
	candidates := []*plexStream{
		{ID: 1, StreamType: 3, LanguageCode: "jpn", Forced: false},
		{ID: 2, StreamType: 3, LanguageCode: "eng", Forced: true},
	}

	got := matchSubtitleStream(ref, refAudio, candidates)
	// nil ref + audio = forced-only in audio language (jpn).
	// Only ID=1 is jpn but not forced, so no forced jpn subs → nil.
	if got != nil {
		t.Errorf("expected nil (no forced jpn subs), got ID=%d", got.ID)
	}
}

func TestMatchSubtitleStreamNoLanguageMatch(t *testing.T) {
	ref := &plexStream{ID: 10, StreamType: 3, LanguageCode: "kor"}
	candidates := []*plexStream{
		{ID: 1, StreamType: 3, LanguageCode: "eng"},
		{ID: 2, StreamType: 3, LanguageCode: "jpn"},
	}
	got := matchSubtitleStream(ref, nil, candidates)
	if got != nil {
		t.Errorf("expected nil for no language match, got ID=%d", got.ID)
	}
}

func TestMatchSubtitleStreamHIOnly(t *testing.T) {
	ref := &plexStream{
		ID: 10, StreamType: 3, LanguageCode: "eng",
		HearingImpaired: true,
	}
	candidates := []*plexStream{
		{ID: 1, StreamType: 3, LanguageCode: "eng", HearingImpaired: false},
		{ID: 2, StreamType: 3, LanguageCode: "eng", HearingImpaired: true},
		{ID: 3, StreamType: 3, LanguageCode: "eng", HearingImpaired: true, Codec: "srt"},
	}
	got := matchSubtitleStream(ref, nil, candidates)
	if got == nil {
		t.Fatal("expected a match")
	}
	if !got.HearingImpaired {
		t.Errorf("expected HI subtitle, got ID=%d", got.ID)
	}
}

// --- Tests: setHealthy ---

func TestSetHealthy(t *testing.T) {
	setHealthy(true)
	if _, err := os.Stat(healthFile); err != nil {
		t.Error("health file should exist after setHealthy(true)")
	}
	setHealthy(false)
	if _, err := os.Stat(healthFile); err == nil {
		t.Error("health file should not exist after setHealthy(false)")
	}
}

// --- Tests: hasIgnoreLabel ---

func TestHasIgnoreLabel(t *testing.T) {
	tests := []struct {
		name         string
		labels       []plexLabel
		ignoreLabels []string
		want         bool
	}{
		{"no labels", nil, []string{"SKIP"}, false},
		{"no ignore list", []plexLabel{{Tag: "SKIP"}}, nil, false},
		{"match first", []plexLabel{{Tag: "SKIP"}, {Tag: "OK"}}, []string{"SKIP"}, true},
		{"match second", []plexLabel{{Tag: "OK"}, {Tag: "PLS_IGNORE"}}, []string{"PLS_IGNORE"}, true},
		{"no match", []plexLabel{{Tag: "OK"}}, []string{"SKIP", "NOPE"}, false},
		{"empty labels and ignore", nil, nil, false},
		{"multiple ignore multiple labels match", []plexLabel{{Tag: "A"}, {Tag: "B"}}, []string{"C", "B"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasIgnoreLabel(tt.labels, tt.ignoreLabels)
			if got != tt.want {
				t.Errorf("hasIgnoreLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Tests: shouldSkipSubtitleForCommentary ---

func TestShouldSkipSubtitleForCommentary(t *testing.T) {
	t.Run("nil refAudio returns false", func(t *testing.T) {
		if shouldSkipSubtitleForCommentary(nil, nil) {
			t.Error("expected false for nil refAudio")
		}
	})

	t.Run("non-commentary refAudio returns false", func(t *testing.T) {
		ref := &plexStream{ID: 1, LanguageCode: "eng", Title: "English"}
		targets := []*plexStream{
			{ID: 2, LanguageCode: "eng", Title: "English"},
		}
		if shouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false for non-commentary audio")
		}
	})

	t.Run("commentary with matching target returns false", func(t *testing.T) {
		ref := &plexStream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*plexStream{
			{ID: 2, LanguageCode: "eng", Title: "English (Commentary)"},
		}
		if shouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false when target has matching commentary track")
		}
	})

	t.Run("commentary without any language match returns true", func(t *testing.T) {
		ref := &plexStream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*plexStream{
			{ID: 2, LanguageCode: "jpn", Title: "Japanese"},
		}
		if !shouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected true when target has no audio in ref language")
		}
	})

	t.Run("commentary with same language match returns false", func(t *testing.T) {
		// matchAudioStream matches by language, so same-language non-commentary
		// track still counts as a match — subtitle changes proceed.
		ref := &plexStream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*plexStream{
			{ID: 2, LanguageCode: "eng", Title: "English"},
		}
		if shouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false when target has same-language audio")
		}
	})

	t.Run("descriptive audio without any language match returns true", func(t *testing.T) {
		ref := &plexStream{ID: 1, LanguageCode: "eng", ExtendedDisplayTitle: "Audio Description"}
		targets := []*plexStream{
			{ID: 2, LanguageCode: "jpn", ExtendedDisplayTitle: "Japanese (AAC Stereo)"},
		}
		if !shouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected true for descriptive audio without language match")
		}
	})
}

// --- Tests: streamDesc additional branches ---

func TestStreamDescAllBranches(t *testing.T) {
	tests := []struct {
		name string
		s    *plexStream
		want string
	}{
		{"nil", nil, "none"},
		{"extended title", &plexStream{ExtendedDisplayTitle: "English (EAC3 5.1)"}, "English (EAC3 5.1)"},
		{"display title", &plexStream{DisplayTitle: "English"}, "English"},
		{"title only", &plexStream{Title: "Eng"}, "Eng"},
		{"fallback to ID", &plexStream{ID: 42}, "stream-42"},
		{"ID zero", &plexStream{}, "stream-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := streamDesc(tt.s)
			if got != tt.want {
				t.Errorf("streamDesc() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: userManager.allUsers ---

func TestUserManagerAllUsers(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		baseURL: parsed,
		admin:   userInfo{ID: "1", Name: "admin"},
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "friend1", Token: "t1"},
			"3": {ID: "3", Name: "friend2", Token: "t2"},
		},
		clients: make(map[string]*plexClient),
	}

	users := um.allUsers("admin-token")
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
	if users[0].ID != "1" || users[0].Token != "admin-token" {
		t.Errorf("admin user: got ID=%q Token=%q", users[0].ID, users[0].Token)
	}
}

// --- Tests: userManager.userName ---

func TestUserManagerUserName(t *testing.T) {
	um := &userManager{
		admin: userInfo{ID: "1", Name: "admin"},
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "friend"},
		},
	}

	if got := um.userName("1"); got != "admin" {
		t.Errorf("userName(admin) = %q, want admin", got)
	}
	if got := um.userName("2"); got != "friend" {
		t.Errorf("userName(friend) = %q, want friend", got)
	}
	if got := um.userName("999"); got != "unknown-999" {
		t.Errorf("userName(unknown) = %q, want unknown-999", got)
	}
}

// --- Tests: userManager.loadFromCache ---

func TestUserManagerLoadFromCache(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		baseURL: parsed,
		admin:   userInfo{ID: "1", Name: "admin"},
		shared:  make(map[string]userInfo),
		clients: make(map[string]*plexClient),
	}

	var c appCache
	c.data.UserTokens = map[string]string{
		"1": "admin-token-should-skip",
		"2": "friend-token",
		"3": "other-token",
	}

	um.loadFromCache(&c)

	if _, ok := um.shared["1"]; ok {
		t.Error("admin user should not be added to shared")
	}
	if info, ok := um.shared["2"]; !ok || info.Token != "friend-token" {
		t.Errorf("user 2: got %+v", um.shared["2"])
	}
	if info, ok := um.shared["3"]; !ok || info.Token != "other-token" {
		t.Errorf("user 3: got %+v", um.shared["3"])
	}
}

// --- Tests: userManager.loadFromCache does not overwrite existing ---

func TestUserManagerLoadFromCacheNoOverwrite(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		baseURL: parsed,
		admin:   userInfo{ID: "1", Name: "admin"},
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "existing-friend", Token: "existing-token"},
		},
		clients: make(map[string]*plexClient),
	}

	var c appCache
	c.data.UserTokens = map[string]string{
		"2": "new-token-should-not-overwrite",
	}

	um.loadFromCache(&c)

	if um.shared["2"].Token != "existing-token" {
		t.Errorf("existing user should not be overwritten, got token=%q", um.shared["2"].Token)
	}
}

// --- Tests: userManager.clientForUser cache invalidation ---

func TestUserManagerClientForUserCacheInvalidation(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		baseURL: parsed,
		admin:   userInfo{ID: "1", Name: "admin"},
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "friend", Token: "token-v1"},
		},
		clients: make(map[string]*plexClient),
	}

	adminClient := &plexClient{baseURL: parsed, token: "admin-token"}

	// First call creates and caches a client.
	c1 := um.clientForUser("2", adminClient)
	if c1.token != "token-v1" {
		t.Fatalf("expected token-v1, got %q", c1.token)
	}

	// Same token returns cached client.
	c2 := um.clientForUser("2", adminClient)
	if c2 != c1 {
		t.Error("expected same cached client for unchanged token")
	}

	// Token change invalidates cache.
	um.shared["2"] = userInfo{ID: "2", Name: "friend", Token: "token-v2"}
	c3 := um.clientForUser("2", adminClient)
	if c3.token != "token-v2" {
		t.Errorf("expected token-v2 after update, got %q", c3.token)
	}
	if c3 == c1 {
		t.Error("expected new client after token change")
	}
}

// --- Tests: matchAudioStream edge cases ---

func TestMatchAudioStreamSingleCandidate(t *testing.T) {
	ref := &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", Codec: "aac"}
	candidates := []*plexStream{
		{ID: 1, StreamType: 2, LanguageCode: "eng", Codec: "eac3"},
	}
	got := matchAudioStream(ref, candidates)
	if got == nil || got.ID != 1 {
		t.Errorf("single candidate should be returned, got %v", got)
	}
}

func TestMatchAudioStreamDescriptiveFiltering(t *testing.T) {
	ref := &plexStream{
		ID: 10, StreamType: 2, LanguageCode: "eng",
		ExtendedDisplayTitle: "English (AAC Stereo)",
	}
	candidates := []*plexStream{
		{ID: 1, StreamType: 2, LanguageCode: "eng", ExtendedDisplayTitle: "English (Commentary)"},
		{ID: 2, StreamType: 2, LanguageCode: "eng", ExtendedDisplayTitle: "English (AAC Stereo)"},
	}
	got := matchAudioStream(ref, candidates)
	if got == nil || got.ID != 2 {
		t.Errorf("should prefer non-commentary track, got ID=%d", got.ID)
	}
}

func TestMatchAudioStreamEmptyCandidates(t *testing.T) {
	ref := &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng"}
	got := matchAudioStream(ref, nil)
	if got != nil {
		t.Error("expected nil for empty candidates")
	}
}

// --- Tests: parseScheduleTime edge cases ---

func TestParseScheduleTimeInvalid(t *testing.T) {
	a := &app{cfg: &config{schedulerTime: "invalid"}}
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	got := a.parseScheduleTime(now)
	if got.Hour() != 2 || got.Minute() != 0 {
		t.Errorf("invalid time should default to 02:00, got %02d:%02d", got.Hour(), got.Minute())
	}
}

func TestParseScheduleTimePartialInvalid(t *testing.T) {
	a := &app{cfg: &config{schedulerTime: "abc:30"}}
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	got := a.parseScheduleTime(now)
	if got.Hour() != 2 {
		t.Errorf("invalid hour should default to 2, got %d", got.Hour())
	}
	if got.Minute() != 30 {
		t.Errorf("valid minute should be 30, got %d", got.Minute())
	}
}

// --- Tests: cache getSubtitleLangForAudio edge cases ---

func TestCacheGetSubtitleLangForAudioNilProfiles(t *testing.T) {
	var c appCache
	// Don't initialize LanguageProfiles — test nil map path.
	lang, ok := c.getSubtitleLangForAudio("1", "eng")
	if ok || lang != "" {
		t.Errorf("expected empty/false for nil profiles, got %q, %v", lang, ok)
	}
}

// --- Tests: newPlexClientForUser ---

func TestNewPlexClientForUser(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	c := newPlexClientForUser(parsed, "test-token", false)
	if c.token != "test-token" {
		t.Errorf("token = %q, want test-token", c.token)
	}
	if c.baseURL != parsed {
		t.Error("baseURL should match")
	}
}

func TestNewPlexClientForUserSkipTLS(t *testing.T) {
	parsed, _ := url.Parse("https://plex:32400")
	c := newPlexClientForUser(parsed, "test-token", true)
	if c.httpClient.Transport == nil {
		t.Fatal("expected custom transport for skipTLS")
	}
}

// --- Tests: filterEpisodesAfter additional edge cases ---

func TestFilterEpisodesAfterAllBefore(t *testing.T) {
	ref := &plexEpisode{ParentIndex: "5", Index: "10"}
	episodes := []plexEpisode{
		{ParentIndex: "1", Index: "1"},
		{ParentIndex: "3", Index: "5"},
		{ParentIndex: "5", Index: "9"},
	}
	got := filterEpisodesAfter(episodes, ref)
	if len(got) != 0 {
		t.Errorf("expected 0 episodes after S05E10, got %d", len(got))
	}
}

func TestFilterEpisodesAfterAllAfter(t *testing.T) {
	ref := &plexEpisode{ParentIndex: "1", Index: "1"}
	episodes := []plexEpisode{
		{ParentIndex: "1", Index: "2", RatingKey: "e2"},
		{ParentIndex: "2", Index: "1", RatingKey: "e3"},
	}
	got := filterEpisodesAfter(episodes, ref)
	if len(got) != 2 {
		t.Errorf("expected 2 episodes, got %d", len(got))
	}
}

// --- Tests: requireEnv with _FILE ---

func TestRequireEnvFromFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("  my-secret-value  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", secretFile)

	got := requireEnv("TEST_SECRET")
	if got != "my-secret-value" {
		t.Errorf("requireEnv via _FILE = %q, want %q", got, "my-secret-value")
	}
}

// --- Tests: logConfig (smoke test — no panic) ---

func TestLogConfig(t *testing.T) {
	cfg := &config{
		plexURL:        "http://plex:32400",
		plexToken:      "test-token",
		updateLevel:    "show",
		updateStrategy: "all",
		schedulerTime:  "02:00",
		ignoreLabels:   []string{"SKIP"},
	}
	logConfig(cfg)
}

// --- Tests: learnLanguageProfile idempotent ---

func TestCacheLearnLanguageProfileIdempotent(t *testing.T) {
	var c appCache
	c.data.LanguageProfiles = make(map[string]map[string]string)

	c.learnLanguageProfile("1", "jpn", "eng")
	c.learnLanguageProfile("1", "jpn", "eng") // same value — should not log again

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("expected eng, got %q", lang)
	}
}

// --- Tests: markProcessed nil map initialization ---

func TestCacheMarkProcessedNilMap(t *testing.T) {
	var c appCache
	// Don't initialize ProcessedEpisodes — test nil map path.
	c.markProcessed("test-key")
	if !c.wasRecentlyProcessed("test-key") {
		t.Error("expected true after markProcessed on nil map")
	}
}

// --- Tests: matchAudioStream with visual impaired preference ---

func TestMatchAudioStreamVisualImpairedPreference(t *testing.T) {
	t.Run("VI ref prefers VI candidate", func(t *testing.T) {
		ref := &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", VisualImpaired: true}
		candidates := []*plexStream{
			{ID: 1, StreamType: 2, LanguageCode: "eng", VisualImpaired: false},
			{ID: 2, StreamType: 2, LanguageCode: "eng", VisualImpaired: true},
		}
		got := matchAudioStream(ref, candidates)
		if got == nil || got.ID != 2 {
			t.Errorf("expected VI track ID=2, got %v", got)
		}
	})

	t.Run("non-VI ref filters out VI", func(t *testing.T) {
		ref := &plexStream{ID: 10, StreamType: 2, LanguageCode: "eng", VisualImpaired: false}
		candidates := []*plexStream{
			{ID: 1, StreamType: 2, LanguageCode: "eng", VisualImpaired: true},
			{ID: 2, StreamType: 2, LanguageCode: "eng", VisualImpaired: false},
		}
		got := matchAudioStream(ref, candidates)
		if got == nil || got.ID != 2 {
			t.Errorf("expected non-VI track ID=2, got %v", got)
		}
	})
}

// --- Tests: matchSubtitleStream with multiple forced subs ---

func TestMatchSubtitleStreamMultipleForced(t *testing.T) {
	ref := (*plexStream)(nil)
	refAudio := &plexStream{ID: 10, StreamType: 2, LanguageCode: "jpn"}
	candidates := []*plexStream{
		{ID: 1, StreamType: 3, LanguageCode: "jpn", Forced: true, Codec: "srt"},
		{ID: 2, StreamType: 3, LanguageCode: "jpn", Forced: true, Codec: "ass"},
	}
	got := matchSubtitleStream(ref, refAudio, candidates)
	if got == nil {
		t.Fatal("expected a forced sub match")
	}
	if !got.Forced {
		t.Error("expected forced subtitle")
	}
}

// --- Tests: scoreAudioStream channel preference ---

func TestScoreAudioStreamChannelPreference(t *testing.T) {
	t.Run("low channel ref prefers higher channels", func(t *testing.T) {
		ref := &plexStream{Channels: 2}
		low := &plexStream{Channels: 2}
		high := &plexStream{Channels: 6}
		scoreLow := scoreAudioStream(ref, low)
		scoreHigh := scoreAudioStream(ref, high)
		if scoreHigh <= scoreLow {
			t.Errorf("6ch (%d) should score higher than 2ch (%d) for 2ch ref", scoreHigh, scoreLow)
		}
	})

	t.Run("high channel ref no bonus for lower", func(t *testing.T) {
		ref := &plexStream{Channels: 8, Codec: "eac3", AudioChannelLayout: "7.1"}
		s := &plexStream{Channels: 2, Codec: "aac", AudioChannelLayout: "stereo"}
		score := scoreAudioStream(ref, s)
		if score != 0 {
			t.Errorf("expected 0 for lower channels with high ref and different codec/layout, got %d", score)
		}
	})
}

// --- Tests: scoreSubtitleStream comprehensive ---

func TestScoreSubtitleStreamComprehensive(t *testing.T) {
	t.Run("all fields match", func(t *testing.T) {
		ref := &plexStream{
			Forced: true, HearingImpaired: true, Codec: "srt",
			Title: "English", DisplayTitle: "English SDH",
			ExtendedDisplayTitle: "English SDH (SRT)",
		}
		s := &plexStream{
			Forced: true, HearingImpaired: true, Codec: "srt",
			Title: "English", DisplayTitle: "English SDH",
			ExtendedDisplayTitle: "English SDH (SRT)",
		}
		got := scoreSubtitleStream(ref, s)
		// Expected: forced(3) + HI(3) + codec(1) + title(5) + display(5) + extended(5).
		if got != 22 {
			t.Errorf("all match score = %d, want 22", got)
		}
	})

	t.Run("nothing matches", func(t *testing.T) {
		ref := &plexStream{Forced: true, HearingImpaired: true, Codec: "srt"}
		s := &plexStream{Forced: false, HearingImpaired: false, Codec: "ass"}
		got := scoreSubtitleStream(ref, s)
		if got != 0 {
			t.Errorf("nothing match score = %d, want 0", got)
		}
	})
}

// --- Tests: containsDescriptive additional terms ---

func TestContainsDescriptiveAllTerms(t *testing.T) {
	terms := []string{
		"commentary", "description", "descriptive",
		"narration", "narrative", "described",
	}
	for _, term := range terms {
		if !containsDescriptive(term) {
			t.Errorf("containsDescriptive(%q) should be true", term)
		}
	}
	if containsDescriptive("normal audio track") {
		t.Error("normal track should not be descriptive")
	}
}

// --- Tests: plexEpisode.shortName formatting ---

func TestEpisodeShortNameFormatting(t *testing.T) {
	tests := []struct {
		name string
		ep   plexEpisode
		want string
	}{
		{
			"single digit season and episode",
			plexEpisode{ParentIndex: "1", Index: "3", GrandparentTitle: "Show"},
			"'Show' (S01E03)",
		},
		{
			"double digit",
			plexEpisode{ParentIndex: "12", Index: "24", GrandparentTitle: "Big Show"},
			"'Big Show' (S12E24)",
		},
		{
			"invalid numbers",
			plexEpisode{ParentIndex: "abc", Index: "xyz", GrandparentTitle: "Bad"},
			"'Bad' (S00E00)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ep.shortName()
			if got != tt.want {
				t.Errorf("shortName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: audioStreams / subtitleStreams nil guards ---

func TestAudioStreamsEmpty(t *testing.T) {
	ep := &plexEpisode{}
	got := audioStreams(ep)
	if got != nil {
		t.Errorf("expected nil for empty episode, got %d streams", len(got))
	}
}

func TestAudioStreamsEmptyParts(t *testing.T) {
	ep := &plexEpisode{Media: []plexMedia{{}}}
	got := audioStreams(ep)
	if got != nil {
		t.Errorf("expected nil for empty parts, got %d streams", len(got))
	}
}

func TestSubtitleStreamsEmpty(t *testing.T) {
	ep := &plexEpisode{}
	got := subtitleStreams(ep)
	if got != nil {
		t.Errorf("expected nil for empty episode, got %d streams", len(got))
	}
}

func TestSubtitleStreamsEmptyParts(t *testing.T) {
	ep := &plexEpisode{Media: []plexMedia{{}}}
	got := subtitleStreams(ep)
	if got != nil {
		t.Errorf("expected nil for empty parts, got %d streams", len(got))
	}
}

// --- Tests: loadConfig with _FILE secrets ---

func TestLoadConfigWithFileSecrets(t *testing.T) {
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "plex_url.txt")
	tokenFile := filepath.Join(dir, "plex_token.txt")
	if err := os.WriteFile(urlFile, []byte("http://plex:32400\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PLEX_URL", "")
	t.Setenv("PLEX_TOKEN", "")
	t.Setenv("PLEX_URL_FILE", urlFile)
	t.Setenv("PLEX_TOKEN_FILE", tokenFile)
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q, want http://plex:32400", cfg.plexURL)
	}
	if cfg.plexToken != "secret-token" {
		t.Errorf("plexToken = %q, want secret-token", cfg.plexToken)
	}
}

// --- Tests: learnLanguageProfile update existing ---

func TestCacheLearnLanguageProfileUpdate(t *testing.T) {
	var c appCache
	c.data.LanguageProfiles = make(map[string]map[string]string)

	c.learnLanguageProfile("1", "jpn", "eng")
	if c.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Fatal("initial profile not set")
	}

	c.learnLanguageProfile("1", "jpn", "fre")
	if c.data.LanguageProfiles["1"]["jpn"] != "fre" {
		t.Errorf("profile should update to fre, got %q", c.data.LanguageProfiles["1"]["jpn"])
	}
}

// --- Tests: selectedStreams with no selected streams ---

func TestSelectedStreamsNoSelection(t *testing.T) {
	ep := &plexEpisode{
		Media: []plexMedia{{
			Part: []plexPart{{
				Stream: []plexStream{
					{ID: 1, StreamType: 2, Selected: false},
					{ID: 2, StreamType: 3, Selected: false},
				},
			}},
		}},
	}
	audio, sub := selectedStreams(ep)
	if audio != nil {
		t.Error("expected nil audio when nothing selected")
	}
	if sub != nil {
		t.Error("expected nil subtitle when nothing selected")
	}
}

// --- Tests: bestByScore with single element ---

func TestBestByScoreSingle(t *testing.T) {
	streams := []*plexStream{{ID: 1}}
	got := bestByScore(streams, func(s *plexStream) int { return 0 })
	if got.ID != 1 {
		t.Errorf("expected ID=1, got ID=%d", got.ID)
	}
}

// --- Tests: filterByLanguage empty language ---

func TestFilterByLanguageEmptyCode(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, LanguageCode: "eng"},
		{ID: 2, LanguageCode: ""},
	}
	got := filterByLanguage(streams, "")
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("expected stream with empty language code, got %v", got)
	}
}

// --- Tests: titleMatchScore partial matches ---

func TestTitleMatchScorePartial(t *testing.T) {
	ref := &plexStream{
		Title:                "English",
		DisplayTitle:         "English (EAC3)",
		ExtendedDisplayTitle: "English (EAC3 5.1)",
	}
	s := &plexStream{
		Title:                "English",
		DisplayTitle:         "English (AAC)",
		ExtendedDisplayTitle: "English (AAC Stereo)",
	}
	got := titleMatchScore(ref, s)
	if got != 5 {
		t.Errorf("only Title matches, expected 5, got %d", got)
	}
}

// --- Tests: loadConfig debug mode ---

func TestLoadConfigDebugMode(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "true")
	t.Setenv("SKIP_TLS_VERIFICATION", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")

	cfg := loadConfig()
	if !cfg.debug {
		t.Error("debug should be true")
	}
}

// --- Tests: loadConfig scheduler time no colon ---

func TestLoadConfigSchedulerTimeNoColon(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "1430")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()
	if cfg.schedulerTime != "02:00" {
		t.Errorf("no-colon time should default to 02:00, got %q", cfg.schedulerTime)
	}
}

// --- Tests: newApp ---

func TestNewApp(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	client := &plexClient{baseURL: parsed, token: "test"}
	cfg := &config{updateLevel: "show"}
	identity := &serverIdentity{FriendlyName: "test"}
	admin := &plexUser{ID: "1", Name: "admin"}

	a := newApp(client, cfg, identity, admin)
	if a.client != client {
		t.Error("client mismatch")
	}
	if a.cfg != cfg {
		t.Error("cfg mismatch")
	}
	if a.identity != identity {
		t.Error("identity mismatch")
	}
	if a.admin != admin {
		t.Error("admin mismatch")
	}
}

// --- Tests: userManager.init ---

func TestUserManagerInit(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	admin := &plexUser{ID: "1", Name: "admin"}

	var um userManager
	um.init(admin, parsed, true)

	if um.admin.ID != "1" || um.admin.Name != "admin" {
		t.Errorf("admin = %+v", um.admin)
	}
	if um.baseURL != parsed {
		t.Error("baseURL mismatch")
	}
	if !um.skipTLS {
		t.Error("skipTLS should be true")
	}
	if um.shared == nil {
		t.Error("shared should be initialized")
	}
	if um.clients == nil {
		t.Error("clients should be initialized")
	}
}

// --- Tests: userManager.init preserves existing shared ---

func TestUserManagerInitPreservesShared(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	um := &userManager{
		shared: map[string]userInfo{
			"2": {ID: "2", Name: "existing"},
		},
	}
	admin := &plexUser{ID: "1", Name: "admin"}
	um.init(admin, parsed, false)

	if _, ok := um.shared["2"]; !ok {
		t.Error("existing shared user should be preserved")
	}
}

// --- Tests: newHTTPClient ---

func TestNewHTTPClientNoTLS(t *testing.T) {
	c := newHTTPClient(false)
	if c.Transport != nil {
		t.Error("expected nil transport for no TLS skip")
	}
}

func TestNewHTTPClientSkipTLS(t *testing.T) {
	c := newHTTPClient(true)
	if c.Transport == nil {
		t.Fatal("expected non-nil transport for TLS skip")
	}
}

// --- Tests: handleNotification dispatch ---

func TestHandleNotificationDisabledTriggers(t *testing.T) {
	a := &app{cfg: &config{
		triggerOnPlay: false,
		triggerOnScan: false,
	}}
	ctx := t.Context()

	// All triggers disabled — should not panic with any notification type.
	types := []string{"playing", "timeline", "unknown"}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			notif := &wsNotification{}
			notif.NotificationContainer.Type = typ
			a.handleNotification(ctx, notif)
		})
	}
}

func TestHandleNotificationPlayingEnabled(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	a := &app{
		client: &plexClient{baseURL: parsed, token: "test"},
		cfg: &config{
			triggerOnPlay: true,
		},
		admin: &plexUser{ID: "1", Name: "admin"},
	}
	a.cache.data.ProcessedEpisodes = make(map[string]int64)
	ctx := t.Context()

	// Empty events — exercises the handlePlaying loop (0 iterations).
	notif := &wsNotification{}
	notif.NotificationContainer.Type = "playing"
	a.handleNotification(ctx, notif)
}

func TestHandleNotificationTimelineEnabled(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	a := &app{
		client: &plexClient{baseURL: parsed, token: "test"},
		cfg: &config{
			triggerOnScan: true,
		},
	}
	a.cache.data.ProcessedEpisodes = make(map[string]int64)
	ctx := t.Context()

	notif := &wsNotification{}
	notif.NotificationContainer.Type = "timeline"
	a.handleNotification(ctx, notif)
}

// --- Tests: handlePlaying filters non-playing states ---

func TestHandlePlayingFiltersStates(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	a := &app{
		client: &plexClient{baseURL: parsed, token: "test"},
		cfg:    &config{triggerOnPlay: true},
		admin:  &plexUser{ID: "1", Name: "admin"},
	}
	a.cache.data.ProcessedEpisodes = make(map[string]int64)
	a.users.init(a.admin, parsed, false)
	ctx := t.Context()

	events := []wsPlayEvent{
		{State: "stopped", RatingKey: "123"},
		{State: "buffering", RatingKey: "456"},
		{State: "playing", RatingKey: ""},
	}
	a.handlePlaying(ctx, events)
}

// --- Tests: handleTimeline filters non-episode types ---

func TestHandleTimelineFiltersNonEpisode(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	a := &app{
		client: &plexClient{baseURL: parsed, token: "test"},
		cfg:    &config{triggerOnScan: true},
	}
	a.cache.data.ProcessedEpisodes = make(map[string]int64)
	ctx := t.Context()

	entries := []wsTimelineEntry{
		{Type: 1, MetadataState: stateCreated, ItemID: "123"},
		{Type: plexTypeEpisode, MetadataState: "deleted", ItemID: "456"},
		{Type: plexTypeEpisode, MetadataState: stateCreated, ItemID: ""},
	}
	a.handleTimeline(ctx, entries)
}

// --- Tests: handleTimeline cache dedup ---

func TestHandleTimelineCacheDedup(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	a := &app{
		client: &plexClient{baseURL: parsed, token: "test"},
		cfg:    &config{triggerOnScan: true},
	}
	a.cache.data.ProcessedEpisodes = make(map[string]int64)

	// Pre-mark as recently processed.
	a.cache.markProcessed("timeline:999")

	ctx := t.Context()
	entries := []wsTimelineEntry{
		{Type: plexTypeEpisode, MetadataState: stateCreated, ItemID: "999"},
	}
	a.handleTimeline(ctx, entries)
}

// ---------------------------------------------------------------------------
// Property-based tests (rapid)
// ---------------------------------------------------------------------------

func TestScoreAudioStreamNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ref := &plexStream{
			Codec:                rapid.StringMatching(`[a-z0-9]{0,10}`).Draw(t, "ref_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{0,15}`).Draw(t, "ref_layout"),
			Channels:             rapid.IntRange(0, 16).Draw(t, "ref_channels"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "ref_ext"),
		}
		s := &plexStream{
			Codec:                rapid.StringMatching(`[a-z0-9]{0,10}`).Draw(t, "s_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{0,15}`).Draw(t, "s_layout"),
			Channels:             rapid.IntRange(0, 16).Draw(t, "s_channels"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "s_ext"),
		}
		score := scoreAudioStream(ref, s)
		if score < 0 {
			t.Errorf("scoreAudioStream() = %d, want >= 0", score)
		}
	})
}

func TestScoreSubtitleStreamNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ref := &plexStream{
			Forced:               rapid.Bool().Draw(t, "ref_forced"),
			HearingImpaired:      rapid.Bool().Draw(t, "ref_hi"),
			Codec:                rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "ref_codec"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "ref_ext"),
		}
		s := &plexStream{
			Forced:               rapid.Bool().Draw(t, "s_forced"),
			HearingImpaired:      rapid.Bool().Draw(t, "s_hi"),
			Codec:                rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "s_codec"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "s_ext"),
		}
		score := scoreSubtitleStream(ref, s)
		if score < 0 {
			t.Errorf("scoreSubtitleStream() = %d, want >= 0", score)
		}
	})
}

func TestScoreAudioStreamSelfMaximal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := &plexStream{
			Codec:                rapid.StringMatching(`[a-z0-9]{1,10}`).Draw(t, "codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{1,15}`).Draw(t, "layout"),
			Channels:             rapid.IntRange(1, 16).Draw(t, "channels"),
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "ext"),
		}
		selfScore := scoreAudioStream(s, s)
		other := &plexStream{
			Codec:                rapid.StringMatching(`[a-z0-9]{1,10}`).Draw(t, "other_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{1,15}`).Draw(t, "other_layout"),
			Channels:             rapid.IntRange(1, 16).Draw(t, "other_channels"),
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "other_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "other_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "other_ext"),
		}
		otherScore := scoreAudioStream(s, other)
		if otherScore > selfScore {
			t.Errorf("scoreAudioStream(s, other)=%d > scoreAudioStream(s, s)=%d",
				otherScore, selfScore)
		}
	})
}

func TestTitleMatchScoreSelfMaximal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := &plexStream{
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "ext"),
		}
		selfScore := titleMatchScore(s, s)
		if selfScore != 15 {
			t.Errorf("titleMatchScore(s, s) = %d, want 15 (all non-empty titles match)", selfScore)
		}
	})
}

func TestMatchAudioStreamNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 5).Draw(t, "n_candidates")
		candidates := make([]*plexStream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &plexStream{
				ID:           i + 1,
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, fmt.Sprintf("lang_%d", i)),
			}
		}
		var ref *plexStream
		if rapid.Bool().Draw(t, "has_ref") {
			ref = &plexStream{
				ID:           100,
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, "ref_lang"),
			}
		}
		// Must not panic.
		matchAudioStream(ref, candidates)
	})
}

func TestMatchSubtitleStreamNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 5).Draw(t, "n_candidates")
		candidates := make([]*plexStream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &plexStream{
				ID:              i + 1,
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, fmt.Sprintf("lang_%d", i)),
				Forced:          rapid.Bool().Draw(t, fmt.Sprintf("forced_%d", i)),
				HearingImpaired: rapid.Bool().Draw(t, fmt.Sprintf("hi_%d", i)),
			}
		}
		var ref *plexStream
		if rapid.Bool().Draw(t, "has_ref") {
			ref = &plexStream{
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, "ref_lang"),
				Forced:          rapid.Bool().Draw(t, "ref_forced"),
				HearingImpaired: rapid.Bool().Draw(t, "ref_hi"),
			}
		}
		var refAudio *plexStream
		if rapid.Bool().Draw(t, "has_ref_audio") {
			refAudio = &plexStream{
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, "ref_audio_lang"),
			}
		}
		matchSubtitleStream(ref, refAudio, candidates)
	})
}

func TestContainsDescriptiveNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.String().Draw(t, "title")
		containsDescriptive(title)
	})
}

func TestFilterEpisodesAfterNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nEps := rapid.IntRange(0, 10).Draw(t, "n_episodes")
		episodes := make([]plexEpisode, nEps)
		for i := range nEps {
			episodes[i] = plexEpisode{
				ParentIndex: json.Number(fmt.Sprintf("%d", rapid.IntRange(0, 20).Draw(t, fmt.Sprintf("season_%d", i)))),
				Index:       json.Number(fmt.Sprintf("%d", rapid.IntRange(0, 30).Draw(t, fmt.Sprintf("ep_%d", i)))),
			}
		}
		ref := &plexEpisode{
			ParentIndex: json.Number(fmt.Sprintf("%d", rapid.IntRange(0, 20).Draw(t, "ref_season"))),
			Index:       json.Number(fmt.Sprintf("%d", rapid.IntRange(0, 30).Draw(t, "ref_ep"))),
		}
		got := filterEpisodesAfter(episodes, ref)
		// Invariant: all returned episodes must be strictly after the reference.
		refS := ref.seasonNum()
		refE := ref.episodeNum()
		for _, ep := range got {
			s := ep.seasonNum()
			e := ep.episodeNum()
			if s < refS || (s == refS && e <= refE) {
				t.Errorf("filterEpisodesAfter returned S%02dE%02d which is not after S%02dE%02d",
					s, e, refS, refE)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Cache load/save round-trip tests
// ---------------------------------------------------------------------------

func TestCacheLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Build cache data.
	original := &appCache{}
	original.data.ProcessedEpisodes = map[string]int64{
		"play:1:100": time.Now().Unix(),
		"play:2:200": time.Now().Unix(),
	}
	original.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng", "eng": ""},
		"2": {"kor": "eng"},
	}
	original.data.UserTokens = map[string]string{
		"2": "friend-token",
		"3": "other-token",
	}
	original.data.LastSchedulerRun = time.Now().Unix()

	// Write via JSON (simulating save to custom path).
	data, err := json.MarshalIndent(&original.data, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load into a new cache by reading the file directly.
	loaded := &appCache{}
	loaded.data.ProcessedEpisodes = make(map[string]int64)
	loaded.data.LanguageProfiles = make(map[string]map[string]string)
	loaded.data.UserTokens = make(map[string]string)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &loaded.data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify round-trip.
	if len(loaded.data.ProcessedEpisodes) != 2 {
		t.Errorf("ProcessedEpisodes: got %d, want 2", len(loaded.data.ProcessedEpisodes))
	}
	if loaded.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng",
			loaded.data.LanguageProfiles["1"]["jpn"])
	}
	if loaded.data.LanguageProfiles["1"]["eng"] != "" {
		t.Errorf("LanguageProfiles[1][eng] = %q, want empty",
			loaded.data.LanguageProfiles["1"]["eng"])
	}
	if loaded.data.UserTokens["2"] != "friend-token" {
		t.Errorf("UserTokens[2] = %q, want friend-token", loaded.data.UserTokens["2"])
	}
	if loaded.data.LastSchedulerRun != original.data.LastSchedulerRun {
		t.Errorf("LastSchedulerRun = %d, want %d",
			loaded.data.LastSchedulerRun, original.data.LastSchedulerRun)
	}
}

func TestCacheLoadNonExistentFile(t *testing.T) {
	// appCache.load() returns nil for non-existent file.
	var c appCache
	// The default cachePath is /config/cache.json which won't exist in test.
	// We test the behavior by calling load() — it should not error for ENOENT.
	err := c.load()
	// In test environment, /config/cache.json doesn't exist, so this should
	// return nil (not an error) per the os.ErrNotExist check.
	if err != nil {
		t.Errorf("load() on non-existent file = %v, want nil", err)
	}
	if c.data.ProcessedEpisodes == nil {
		t.Error("ProcessedEpisodes should be initialized even on missing file")
	}
	if c.data.LanguageProfiles == nil {
		t.Error("LanguageProfiles should be initialized even on missing file")
	}
	if c.data.UserTokens == nil {
		t.Error("UserTokens should be initialized even on missing file")
	}
}

func TestCacheDataJSONRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nEntries := rapid.IntRange(0, 5).Draw(t, "n_entries")
		processed := make(map[string]int64, nEntries)
		for i := range nEntries {
			key := rapid.StringMatching(`[a-z:0-9]{1,20}`).Draw(t, fmt.Sprintf("key_%d", i))
			processed[key] = int64(rapid.IntRange(0, 2000000000).Draw(t, fmt.Sprintf("ts_%d", i)))
		}

		original := cacheData{
			ProcessedEpisodes: processed,
			LanguageProfiles:  make(map[string]map[string]string),
			UserTokens:        make(map[string]string),
			LastSchedulerRun:  int64(rapid.IntRange(0, 2000000000).Draw(t, "last_run")),
		}

		data, err := json.Marshal(&original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded cacheData
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if len(decoded.ProcessedEpisodes) != len(original.ProcessedEpisodes) {
			t.Errorf("ProcessedEpisodes length: got %d, want %d",
				len(decoded.ProcessedEpisodes), len(original.ProcessedEpisodes))
		}
		if decoded.LastSchedulerRun != original.LastSchedulerRun {
			t.Errorf("LastSchedulerRun: got %d, want %d",
				decoded.LastSchedulerRun, original.LastSchedulerRun)
		}
	})
}

// ---------------------------------------------------------------------------
// Additional edge case tests
// ---------------------------------------------------------------------------

func TestSplitTrimEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single item", "foo", []string{"foo"}},
		{"trailing comma", "foo,bar,", []string{"foo", "bar"}},
		{"leading comma", ",foo,bar", []string{"foo", "bar"}},
		{"only commas", ",,,", nil},
		{"only spaces", "   ", nil},
		{"spaces and commas", " , , , ", nil},
		{"unicode", "日本語, 한국어", []string{"日本語", "한국어"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitTrim(tt.input)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("splitTrim(%q) = %v (len %d), want %v (len %d)",
					tt.input, got, len(got), tt.want, len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitTrim(%q)[%d] = %q, want %q",
						tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEnvBoolCaseInsensitive(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"TRUE", true},
		{"True", true},
		{"YES", true},
		{"Yes", true},
		{"FALSE", false},
		{"False", false},
		{"NO", false},
		{"No", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("TEST_CASE_BOOL", tt.val)
			got := envBool("TEST_CASE_BOOL", !tt.want)
			if got != tt.want {
				t.Errorf("envBool(%q, %v) = %v, want %v",
					tt.val, !tt.want, got, tt.want)
			}
		})
	}
}

func TestLoadConfigInvalidUpdateStrategy(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "show")
	t.Setenv("UPDATE_STRATEGY", "bogus")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()
	if cfg.updateStrategy != "all" {
		t.Errorf("invalid updateStrategy should default to all, got %q", cfg.updateStrategy)
	}
}

func TestLoadConfigValidNextStrategy(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "show")
	t.Setenv("UPDATE_STRATEGY", "next")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "true")

	cfg := loadConfig()
	if cfg.updateStrategy != "next" {
		t.Errorf("loadConfig() updateStrategy = %q, want next", cfg.updateStrategy)
	}
	if !cfg.skipTLSVerification {
		t.Error("loadConfig() skipTLSVerification should be true")
	}
}

func TestLoadConfigSchedulerTimeOutOfRange(t *testing.T) {
	tests := []struct {
		name string
		time string
	}{
		{"hour too high", "24:00"},
		{"minute too high", "12:60"},
		{"negative hour", "-1:00"},
		{"negative minute", "12:-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PLEX_URL", "http://plex:32400")
			t.Setenv("PLEX_TOKEN", "test-token")
			t.Setenv("UPDATE_LEVEL", "")
			t.Setenv("UPDATE_STRATEGY", "")
			t.Setenv("SCHEDULER_SCHEDULE_TIME", tt.time)
			t.Setenv("PLEX_URL_FILE", "")
			t.Setenv("PLEX_TOKEN_FILE", "")
			t.Setenv("IGNORE_LABELS", "")
			t.Setenv("IGNORE_LIBRARIES", "")
			t.Setenv("DEBUG", "")
			t.Setenv("SKIP_TLS_VERIFICATION", "")

			cfg := loadConfig()
			if cfg.schedulerTime != "02:00" {
				t.Errorf("loadConfig(%q).schedulerTime = %q, want 02:00",
					tt.time, cfg.schedulerTime)
			}
		})
	}
}

func TestParseScheduleTimeValidTimes(t *testing.T) {
	tests := []struct {
		name       string
		time       string
		wantHour   int
		wantMinute int
	}{
		{"midnight", "00:00", 0, 0},
		{"end of day", "23:59", 23, 59},
		{"noon", "12:00", 12, 0},
		{"early morning", "05:30", 5, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app{cfg: &config{schedulerTime: tt.time}}
			now := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
			got := a.parseScheduleTime(now)
			if got.Hour() != tt.wantHour || got.Minute() != tt.wantMinute {
				t.Errorf("parseScheduleTime(%q) = %02d:%02d, want %02d:%02d",
					tt.time, got.Hour(), got.Minute(), tt.wantHour, tt.wantMinute)
			}
		})
	}
}

func TestStreamDescPriorityOrder(t *testing.T) {
	// Verify the priority: ExtendedDisplayTitle > DisplayTitle > Title > ID fallback.
	s := &plexStream{
		ID:                   99,
		Title:                "Title",
		DisplayTitle:         "Display",
		ExtendedDisplayTitle: "Extended",
	}
	if got := streamDesc(s); got != "Extended" {
		t.Errorf("streamDesc with all fields = %q, want Extended", got)
	}

	s.ExtendedDisplayTitle = ""
	if got := streamDesc(s); got != "Display" {
		t.Errorf("streamDesc without extended = %q, want Display", got)
	}

	s.DisplayTitle = ""
	if got := streamDesc(s); got != "Title" {
		t.Errorf("streamDesc without display = %q, want Title", got)
	}

	s.Title = ""
	if got := streamDesc(s); got != "stream-99" {
		t.Errorf("streamDesc with only ID = %q, want stream-99", got)
	}
}

func TestHasIgnoreLabelEmptyInputs(t *testing.T) {
	// Both empty — should return false.
	if hasIgnoreLabel(nil, nil) {
		t.Error("hasIgnoreLabel(nil, nil) should be false")
	}
	// Labels but no ignore list.
	if hasIgnoreLabel([]plexLabel{{Tag: "A"}}, nil) {
		t.Error("hasIgnoreLabel with nil ignore list should be false")
	}
	// Ignore list but no labels.
	if hasIgnoreLabel(nil, []string{"A"}) {
		t.Error("hasIgnoreLabel with nil labels should be false")
	}
}

func TestMatchAudioStreamPrefersSameCodecAndLayout(t *testing.T) {
	ref := &plexStream{
		ID: 10, StreamType: 2, LanguageCode: "eng",
		Codec: "truehd", AudioChannelLayout: "7.1",
		Channels: 8,
	}
	candidates := []*plexStream{
		{ID: 1, StreamType: 2, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "stereo", Channels: 2},
		{ID: 2, StreamType: 2, LanguageCode: "eng", Codec: "truehd", AudioChannelLayout: "7.1", Channels: 8},
		{ID: 3, StreamType: 2, LanguageCode: "eng", Codec: "eac3", AudioChannelLayout: "5.1(side)", Channels: 6},
	}
	got := matchAudioStream(ref, candidates)
	if got == nil || got.ID != 2 {
		t.Errorf("matchAudioStream should prefer exact codec+layout match, got ID=%v", got)
	}
}

func TestMatchSubtitleStreamPrefersSameCodecAndFlags(t *testing.T) {
	ref := &plexStream{
		ID: 10, StreamType: 3, LanguageCode: "eng",
		Forced: false, HearingImpaired: false, Codec: "srt",
		Title: "English",
	}
	candidates := []*plexStream{
		{ID: 1, StreamType: 3, LanguageCode: "eng", Forced: false, HearingImpaired: false, Codec: "ass", Title: "English"},
		{ID: 2, StreamType: 3, LanguageCode: "eng", Forced: false, HearingImpaired: false, Codec: "srt", Title: "English"},
	}
	got := matchSubtitleStream(ref, nil, candidates)
	if got == nil || got.ID != 2 {
		t.Errorf("matchSubtitleStream should prefer matching codec, got ID=%v", got)
	}
}

func TestFilterByBoolPrefAllMatch(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, Forced: true},
		{ID: 2, Forced: true},
	}
	got := filterByBoolPref(streams, true, func(s *plexStream) bool { return s.Forced })
	if len(got) != 2 {
		t.Errorf("filterByBoolPref all match: got %d, want 2", len(got))
	}
}

func TestFilterByBoolPrefNoneMatch(t *testing.T) {
	streams := []*plexStream{
		{ID: 1, Forced: false},
		{ID: 2, Forced: false},
	}
	got := filterByBoolPref(streams, true, func(s *plexStream) bool { return s.Forced })
	// None match desired=true, so returns original list.
	if len(got) != 2 {
		t.Errorf("filterByBoolPref none match: got %d, want 2 (fallback)", len(got))
	}
}

func TestSelectedStreamsMultipleMedia(t *testing.T) {
	// selectedStreams only looks at first media, first part.
	ep := &plexEpisode{
		Media: []plexMedia{
			{Part: []plexPart{{Stream: []plexStream{
				{ID: 1, StreamType: 2, Selected: true, LanguageCode: "eng"},
			}}}},
			{Part: []plexPart{{Stream: []plexStream{
				{ID: 2, StreamType: 2, Selected: true, LanguageCode: "jpn"},
			}}}},
		},
	}
	audio, _ := selectedStreams(ep)
	if audio == nil || audio.ID != 1 {
		t.Errorf("selectedStreams should use first media, got audio ID=%v", audio)
	}
}

func TestFirstPartIDMultipleMedia(t *testing.T) {
	ep := &plexEpisode{
		Media: []plexMedia{
			{Part: []plexPart{{ID: 100}, {ID: 200}}},
			{Part: []plexPart{{ID: 300}}},
		},
	}
	if got := firstPartID(ep); got != 100 {
		t.Errorf("firstPartID() = %d, want 100 (first media, first part)", got)
	}
}

func TestCacheLearnLanguageProfileMultipleLanguages(t *testing.T) {
	var c appCache
	c.data.LanguageProfiles = make(map[string]map[string]string)

	c.learnLanguageProfile("1", "jpn", "eng")
	c.learnLanguageProfile("1", "kor", "eng")
	c.learnLanguageProfile("1", "eng", "")

	if lang, ok := c.getSubtitleLangForAudio("1", "jpn"); !ok || lang != "eng" {
		t.Errorf("jpn profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.getSubtitleLangForAudio("1", "kor"); !ok || lang != "eng" {
		t.Errorf("kor profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.getSubtitleLangForAudio("1", "eng"); !ok || lang != "" {
		t.Errorf("eng profile: got %q, %v (want empty string, true)", lang, ok)
	}
	if _, ok := c.getSubtitleLangForAudio("1", "fre"); ok {
		t.Error("fre profile should not exist")
	}
}

// ---------------------------------------------------------------------------
// Tests for extracted pure functions (Round 1)
// ---------------------------------------------------------------------------

func TestIsRelevantPlayEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   wsPlayEvent
		want bool
	}{
		{"playing with key", wsPlayEvent{State: "playing", RatingKey: "123"}, true},
		{"paused with key", wsPlayEvent{State: "paused", RatingKey: "456"}, true},
		{"stopped with key", wsPlayEvent{State: "stopped", RatingKey: "789"}, false},
		{"playing empty key", wsPlayEvent{State: "playing", RatingKey: ""}, false},
		{"empty state with key", wsPlayEvent{State: "", RatingKey: "123"}, false},
		{"buffering with key", wsPlayEvent{State: "buffering", RatingKey: "123"}, false},
		{"both empty", wsPlayEvent{State: "", RatingKey: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isRelevantPlayEvent(tt.ev)
			if got != tt.want {
				t.Errorf("isRelevantPlayEvent(%+v) = %v, want %v", tt.ev, got, tt.want)
			}
		})
	}
}

func TestBuildStreamCacheKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		userID    string
		ratingKey string
		audioID   int
		subID     int
		want      string
	}{
		{"typical", "42", "1234", 100, 200, "streams:42:1234:100:200"},
		{"zero IDs", "1", "999", 0, 0, "streams:1:999:0:0"},
		{"large IDs", "100", "99999", 65535, 32768, "streams:100:99999:65535:32768"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildStreamCacheKey(tt.userID, tt.ratingKey, tt.audioID, tt.subID)
			if got != tt.want {
				t.Errorf("buildStreamCacheKey(%q, %q, %d, %d) = %q, want %q",
					tt.userID, tt.ratingKey, tt.audioID, tt.subID, got, tt.want)
			}
		})
	}
}

func TestIsRelevantTimelineEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry wsTimelineEntry
		want  bool
	}{
		{
			"episode metadata created",
			wsTimelineEntry{Type: plexTypeEpisode, MetadataState: stateCreated, ItemID: "123"},
			true,
		},
		{
			"episode metadata updated",
			wsTimelineEntry{Type: plexTypeEpisode, MetadataState: stateUpdated, ItemID: "456"},
			true,
		},
		{
			"episode media created",
			wsTimelineEntry{Type: plexTypeEpisode, MediaState: stateCreated, ItemID: "789"},
			true,
		},
		{
			"episode media updated",
			wsTimelineEntry{Type: plexTypeEpisode, MediaState: stateUpdated, ItemID: "101"},
			true,
		},
		{
			"non-episode type",
			wsTimelineEntry{Type: 1, MetadataState: stateCreated, ItemID: "123"},
			false,
		},
		{
			"episode no relevant state",
			wsTimelineEntry{Type: plexTypeEpisode, MetadataState: "deleted", ItemID: "123"},
			false,
		},
		{
			"episode created but empty ID",
			wsTimelineEntry{Type: plexTypeEpisode, MetadataState: stateCreated, ItemID: ""},
			false,
		},
		{
			"all empty",
			wsTimelineEntry{},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isRelevantTimelineEntry(&tt.entry)
			if got != tt.want {
				t.Errorf("isRelevantTimelineEntry(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestTimelineAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry wsTimelineEntry
		want  string
	}{
		{"metadata created", wsTimelineEntry{MetadataState: stateCreated}, "scan_new"},
		{"media created", wsTimelineEntry{MediaState: stateCreated}, "scan_new"},
		{"both created", wsTimelineEntry{MetadataState: stateCreated, MediaState: stateCreated}, "scan_new"},
		{"metadata updated", wsTimelineEntry{MetadataState: stateUpdated}, "scan_updated"},
		{"media updated", wsTimelineEntry{MediaState: stateUpdated}, "scan_updated"},
		{"neither", wsTimelineEntry{}, "scan_updated"},
		{"metadata created media updated", wsTimelineEntry{MetadataState: stateCreated, MediaState: stateUpdated}, "scan_new"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := timelineAction(&tt.entry)
			if got != tt.want {
				t.Errorf("timelineAction(%+v) = %q, want %q", tt.entry, got, tt.want)
			}
		})
	}
}

func TestFindSubtitleByLanguage(t *testing.T) {
	t.Parallel()
	eng := &plexStream{ID: 1, StreamType: 3, LanguageCode: "eng"}
	jpn := &plexStream{ID: 2, StreamType: 3, LanguageCode: "jpn"}
	fra := &plexStream{ID: 3, StreamType: 3, LanguageCode: "fra"}

	tests := []struct {
		name     string
		streams  []*plexStream
		langCode string
		wantID   int
		wantNil  bool
	}{
		{"finds english", []*plexStream{eng, jpn, fra}, "eng", 1, false},
		{"finds japanese", []*plexStream{eng, jpn, fra}, "jpn", 2, false},
		{"finds french", []*plexStream{eng, jpn, fra}, "fra", 3, false},
		{"not found", []*plexStream{eng, jpn}, "kor", 0, true},
		{"empty streams", nil, "eng", 0, true},
		{"empty language", []*plexStream{eng}, "", 0, true},
		{"returns first match", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng"},
			{ID: 11, StreamType: 3, LanguageCode: "eng"},
		}, "eng", 10, false},
		{"prefers ASS over SRT", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "ass"},
		}, "eng", 11, false},
		{"prefers ASS over PGS", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: "pgs"},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "ass"},
		}, "eng", 11, false},
		{"prefers PGS over SRT", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "pgs"},
		}, "eng", 11, false},
		{"prefers vobsub over SRT", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "vobsub"},
		}, "eng", 11, false},
		{"unknown codec loses to SRT", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: ""},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
		}, "eng", 11, false},
		{"same codec picks first", []*plexStream{
			{ID: 10, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: 3, LanguageCode: "eng", Codec: "srt"},
		}, "eng", 10, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := findSubtitleByLanguage(tt.streams, tt.langCode)
			if tt.wantNil {
				if got != nil {
					t.Errorf("findSubtitleByLanguage(streams, %q) = stream %d, want nil",
						tt.langCode, got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("findSubtitleByLanguage(streams, %q) = nil, want stream %d",
					tt.langCode, tt.wantID)
			}
			if got.ID != tt.wantID {
				t.Errorf("findSubtitleByLanguage(streams, %q).ID = %d, want %d",
					tt.langCode, got.ID, tt.wantID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Property-based tests for extracted functions
// ---------------------------------------------------------------------------

func TestSubtitleCodecScore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		codec string
		want  int
	}{
		{"ass", 3}, {"ssa", 3}, {"ASS", 3},
		{"pgs", 2}, {"vobsub", 2}, {"dvdsub", 2},
		{"dvb_subtitle", 2}, {"hdmv_pgs_subtitle", 2},
		{"srt", 1}, {"subrip", 1}, {"mov_text", 1}, {"webvtt", 1},
		{"", 0}, {"unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			t.Parallel()
			got := subtitleCodecScore(tt.codec)
			if got != tt.want {
				t.Errorf("subtitleCodecScore(%q) = %d, want %d", tt.codec, got, tt.want)
			}
		})
	}
}

func TestIsRelevantPlayEventNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ev := wsPlayEvent{
			State:     rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "state"),
			RatingKey: rapid.StringMatching(`[0-9]{0,5}`).Draw(t, "key"),
		}
		_ = isRelevantPlayEvent(ev)
	})
}

func TestBuildStreamCacheKeyFormat(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		userID := rapid.StringMatching(`[0-9]{1,5}`).Draw(t, "userID")
		ratingKey := rapid.StringMatching(`[0-9]{1,5}`).Draw(t, "ratingKey")
		audioID := rapid.IntRange(0, 100000).Draw(t, "audioID")
		subID := rapid.IntRange(0, 100000).Draw(t, "subID")

		got := buildStreamCacheKey(userID, ratingKey, audioID, subID)

		// Invariant: key always starts with "streams:"
		if !strings.HasPrefix(got, "streams:") {
			t.Errorf("buildStreamCacheKey(%q, %q, %d, %d) = %q, want prefix 'streams:'",
				userID, ratingKey, audioID, subID, got)
		}
		// Invariant: key contains the userID and ratingKey
		if !strings.Contains(got, userID) {
			t.Errorf("buildStreamCacheKey result %q does not contain userID %q", got, userID)
		}
		if !strings.Contains(got, ratingKey) {
			t.Errorf("buildStreamCacheKey result %q does not contain ratingKey %q", got, ratingKey)
		}
	})
}

func TestIsRelevantTimelineEntryNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		entry := wsTimelineEntry{
			Type:          rapid.IntRange(0, 10).Draw(t, "type"),
			MetadataState: rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "metaState"),
			MediaState:    rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "mediaState"),
			ItemID:        rapid.StringMatching(`[0-9]{0,5}`).Draw(t, "itemID"),
		}
		_ = isRelevantTimelineEntry(&entry)
	})
}

func TestTimelineActionAlwaysReturnsValidAction(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		entry := wsTimelineEntry{
			MetadataState: rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "metaState"),
			MediaState:    rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "mediaState"),
		}
		got := timelineAction(&entry)
		if got != "scan_new" && got != "scan_updated" {
			t.Errorf("timelineAction(%+v) = %q, want 'scan_new' or 'scan_updated'", entry, got)
		}
	})
}

func TestFindSubtitleByLanguageNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nStreams := rapid.IntRange(0, 5).Draw(t, "n_streams")
		streams := make([]*plexStream, nStreams)
		for i := range nStreams {
			streams[i] = &plexStream{
				ID:           rapid.IntRange(1, 1000).Draw(t, fmt.Sprintf("id_%d", i)),
				StreamType:   3,
				LanguageCode: rapid.StringMatching(`[a-z]{0,3}`).Draw(t, fmt.Sprintf("lang_%d", i)),
			}
		}
		langCode := rapid.StringMatching(`[a-z]{0,3}`).Draw(t, "target_lang")
		result := findSubtitleByLanguage(streams, langCode)
		// Invariant: if result is non-nil, its language must match.
		if result != nil && result.LanguageCode != langCode {
			t.Errorf("findSubtitleByLanguage returned stream with lang %q, want %q",
				result.LanguageCode, langCode)
		}
	})
}

// ---------------------------------------------------------------------------
// Tests for drainBody and other small functions (Round 2)
// ---------------------------------------------------------------------------

func TestDrainBody(t *testing.T) {
	t.Parallel()
	t.Run("drains small body", func(t *testing.T) {
		t.Parallel()
		body := io.NopCloser(strings.NewReader("hello world"))
		drainBody(body) // should not panic
	})
	t.Run("drains empty body", func(t *testing.T) {
		t.Parallel()
		body := io.NopCloser(strings.NewReader(""))
		drainBody(body) // should not panic
	})
	t.Run("drains large body up to 4KB", func(t *testing.T) {
		t.Parallel()
		data := strings.Repeat("x", 8192)
		body := io.NopCloser(strings.NewReader(data))
		drainBody(body) // reads up to 4KB, discards rest
	})
}

func TestDrainBodyErrorReader(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(&errReader{err: fmt.Errorf("read error")})
	drainBody(body) // should log debug, not panic
}

// errReader is a reader that always returns an error.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

func TestCacheLearnLanguageProfileNilMaps(t *testing.T) {
	t.Parallel()
	var c appCache
	// Don't initialize LanguageProfiles — test nil map initialization path.
	c.learnLanguageProfile("1", "jpn", "eng")

	lang, ok := c.getSubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("expected profile to exist after learn")
	}
	if lang != "eng" {
		t.Errorf("getSubtitleLangForAudio(1, jpn) = %q, want eng", lang)
	}
}

func TestCacheLearnLanguageProfileNoChange(t *testing.T) {
	t.Parallel()
	var c appCache
	c.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng"},
	}
	// Call with same value — should be a no-op (no log, no change).
	c.learnLanguageProfile("1", "jpn", "eng")

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng", lang)
	}
}

// ---------------------------------------------------------------------------
// Boundary tests to kill lived mutants in cache functions
// ---------------------------------------------------------------------------

func TestCacheWasRecentlyProcessedBoundary(t *testing.T) {
	t.Parallel()
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)

	// Entry exactly at the 5-minute boundary should NOT be recent.
	c.data.ProcessedEpisodes["old"] = time.Now().Add(-5 * time.Minute).Unix()
	if c.wasRecentlyProcessed("old") {
		t.Error("wasRecentlyProcessed(old) = true, want false for entry exactly 5 min ago")
	}

	// Entry 4m59s ago should still be recent.
	c.data.ProcessedEpisodes["recent"] = time.Now().Add(-4*time.Minute - 59*time.Second).Unix()
	if !c.wasRecentlyProcessed("recent") {
		t.Error("wasRecentlyProcessed(recent) = false, want true for entry 4m59s ago")
	}

	// Entry just now should be recent.
	c.data.ProcessedEpisodes["now"] = time.Now().Unix()
	if !c.wasRecentlyProcessed("now") {
		t.Error("wasRecentlyProcessed(now) = false, want true for entry just now")
	}

	// Entry 6 minutes ago should not be recent.
	c.data.ProcessedEpisodes["stale"] = time.Now().Add(-6 * time.Minute).Unix()
	if c.wasRecentlyProcessed("stale") {
		t.Error("wasRecentlyProcessed(stale) = true, want false for entry 6 min ago")
	}
}

func TestCacheMarkProcessedPruneBoundary(t *testing.T) {
	t.Parallel()
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)

	// Fill exactly 10000 entries — should NOT trigger prune.
	for i := range 10000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("key-%d", i)] = time.Now().Unix()
	}
	c.markProcessed("trigger")
	// After adding one more (10001 total), prune should have run.
	// Since all entries are recent, none should be pruned.
	if len(c.data.ProcessedEpisodes) != 10001 {
		t.Errorf("after markProcessed with 10001 entries, got %d entries, want 10001",
			len(c.data.ProcessedEpisodes))
	}

	// Now add old entries to make prune effective.
	oldTS := time.Now().Add(-25 * time.Hour).Unix()
	for i := range 5000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("old-%d", i)] = oldTS
	}
	// Total is now 15001. markProcessed triggers prune (>10000).
	c.markProcessed("trigger2")
	// Old entries should be pruned.
	if len(c.data.ProcessedEpisodes) > 10002 {
		t.Errorf("after prune, got %d entries, want ≤10002 (old entries removed)",
			len(c.data.ProcessedEpisodes))
	}
}

func TestCachePruneOldEntriesBoundary(t *testing.T) {
	t.Parallel()
	var c appCache
	c.data.ProcessedEpisodes = make(map[string]int64)

	now := time.Now()
	// Entry exactly 24h ago — should NOT be pruned (cutoff is -24h, ts < cutoff means strictly older).
	c.data.ProcessedEpisodes["exact-24h"] = now.Add(-24 * time.Hour).Unix()
	// Entry 23h59m ago — should NOT be pruned.
	c.data.ProcessedEpisodes["23h59m"] = now.Add(-23*time.Hour - 59*time.Minute).Unix()
	// Entry 25h ago — should be pruned.
	c.data.ProcessedEpisodes["25h"] = now.Add(-25 * time.Hour).Unix()
	// Entry just now — should NOT be pruned.
	c.data.ProcessedEpisodes["now"] = now.Unix()

	c.pruneOldEntries()

	if _, ok := c.data.ProcessedEpisodes["exact-24h"]; !ok {
		t.Error("entry at exactly 24h should NOT be pruned (boundary: ts == cutoff)")
	}
	if _, ok := c.data.ProcessedEpisodes["23h59m"]; !ok {
		t.Error("entry at 23h59m should NOT be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["25h"]; ok {
		t.Error("entry at 25h should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["now"]; !ok {
		t.Error("entry at now should NOT be pruned")
	}
}
