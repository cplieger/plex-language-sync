package tracksync

import (
	"errors"
	"strings"
	"testing"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/users"
	"github.com/cplieger/plexapi/v3"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func newSyncer(cfg Config, plx *fakeapi.Plex, c cacheStore, lookup userLookup) *Syncer {
	return New(cfg, Deps{Cache: c, Users: lookup, UserClient: func(_ string) PlexReadWriter { return plx }})
}

// ---------------------------------------------------------------------------
// Ignore integration: the policy's own table lives in internal/ignore; this
// checks that a policy set as Config.Ignore is what the syncer consults.
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
					tc.episodeShowKey: {Label: tc.showLabels},
				},
			}
			policy := ignore.New(ignore.Config{Reader: plx, Libraries: tc.ignoreLibs, Labels: tc.ignoreLabels})
			ref := &streams.Episode{LibraryTitle: textsafe.NewUntrusted(tc.episodeLibrary), GrandparentRatingKey: tc.episodeShowKey}
			if got := policy.ShouldSkipEpisode(t.Context(), ref); got != tc.want {
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
			s := newSyncer(Config{LanguageProfiles: tc.languageProfiles}, &fakeapi.Plex{}, c, &fakeUsers{})
			audioLang := ""
			if tc.refAudio != nil {
				audioLang = tc.refAudio.LanguageCode
			}
			s.learnProfileFromReference("1", streams.Pair{Audio: tc.refAudio, Subtitle: tc.refSub})
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
// Observe — show/season level, strategy, and skip branches
// ---------------------------------------------------------------------------

// refWithSelectedAudio builds the active reference episode: one part with a
// single selected audio stream in the given language and no subtitle.
func refWithSelectedAudio(lang, show, parent string, season, index int) *streams.Episode {
	return &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: show,
		ParentRatingKey:      parent,
		GrandparentTitle:     textsafe.NewUntrusted("Show"),
		ParentIndex:          streams.FlexInt(season),
		Index:                streams.FlexInt(index),
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: lang, Selected: true},
		}}}}},
	}
}

// listEp is an item of show 42's season 1 list ("7") as Plex sends it, with
// the show, season and position the scope reads.
func listEp(key string) streams.Episode {
	return streams.Episode{RatingKey: key, GrandparentRatingKey: "42", ParentRatingKey: "7", ParentIndex: 1, Index: 2}
}

// targetNeedingAudioSwitch builds an episode whose selected audio is eng with
// a jpn alternative available, so a jpn reference triggers exactly one
// SetAudioStream write when the episode is reloaded.
func targetNeedingAudioSwitch(key string) *streams.Episode {
	return &streams.Episode{
		RatingKey: key,
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 10, StreamType: plexapi.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: "jpn"},
		}}}}},
	}
}

func TestObserve(t *testing.T) {
	t.Parallel()

	t.Run("show level updates every episode via ShowEpisodes", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{
				"42": {listEp("2"), listEp("3")},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": targetNeedingAudioSwitch("2"),
				"3": targetNeedingAudioSwitch("3"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeUsers{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		observeOK(t, s, ref)

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
				"7": {listEp("2")},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"2": targetNeedingAudioSwitch("2"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeUsers{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		observeOK(t, s, ref)

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
					{RatingKey: "1", GrandparentRatingKey: "42", ParentIndex: 1, Index: 1},
					{RatingKey: "2", GrandparentRatingKey: "42", ParentIndex: 1, Index: 2}, // the reference itself
					{RatingKey: "3", GrandparentRatingKey: "42", ParentIndex: 1, Index: 3},
				},
			},
			EpisodeByKey: map[string]*streams.Episode{
				"3": targetNeedingAudioSwitch("3"),
			},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyNext}, plx, fakeapi.NewCache(), &fakeUsers{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 2) // S1E2

		observeOK(t, s, ref)

		// Only S1E3 (key "3") is strictly after the reference; earlier
		// episodes must never be reloaded.
		if got := countCalls(plx.CallNames(), "Episode:1"); got != 0 {
			t.Errorf("Episode:1 reloaded %d times, want 0 (before reference)", got)
		}
		if got := countCalls(plx.CallNames(), "Episode:2"); got != 0 {
			t.Errorf("Episode:2 reloaded %d times, want 0 (the reference itself)", got)
		}
		if got := countCalls(plx.CallNames(), "Episode:3"); got != 2 {
			t.Errorf("Episode:3 reloaded %d times, want 2 (after reference: before the write and after the audio write)", got)
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
			t.Errorf("SetAudio called %d times, want 1", got)
		}
	})

	t.Run("reference without selected audio is skipped before any fetch", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		}
		s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeUsers{})
		// Audio present but not selected → streams.Selected returns nil audio.
		ref := &streams.Episode{
			RatingKey:            "1",
			GrandparentRatingKey: "42",
			Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
				{ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: "jpn"},
			}}}}},
		}

		observeOK(t, s, ref)

		if calls := plx.CallNames(); len(calls) != 0 {
			t.Errorf("no Plex calls expected when the reference has no selected audio, got %v", calls)
		}
	})

	t.Run("empty grandparent rating key is skipped before any fetch", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeUsers{})
		ref := refWithSelectedAudio("jpn", "", "7", 1, 1) // no show rating key

		observeOK(t, s, ref)

		if calls := plx.CallNames(); len(calls) != 0 {
			t.Errorf("no Plex calls expected when the show rating key is empty, got %v", calls)
		}
	})

	t.Run("ignored show is skipped before fetching episodes", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			ShowMetadataByKey: map[string]*plex.Show{
				"42": {Label: []streams.Label{{Tag: "SKIP"}}},
			},
			ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		}
		policy := ignore.New(ignore.Config{Reader: plx, Labels: []string{"SKIP"}})
		s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, fakeapi.NewCache(), &fakeUsers{})
		ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

		observeOK(t, s, ref)

		if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
			t.Errorf("ShowEpisodes must not be called when the show is ignored; calls=%v", plx.CallNames())
		}
		if got := countCalls(plx.CallNames(), "SetAudio"); got != 0 {
			t.Errorf("SetAudio called %d times, want 0 (show ignored)", got)
		}
	})
}

// TestObserve_IgnoredShowDoesNotLearnProfile pins that an ignored show is
// treated as absent: playing one of its episodes teaches the profile nothing
// and writes no other episode, so the ignore gate must run before learning.
func TestObserve_IgnoredShowDoesNotLearnProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey: map[string]*plex.Show{
			"42": {Label: []streams.Label{{Tag: "SKIP"}}},
		},
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	policy := ignore.New(ignore.Config{Reader: plx, Labels: []string{"SKIP"}})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy, LanguageProfiles: true}, plx, c, &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	observeOK(t, s, ref)

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

// TestObserve_NonIgnoredShowStillLearnsProfile is the ignore test's
// companion: with an ignore policy that does not match the show, the play
// still teaches the profile jpn with no subtitle.
func TestObserve_NonIgnoredShowStillLearnsProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	policy := ignore.New(ignore.Config{Reader: plx, Labels: []string{"SKIP"}}) // policy present but show carries no SKIP label
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy, LanguageProfiles: true}, plx, c, &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	observeOK(t, s, ref)

	got, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("non-ignored show did not learn a profile; want jpn entry recorded")
	}
	if got != "" {
		t.Errorf("learned subtitle lang = %q, want \"\" (reference has no selected subtitle)", got)
	}
}

// TestObserve_LogsCompletionWithUpdatedCount pins the "language update
// complete" summary and its episodes_updated tally, which no other output
// carries: two episodes that each need an audio switch report 2.
//
// Not parallel: it swaps the process-global default slog logger.
func TestObserve_LogsCompletionWithUpdatedCount(t *testing.T) {
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {listEp("2"), listEp("3")},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2": targetNeedingAudioSwitch("2"),
			"3": targetNeedingAudioSwitch("3"),
		},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	buf := captureLogs(t)

	observeOK(t, s, ref)

	out := buf.String()
	if !strings.Contains(out, "language update complete") {
		t.Errorf("missing 'language update complete' summary after 2 successful updates; log = %q", out)
	}
	if !strings.Contains(out, "episodes_updated=2") {
		t.Errorf("summary must report episodes_updated=2 (exact change tally); log = %q", out)
	}
}

// No "language update complete" line when no episode changed: every show
// episode here already holds the choice's Japanese audio.
//
// Not parallel: it swaps the process-global default slog logger.
func TestObserve_SilentWhenNothingChanged(t *testing.T) {
	alreadyJpn := func(key string) *streams.Episode {
		return &streams.Episode{
			RatingKey: key,
			Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
				{ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
			}}}}},
		}
	}
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {listEp("2"), listEp("3")},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2": alreadyJpn("2"),
			"3": alreadyJpn("3"),
		},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	buf := captureLogs(t)

	observeOK(t, s, ref)

	if out := buf.String(); strings.Contains(out, "language update complete") {
		t.Errorf("'language update complete' must not be logged when no episode changed; log = %q", out)
	}
}

// TestObserve_logsEpisodeFetchError pins the inviolate
// "failed to fetch episodes for update" WARN branch of Observe:
// when the per-user client's episode fetch errors, the update aborts with that
// exact log key and writes nothing.
func TestObserve_logsEpisodeFetchError(t *testing.T) {
	plx := &fakeapi.Plex{ShowEpisodesErr: errors.New("plex 503")}
	s := newSyncer(Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}, plx, fakeapi.NewCache(), &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	buf := captureLogs(t)

	observeOK(t, s, ref)

	out := buf.String()
	if !strings.Contains(out, "failed to fetch episodes for update") {
		t.Errorf("missing inviolate WARN key 'failed to fetch episodes for update' when the episode fetch errors; log = %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("an episode-fetch error must log at WARN; log = %q", out)
	}
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 0 {
		t.Errorf("SetAudio called %d times after a fetch error; want 0 (must abort before applying any change)", got)
	}
}

// ---------------------------------------------------------------------------
// logSubstitution — which track changes reach INFO
// ---------------------------------------------------------------------------

// TestLogSubstitution_levelByKindAndDistance pins logSubstitution's level
// policy on both sides of its tier cut, for each kind.
// Not parallel: it swaps the process-global default logger.
func TestLogSubstitution_levelByKindAndDistance(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		refLang   string
		matchLang string
		wantLevel string
		wantMsg   string
	}{
		{"audio at same-language is routine", kindAudio, "nob", "nor", "level=DEBUG", `msg="track language matched"`},
		{"audio at other-script is still routine", kindAudio, "zh-CN", "zh-TW", "level=DEBUG", `msg="track language matched"`},
		{"subtitle at same-language is routine", kindSubtitle, "nob", "nor", "level=DEBUG", `msg="track language matched"`},
		{"subtitle at other-region is routine", kindSubtitle, "pt-BR", "pt-PT", "level=DEBUG", `msg="track language matched"`},
		{"subtitle at same-language in another script is routine", kindSubtitle, "sr-Latn", "sr-Cyrl", "level=DEBUG", `msg="track language matched"`},
		{"subtitle at other-script is recorded", kindSubtitle, "zh-Hans", "zh-Hant", "level=INFO", `msg="track language substituted"`},
		{"subtitle at intelligible is recorded", kindSubtitle, "nob", "nno", "level=INFO", `msg="track language substituted"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLogs(t)

			ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: textsafe.NewUntrusted("Show")}
			logSubstitution(ep, "alice", tt.kind,
				&streams.Stream{LanguageCode: tt.refLang},
				&streams.Stream{LanguageCode: tt.matchLang})

			out := buf.String()
			if !strings.Contains(out, tt.wantLevel) {
				t.Errorf("logSubstitution(%s, %s->%s) logged %q, want %s", tt.kind, tt.refLang, tt.matchLang, out, tt.wantLevel)
			}
			if !strings.Contains(out, tt.wantMsg) {
				t.Errorf("logSubstitution(%s, %s->%s) logged %q, want %s", tt.kind, tt.refLang, tt.matchLang, out, tt.wantMsg)
			}
		})
	}
}

// Not parallel: it swaps the process-global default logger.
func TestLogSubstitution_sanitizesLanguageFields(t *testing.T) {
	rec := capture.Default(t)
	const hostile = "nob\nlevel=ERROR \u202ex"

	logSubstitution(&streams.Episode{GrandparentTitle: textsafe.NewUntrusted("Show")}, "alice", kindSubtitle,
		&streams.Stream{LanguageCode: hostile, LanguageTag: hostile},
		&streams.Stream{LanguageCode: hostile, LanguageTag: hostile})

	want := textsafe.SanitizeSingleLine(hostile)
	for _, key := range []string{"from", "to", "from_tag", "to_tag"} {
		if !rec.HasAttr("track language", capture.AttrName(key), want) {
			t.Errorf("%s was not logged as %q; a stream's language comes from the media file and reached the log raw: %v", key, want, rec.Records())
		}
	}
}

// User IDs come from plex.tv and the session list, and rating keys from the
// server's REST listings, so a hostile value must reach the log as one line.
//
// Not parallel: it swaps the process-global default logger.
func TestSyncerLogs_sanitizePlexIdentifiers(t *testing.T) {
	const hostile = "7\nlevel=ERROR \u202ex"
	want := textsafe.SanitizeSingleLine(hostile)
	noClient := func(string) PlexReadWriter { return nil }

	t.Run("fan-out user id", func(t *testing.T) {
		rec := capture.Default(t)
		s := New(Config{SeedNewEpisodes: true}, Deps{
			Cache: fakeapi.NewCache(), UserClient: noClient,
			Users: &fakeUsers{AllResult: []users.Account{{ID: hostile}}},
		})
		s.SeedEpisode(t.Context(), &streams.Episode{RatingKey: "100"}, "scan_new")
		if !rec.HasAttr("sync: no per-user client, skipping user", "user_id", want) {
			t.Errorf("user_id was not logged as %q: %v", want, rec.Records())
		}
	})
	t.Run("reconcile user id", func(t *testing.T) {
		rec := capture.Default(t)
		s := New(Config{}, Deps{
			Cache: fakeapi.NewCache(), UserClient: noClient, Users: &fakeUsers{},
		})
		if err := s.Reconcile(t.Context(), hostile, &streams.Episode{RatingKey: "100"}, "scheduler"); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
		if !rec.HasAttr("reconcile: no per-user client, skipping user", "user_id", want) {
			t.Errorf("user_id was not logged as %q: %v", want, rec.Records())
		}
	})
	t.Run("reload key", func(t *testing.T) {
		rec := capture.Default(t)
		plx := &fakeapi.Plex{EpisodeErr: errors.New("boom")}
		s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeUsers{})
		p := &plan{pick: pickForIntent(streams.Pair{}, 0), targets: []streams.Episode{{RatingKey: hostile}}}
		if _, err := s.apply.applyLocked(t.Context(), plx, "1", p); err != nil {
			t.Fatalf("applyLocked = %v", err)
		}
		if !rec.HasAttr("failed to reload episode", "key", want) {
			t.Errorf("key was not logged as %q: %v", want, rec.Records())
		}
	})
}
