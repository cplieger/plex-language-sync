package tracksync

import (
	"strconv"
	"testing"

	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v3"
)

// pairView is a pair reduced to "<id>:<language>" per kind, "" for none. An
// intent's reference streams carry id 0, so the view shows which side of a
// merge each kind came from.
type pairView struct{ audio, subtitle string }

func viewOf(p streams.Pair) pairView {
	view := func(s *streams.Stream) string {
		if s == nil {
			return ""
		}
		return strconv.Itoa(int(s.ID)) + ":" + s.LanguageCode
	}
	return pairView{audio: view(p.Audio), subtitle: view(p.Subtitle)}
}

func on(s streams.Stream) streams.Stream {
	s.Selected = true
	return s
}

func playedEp(key string, index int, list ...streams.Stream) *streams.Episode {
	ep := pickEp(list...)
	ep.RatingKey, ep.ParentIndex, ep.Index = key, 1, streams.FlexInt(index)
	return ep
}

func regionAudio(id int, tag string) streams.Stream {
	return streams.Stream{ID: plexapi.FlexInt(id), StreamType: plexapi.StreamTypeAudio, LanguageCode: "por", LanguageTag: tag}
}

func intentOf(audio streams.Stream, sub *streams.Stream, anchor streams.Anchor) streams.Intent {
	return *streams.NewIntent(streams.Pair{Audio: &audio, Subtitle: sub}, anchor, 100)
}

func appliedOn(audioID, subID int) streams.Applied {
	return streams.Applied{Basis: streams.Basis{Version: 100, Source: streams.SourceIntent}, At: 200, PartID: 5001, AudioID: audioID, SubtitleID: subID}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	dub := pickAudio(9001, "eng", "")
	jpn := pickAudio(9002, "jpn", "")
	ptPT := regionAudio(9003, "pt-PT")
	commentary := pickAudio(9004, "jpn", "Commentary")
	eng := pickSub(9011, "eng", "")
	fre := pickSub(9012, "fre", "")

	onE1 := streams.Anchor{RatingKey: "1001", Season: 1, Index: 1, AudioID: 9002, SubtitleID: 9011}
	intentJ := intentOf(jpn, &eng, onE1)
	intentDub := intentOf(dub, nil, streams.Anchor{RatingKey: "1001", Season: 1, Index: 1, AudioID: 9001})
	intentPtBR := intentOf(regionAudio(9005, "pt-BR"), nil, streams.Anchor{RatingKey: "1001", Season: 1, Index: 1, AudioID: 9005})
	intentNoSub := intentOf(jpn, nil, streams.Anchor{RatingKey: "1001", Season: 1, Index: 1, AudioID: 9002})
	intentCommentary := intentOf(commentary, &eng, streams.Anchor{RatingKey: "1001", Season: 1, Index: 1, AudioID: 9004, SubtitleID: 9011})
	intentLegacy := intentOf(jpn, &eng, streams.Anchor{})
	stale := appliedOn(9001, 9011)
	stale.PartID = 4001
	subtitleUnknown := streams.Applied{At: 200, PartID: 5001, AudioID: 9002, SubtitleUnknown: true}

	inherited := pairView{audio: "0:jpn", subtitle: "0:eng"}
	tests := []struct {
		name string
		in   ClassifyInput
		want Verdict
		pair pairView
	}{
		{
			name: "no selected audio and no intent",
			in:   ClassifyInput{Played: playedEp("1001", 1, jpn, dub, on(eng))},
			want: VerdictNoAudio,
		},
		{
			name: "no selected audio with an intent and a current record",
			in: ClassifyInput{
				Played: playedEp("1007", 7, jpn, dub, on(eng)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9002, 9011), HasApplied: true,
			},
			want: VerdictNoAudio,
		},
		{
			name: "no intent with a seed record equal to the play picks both kinds",
			in: ClassifyInput{
				Played:  playedEp("1007", 7, on(jpn), dub, on(eng)),
				Applied: streams.Applied{Basis: streams.Basis{From: "bob", Version: 90, Source: streams.SourceHousehold}, PartID: 5001, AudioID: 9002, SubtitleID: 9011}, HasApplied: true,
			},
			want: VerdictChoice, pair: pairView{audio: "9002:jpn", subtitle: "9011:eng"},
		},
		{
			name: "no intent and no record picks both kinds",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(dub), on(fre))},
			want: VerdictChoice, pair: pairView{audio: "9001:eng", subtitle: "9012:fre"},
		},
		{
			name: "missing track: the dub the app left is an echo",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(dub), on(eng)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9001, 9011), HasApplied: true,
			},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "missing track with no record is an echo",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(dub), on(eng)), Intent: intentJ, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "substitute region the app left is an echo",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(ptPT)), Intent: intentPtBR, HasIntent: true,
				Applied: appliedOn(9003, 0), HasApplied: true,
			},
			want: VerdictEcho, pair: pairView{audio: "0:por"},
		},
		{
			name: "substitute region with no record is an echo",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(ptPT)), Intent: intentPtBR, HasIntent: true},
			want: VerdictEcho, pair: pairView{audio: "0:por"},
		},
		{
			name: "no subtitle left where the chosen subtitle is missing is an echo",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(jpn), fre), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9002, 0), HasApplied: true,
			},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "a part recorded with what it holds after a failed write is an echo",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(jpn), dub, eng, on(fre)), Intent: intentJ, HasIntent: true,
				Applied: streams.Applied{At: 200, PartID: 5001, AudioID: 9002, SubtitleID: 9012}, HasApplied: true,
			},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "a subtitle not read back after an audio write is inherited whatever plays",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(jpn), dub, eng, on(fre)), Intent: intentJ, HasIntent: true,
				Applied: subtitleUnknown, HasApplied: true,
			},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "a subtitle not read back still judges the audio against the record",
			in: ClassifyInput{
				Played: playedEp("1007", 7, jpn, on(dub), eng, on(fre)), Intent: intentJ, HasIntent: true,
				Applied: subtitleUnknown, HasApplied: true,
			},
			want: VerdictChoice, pair: pairView{audio: "9001:eng", subtitle: "0:eng"},
		},
		{
			name: "no subtitle where the chosen subtitle is missing, no record, is an echo",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(jpn), fre), Intent: intentJ, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "another subtitle where the chosen subtitle is missing, no record, is an echo",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(jpn), on(fre)), Intent: intentJ, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "subtitle-only pick on a dub-only part keeps the intent's audio",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(dub), eng, on(fre)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9001, 9011), HasApplied: true,
			},
			want: VerdictChoice, pair: pairView{audio: "0:jpn", subtitle: "9012:fre"},
		},
		{
			name: "switch back on the anchor is a choice",
			in: ClassifyInput{
				Played: playedEp("1001", 1, dub, on(jpn), eng), Intent: intentDub, HasIntent: true,
				Applied: appliedOn(9001, 0), HasApplied: true,
			},
			want: VerdictChoice, pair: pairView{audio: "9002:jpn"},
		},
		{
			name: "re-announcement of the anchor is a repeat",
			in: ClassifyInput{
				Played: playedEp("1001", 1, dub, on(jpn), on(eng)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9002, 9011), HasApplied: true,
			},
			want: VerdictRepeat, pair: inherited,
		},
		{
			name: "anchor ids are inherited where the record disagrees",
			in: ClassifyInput{
				Played: playedEp("1001", 1, dub, on(jpn), on(eng)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9001, 9011), HasApplied: true,
			},
			want: VerdictRepeat, pair: inherited,
		},
		{
			name: "legacy anchor never inherits by the anchor rule",
			in:   ClassifyInput{Played: playedEp("", 1, on(jpn), eng), Intent: intentLegacy, HasIntent: true},
			want: VerdictChoice, pair: pairView{audio: "0:jpn"},
		},
		{
			name: "legacy anchor with every kind inherited is an echo, never a repeat",
			in:   ClassifyInput{Played: playedEp("", 1, on(jpn), on(eng)), Intent: intentLegacy, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "commentary choice keeps any subtitle where its language is missing",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(dub), eng, on(fre)), Intent: intentCommentary, HasIntent: true},
			want: VerdictEcho, pair: pairView{audio: "0:jpn", subtitle: "0:eng"},
		},
		{
			name: "subtitle turned on against a no-subtitle choice is a choice",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(jpn), on(eng)), Intent: intentNoSub, HasIntent: true},
			want: VerdictChoice, pair: pairView{audio: "0:jpn", subtitle: "9011:eng"},
		},
		{
			name: "subtitle off under a no-subtitle choice is an echo",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(jpn), eng), Intent: intentNoSub, HasIntent: true},
			want: VerdictEcho, pair: pairView{audio: "0:jpn"},
		},
		// A failure or cancellation that changed nothing, an episode before
		// the anchor under next and an evicted record reach Classify as a
		// part with no record; a part an earlier PUT changed is the
		// zero-basis row above.
		{
			name: "no record, observed equals the match",
			in:   ClassifyInput{Played: playedEp("1007", 7, dub, on(jpn), on(eng)), Intent: intentJ, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "no record, no candidate for the chosen audio",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(dub), on(eng)), Intent: intentJ, HasIntent: true},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "no record, a different candidate is a choice",
			in:   ClassifyInput{Played: playedEp("1007", 7, on(dub), jpn, on(eng)), Intent: intentJ, HasIntent: true},
			want: VerdictChoice, pair: pairView{audio: "9001:eng", subtitle: "0:eng"},
		},
		{
			name: "a stale record is not read even when its ids equal the play",
			in: ClassifyInput{
				Played: playedEp("1007", 7, on(dub), jpn, on(eng)), Intent: intentJ, HasIntent: true,
				Applied: stale,
			},
			want: VerdictChoice, pair: pairView{audio: "9001:eng", subtitle: "0:eng"},
		},
		{
			name: "a stale record falls back to the match",
			in: ClassifyInput{
				Played: playedEp("1007", 7, dub, on(jpn), on(eng)), Intent: intentJ, HasIntent: true,
				Applied: stale,
			},
			want: VerdictEcho, pair: inherited,
		},
		{
			name: "a current record decides without the match",
			in: ClassifyInput{
				Played: playedEp("1007", 7, dub, on(jpn), on(eng)), Intent: intentJ, HasIntent: true,
				Applied: appliedOn(9001, 9011), HasApplied: true,
			},
			want: VerdictChoice, pair: pairView{audio: "9002:jpn", subtitle: "0:eng"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.Floor = langtag.TierSameLanguage
			got, pair := Classify(&tc.in)
			if got != tc.want {
				t.Errorf("verdict = %v, want %v", got, tc.want)
			}
			if v := viewOf(pair); v != tc.pair {
				t.Errorf("pair = %+v, want %+v", v, tc.pair)
			}
		})
	}
}

func TestVerdictString(t *testing.T) {
	t.Parallel()
	for v, want := range map[Verdict]string{
		VerdictRepeat: "repeat", VerdictEcho: "echo", VerdictChoice: "choice", VerdictNoAudio: "no_audio", 0: "unknown",
	} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}
