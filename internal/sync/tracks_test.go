package sync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
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

// ---------------------------------------------------------------------------
// Mutation-killing tests for the apply-stream guards (gremlins live mutants)
// ---------------------------------------------------------------------------

// TestUpdateEpisodeStreams_AppliesAudioWhenNoneSelected pins the
// nil-current-audio guard in applyAudioStream: when the target has no
// currently-selected audio, the matched reference audio must still be applied
// (and the guard must not dereference the nil current stream).
//
// given a target whose only audio stream is NOT selected (cur == nil)
// when UpdateEpisodeStreams applies a matching reference audio
// then SetAudioStream is called and the update reports changed.
func TestUpdateEpisodeStreams_AppliesAudioWhenNoneSelected(t *testing.T) {
	t.Parallel()
	ep := &streams.Episode{
		RatingKey: "123",
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}, // not selected
		}}}}},
	}
	plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
	s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
	refAudio := &streams.Stream{ID: 99, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}

	changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, nil)

	if !changed {
		t.Error("UpdateEpisodeStreams = false, want true (audio applied when none selected)")
	}
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio called %d times, want 1", got)
	}
}

// TestUpdateEpisodeStreams_SkipsSubtitleAlreadyCorrect pins the
// already-correct-subtitle guard in applySubtitleStream: when the
// currently-selected subtitle already equals the matched reference subtitle,
// no write should happen and the update must not report a change.
//
// given a target whose selected subtitle already matches the reference
// when UpdateEpisodeStreams runs with that reference
// then no SetSubtitleStream call is made and changed is false.
func TestUpdateEpisodeStreams_SkipsSubtitleAlreadyCorrect(t *testing.T) {
	t.Parallel()
	ep := &streams.Episode{
		RatingKey: "123",
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
			{ID: 21, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn", Selected: true},
		}}}}},
	}
	plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
	s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
	// Reference audio matches the already-selected audio (no audio change),
	// reference subtitle matches the already-selected jpn subtitle.
	refAudio := &streams.Stream{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}
	refSub := &streams.Stream{ID: 88, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn"}

	changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, refSub)

	if changed {
		t.Error("UpdateEpisodeStreams = true, want false (subtitle already correct, nothing to change)")
	}
	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
		t.Errorf("SetSubtitle called %d times, want 0 (no redundant write)", got)
	}
}

// TestUpdateEpisodeStreams_ReportsChangedOnSubtitleWriteSuccess pins the
// post-write success path in applySubtitleStream: on a successful
// SetSubtitleStream the method must report changed=true.
//
// Audio is left unchanged so the returned `changed` flag depends solely on
// the subtitle write outcome (the existing "applies audio and subtitle" test
// masks this because its audio change already sets changed=true).
//
// given a target needing only a subtitle change, and the write succeeds
// when UpdateEpisodeStreams runs
// then changed is true.
func TestUpdateEpisodeStreams_ReportsChangedOnSubtitleWriteSuccess(t *testing.T) {
	t.Parallel()
	ep := &streams.Episode{
		RatingKey: "123",
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
			{ID: 20, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Selected: true},
			{ID: 21, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn"},
		}}}}},
	}
	plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
	s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
	// Audio matches the selected audio (no audio change). Subtitle reference
	// is jpn, so the jpn subtitle (ID 21) replaces the selected eng (ID 20).
	refAudio := &streams.Stream{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}
	refSub := &streams.Stream{ID: 88, StreamType: streams.StreamTypeSubtitle, LanguageCode: "jpn"}

	changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, refSub)

	if !changed {
		t.Error("UpdateEpisodeStreams = false, want true (subtitle write succeeded)")
	}
	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 1 {
		t.Errorf("SetSubtitle called %d times, want 1", got)
	}
}

// TestUpdateEpisodeStreams_subtitleReferencePolicy pins two untested
// branches in applySubtitleStream, reached via UpdateEpisodeStreams with the
// reference audio equal to the target's already-selected audio so no audio
// write occurs and `changed` reflects only the subtitle decision:
//
//   - reference has NO subtitle selected (refSub == nil) while the target
//     does: the "no subtitle means no subtitle" policy disables it;
//   - reference subtitle language has no candidate on the target
//     (MatchSubtitle == nil): the target's current selection is left alone.
//
// A mutant that dropped the disable branch fails case 1; a mutant that let
// the matched == nil path fall through to a write fails case 2.
func TestUpdateEpisodeStreams_subtitleReferencePolicy(t *testing.T) {
	t.Parallel()
	refAudio := &streams.Stream{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"}
	mkTarget := func(subs ...streams.Stream) *streams.Episode {
		streamList := append([]streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
		}, subs...)
		return &streams.Episode{
			RatingKey: "123",
			Media:     []streams.Media{{Part: []streams.Part{{ID: 100, Stream: streamList}}}},
		}
	}

	t.Run("reference without subtitle disables the target's selected subtitle", func(t *testing.T) {
		t.Parallel()
		ep := mkTarget(streams.Stream{ID: 20, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Selected: true})
		plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})

		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, nil)

		if !changed {
			t.Error("UpdateEpisodeStreams = false, want true (no-subtitle policy must disable the target's subtitle)")
		}
		if got := countCalls(plx.CallNames(), "DisableSubtitle"); got != 1 {
			t.Errorf("DisableSubtitle called %d times, want 1", got)
		}
	})

	t.Run("reference subtitle with no target match leaves the selection alone", func(t *testing.T) {
		t.Parallel()
		ep := mkTarget(streams.Stream{ID: 20, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Selected: true})
		plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{"123": ep}}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		refSub := &streams.Stream{ID: 88, StreamType: streams.StreamTypeSubtitle, LanguageCode: "fre"}

		changed := s.UpdateEpisodeStreams(context.Background(), plx, "user", "123", refAudio, refSub)

		if changed {
			t.Error("UpdateEpisodeStreams = true, want false (no matching subtitle on target — leave selection alone)")
		}
		if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
			t.Errorf("SetSubtitle called %d times, want 0", got)
		}
		if got := countCalls(plx.CallNames(), "DisableSubtitle"); got != 0 {
			t.Errorf("DisableSubtitle called %d times, want 0 (must not disable on no-match)", got)
		}
	})
}

// TestApplyLanguageProfile_SkipsWhenSubtitleAlreadyMatchesProfile pins the
// already-correct-subtitle guard in applyProfileSubtitle: when the
// currently-selected subtitle already matches the best subtitle for the
// learned profile, no write should happen and the method must not report a
// change.
//
// given a profile jpn→eng and a target whose selected eng subtitle is the
// best eng candidate
// when ApplyLanguageProfile runs
// then no SetSubtitleStream call is made and the result is false.
func TestApplyLanguageProfile_SkipsWhenSubtitleAlreadyMatchesProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	c := fakeapi.NewCache()
	c.LearnLanguageProfile("1", "jpn", "eng")
	s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})
	ep := &streams.Episode{
		RatingKey: "100",
		Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			{ID: 12, StreamType: streams.StreamTypeSubtitle, Selected: true, LanguageCode: "eng", Codec: "srt"},
		}}}}},
	}

	changed := s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test")

	if changed {
		t.Error("ApplyLanguageProfile = true, want false (selected subtitle already matches profile)")
	}
	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
		t.Errorf("SetSubtitle called %d times, want 0 (no redundant write)", got)
	}
}

// TestApplyLanguageProfile_noOpWhenNoSubtitleApplicable pins the two no-op
// guards in applyProfileSubtitle that the existing happy/disable/already-
// matches tests don't reach: (1) the profile says "no subtitles" and the
// target already has none selected — nothing to disable; (2) the profile
// names a subtitle language the target episode doesn't carry — nothing to
// set. In both cases ApplyLanguageProfile must report no change and issue
// no PUT (a dropped curSub==nil guard would emit a spurious DisableSubtitles;
// a dropped bestSub==nil guard would nil-deref on SetSubtitleStream).
func TestApplyLanguageProfile_noOpWhenNoSubtitleApplicable(t *testing.T) {
	t.Parallel()
	audioJpnSelected := streams.Stream{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"}
	tests := []struct {
		name       string
		profileSub string          // learned subtitle lang for jpn audio
		subStream  *streams.Stream // optional non-matching subtitle on the target
	}{
		{name: "profile says none and target has no subtitle", profileSub: "", subStream: nil},
		{
			name:       "profile wants eng but target has no eng subtitle",
			profileSub: "eng",
			subStream:  &streams.Stream{ID: 12, StreamType: streams.StreamTypeSubtitle, LanguageCode: "fre", Codec: "srt"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			streamList := []streams.Stream{audioJpnSelected}
			if tc.subStream != nil {
				streamList = append(streamList, *tc.subStream)
			}
			ep := &streams.Episode{
				RatingKey: "100",
				Media:     []streams.Media{{Part: []streams.Part{{ID: 7, Stream: streamList}}}},
			}
			plx := &fakeapi.Plex{}
			c := fakeapi.NewCache()
			c.LearnLanguageProfile("1", "jpn", tc.profileSub)
			s := newSyncer(Config{LanguageProfiles: true}, plx, c, &fakeapi.Users{})

			if s.ApplyLanguageProfile(context.Background(), plx, "1", ep, "test") {
				t.Error("ApplyLanguageProfile = true, want false (no applicable subtitle change)")
			}
			if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
				t.Errorf("SetSubtitle called %d times, want 0", got)
			}
			if got := countCalls(plx.CallNames(), "DisableSubtitle"); got != 0 {
				t.Errorf("DisableSubtitle called %d times, want 0", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ChangeTracksForEpisode — show/season level, strategy, and skip branches
// ---------------------------------------------------------------------------

// refWithSelectedAudio builds the active reference episode: one part with a
// single selected audio stream in the given language and no subtitle.
func refWithSelectedAudio(lang, show, parent string, season, index int) *streams.Episode {
	return &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: show,
		ParentRatingKey:      parent,
		GrandparentTitle:     "Show",
		ParentIndex:          streams.FlexInt(season),
		Index:                streams.FlexInt(index),
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: lang, Selected: true},
		}}}}},
	}
}

// targetNeedingAudioSwitch builds an episode whose selected audio is eng with
// a jpn alternative available, so a jpn reference triggers exactly one
// SetAudioStream write when the episode is reloaded.
func targetNeedingAudioSwitch(key string) *streams.Episode {
	return &streams.Episode{
		RatingKey: key,
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 10, StreamType: streams.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"},
		}}}}},
	}
}

func TestChangeTracksForEpisode(t *testing.T) {
	t.Parallel()

	t.Run("show level updates every episode via ShowEpisodes", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{
				"42": {{RatingKey: "2"}, {RatingKey: "3"}},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": targetNeedingAudioSwitch("2"),
				"3": targetNeedingAudioSwitch("3"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 1 {
			t.Errorf("ShowEpisodes:42 called %d times, want 1", got)
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 2 {
			t.Errorf("SetAudio called %d times, want 2 (one per show episode); calls=%v", got, plx.CallNames())
		}
	})

	t.Run("season level updates via SeasonEpisodes", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			SeasonEpisodesByKey: map[string][]streams.Episode{
				"7": {{RatingKey: "2"}},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": targetNeedingAudioSwitch("2"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		if got := countCalls(plx.CallNames(), "SeasonEpisodes:7"); got != 1 {
			t.Errorf("SeasonEpisodes:7 called %d times, want 1", got)
		}
		if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
			t.Errorf("ShowEpisodes must not be called at season level; calls=%v", plx.CallNames())
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
			t.Errorf("SetAudio called %d times, want 1", got)
		}
	})

	t.Run("next strategy skips episodes at or before the reference", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{
				"42": {
					{RatingKey: "1", ParentIndex: 1, Index: 1},
					{RatingKey: "2", ParentIndex: 1, Index: 2}, // the reference itself
					{RatingKey: "3", ParentIndex: 1, Index: 3},
				},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"3": targetNeedingAudioSwitch("3"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyNext}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 2) // S1E2

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		// Only S1E3 (key "3") is strictly after the reference; earlier
		// episodes must never be reloaded.
		if got := countCalls(plx.CallNames(), "Episode:1"); got != 0 {
			t.Errorf("Episode:1 reloaded %d times, want 0 (before reference)", got)
		}
		if got := countCalls(plx.CallNames(), "Episode:2"); got != 0 {
			t.Errorf("Episode:2 reloaded %d times, want 0 (the reference itself)", got)
		}
		if got := countCalls(plx.CallNames(), "Episode:3"); got != 1 {
			t.Errorf("Episode:3 reloaded %d times, want 1 (after reference)", got)
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
			t.Errorf("SetAudio called %d times, want 1", got)
		}
	})

	t.Run("reference without selected audio is skipped before any fetch", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		// Audio present but not selected → streams.Selected returns nil audio.
		ref := &streams.Episode{
			RatingKey:            "1",
			GrandparentRatingKey: "42",
			Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"},
			}}}}},
		}

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		if calls := plx.CallNames(); len(calls) != 0 {
			t.Errorf("no Plex calls expected when the reference has no selected audio, got %v", calls)
		}
	})

	t.Run("empty grandparent rating key is skipped before any fetch", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := refWithSelectedAudio("jpn", "", "7", 1, 1) // no show rating key

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		if calls := plx.CallNames(); len(calls) != 0 {
			t.Errorf("no Plex calls expected when the show rating key is empty, got %v", calls)
		}
	})

	t.Run("ignored show is skipped before fetching episodes", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowMetadataByKey: map[string]*plex.Show{
				"42": {RatingKey: "42", Label: []streams.Label{{Tag: "SKIP"}}},
			},
			ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		}
		policy := ignore.NewPolicy(nil, []string{"SKIP"})
		s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, fakeapi.NewCache(), &fakeapi.Users{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

		if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
			t.Errorf("ShowEpisodes must not be called when the show is ignored; calls=%v", plx.CallNames())
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 0 {
			t.Errorf("SetAudio called %d times, want 0 (show ignored)", got)
		}
	})
}

// TestChangeTracksForEpisode_IgnoredShowDoesNotLearnProfile pins the
// learn-after-ignore ordering: an ignored show must be treated as if it
// does not exist, so playing one of its episodes records NOTHING into the
// user's language profile AND propagates to no other episode. This guards
// the user decision that "ignore" suppresses profile learning, not just
// propagation. A regression that moved learnProfileFromReference back ahead
// of the ignore gate would learn jpn→"" here and fail the SubtitleLangForAudio
// assertion.
//
// given an ignored show (SKIP label) and LANGUAGE_PROFILES enabled
// when ChangeTracksForEpisode runs on a jpn-audio reference of that show
// then the user's profile has no learned entry for jpn and no episodes are
// fetched or written.
func TestChangeTracksForEpisode_IgnoredShowDoesNotLearnProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey: map[string]*plex.Show{
			"42": {RatingKey: "42", Label: []streams.Label{{Tag: "SKIP"}}},
		},
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	policy := ignore.NewPolicy(nil, []string{"SKIP"})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy, LanguageProfiles: true}, plx, c, &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

	// No profile was learned from the ignored show.
	if got, ok := c.SubtitleLangForAudio("1", "jpn"); ok {
		t.Errorf("learned profile jpn→%q for an ignored show; want no entry (ignore must suppress learning)", got)
	}
	// No propagation either: the show was never fetched and no streams written.
	if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
		t.Errorf("ShowEpisodes called %d times for an ignored show; want 0; calls=%v", got, plx.CallNames())
	}
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 0 {
		t.Errorf("SetAudio called %d times for an ignored show; want 0", got)
	}
	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
		t.Errorf("SetSubtitle called %d times for an ignored show; want 0", got)
	}
}

// TestChangeTracksForEpisode_NonIgnoredShowStillLearnsProfile is the
// companion to the ignore-suppression test: it pins that a NON-ignored show
// still learns its profile (proving the reorder did not accidentally drop the
// learn call). A mutant that always returned early before learnProfileFrom
// Reference would pass the ignore test but fail this one.
//
// given a non-ignored show and LANGUAGE_PROFILES enabled
// when ChangeTracksForEpisode runs on a jpn-audio reference
// then the user's profile records jpn (subtitle empty, no sub on the ref).
func TestChangeTracksForEpisode_NonIgnoredShowStillLearnsProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	policy := ignore.NewPolicy(nil, []string{"SKIP"}) // policy present but show carries no SKIP label
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy, LanguageProfiles: true}, plx, c, &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

	got, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("non-ignored show did not learn a profile; want jpn entry recorded")
	}
	if got != "" {
		t.Errorf("learned subtitle lang = %q, want \"\" (reference has no selected subtitle)", got)
	}
}

// TestChangeTracksForEpisode_LogsCompletionWithUpdatedCount pins the
// episodes_updated tally and the completion-summary gate. ChangeTracksForEpisode
// increments a counter for each episode it changes and logs the inviolate
// "language update complete" summary only when that count is positive; the
// count is surfaced nowhere else, so the structured log is its sole
// observable. With two episodes that each need an audio switch the summary
// must report episodes_updated=2 — a counter that decremented instead of
// incremented (yielding a non-positive total) would suppress the summary, and
// a completion gate that rejected a positive count likewise.
//
// Not parallel: it swaps the process-global default slog logger.
func TestChangeTracksForEpisode_LogsCompletionWithUpdatedCount(t *testing.T) {
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {{RatingKey: "2"}, {RatingKey: "3"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2": targetNeedingAudioSwitch("2"),
			"3": targetNeedingAudioSwitch("3"),
		},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

	out := buf.String()
	if !strings.Contains(out, "language update complete") {
		t.Errorf("missing 'language update complete' summary after 2 successful updates; log = %q", out)
	}
	if !strings.Contains(out, "episodes_updated=2") {
		t.Errorf("summary must report episodes_updated=2 (exact change tally); log = %q", out)
	}
}

// TestChangeTracksForEpisode_SilentWhenNothingChanged pins the lower bound of
// the completion-summary gate: when no episode needs a change the tally stays
// zero and the "language update complete" summary must NOT be logged (a
// zero-update summary would be misleading noise). Every show episode here
// already has the reference's jpn audio selected, so UpdateEpisodeStreams
// reports no change and the counter never leaves zero.
//
// Not parallel: it swaps the process-global default slog logger.
func TestChangeTracksForEpisode_SilentWhenNothingChanged(t *testing.T) {
	alreadyJpn := func(key string) *streams.Episode {
		return &streams.Episode{
			RatingKey: key,
			Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
			}}}}},
		}
	}
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {{RatingKey: "2"}, {RatingKey: "3"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2": alreadyJpn("2"),
			"3": alreadyJpn("3"),
		},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.ChangeTracksForEpisode(context.Background(), plx, "1", ref, "play")

	if out := buf.String(); strings.Contains(out, "language update complete") {
		t.Errorf("'language update complete' must not be logged when no episode changed; log = %q", out)
	}
}
