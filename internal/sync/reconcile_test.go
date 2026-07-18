package sync

import (
	"context"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// ---------------------------------------------------------------------------
// Event plane: ObserveAndPropagate records intents
// ---------------------------------------------------------------------------

// TestObserveAndPropagate_RecordsIntent pins the event-plane knowledge
// creation: a resolved play observation writes the (user, show) intent —
// audio language plus the "no subtitles" nil marker — so the reconcile
// plane and new-episode seeding can re-apply it later without re-deriving
// the user's choice from ambient reads.
func TestObserveAndPropagate_RecordsIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	s.ObserveAndPropagate(context.Background(), plx, "1", ref, "play")

	intent, ok := c.IntentFor("1", "42")
	if !ok {
		t.Fatal("ObserveAndPropagate did not record an intent for (user 1, show 42)")
	}
	if intent.Audio.LanguageCode != "jpn" {
		t.Errorf("intent audio = %q, want jpn", intent.Audio.LanguageCode)
	}
	if intent.Subtitle != nil {
		t.Errorf("intent subtitle = %+v, want nil (reference has no selected subtitle)", intent.Subtitle)
	}
	if intent.ObservedAt <= 0 {
		t.Errorf("intent ObservedAt = %d, want a positive unix timestamp", intent.ObservedAt)
	}
}

// TestObserveAndPropagate_IgnoredShowRecordsNoIntent extends the
// ignore-before-learn rule to the intent ledger: an ignored show must not
// contribute intents any more than profiles.
func TestObserveAndPropagate_IgnoredShowRecordsNoIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey: map[string]*plex.Show{
			"42": {RatingKey: "42", Label: []streams.Label{{Tag: "SKIP"}}},
		},
	}
	c := fakeapi.NewCache()
	policy := ignore.NewPolicy(nil, []string{"SKIP"})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, c, &fakeapi.Users{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	s.ObserveAndPropagate(context.Background(), plx, "1", ref, "play")

	if _, ok := c.IntentFor("1", "42"); ok {
		t.Error("intent recorded for an ignored show; the ignore gate must precede intent recording")
	}
}

// TestObserveAndPropagate_CommentaryAudioStillRecordsIntent pins the
// deliberate asymmetry between the two knowledge stores: profile learning
// skips commentary/descriptive tracks (cross-show generalization), but the
// per-show intent records them — a deliberate atypical choice for THIS
// show is exactly what the ledger should remember.
func TestObserveAndPropagate_CommentaryAudioStillRecordsIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {}},
	}
	c := fakeapi.NewCache()
	s := newSyncer(Config{UpdateLevel: LevelShow, LanguageProfiles: true}, plx, c, &fakeapi.Users{})
	ref := &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: "42",
		ParentRatingKey:      "7",
		GrandparentTitle:     "Show",
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{
				ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "eng",
				Title: "Director Commentary", Selected: true,
			},
		}}}}},
	}

	s.ObserveAndPropagate(context.Background(), plx, "1", ref, "play")

	if _, ok := c.SubtitleLangForAudio("1", "eng"); ok {
		t.Error("commentary track was learned into the cross-show profile; learning must skip descriptive tracks")
	}
	intent, ok := c.IntentFor("1", "42")
	if !ok {
		t.Fatal("commentary track did not record a per-show intent; intents must record deliberate atypical choices")
	}
	if intent.Audio.Title != "Director Commentary" {
		t.Errorf("intent audio title = %q, want the commentary title preserved", intent.Audio.Title)
	}
}

// ---------------------------------------------------------------------------
// Reconcile plane: ReconcileWithIntent
// ---------------------------------------------------------------------------

// seedIntent records a jpn-audio/no-subtitle intent for (user 1, show 42)
// observed at the given timestamp.
func seedIntent(c *fakeapi.Cache, observedAt int64) {
	c.RecordIntent("1", "42", streams.NewIntent(
		&streams.Stream{LanguageCode: "jpn"}, nil, observedAt))
}

// replayedEpisode builds the episode a history replay fetched: note its
// CURRENT selection is eng (someone else's later choice, or a media
// replacement default) — the reconcile plane must NOT propagate or learn
// from it.
func replayedEpisode() *streams.Episode {
	return &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: "42",
		ParentRatingKey:      "7",
		GrandparentTitle:     "Show",
		ParentIndex:          1,
		Index:                1,
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 10, StreamType: streams.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: streams.StreamTypeAudio, LanguageCode: "jpn"},
		}}}}},
	}
}

// TestReconcileWithIntent_AppliesRecordedIntentNotCurrentSelection is the
// core reconcile-plane contract: propagation is driven by the RECORDED
// intent (jpn), never by the replayed episode's current selection (eng).
// Under the old fabricating design this test would have propagated eng —
// another user's later choice — under user 1's identity.
func TestReconcileWithIntent_AppliesRecordedIntentNotCurrentSelection(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {{RatingKey: "2"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"2": targetNeedingAudioSwitch("2"), // eng selected, jpn available
		},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeapi.Users{})

	s.ReconcileWithIntent(context.Background(), plx, "1", replayedEpisode(), 500, "scheduler")

	// The target (eng selected, jpn available) must be switched to jpn —
	// the intent — proving the eng current selection of the replayed
	// episode was not used as the reference.
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio to the intent's jpn stream called %d times, want 1; calls=%v",
			got, plx.CallNames())
	}
	// And the replay must not learn: the profile store stays empty even
	// though the replayed episode carries an eng selection.
	if _, ok := c.SubtitleLangForAudio("1", "eng"); ok {
		t.Error("reconcile learned a profile from the replayed episode's current selection; the reconcile plane must never learn")
	}
	if _, ok := c.SubtitleLangForAudio("1", "jpn"); ok {
		t.Error("reconcile learned a profile from the intent; the reconcile plane must never learn")
	}
}

// TestReconcileWithIntent_NoIntentSkips pins "the safety net only replays
// knowledge, it never invents it": with no recorded intent, the replay
// does nothing — no episode fetches, no writes.
func TestReconcileWithIntent_NoIntentSkips(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeapi.Users{})

	s.ReconcileWithIntent(context.Background(), plx, "1", replayedEpisode(), 500, "scheduler")

	if calls := plx.CallNames(); len(calls) != 0 {
		t.Errorf("no Plex calls expected when no intent is recorded, got %v", calls)
	}
}

// TestReconcileWithIntent_NewerPlaySkips pins the freshness guard: a play
// NEWER than the recorded intent marks an unobserved interaction, and
// applying the older intent could revert a manual change made during the
// unobserved window. The item must be skipped entirely.
func TestReconcileWithIntent_NewerPlaySkips(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeapi.Users{})

	s.ReconcileWithIntent(context.Background(), plx, "1", replayedEpisode(), 2000, "scheduler")

	if calls := plx.CallNames(); len(calls) != 0 {
		t.Errorf("no Plex calls expected for a play newer than the intent (viewedAt 2000 > observedAt 1000), got %v", calls)
	}
}

// TestReconcileWithIntent_EqualTimestampApplies pins the guard's boundary:
// viewedAt == observedAt is the same-interaction case (the observation that
// recorded the intent IS the replayed play) and must reconcile, not skip —
// only a STRICTLY newer play marks an unobserved interaction.
func TestReconcileWithIntent_EqualTimestampApplies(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeapi.Users{})

	s.ReconcileWithIntent(context.Background(), plx, "1", replayedEpisode(), 1000, "scheduler")

	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio called %d times, want 1 (equal timestamps must reconcile); calls=%v",
			got, plx.CallNames())
	}
}

// TestReconcileWithIntent_IgnoredShowSkips pins that the ignore gate still
// guards the reconcile plane's writes: an intent recorded before a show
// was ignored must not propagate once the show carries the ignore label.
func TestReconcileWithIntent_IgnoredShowSkips(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey: map[string]*plex.Show{
			"42": {RatingKey: "42", Label: []streams.Label{{Tag: "SKIP"}}},
		},
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {{RatingKey: "2"}}},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	policy := ignore.NewPolicy(nil, []string{"SKIP"})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, c, &fakeapi.Users{})

	s.ReconcileWithIntent(context.Background(), plx, "1", replayedEpisode(), 500, "scheduler")

	if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
		t.Errorf("ShowEpisodes called for an ignored show; calls=%v", plx.CallNames())
	}
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 0 {
		t.Errorf("SetAudio called %d times for an ignored show, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// New-episode seeding: the intent tier
// ---------------------------------------------------------------------------

// TestProcessNewOrUpdatedEpisode_IntentTierBeatsSharedReference pins the
// per-user seeding order: a user with a recorded intent gets THEIR choice
// applied, and the shared reference search never runs when every user has
// an intent (the lazy OnceValue is never invoked — no ShowEpisodes call).
func TestProcessNewOrUpdatedEpisode_IntentTierBeatsSharedReference(t *testing.T) {
	t.Parallel()
	newEp := targetNeedingAudioSwitch("9") // eng selected, jpn available
	newEp.GrandparentRatingKey = "42"
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{"9": newEp},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000) // user 1 wants jpn for show 42
	users := &fakeapi.Users{AllResult: []api.UserInfo{{ID: "1", Name: "one"}}}
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, users)

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), newEp, "scan_new")

	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio to the intent's jpn stream called %d times, want 1; calls=%v",
			got, plx.CallNames())
	}
	if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 0 {
		t.Errorf("shared reference search ran (ShowEpisodes called) although every user has an intent; the lazy search must not fire; calls=%v", plx.CallNames())
	}
}

// TestProcessNewOrUpdatedEpisode_IntentlessUserFallsToReference pins the
// fallback: a user WITHOUT an intent still gets the shared reference
// search (existing behavior preserved as the second tier).
func TestProcessNewOrUpdatedEpisode_IntentlessUserFallsToReference(t *testing.T) {
	t.Parallel()
	newEp := targetNeedingAudioSwitch("9")
	newEp.GrandparentRatingKey = "42"
	// Reference episode "2" carries a selected jpn stream for the search.
	refEp := refWithSelectedAudio("jpn", "42", "7", 1, 1)
	refEp.RatingKey = "2"
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{
			"42": {{RatingKey: "2"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"9": newEp,
			"2": refEp,
		},
	}
	c := fakeapi.NewCache() // no intents
	users := &fakeapi.Users{AllResult: []api.UserInfo{{ID: "1", Name: "one"}}}
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, users)

	s.ProcessNewOrUpdatedEpisodeAllUsers(context.Background(), newEp, "scan_new")

	if got := countCalls(plx.CallNames(), "ShowEpisodes:42"); got != 1 {
		t.Errorf("shared reference search ran %d times, want 1 (intent-less user must fall back to it); calls=%v",
			got, plx.CallNames())
	}
	if got := countCalls(plx.CallNames(), "SetAudio"); got != 1 {
		t.Errorf("SetAudio from the searched reference called %d times, want 1; calls=%v",
			got, plx.CallNames())
	}
}
