package tracksync

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/testsupport/metrictest"
	"github.com/cplieger/plexapi/v3"
)

// Stream ids every applyTarget part carries.
const (
	jpnAudioID = 9001
	engAudioID = 9002
	engSubID   = 9011
	freSubID   = 9012
)

// applyTarget is an episode of show 1001 as an episode list gives it: one
// part with Japanese and English audio and English and French subtitles,
// the streams named by selected marked selected, and the reading user's
// lastViewedAt.
func applyTarget(key string, part int, viewed int64, selected ...int) streams.Episode {
	list := []streams.Stream{
		pickAudio(jpnAudioID, "jpn", ""), pickAudio(engAudioID, "eng", ""),
		pickSub(engSubID, "eng", ""), pickSub(freSubID, "fre", ""),
	}
	for i := range list {
		list[i].Selected = slices.Contains(selected, int(list[i].ID))
	}
	return streams.Episode{
		RatingKey: key, GrandparentRatingKey: "1001", ParentIndex: 1,
		Media:        []streams.Media{{Part: []streams.Part{{ID: part, Stream: list}}}},
		LastViewedAt: streams.FlexInt(viewed),
	}
}

// withoutParts is ep as a list that sends no media gives it.
func withoutParts(ep streams.Episode) streams.Episode {
	ep.Media = nil
	return ep
}

// applyFixture serves each episode as the user's own read of it.
func applyFixture(t *testing.T, eps ...streams.Episode) (*applier, *fakeapi.Plex, *fakeapi.Cache) {
	t.Helper()
	plx := &fakeapi.Plex{EpisodeByKey: map[string]*streams.Episode{}}
	for i := range eps {
		ep := eps[i]
		plx.EpisodeByKey[ep.RatingKey] = &ep
	}
	c := fakeapi.NewCache()
	a := newApplier(c, &fakeUsers{}, nil)
	a.now = func() time.Time { return time.Unix(500, 0) }
	return a, plx, c
}

var intentBasis = streams.Basis{Version: 100, Source: streams.SourceIntent}

// intentPlan applies a Japanese audio and English subtitle choice observed
// at 100 to targets listed through the user's own client.
func intentPlan(targets ...streams.Episode) *plan {
	jpn, eng := pickAudio(0, "jpn", ""), pickSub(0, "eng", "")
	ref := streams.Pair{Audio: &jpn, Subtitle: &eng}
	return &plan{
		pick: pickForIntent(ref, langtag.TierSameLanguage), ref: ref,
		basis: intentBasis, targets: targets, observedAt: 100, userList: true,
	}
}

// profilePlan seeds English subtitles from a learned profile onto targets
// read with the admin token.
func profilePlan(targets ...streams.Episode) *plan {
	return &plan{
		pick:    pickForProfile("eng", langtag.TierSameLanguage),
		basis:   streams.Basis{Version: profileVersion("jpn", "eng"), Source: streams.SourceProfile},
		targets: targets,
	}
}

func episodeReads(plx *fakeapi.Plex) int {
	n := 0
	for _, c := range plx.CallNames() {
		if strings.HasPrefix(c, "Episode:") {
			n++
		}
	}
	return n
}

func recordOn(part int, basis streams.Basis, at int64) streams.Applied {
	return streams.Applied{Basis: basis, At: at, PartID: part, AudioID: jpnAudioID, SubtitleID: engSubID}
}

func TestApplyRule1SkipsWithoutRead(t *testing.T) {
	t.Parallel()
	ep := applyTarget("2001", 5001, 0, engAudioID, freSubID)
	a, plx, c := applyFixture(t, ep)
	c.RecordApplied("alice", "2001", recordOn(5001, intentBasis, 200))

	written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(ep))
	if err != nil || written != 0 {
		t.Errorf("applyLocked = %d, %v; want 0, nil", written, err)
	}
	if calls := plx.CallNames(); len(calls) != 0 {
		t.Errorf("a part already holding the plan's basis was read or written: %v", calls)
	}
}

func TestApplyRule1PartChanged(t *testing.T) {
	t.Parallel()
	ep := applyTarget("2001", 5001, 0, engAudioID, engSubID)
	a, plx, c := applyFixture(t, ep)
	c.RecordApplied("alice", "2001", recordOn(4001, intentBasis, 200))

	written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(ep))
	if err != nil || written != 1 {
		t.Errorf("applyLocked = %d, %v; want 1, nil", written, err)
	}
	if got, want := plx.Writes(), []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}}; !slices.Equal(got, want) {
		t.Errorf("writes = %v; want %v", got, want)
	}
	if rec, _ := c.AppliedFor("alice", "2001"); rec.PartID != 5001 {
		t.Errorf("record part = %d; want 5001", rec.PartID)
	}
}

func TestApplyStaleRecordIgnoresViews(t *testing.T) {
	t.Parallel()
	ep := applyTarget("2001", 5001, 900, engAudioID, engSubID)
	a, plx, c := applyFixture(t, ep)
	c.RecordApplied("alice", "2001", recordOn(4001, streams.Basis{Version: 50, Source: streams.SourceIntent}, 200))

	written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(ep))
	if err != nil || written != 1 {
		t.Errorf("applyLocked = %d, %v; want 1, nil: a view of the previous file does not hold the new one back", written, err)
	}
	rec, ok := c.AppliedFor("alice", "2001")
	if !ok {
		t.Fatal("no record after writing the new file")
	}
	if want := (streams.Applied{Basis: intentBasis, At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleID: engSubID}); rec != want {
		t.Errorf("record = %+v; want %+v", rec, want)
	}
}

// TestApplyUnknownSubtitleIgnoresViews pins that a part whose subtitle was
// not read back is rewritten after the user's view of it: the gap is in the
// app's own record, and the next pass is what closes it.
func TestApplyUnknownSubtitleIgnoresViews(t *testing.T) {
	t.Parallel()
	unknown := streams.Applied{At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleUnknown: true}
	tests := []struct {
		plan  func(...streams.Episode) *plan
		name  string
		basis streams.Basis
		// listedView is the view in the target as listed; the user's own
		// read always carries 600.
		listedView int64
	}{
		{name: "an intent plan decides from the user's list", plan: intentPlan, basis: intentBasis, listedView: 600},
		{
			name: "a seed plan decides from the user's read", plan: profilePlan, listedView: 0,
			basis: streams.Basis{Version: profileVersion("jpn", "eng"), Source: streams.SourceProfile},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			user := applyTarget("2001", 5001, 600, jpnAudioID, freSubID)
			listed := user
			listed.LastViewedAt = streams.FlexInt(tc.listedView)
			a, plx, c := applyFixture(t, user)
			c.RecordApplied("alice", "2001", unknown)

			written, err := a.applyLocked(t.Context(), plx, "alice", tc.plan(listed))
			if err != nil || written != 1 {
				t.Errorf("applyLocked = %d, %v; want 1, nil: a view does not hold back an unknown subtitle", written, err)
			}
			if got, want := plx.Writes(), []fakeapi.Write{{Kind: "subtitle", PartID: 5001, StreamID: engSubID}}; !slices.Equal(got, want) {
				t.Errorf("writes = %v; want %v", got, want)
			}
			want := streams.Applied{Basis: tc.basis, At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleID: engSubID}
			if rec, ok := c.AppliedFor("alice", "2001"); !ok || rec != want {
				t.Errorf("record = %+v, %v; want %+v", rec, ok, want)
			}
		})
	}
}

func TestApplyRule2SkipsViewedAfter(t *testing.T) {
	t.Parallel()
	older := streams.Basis{Version: 50, Source: streams.SourceIntent}
	tests := []struct {
		name      string
		record    *streams.Applied
		viewed    int64
		wantWrite bool
	}{
		{name: "viewed after the record", record: new(recordOn(5001, older, 200)), viewed: 300},
		{name: "viewed before the record", record: new(recordOn(5001, older, 200)), viewed: 150, wantWrite: true},
		{name: "viewed when the record was written", record: new(recordOn(5001, older, 200)), viewed: 200, wantWrite: true},
		{name: "viewed after the choice with no record", viewed: 150},
		{name: "viewed before the choice with no record", viewed: 50, wantWrite: true},
		{name: "viewed after an older record and before the choice", record: new(recordOn(5001, older, 80)), viewed: 90, wantWrite: true},
		{name: "never viewed with no record", wantWrite: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := applyTarget("2001", 5001, tc.viewed, engAudioID, engSubID)
			a, plx, c := applyFixture(t, ep)
			if tc.record != nil {
				c.RecordApplied("alice", "2001", *tc.record)
			}
			written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(ep))
			if err != nil {
				t.Fatalf("applyLocked error = %v", err)
			}
			if got := written == 1; got != tc.wantWrite {
				t.Errorf("written = %d; want a write: %v (writes %v)", written, tc.wantWrite, plx.Writes())
			}
			if !tc.wantWrite && len(plx.CallNames()) != 0 {
				t.Errorf("a listed view decides without a read; calls = %v", plx.CallNames())
			}
		})
	}
}

func TestApplyRule2ReadsUserViewNotList(t *testing.T) {
	t.Parallel()
	t.Run("an admin view in the listed target is not the user's", func(t *testing.T) {
		t.Parallel()
		user := applyTarget("2001", 5001, 0, jpnAudioID, freSubID)
		listed := user
		listed.LastViewedAt = 900
		a, plx, _ := applyFixture(t, user)

		written, err := a.applyLocked(t.Context(), plx, "alice", profilePlan(listed))
		if err != nil || written != 1 {
			t.Errorf("applyLocked = %d, %v; want 1, nil", written, err)
		}
		if got, want := plx.Writes(), []fakeapi.Write{{Kind: "subtitle", PartID: 5001, StreamID: engSubID}}; !slices.Equal(got, want) {
			t.Errorf("writes = %v; want %v", got, want)
		}
	})
	t.Run("the user's own view from the read holds the write back", func(t *testing.T) {
		t.Parallel()
		user := applyTarget("2001", 5001, 900, jpnAudioID, freSubID)
		listed := user
		listed.LastViewedAt = 0
		a, plx, c := applyFixture(t, user)

		written, err := a.applyLocked(t.Context(), plx, "alice", profilePlan(listed))
		if err != nil || written != 0 {
			t.Errorf("applyLocked = %d, %v; want 0, nil", written, err)
		}
		if reads := episodeReads(plx); reads != 1 {
			t.Errorf("episode reads = %d; want 1", reads)
		}
		if w := plx.Writes(); len(w) != 0 {
			t.Errorf("writes = %v; want none", w)
		}
		if _, ok := c.AppliedFor("alice", "2001"); ok {
			t.Error("a skipped target was recorded")
		}
	})
}

func TestApplyWritesOnlyDifferingKinds(t *testing.T) {
	t.Parallel()
	jpn := pickAudio(0, "jpn", "")
	spa := pickSub(0, "spa", "")
	tests := []struct {
		name       string
		ref        streams.Pair
		selected   []int
		writes     []fakeapi.Write
		wantRecord [2]int // audio, subtitle
	}{
		{
			name:       "audio already right and subtitle wrong",
			ref:        streams.Pair{Audio: &jpn, Subtitle: new(pickSub(0, "eng", ""))},
			selected:   []int{jpnAudioID, freSubID},
			writes:     []fakeapi.Write{{Kind: "subtitle", PartID: 5001, StreamID: engSubID}},
			wantRecord: [2]int{jpnAudioID, engSubID},
		},
		{
			name:       "nothing differs",
			ref:        streams.Pair{Audio: &jpn, Subtitle: new(pickSub(0, "eng", ""))},
			selected:   []int{jpnAudioID, engSubID},
			wantRecord: [2]int{jpnAudioID, engSubID},
		},
		{
			name:       "a choice of no subtitles turns the selected one off",
			ref:        streams.Pair{Audio: &jpn},
			selected:   []int{engAudioID, engSubID},
			writes:     []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}, {Kind: "subtitle-off", PartID: 5001}},
			wantRecord: [2]int{jpnAudioID, 0},
		},
		{
			name:       "subtitles already off",
			ref:        streams.Pair{Audio: &jpn},
			selected:   []int{jpnAudioID},
			wantRecord: [2]int{jpnAudioID, 0},
		},
		{
			name:       "no subtitle candidate records the current one",
			ref:        streams.Pair{Audio: &jpn, Subtitle: &spa},
			selected:   []int{engAudioID, freSubID},
			writes:     []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}},
			wantRecord: [2]int{jpnAudioID, freSubID},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := applyTarget("2001", 5001, 0, tc.selected...)
			a, plx, c := applyFixture(t, ep)
			p := intentPlan(ep)
			p.ref, p.pick = tc.ref, pickForIntent(tc.ref, langtag.TierSameLanguage)

			written, err := a.applyLocked(t.Context(), plx, "alice", p)
			if err != nil {
				t.Fatalf("applyLocked error = %v", err)
			}
			if want := min(len(tc.writes), 1); written != want {
				t.Errorf("written = %d; want %d", written, want)
			}
			if got := plx.Writes(); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %v; want %v", got, tc.writes)
			}
			rec, ok := c.AppliedFor("alice", "2001")
			if !ok {
				t.Fatal("no record after applying")
			}
			if got := [2]int{rec.AudioID, rec.SubtitleID}; got != tc.wantRecord {
				t.Errorf("record ids = %v; want %v", got, tc.wantRecord)
			}
			if rec.Basis != intentBasis || rec.At != 500 || rec.PartID != 5001 {
				t.Errorf("record = %+v; want basis %+v at 500 on part 5001", rec, intentBasis)
			}
		})
	}
}

// autoSubClient behaves as Plex does on a part where the user never set a
// subtitle: an accepted audio write also selects sub there (0 turns
// subtitles off). Once a write was accepted, reads fail with reloadErr, or
// end the context through cancelOnReload.
type autoSubClient struct {
	*fakeapi.Plex
	reloadErr      error
	cancelOnReload context.CancelFunc
	sub            int
	written        bool
}

func (c *autoSubClient) Episode(ctx context.Context, key plex.RatingKey) (*streams.Episode, error) {
	ep, err := c.Plex.Episode(ctx, key)
	switch {
	case err != nil || !c.written:
		return ep, err
	case c.cancelOnReload != nil:
		c.cancelOnReload()
		return nil, ctx.Err()
	case c.reloadErr != nil:
		return nil, c.reloadErr
	}
	return ep, nil
}

func (c *autoSubClient) SetAudioStream(ctx context.Context, sel plexapi.StreamSelection) error {
	if err := c.Plex.SetAudioStream(ctx, sel); err != nil {
		return err
	}
	c.written = true
	for _, ep := range c.EpisodeByKey {
		for i := range ep.Media[0].Part[0].Stream {
			s := &ep.Media[0].Part[0].Stream[i]
			switch {
			case s.IsAudio():
				s.Selected = int(s.ID) == sel.StreamID
			case s.IsSubtitle():
				s.Selected = int(s.ID) == c.sub
			}
		}
	}
	return nil
}

func TestApplySubtitleDecidedAfterAudioWrite(t *testing.T) {
	t.Parallel()
	jpn := pickAudio(0, "jpn", "")
	eng, spa := pickSub(0, "eng", ""), pickSub(0, "spa", "")
	tests := []struct {
		name       string
		ref        streams.Pair
		selected   []int
		writes     []fakeapi.Write
		auto       int    // the subtitle Plex selects when the audio changes
		wantRecord [2]int // audio, subtitle
	}{
		{
			name: "a subtitle Plex switched with the audio is set back", ref: streams.Pair{Audio: &jpn, Subtitle: &eng},
			selected: []int{engAudioID, engSubID}, auto: freSubID,
			writes:     []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}, {Kind: "subtitle", PartID: 5001, StreamID: engSubID}},
			wantRecord: [2]int{jpnAudioID, engSubID},
		},
		{
			name: "subtitles Plex turned on with the audio are turned off", ref: streams.Pair{Audio: &jpn},
			selected: []int{engAudioID}, auto: engSubID,
			writes:     []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}, {Kind: "subtitle-off", PartID: 5001}},
			wantRecord: [2]int{jpnAudioID, 0},
		},
		{
			name: "no subtitle candidate records the one Plex picked", ref: streams.Pair{Audio: &jpn, Subtitle: &spa},
			selected: []int{engAudioID, engSubID}, auto: freSubID,
			writes:     []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}},
			wantRecord: [2]int{jpnAudioID, freSubID},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := applyTarget("2001", 5001, 0, tc.selected...)
			a, plx, c := applyFixture(t, ep)
			client := &autoSubClient{Plex: plx, sub: tc.auto}
			p := intentPlan(ep)
			p.ref, p.pick = tc.ref, pickForIntent(tc.ref, langtag.TierSameLanguage)

			if _, err := a.applyLocked(t.Context(), client, "alice", p); err != nil {
				t.Fatalf("applyLocked error = %v", err)
			}
			if got := plx.Writes(); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %v; want %v", got, tc.writes)
			}
			rec, _ := c.AppliedFor("alice", "2001")
			if got := [2]int{rec.AudioID, rec.SubtitleID}; got != tc.wantRecord {
				t.Errorf("record ids = %v; want %v, what the part holds", got, tc.wantRecord)
			}
		})
	}
}

// TestApplyReloadFailsAfterAudioWrite pins that a part whose subtitle could
// not be read back after its audio write is recorded with the subtitle
// unknown, so a later play of it, on whatever subtitle Plex picked, is an
// echo of the app's write and not the user's choice.
func TestApplyReloadFailsAfterAudioWrite(t *testing.T) {
	tests := []struct {
		name     string
		wantErr  error
		auto     int // the subtitle Plex selects when the audio changes
		cancel   bool
		warnings int
	}{
		{name: "failed read, subtitle kept", auto: engSubID, warnings: 1},
		{name: "failed read, Plex picked another subtitle", auto: freSubID, warnings: 1},
		{name: "context ended during the read", auto: freSubID, cancel: true, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			ep := applyTarget("2001", 5001, 0, engAudioID, engSubID)
			a, plx, c := applyFixture(t, ep)
			ctx := t.Context()
			client := &autoSubClient{Plex: plx, sub: tc.auto, reloadErr: errors.New("plex unavailable")}
			if tc.cancel {
				ctx, client.cancelOnReload = context.WithCancel(ctx)
			}
			p := intentPlan(ep)

			if _, err := a.applyLocked(ctx, client, "alice", p); !errors.Is(err, tc.wantErr) {
				t.Fatalf("applyLocked error = %v; want %v", err, tc.wantErr)
			}
			if got, want := plx.Writes(), []fakeapi.Write{{Kind: "audio", PartID: 5001, StreamID: jpnAudioID}}; !slices.Equal(got, want) {
				t.Errorf("writes = %v; want %v", got, want)
			}
			want := streams.Applied{At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleUnknown: true}
			rec, ok := c.AppliedFor("alice", "2001")
			if !ok || rec != want {
				t.Errorf("record = %+v, %v; want %+v: the audio written, the subtitle unknown, no basis", rec, ok, want)
			}
			if got := strings.Count(logs.String(), `level=WARN msg="failed to reload episode"`); got != tc.warnings {
				t.Errorf("reload warnings = %d; want %d:\n%s", got, tc.warnings, logs)
			}
			intent := streams.NewIntent(p.ref, streams.Anchor{RatingKey: "2000", Season: 1, Index: 1, AudioID: jpnAudioID, SubtitleID: engSubID}, 100)
			in := classifyInput{
				Played: plx.EpisodeByKey["2001"], Intent: *intent, Applied: rec,
				Floor: langtag.TierSameLanguage, HasIntent: true, HasApplied: ok,
			}
			if got, _ := classify(&in); got != verdictEcho {
				t.Errorf("classify of the part as it now stands = %v; want %v", got, verdictEcho)
			}
		})
	}
}

func TestApplyNotFoundForgets(t *testing.T) {
	t.Parallel()
	gone := applyTarget("2001", 5001, 0, engAudioID)
	a, plx, c := applyFixture(t)
	c.RecordApplied("alice", "2001", recordOn(5001, streams.Basis{Version: 50, Source: streams.SourceIntent}, 200))

	written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(gone))
	if err != nil || written != 0 {
		t.Errorf("applyLocked = %d, %v; want 0, nil", written, err)
	}
	if _, ok := c.AppliedFor("alice", "2001"); ok {
		t.Error("the record of an episode Plex no longer has was kept")
	}
}

func TestApplyFailedRequest(t *testing.T) {
	failure := errors.New("plex unavailable")
	// held is what a part holds after its audio write was accepted and its
	// subtitle write failed: no basis, so no plan's rule 1 skips it.
	held := func(part int) *streams.Applied {
		return &streams.Applied{At: 500, PartID: part, AudioID: jpnAudioID, SubtitleID: freSubID}
	}
	tests := []struct {
		name   string
		setup  func(*fakeapi.Plex)
		noSubs bool                // the choice is Japanese audio with no subtitles
		want   [2]*streams.Applied // records on 2001 and 2002
		msg    string
		calls  int // writes attempted across both targets
		reads  int // episode reads across both targets
	}{
		{name: "failed audio write", setup: func(p *fakeapi.Plex) { p.SetAudioErr = failure }, msg: "failed to set audio stream", calls: 2, reads: 2},
		{
			name: "audio written and subtitle failed", setup: func(p *fakeapi.Plex) { p.SetSubtitleErr = failure },
			want: [2]*streams.Applied{held(5001), held(5002)}, msg: "failed to set subtitle stream", calls: 4, reads: 4,
		},
		{
			name: "audio written and turning subtitles off failed", setup: func(p *fakeapi.Plex) { p.DisableErr = failure }, noSubs: true,
			want: [2]*streams.Applied{held(5001), held(5002)}, msg: "failed to disable subtitles", calls: 4, reads: 4,
		},
		{name: "failed read", setup: func(p *fakeapi.Plex) { p.EpisodeErr = failure }, msg: "failed to reload episode", reads: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			e1 := applyTarget("2001", 5001, 0, engAudioID, freSubID)
			e2 := applyTarget("2002", 5002, 0, engAudioID, freSubID)
			a, plx, c := applyFixture(t, e1, e2)
			tc.setup(plx)
			p := intentPlan(e1, e2)
			if tc.noSubs {
				p.ref.Subtitle = nil
				p.pick = pickForIntent(p.ref, langtag.TierSameLanguage)
			}

			if _, err := a.applyLocked(t.Context(), plx, "alice", p); err != nil {
				t.Fatalf("applyLocked error = %v; a failed request is logged, not returned", err)
			}
			if reads := episodeReads(plx); reads != tc.reads {
				t.Errorf("episode reads = %d; want %d: a failure on one target does not stop the next", reads, tc.reads)
			}
			attempted := countCalls(plx.CallNames(), "SetAudio") + countCalls(plx.CallNames(), "SetSubtitle") +
				countCalls(plx.CallNames(), "DisableSubtitle")
			if attempted != tc.calls {
				t.Errorf("writes attempted = %d; want %d", attempted, tc.calls)
			}
			for i, key := range []string{"2001", "2002"} {
				rec, ok := c.AppliedFor("alice", key)
				switch want := tc.want[i]; {
				case want == nil && ok:
					t.Errorf("episode %s was recorded after a failed request that changed nothing: %+v", key, rec)
				case want != nil && (!ok || rec != *want):
					t.Errorf("episode %s record = %+v, %v; want %+v", key, rec, ok, *want)
				}
			}
			if got := strings.Count(logs.String(), "level=WARN msg=\""+tc.msg+"\""); got != 2 {
				t.Errorf("%q warnings = %d; want one per target:\n%s", tc.msg, got, logs)
			}
		})
	}
}

func TestApplyCancelledBetweenTargets(t *testing.T) {
	tests := []struct {
		name      string
		on        string // the write kind whose call cancels
		failWrite bool   // the cancelled write itself fails, as a request the context ended does
		selected  []int  // what the first target holds before the run
		want      *streams.Applied
	}{
		{
			name: "cancelled after a write", on: "audio", selected: []int{engAudioID, engSubID},
			want: &streams.Applied{Basis: intentBasis, At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleID: engSubID},
		},
		{name: "a write ended by the cancellation", on: "audio", failWrite: true, selected: []int{engAudioID, engSubID}},
		{
			name: "a subtitle write ended by the cancellation after the audio write",
			on:   "subtitle", failWrite: true, selected: []int{engAudioID, freSubID},
			want: &streams.Applied{At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleID: freSubID},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			e1 := applyTarget("2001", 5001, 0, tc.selected...)
			e2 := applyTarget("2002", 5002, 0, engAudioID, engSubID)
			a, plx, c := applyFixture(t, e1, e2)
			ctx, cancel := context.WithCancel(t.Context())
			client := &cancellingClient{Plex: plx, cancel: cancel, on: tc.on, fail: tc.failWrite}

			_, err := a.applyLocked(ctx, client, "alice", intentPlan(e1, e2))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("applyLocked error = %v; want context.Canceled", err)
			}
			if got := countCalls(plx.CallNames(), "Episode:2002"); got != 0 {
				t.Errorf("the target after the cancellation was read %d times", got)
			}
			if _, ok := c.AppliedFor("alice", "2002"); ok {
				t.Error("the target after the cancellation was recorded")
			}
			rec, ok := c.AppliedFor("alice", "2001")
			switch {
			case tc.want == nil && ok:
				t.Errorf("first target recorded %+v; want no record", rec)
			case tc.want != nil && (!ok || rec != *tc.want):
				t.Errorf("first target record = %+v, %v; want %+v", rec, ok, *tc.want)
			}
			if strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("a cancellation logged above Debug:\n%s", logs)
			}
		})
	}
}

func TestApplyCancelledOnLastTarget(t *testing.T) {
	t.Parallel()
	ep := applyTarget("2001", 5001, 0, engAudioID, freSubID)
	a, plx, c := applyFixture(t, ep)
	ctx, cancel := context.WithCancel(t.Context())
	client := &cancellingClient{Plex: plx, cancel: cancel, on: "subtitle", fail: true}

	written, err := a.applyLocked(ctx, client, "alice", intentPlan(ep))
	if !errors.Is(err, context.Canceled) || written != 1 {
		t.Errorf("applyLocked = %d, %v; want 1, context.Canceled: the run ended inside its last target", written, err)
	}
	want := streams.Applied{At: 500, PartID: 5001, AudioID: jpnAudioID, SubtitleID: freSubID}
	if rec, ok := c.AppliedFor("alice", "2001"); !ok || rec != want {
		t.Errorf("record = %+v, %v; want %+v", rec, ok, want)
	}
}

func TestApplyListWithoutParts(t *testing.T) {
	t.Parallel()
	t.Run("a current record skips after the read", func(t *testing.T) {
		t.Parallel()
		ep := applyTarget("2001", 5001, 0, engAudioID, freSubID)
		a, plx, c := applyFixture(t, ep)
		c.RecordApplied("alice", "2001", recordOn(5001, intentBasis, 200))

		written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(withoutParts(ep)))
		if err != nil || written != 0 {
			t.Errorf("applyLocked = %d, %v; want 0, nil", written, err)
		}
		if reads := episodeReads(plx); reads != 1 {
			t.Errorf("episode reads = %d; want 1", reads)
		}
		if w := plx.Writes(); len(w) != 0 {
			t.Errorf("writes = %v; want none", w)
		}
	})
	t.Run("no record writes after the read", func(t *testing.T) {
		t.Parallel()
		ep := applyTarget("2001", 5001, 0, engAudioID, engSubID)
		a, plx, c := applyFixture(t, ep)

		written, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(withoutParts(ep)))
		if err != nil || written != 1 {
			t.Errorf("applyLocked = %d, %v; want 1, nil", written, err)
		}
		if rec, ok := c.AppliedFor("alice", "2001"); !ok || rec.PartID != 5001 {
			t.Errorf("record = %+v, %v; want one on part 5001", rec, ok)
		}
	})
}

func TestShowLockSerializes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		held := applyTarget("2001", 5001, 0, engAudioID, engSubID)
		waiting := applyTarget("2002", 5002, 0, engAudioID, engSubID)
		otherShow := applyTarget("3001", 5003, 0, engAudioID, engSubID)
		otherShow.GrandparentRatingKey = "1002"
		otherUser := applyTarget("2003", 5004, 0, engAudioID, engSubID)
		a, plx, _ := applyFixture(t, held, waiting, otherShow, otherUser)

		run := func(user, show string, ep streams.Episode) {
			unlock, err := a.lock(t.Context(), user, show)
			if err != nil {
				t.Errorf("lock(%s, %s) error = %v", user, show, err)
				return
			}
			defer unlock()
			if _, err := a.applyLocked(t.Context(), plx, user, intentPlan(ep)); err != nil {
				t.Errorf("applyLocked error = %v", err)
			}
		}
		unlock, err := a.lock(t.Context(), "alice", "1001")
		if err != nil {
			t.Fatalf("lock error = %v", err)
		}
		go run("alice", "1001", waiting)
		go run("alice", "1002", otherShow)
		go run("bob", "1001", otherUser)
		synctest.Wait()
		got := writtenParts(plx)
		slices.Sort(got)
		if want := []int{5003, 5004}; !slices.Equal(got, want) {
			t.Errorf("parts written while alice's show is held = %v; want %v: another show and another user proceed", got, want)
		}

		if _, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(held)); err != nil {
			t.Errorf("applyLocked error = %v", err)
		}
		unlock()
		synctest.Wait()
		if got, want := writtenParts(plx), []int{5001, 5002}; len(got) != 4 || !slices.Equal(got[2:], want) {
			t.Errorf("parts written = %v; want two others, then %v: the waiting decision writes after the holder's", got, want)
		}
	})
}

// writtenParts is the part of each accepted write, a part written for several
// kinds in a row listed once.
func writtenParts(plx *fakeapi.Plex) []int {
	var parts []int
	for _, w := range plx.Writes() {
		if len(parts) == 0 || parts[len(parts)-1] != w.PartID {
			parts = append(parts, w.PartID)
		}
	}
	return parts
}

func TestApplyCountsEachPutOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subErr                  error
		name                    string
		wantSubSet, wantSubFail float64
	}{
		{name: "both accepted", wantSubSet: 1},
		{name: "subtitle refused", subErr: errors.New("refused"), wantSubFail: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := applyTarget("2001", 5001, 0, engAudioID, freSubID)
			a, plx, _ := applyFixture(t, ep)
			plx.SetSubtitleErr = tc.subErr
			m := obs.New()
			a.metrics = m

			if _, err := a.applyLocked(t.Context(), plx, "alice", intentPlan(ep)); err != nil {
				t.Fatalf("applyLocked: %v", err)
			}
			for _, c := range []struct {
				kind, result string
				want         float64
			}{
				{"audio", "set", 1},
				{"audio", "failed", 0},
				{"subtitle", "set", tc.wantSubSet},
				{"subtitle", "failed", tc.wantSubFail},
			} {
				if got := metrictest.Value(t, m, "plex_language_sync_track_updates_total", c.kind, c.result); got != c.want {
					t.Errorf("track_updates{%s,%s} = %v, want %v", c.kind, c.result, got, c.want)
				}
			}
		})
	}
}
