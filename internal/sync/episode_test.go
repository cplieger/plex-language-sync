package sync

import (
	"context"
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

	ref, searched := findReferenceEpisode(context.Background(), plx, episodes, "exclude-none", maxDepth)

	if ref != nil {
		t.Errorf("findReferenceEpisode ref = %+v, want nil (no selected audio anywhere)", ref)
	}
	if searched != maxDepth {
		t.Errorf("findReferenceEpisode searched = %d, want %d (must stop exactly at the depth cap)", searched, maxDepth)
	}
}
