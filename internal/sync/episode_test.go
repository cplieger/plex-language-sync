package sync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// mkSelectedAudioEpisode builds an episode whose single part carries one
// selected audio stream, so streams.Selected returns a non-nil audio.
func mkSelectedAudioEpisode(key string) *streams.Episode {
	return &streams.Episode{
		RatingKey: key,
		Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "eng"},
		}}}}},
	}
}

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// TestProcessNewOrUpdatedEpisodeAllUsers_ProcessesEveryUserWhenLive pins the
// per-user loop's live-context guard: with a non-cancelled context the loop
// body must run for every user, so a guard that bailed out early would leave
// zero per-user reloads.
//
// given a found reference and two known users on a live context
// when ProcessNewOrUpdatedEpisodeAllUsers runs
// then the target episode is reloaded once per user (UpdateEpisodeStreams).
func TestProcessNewOrUpdatedEpisodeAllUsers_ProcessesEveryUserWhenLive(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {{RatingKey: "2"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2":   mkSelectedAudioEpisode("2"),
			"100": mkSelectedAudioEpisode("100"),
		},
	}
	users := &fakeapi.Users{
		AllResult: []api.UserInfo{
			{ID: "1", Name: "admin"},
			{ID: "2", Name: "bob"},
		},
	}
	s := newSyncer(Config{LanguageProfiles: false}, plx, fakeapi.NewCache(), users)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), ep, "scan_new")

	got := countCalls(plx.CallNames(), "Episode:100")
	if got != 2 {
		t.Errorf("target episode reloaded %d times for 2 users; want 2 (loop must run per user on a live context)", got)
	}
}

// TestFindReferenceEpisode_CapsSearchAtMaxDepth pins both the depth cap and
// the per-fetch search counter: with more episodes than the cap and none
// carrying a selected audio, the search must stop after exactly maxDepth
// fetches. An off-by-one in the cap would let one extra episode through, and
// a counter that failed to advance would never trip the cap and would scan
// every episode.
func TestFindReferenceEpisode_CapsSearchAtMaxDepth(t *testing.T) {
	t.Parallel()
	const maxDepth = 3
	episodes := []streams.Episode{
		{RatingKey: "1"},
		{RatingKey: "2"},
		{RatingKey: "3"},
		{RatingKey: "4"},
		{RatingKey: "5"},
	}
	// Empty EpisodeByKey → every reader.Episode returns ErrNotFound, so the
	// search never finds a candidate and runs until the depth cap.
	plx := &fakeapi.Plex{}

	ref, searched, _ := findReferenceEpisode(context.Background(), plx, episodes, "exclude-none", maxDepth)

	if ref != nil {
		t.Errorf("findReferenceEpisode ref = %+v, want nil (no selected audio anywhere)", ref)
	}
	if searched != maxDepth {
		t.Errorf("findReferenceEpisode searched = %d, want %d (must stop exactly at the depth cap)", searched, maxDepth)
	}
}

// TestFindReferenceEpisode_SkipsTriggeringEpisode pins the excludeKey guard:
// the episode that triggered the search appears in the show's episode list,
// and must never become its own reference. slices.Backward visits newest
// first, so the excluded "100" is seen first and skipped without a metadata
// fetch, and the older "2" becomes the reference.
//
// A mutant that dropped the `ep.RatingKey == excludeKey` skip would fetch
// "100" and return it as the reference, failing both assertions below.
func TestFindReferenceEpisode_SkipsTriggeringEpisode(t *testing.T) {
	t.Parallel()
	episodes := []streams.Episode{
		{RatingKey: "2"},   // older — the real reference
		{RatingKey: "100"}, // the triggering episode itself — must be excluded
	}
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{
			"2":   mkSelectedAudioEpisode("2"),
			"100": mkSelectedAudioEpisode("100"),
		},
	}

	ref, _, _ := findReferenceEpisode(context.Background(), plx, episodes, "100", maxRefSearchDepth)

	if ref == nil || ref.RatingKey != "2" {
		t.Fatalf("findReferenceEpisode ref = %+v, want episode 2 (triggering episode 100 must be excluded)", ref)
	}
	if got := countCalls(plx.CallNames(), "Episode:100"); got != 0 {
		t.Errorf("Episode:100 fetched %d times, want 0 (excluded episode must not be fetched)", got)
	}
	if got := countCalls(plx.CallNames(), "Episode:2"); got != 1 {
		t.Errorf("Episode:2 fetched %d times, want 1", got)
	}
}

// TestFindReferenceEpisode_CountsFetchErrors pins the fetchErrors return
// value of findReferenceEpisode and its ErrNotFound-vs-real-error
// discrimination: a candidate whose metadata fetch fails with a real
// transport/server error increments fetchErrors, while a benign
// plex.ErrNotFound (candidate has no metadata) does not. searched counts
// every candidate visited regardless of fetch outcome.
func TestFindReferenceEpisode_CountsFetchErrors(t *testing.T) {
	t.Parallel()
	episodes := []streams.Episode{{RatingKey: "1"}, {RatingKey: "2"}}
	tests := []struct {
		name            string
		plex            *fakeapi.Plex
		wantSearched    int
		wantFetchErrors int
	}{
		{
			name:            "real transport errors are counted",
			plex:            &fakeapi.Plex{EpisodeErr: errors.New("plex 503")},
			wantSearched:    2,
			wantFetchErrors: 2,
		},
		{
			name:            "ErrNotFound candidates are benign and not counted",
			plex:            &fakeapi.Plex{},
			wantSearched:    2,
			wantFetchErrors: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref, searched, fetchErrors := findReferenceEpisode(
				context.Background(), tc.plex, episodes, "exclude-none", maxRefSearchDepth)
			if ref != nil {
				t.Errorf("ref = %+v, want nil (no candidate has selected audio)", ref)
			}
			if searched != tc.wantSearched {
				t.Errorf("searched = %d, want %d", searched, tc.wantSearched)
			}
			if fetchErrors != tc.wantFetchErrors {
				t.Errorf("fetchErrors = %d, want %d", fetchErrors, tc.wantFetchErrors)
			}
		})
	}
}

// TestFindEpisodeReference_logsDegradedPlexAsWarn pins the
// candidate_fetch_errors observability contract: when every reference
// candidate fetch fails with a real (non-ErrNotFound) error, the search
// returns nil AND emits a WARN tagged reason=candidate_fetch_errors so
// operators can distinguish a degraded Plex from a show that genuinely
// has no reference yet (which stays a DEBUG reason=no_candidate). Both
// paths return nil, so the slog output is the only observable difference.
func TestFindEpisodeReference_logsDegradedPlexAsWarn(t *testing.T) {
	episodes := []streams.Episode{{RatingKey: "2"}, {RatingKey: "3"}}
	tests := []struct {
		name       string
		plex       *fakeapi.Plex
		wantReason string
		wantWarn   bool
	}{
		{
			name: "candidate fetches error logs WARN candidate_fetch_errors",
			plex: &fakeapi.Plex{
				ShowEpisodesByShow: map[string][]streams.Episode{"42": episodes},
				EpisodeErr:         errors.New("plex 503"),
			},
			wantReason: "candidate_fetch_errors",
			wantWarn:   true,
		},
		{
			name: "no candidate without fetch errors logs DEBUG no_candidate",
			plex: &fakeapi.Plex{
				ShowEpisodesByShow: map[string][]streams.Episode{"42": episodes},
			},
			wantReason: "no_candidate",
			wantWarn:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			s := newSyncer(Config{}, tc.plex, fakeapi.NewCache(), &fakeapi.Users{})
			ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}

			if ref := s.FindEpisodeReference(context.Background(), ep); ref != nil {
				t.Fatalf("FindEpisodeReference = %+v, want nil (no usable reference)", ref)
			}
			out := buf.String()
			if !strings.Contains(out, "reason="+tc.wantReason) {
				t.Errorf("log %q missing reason=%s", out, tc.wantReason)
			}
			if gotWarn := strings.Contains(out, "level=WARN"); gotWarn != tc.wantWarn {
				t.Errorf("WARN present = %v, want %v; log = %q", gotWarn, tc.wantWarn, out)
			}
		})
	}
}

// TestFindEpisodeReference_logsShowEpisodesFetchError pins the
// get_show_episodes_error observability branch of FindEpisodeReference
// (episode.go): when the admin reader's ShowEpisodes call fails, the search
// returns nil AND emits a WARN carrying the inviolate key "failed to fetch
// show episodes for reference" and the frozen reason label
// get_show_episodes_error. This is symmetric to the already-pinned
// candidate_fetch_errors branch and is the only observable difference from the
// benign no_grandparent_key / no_candidate DEBUG paths, which also return nil.
func TestFindEpisodeReference_logsShowEpisodesFetchError(t *testing.T) {
	plx := &fakeapi.Plex{ShowEpisodesErr: errors.New("plex 503")}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}
	s := newSyncer(Config{}, plx, fakeapi.NewCache(), &fakeapi.Users{})

	if ref := s.FindEpisodeReference(context.Background(), ep); ref != nil {
		t.Fatalf("FindEpisodeReference = %+v, want nil when ShowEpisodes errors", ref)
	}
	out := buf.String()
	if !strings.Contains(out, "reason=get_show_episodes_error") {
		t.Errorf("log %q missing reason=get_show_episodes_error", out)
	}
	if !strings.Contains(out, "failed to fetch show episodes for reference") {
		t.Errorf("log %q missing the inviolate WARN key 'failed to fetch show episodes for reference'", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("a ShowEpisodes fetch error must log at WARN; log = %q", out)
	}
}

// TestProcessNewOrUpdatedEpisodeAllUsers_SkipsUserWithNilClient pins the
// per-user-write-isolation fail-closed contract in the fan-out loop: when
// s.userClient returns nil for a user (its per-user Plex client cannot be
// built), that user is SKIPPED with a warn rather than processed under the
// admin client. Writing a shared user's selection under the admin token
// corrupts the admin's own per-user state (the documented h-f6 bug), so the
// skip is security-critical. A regression that dropped the guard would call
// applyEpisodeForUser with a nil client and panic on the first nil-interface
// Episode() call, so this fails loudly if the skip is removed.
//
// Not parallel: it swaps the process-global default slog logger.
func TestProcessNewOrUpdatedEpisodeAllUsers_SkipsUserWithNilClient(t *testing.T) {
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey: map[string]*streams.Episode{
			"2":   mkSelectedAudioEpisode("2"),
			"100": mkSelectedAudioEpisode("100"),
		},
	}
	users := &fakeapi.Users{AllResult: []api.UserInfo{
		{ID: "1", Name: "admin"},
		{ID: "2", Name: "bob"},
	}}
	s := NewSyncer(Config{}, plx, fakeapi.NewCache(), users,
		func(uid string) api.PlexReadWriter {
			if uid == "2" {
				return nil
			}
			return plx
		})
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), ep, "scan_new")

	if got := countCalls(plx.CallNames(), "Episode:100"); got != 1 {
		t.Errorf("target episode reloaded %d times; want 1 (user 2 has a nil client and must be skipped, not processed under admin)", got)
	}
	out := buf.String()
	if !strings.Contains(out, "skipping user") || !strings.Contains(out, "user_id=2") {
		t.Errorf("expected a warn skipping user_id=2 for a nil per-user client; log = %q", out)
	}
}

// TestProcessNewOrUpdatedEpisodeAllUsers_FallsBackToLanguageProfile pins the
// no-reference language-profile fallback in the fan-out path: when a show has
// no prior episode with a selection (ref == nil) and LANGUAGE_PROFILES is
// enabled, a new episode receives the user's learned audio->subtitle profile
// applied per user. This is the documented "applies your habits to brand-new
// shows" feature; a regression that skipped the fallback branch would leave
// the new episode's subtitle unset (SetSubtitle count 0).
func TestProcessNewOrUpdatedEpisodeAllUsers_FallsBackToLanguageProfile(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": nil}, // no candidate -> ref nil
	}
	users := &fakeapi.Users{AllResult: []api.UserInfo{{ID: "1", Name: "admin"}}}
	c := fakeapi.NewCache()
	c.LearnLanguageProfile("1", "jpn", "eng")
	s := newSyncer(Config{LanguageProfiles: true}, plx, c, users)
	ep := &streams.Episode{
		RatingKey:            "100",
		GrandparentRatingKey: "42",
		Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			{ID: 12, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
		}}}}},
	}

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), ep, "scan_new")

	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 1 {
		t.Errorf("SetSubtitle called %d times; want 1 (language-profile fallback must apply the learned eng subtitle when no reference exists)", got)
	}
}

// TestProcessNewOrUpdatedEpisodeAllUsers_AppliesReferenceAndLogsPerUser pins
// the successful per-user apply path: when a shared reference episode is found
// and a target episode needs a different audio track, the fan-out applies the
// change through the per-user client and emits the inviolate "new/updated
// episode language set" summary. The existing fan-out tests only exercise
// no-change targets, so the changed==true branch of applyEpisodeForUser and
// its documented log key are otherwise unverified.
//
// Not parallel: it swaps the process-global default slog logger.
func TestProcessNewOrUpdatedEpisodeAllUsers_AppliesReferenceAndLogsPerUser(t *testing.T) {
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey: map[string]*streams.Episode{
			"2": {RatingKey: "2", Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
				{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn", Selected: true},
			}}}}}},
			"100": targetNeedingAudioSwitch("100"),
		},
	}
	users := &fakeapi.Users{AllResult: []api.UserInfo{{ID: "1", Name: "admin"}}}
	s := newSyncer(Config{}, plx, fakeapi.NewCache(), users)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42", GrandparentTitle: "Show"}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), ep, "scan_new")

	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio called %d times; want 1 (reference jpn audio applied to the target for the one user)", got)
	}
	if out := buf.String(); !strings.Contains(out, "new/updated episode language set") {
		t.Errorf("missing inviolate 'new/updated episode language set' summary after a per-user change; log = %q", out)
	}
}

// TestFindReferenceEpisode_StopsOnCancelledContext pins the top-of-loop context
// guard in findReferenceEpisode (episode.go): on an already-cancelled context
// the search must return immediately with searched==0 and issue no per-candidate
// metadata fetch, so a shutdown mid-scan cannot keep hitting Plex. A mutant that
// dropped the top-of-loop ctx.Err() guard would fetch the newest candidate
// (which has a selected audio here) and return it as the reference.
func TestFindReferenceEpisode_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	episodes := []streams.Episode{{RatingKey: "1"}, {RatingKey: "2"}}
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{
			"1": mkSelectedAudioEpisode("1"),
			"2": mkSelectedAudioEpisode("2"),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ref, searched, fetchErrors := findReferenceEpisode(ctx, plx, episodes, "exclude-none", maxRefSearchDepth)

	if ref != nil {
		t.Errorf("findReferenceEpisode ref = %+v, want nil on a cancelled context", ref)
	}
	if searched != 0 {
		t.Errorf("searched = %d, want 0 (the top-of-loop ctx guard must stop before the first fetch)", searched)
	}
	if fetchErrors != 0 {
		t.Errorf("fetchErrors = %d, want 0", fetchErrors)
	}
	if got := len(plx.CallNames()); got != 0 {
		t.Errorf("Plex Episode fetched %d times on a cancelled context, want 0", got)
	}
}

// TestProcessNewOrUpdatedEpisodeAllUsers_StopsOnCancelledContext pins the
// per-user fan-out loop's context guard: a cancelled context halts the fan-out
// before any per-user apply. The reference search returns nil (empty show), so
// without the guard the loop would fall through to the language-profile path
// and apply the learned subtitle; the guard prevents that. Mirrors
// TestProcessNewOrUpdatedEpisodeAllUsers_FallsBackToLanguageProfile, which shows
// the same setup DOES apply the profile on a live context.
func TestProcessNewOrUpdatedEpisodeAllUsers_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": nil},
	}
	users := &fakeapi.Users{AllResult: []api.UserInfo{{ID: "1", Name: "admin"}}}
	c := fakeapi.NewCache()
	c.LearnLanguageProfile("1", "jpn", "eng")
	s := newSyncer(Config{LanguageProfiles: true}, plx, c, users)
	ep := &streams.Episode{
		RatingKey:            "100",
		GrandparentRatingKey: "42",
		Media: []streams.Media{{Part: []streams.Part{{ID: 7, Stream: []streams.Stream{
			{ID: 11, StreamType: streams.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			{ID: 12, StreamType: streams.StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
		}}}}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.ProcessNewOrUpdatedEpisodeAllUsers(ctx, ep, "scan_new")

	if got := countCalls(plx.CallNames(), "SetSubtitle"); got != 0 {
		t.Errorf("SetSubtitle called %d times on a cancelled context; want 0 (the fan-out loop's ctx guard must halt before applying the language profile)", got)
	}
}
