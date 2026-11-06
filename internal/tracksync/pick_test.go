package tracksync

import (
	"testing"

	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v3"
)

// picked is a selection reduced to comparable values: stream ids, 0 for none.
type picked struct {
	audio, subtitle int
	off, keep       bool
}

func pickedOf(sel selection) picked {
	return picked{audio: streams.ID(sel.Audio), subtitle: streams.ID(sel.Subtitle), off: sel.SubtitleOff, keep: sel.KeepSub}
}

func pickAudio(id int, lang, title string) streams.Stream {
	return streams.Stream{ID: plexapi.FlexInt(id), StreamType: plexapi.StreamTypeAudio, LanguageCode: lang, Title: title}
}

func pickSub(id int, lang, tag string) streams.Stream {
	return streams.Stream{ID: plexapi.FlexInt(id), StreamType: plexapi.StreamTypeSubtitle, LanguageCode: lang, LanguageTag: tag, Codec: "srt"}
}

func pickEp(list ...streams.Stream) *streams.Episode {
	return &streams.Episode{RatingKey: "1001", Media: []streams.Media{{Part: []streams.Part{{ID: 5001, Stream: list}}}}}
}

func TestPickForIntent(t *testing.T) {
	t.Parallel()
	jpn := pickAudio(9101, "jpn", "")
	commentary := pickAudio(9102, "jpn", "Director's Commentary")
	eng := pickSub(9111, "eng", "")
	ptBR := pickSub(9112, "por", "pt-BR")
	tests := []struct {
		name  string
		ref   streams.Pair
		floor langtag.Tier
		ep    *streams.Episode
		want  picked
	}{
		{
			name: "audio matched by language, subtitle matched",
			ref:  streams.Pair{Audio: &jpn, Subtitle: &eng},
			ep:   pickEp(pickAudio(9001, "eng", ""), pickAudio(9002, "jpn", ""), pickSub(9011, "eng", "")),
			want: picked{audio: 9002, subtitle: 9011},
		},
		{
			name: "no audio candidate leaves audio",
			ref:  streams.Pair{Audio: &jpn, Subtitle: &eng},
			ep:   pickEp(pickAudio(9001, "eng", ""), pickSub(9011, "eng", "")),
			want: picked{subtitle: 9011},
		},
		{
			name: "no reference subtitle turns subtitles off",
			ref:  streams.Pair{Audio: &jpn},
			ep:   pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "eng", "")),
			want: picked{audio: 9002, off: true},
		},
		{
			name: "no subtitle candidate keeps the subtitle",
			ref:  streams.Pair{Audio: &jpn, Subtitle: &eng},
			ep:   pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "fre", "")),
			want: picked{audio: 9002, keep: true},
		},
		{
			name: "commentary reference with no match on the target keeps the subtitle",
			ref:  streams.Pair{Audio: &commentary},
			ep:   pickEp(pickAudio(9001, "eng", ""), pickSub(9011, "eng", "")),
			want: picked{keep: true},
		},
		{
			name: "commentary reference matched on the target applies the subtitle rule",
			ref:  streams.Pair{Audio: &commentary},
			ep:   pickEp(pickAudio(9002, "jpn", "Commentary"), pickSub(9011, "eng", "")),
			want: picked{audio: 9002, off: true},
		},
		{
			name:  "other region subtitle refused below its tier",
			ref:   streams.Pair{Audio: &jpn, Subtitle: &ptBR},
			floor: langtag.TierSameLanguage,
			ep:    pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "por", "pt-PT")),
			want:  picked{audio: 9002, keep: true},
		},
		{
			name:  "other region subtitle accepted at its tier",
			ref:   streams.Pair{Audio: &jpn, Subtitle: &ptBR},
			floor: langtag.TierOtherRegion,
			ep:    pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "por", "pt-PT")),
			want:  picked{audio: 9002, subtitle: 9011},
		},
		{
			name: "episode with no media picks nothing and keeps the subtitle",
			ref:  streams.Pair{Audio: &jpn, Subtitle: &eng},
			ep:   &streams.Episode{RatingKey: "1001"},
			want: picked{keep: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pickedOf(pickForIntent(tc.ref, tc.floor)(tc.ep)); got != tc.want {
				t.Errorf("pickForIntent = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPickForProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		subLang string
		floor   langtag.Tier
		ep      *streams.Episode
		want    picked
	}{
		{
			name:    "learned no subtitle turns subtitles off",
			subLang: "",
			ep:      pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "eng", "")),
			want:    picked{off: true},
		},
		{
			name:    "learned language found sets it and leaves audio",
			subLang: "eng",
			ep:      pickEp(pickAudio(9001, "eng", ""), pickAudio(9002, "jpn", ""), pickSub(9011, "fre", ""), pickSub(9012, "eng", "")),
			want:    picked{subtitle: 9012},
		},
		{
			name:    "no candidate keeps the subtitle",
			subLang: "eng",
			ep:      pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "fre", "")),
			want:    picked{keep: true},
		},
		{
			name:    "other region refused below its tier",
			subLang: "pt-BR",
			floor:   langtag.TierSameLanguage,
			ep:      pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "por", "pt-PT")),
			want:    picked{keep: true},
		},
		{
			name:    "other region accepted at its tier",
			subLang: "pt-BR",
			floor:   langtag.TierOtherRegion,
			ep:      pickEp(pickAudio(9002, "jpn", ""), pickSub(9011, "por", "pt-PT")),
			want:    picked{subtitle: 9011},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pickedOf(pickForProfile(tc.subLang, tc.floor)(tc.ep)); got != tc.want {
				t.Errorf("pickForProfile(%q) = %+v, want %+v", tc.subLang, got, tc.want)
			}
		})
	}
}

// The expected values are FNV-1a 64 of the audio key, a NUL byte and the
// subtitle language, computed outside Go and read as a signed integer.
// state.json persists them, so a change to the hash re-applies every seed.
func TestProfileVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		audio, sub string
		want       int64
	}{
		{audio: "jpn", sub: "eng", want: 6074074944797846099},
		{audio: "jpn", sub: "", want: 1618881192571457441},
		{audio: "ja", sub: "en", want: 6425248896226237167},
		{audio: "ja", sub: "", want: 5009298118688668092},
		{audio: "eng", sub: "", want: -7988849862906077739},
		{audio: "spa", sub: "eng", want: -3585055565694493131},
	}
	for _, tc := range tests {
		t.Run(tc.audio+"_"+tc.sub, func(t *testing.T) {
			t.Parallel()
			if got := profileVersion(tc.audio, tc.sub); got != tc.want {
				t.Errorf("profileVersion(%q, %q) = %d, want %d", tc.audio, tc.sub, got, tc.want)
			}
		})
	}
	t.Run("separator keeps the two fields apart", func(t *testing.T) {
		t.Parallel()
		if profileVersion("ja", "en") == profileVersion("jae", "n") {
			t.Error(`profileVersion("ja", "en") equals profileVersion("jae", "n")`)
		}
	})
}
