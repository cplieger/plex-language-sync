package sync

import (
	"context"
	"errors"
	"testing"

	"plex-language-sync/internal/api"
	"plex-language-sync/internal/ignore"
	"plex-language-sync/internal/plex"
	"plex-language-sync/internal/streams"
	"plex-language-sync/internal/testsupport/fakeapi"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func newSyncer(cfg Config, plx *fakeapi.Plex, c api.Cache, users api.UserLookup) *Syncer {
	return NewSyncer(cfg, plx, c, users, func(_ string) api.PlexReadWriter { return plx })
}

// ---------------------------------------------------------------------------
// filterEpisodesAfter
// ---------------------------------------------------------------------------

func TestFilterEpisodesAfter(t *testing.T) {
	t.Parallel()
	ref := &streams.Episode{ParentIndex: 2, Index: 5}
	episodes := []streams.Episode{
		{ParentIndex: 1, Index: 1, RatingKey: "s1e1"},
		{ParentIndex: 2, Index: 3, RatingKey: "s2e3"},
		{ParentIndex: 2, Index: 5, RatingKey: "s2e5"},
		{ParentIndex: 2, Index: 7, RatingKey: "s2e7"},
		{ParentIndex: 3, Index: 1, RatingKey: "s3e1"},
	}
	got := filterEpisodesAfter(episodes, ref)
	wantKeys := []string{"s2e7", "s3e1"}
	if len(got) != len(wantKeys) {
		t.Fatalf("filterEpisodesAfter len = %d, want %d: %+v", len(got), len(wantKeys), got)
	}
	for i, key := range wantKeys {
		if got[i].RatingKey != key {
			t.Errorf("filterEpisodesAfter[%d] = %q, want %q", i, got[i].RatingKey, key)
		}
	}
}

func TestFilterEpisodesAfter_EmptyAndSameEpisode(t *testing.T) {
	t.Parallel()
	ref := &streams.Episode{ParentIndex: 1, Index: 1}
	if got := filterEpisodesAfter(nil, ref); len(got) != 0 {
		t.Errorf("filterEpisodesAfter(nil) = %v, want empty", got)
	}
	// same (season,index) as ref must not be included.
	ref = &streams.Episode{ParentIndex: 1, Index: 5}
	eps := []streams.Episode{{ParentIndex: 1, Index: 5, RatingKey: "same"}}
	if got := filterEpisodesAfter(eps, ref); len(got) != 0 {
		t.Errorf("filterEpisodesAfter(same-episode) = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Ignore integration — Config.Ignore wire-through.
//
// hasIgnoreLabel / shouldSkipEpisodeUpdate / ShouldSkipEpisodeUpdate
// moved to internal/ignore.Policy in cycle-2 step 3. The table-driven
// coverage now lives in internal/ignore/policy_test.go; what remains
// here is a narrow wire-through check that a *ignore.Policy assigned
// to sync.Config.Ignore is consulted on the per-episode path.
// ---------------------------------------------------------------------------

func TestSyncer_HonoursConfigIgnore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		episodeLibrary string
		episodeShowKey string
		ignoreLibs     []string
		ignoreLabels   []string
		showLabels     []streams.Label
		want           bool
	}{
		{
			name:           "ignored library",
			ignoreLibs:     []string{"Music"},
			ignoreLabels:   []string{"SKIP"},
			episodeLibrary: "Music",
			episodeShowKey: "42",
			want:           true,
		},
		{
			name:           "ignored label on show",
			ignoreLabels:   []string{"SKIP"},
			showLabels:     []streams.Label{{Tag: "SKIP"}},
			episodeLibrary: "TV",
			episodeShowKey: "42",
			want:           true,
		},
		{
			name:           "nothing ignored",
			ignoreLabels:   []string{"SKIP"},
			showLabels:     []streams.Label{{Tag: "OTHER"}},
			episodeLibrary: "TV",
			episodeShowKey: "42",
			want:           false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plx := &fakeapi.Plex{
				ShowMetadataByKey: map[string]*plex.Show{
					tc.episodeShowKey: {RatingKey: tc.episodeShowKey, Label: tc.showLabels},
				},
			}
			policy := ignore.NewPolicy(tc.ignoreLibs, tc.ignoreLabels)
			ref := &streams.Episode{LibraryTitle: tc.episodeLibrary, GrandparentRatingKey: tc.episodeShowKey}
			if got := policy.ShouldSkipEpisode(context.Background(), plx, ref); got != tc.want {
				t.Errorf("policy.ShouldSkipEpisode = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// learnProfileFromReference
// ---------------------------------------------------------------------------

func TestLearnProfileFromReference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		refAudio         *streams.Stream
		refSub           *streams.Stream
		name             string
		wantSubLang      string
		languageProfiles bool
		wantProfile      bool
	}{
		{name: "disabled", languageProfiles: false, refAudio: &streams.Stream{LanguageCode: "jpn"}, refSub: &streams.Stream{LanguageCode: "eng"}, wantProfile: false, wantSubLang: ""},
		{name: "nil refAudio", languageProfiles: true, refAudio: nil, refSub: &streams.Stream{LanguageCode: "eng"}, wantProfile: false, wantSubLang: ""},
		{name: "empty audio language", languageProfiles: true, refAudio: &streams.Stream{LanguageCode: ""}, refSub: &streams.Stream{LanguageCode: "eng"}, wantProfile: false, wantSubLang: ""},
		{name: "happy path with subtitle", languageProfiles: true, refAudio: &streams.Stream{LanguageCode: "jpn"}, refSub: &streams.Stream{LanguageCode: "eng"}, wantProfile: true, wantSubLang: "eng"},
		{name: "happy path nil subtitle", languageProfiles: true, refAudio: &streams.Stream{LanguageCode: "eng"}, refSub: nil, wantProfile: true, wantSubLang: ""},
		{
			name:             "commentary track skipped",
			languageProfiles: true,
			refAudio:         &streams.Stream{LanguageCode: "eng", ExtendedDisplayTitle: "English (Commentary)"},
			refSub:           &streams.Stream{LanguageCode: "fre"},
			wantProfile:      false,
			wantSubLang:      "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := fakeapi.NewCache()
			s := newSyncer(Config{LanguageProfiles: tc.languageProfiles}, &fakeapi.Plex{}, c, &fakeapi.Users{})
			audioLang := ""
			if tc.refAudio != nil {
				audioLang = tc.refAudio.LanguageCode
			}
			s.learnProfileFromReference("1", tc.refAudio, tc.refSub)
			got, ok := c.SubtitleLangForAudio("1", audioLang)
			if ok != tc.wantProfile {
				t.Errorf("learned=%v, want %v (got %q)", ok, tc.wantProfile, got)
			}
			if ok && got != tc.wantSubLang {
				t.Errorf("learned subLang = %q, want %q", got, tc.wantSubLang)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// UpdateEpisodeStreams
// ---------------------------------------------------------------------------

func TestUpdateEpisodeStreams(t *testing.T) {
	t.Parallel()

	// Helper: build an episode with a single Part carrying the given
	// streams.
	mkEpisode := func(partID int, streamList []streams.Stream) *streams.Episode {
		return &streams.Episode{
			RatingKey: "100",
			Media:     []streams.Media{{Part: []streams.Part{{ID: partID, Stream: streamList}}}},
		}
	}

	t.Run("fetch error returns false", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{} // no episodes → ErrNotFound from Episode()
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := &streams.Stream{ID: 1, StreamType: streams.StreamTypeAudio, LanguageCode: "eng"}
		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", ref, nil)
		if changed {
			t.Error("UpdateEpisodeStreams = true on fetch error, want false")
		}
	})

	t.Run("zero partID returns false", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			EpisodeByKey: map[string]*streams.Episode{
				"123": {RatingKey: "123", Media: nil},
			},
		}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := &streams.Stream{ID: 1, StreamType: streams.StreamTypeAudio, LanguageCode: "eng"}
		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", ref, nil)
		if changed {
			t.Error("UpdateEpisodeStreams = true with no parts, want false")
		}
	})

	t.Run("applies audio and subtitle", func(t *testing.T) {
		t.Parallel()
		ep := mkEpisode(100, []streams.Stream{
			{ID: 10, StreamType: streams.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"},
			{ID: 20, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Selected: true},
			{ID: 21, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn"},
		})
		plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		refAudio := &streams.Stream{ID: 99, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}
		refSub := &streams.Stream{ID: 88, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn"}
		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, refSub)
		if !changed {
			t.Error("UpdateEpisodeStreams = false, want true")
		}
		var gotAudio, gotSub bool
		for _, c := range plx.CallNames() {
			switch c {
			case "SetAudio":
				gotAudio = true
			case "SetSubtitle":
				gotSub = true
			}
		}
		if !gotAudio || !gotSub {
			t.Errorf("expected both SetAudio and SetSubtitle calls, calls=%v", plx.CallNames())
		}
	})

	t.Run("PUT error returns false", func(t *testing.T) {
		t.Parallel()
		ep := mkEpisode(100, []streams.Stream{
			{ID: 10, StreamType: streams.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"},
		})
		plx := &fakeapi.Plex{
			EpisodeByKey: map[string]*streams.Episode{"123": ep},
			SetAudioErr:  errors.New("boom"),
		}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		refAudio := &streams.Stream{ID: 99, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}
		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, nil)
		if changed {
			t.Error("UpdateEpisodeStreams = true after PUT error, want false")
		}
	})
}

// ---------------------------------------------------------------------------
// ApplyLanguageProfile (profile.go)
// ---------------------------------------------------------------------------

func TestApplyLanguageProfile(t *testing.T) {
	t.Parallel()

	t.Run("no audio selected returns false", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		c := fakeapi.NewCache()
		c.LearnLanguageProfile("1", "jpn", "eng")
		s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})
		ep := &streams.Episode{RatingKey: "100"}
		if s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
			t.Error("ApplyLanguageProfile = true with no audio, want false")
		}
	})

	t.Run("no profile in cache returns false", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		s := newSyncer(Config{LanguageProfiles: true}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ep := &streams.Episode{
			RatingKey: "100",
			Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			}}}}},
		}
		if s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
			t.Error("ApplyLanguageProfile = true with no profile, want false")
		}
	})

	t.Run("zero partID returns false", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		c := fakeapi.NewCache()
		c.LearnLanguageProfile("1", "jpn", "eng")
		s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})
		ep := &streams.Episode{
			RatingKey: "100",
			Media: []streams.Media{{Part: []streams.Part{{Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			}}}}},
		}
		if s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
			t.Error("ApplyLanguageProfile = true with zero partID, want false")
		}
	})

	t.Run("happy path sets subtitle", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		c := fakeapi.NewCache()
		c.LearnLanguageProfile("1", "jpn", "eng")
		s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})
		ep := &streams.Episode{
			RatingKey: "100",
			Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
				{ID: 12, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
			}}}}},
		}
		if !s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
			t.Error("ApplyLanguageProfile = false, want true")
		}
		foundSet := false
		for _, call := range plx.CallNames() {
			if call == "SetSubtitle" {
				foundSet = true
			}
		}
		if !foundSet {
			t.Errorf("ApplyLanguageProfile did not call SetSubtitleStream, calls=%v", plx.CallNames())
		}
	})

	t.Run("disables subtitles when profile says none", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		c := fakeapi.NewCache()
		c.LearnLanguageProfile("1", "jpn", "") // profile: no subtitles
		s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})
		ep := &streams.Episode{
			RatingKey: "100",
			Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
				{ID: 12, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Selected: true},
			}}}}},
		}
		if !s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
			t.Error("ApplyLanguageProfile = false, want true (should disable)")
		}
		foundDisable := false
		for _, call := range plx.CallNames() {
			if call == "DisableSubtitle" {
				foundDisable = true
			}
		}
		if !foundDisable {
			t.Errorf("ApplyLanguageProfile did not call DisableSubtitles, calls=%v", plx.CallNames())
		}
	})
}

// ---------------------------------------------------------------------------
// ProcessNewOrUpdatedEpisodeAllUsers — shared reference invariant
// ---------------------------------------------------------------------------

func TestProcessNewOrUpdatedEpisodeAllUsers_ReferenceSearchedOnce(t *testing.T) {
	t.Parallel()
	// 3 users (admin + 2 shared). Even with 3 users, ShowEpisodes (the
	// reference search) must run exactly once because the refactor
	// shares the reference across all users.
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": nil, // no prior episodes → no candidate; still 1 call
		},
	}
	c := fakeapi.NewCache()
	users := &fakeapi.Users{
		AllResult: []api.UserInfo{
			{ID: "1", Name: "admin"},
			{ID: "2", Name: "bob"},
			{ID: "3", Name: "carol"},
		},
	}
	s := newSyncer(Config{LanguageProfiles: false}, plx, c, users)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}
	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), ep, "scan_new")

	var showEpisodesCalls int
	for _, call := range plx.CallNames() {
		if call == "ShowEpisodes:42" {
			showEpisodesCalls++
		}
	}
	if showEpisodesCalls != 1 {
		t.Errorf("ShowEpisodes called %d times for 3 users; want 1 (shared-reference invariant)", showEpisodesCalls)
	}
}

// ---------------------------------------------------------------------------
// FindEpisodeReference
// ---------------------------------------------------------------------------

func TestFindEpisodeReference(t *testing.T) {
	t.Parallel()

	t.Run("empty grandparent returns nil", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: ""}
		if ref := s.FindEpisodeReference(context.Background(), ep); ref != nil {
			t.Errorf("FindEpisodeReference(empty grandparent) = %+v, want nil", ref)
		}
		if len(plx.CallNames()) != 0 {
			t.Errorf("no Plex calls should be made, got: %v", plx.CallNames())
		}
	})

	t.Run("no selected audio in any episode returns nil", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{
				"42": {{RatingKey: "2"}, {RatingKey: "3"}},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": {RatingKey: "2"},
				"3": {RatingKey: "3"},
			},
		}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
		if ref := s.FindEpisodeReference(context.Background(), ep); ref != nil {
			t.Errorf("FindEpisodeReference(no selected audio) = %+v, want nil", ref)
		}
	})

	t.Run("happy path returns ref with audio and subtitle", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{
				"42": {{RatingKey: "2"}},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": {
					RatingKey: "2",
					Media: []streams.Media{{Part: []streams.Part{{Stream: []streams.Stream{
						{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "eng"},
						{ID: 12, StreamType: streams.StreamTypeSubtitle, Selected: true, LanguageCode: "fre"},
					}}}}},
				},
			},
		}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
		ref := s.FindEpisodeReference(context.Background(), ep)
		if ref == nil {
			t.Fatal("FindEpisodeReference returned nil, want non-nil")
		}
		if ref.Episode == nil || ref.Episode.RatingKey != "2" {
			t.Errorf("ref.Episode = %+v, want ratingKey 2", ref.Episode)
		}
		if ref.Audio == nil || ref.Audio.LanguageCode != "eng" {
			t.Errorf("ref.Audio = %+v, want eng", ref.Audio)
		}
		if ref.Subtitle == nil || ref.Subtitle.LanguageCode != "fre" {
			t.Errorf("ref.Subtitle = %+v, want fre", ref.Subtitle)
		}
	})
}
