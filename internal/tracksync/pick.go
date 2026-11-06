package tracksync

import (
	"hash/fnv"

	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// selection is what a decision wants on one episode. Exactly one of Subtitle,
// SubtitleOff and KeepSub is set.
type selection struct {
	Audio       *streams.Stream // nil leaves the audio as it is
	Subtitle    *streams.Stream
	SubtitleOff bool // turn subtitles off
	KeepSub     bool // leave the subtitle as it is
}

// pickForIntent returns what a recorded choice wants on an episode. The
// writer and the play-time classifier both call it, so "what the app would
// leave there" has one definition.
func pickForIntent(ref streams.Pair, floor langtag.Tier) func(*streams.Episode) selection {
	return func(ep *streams.Episode) selection {
		audio := streams.Audio(ep)
		sel := selection{Audio: streams.MatchAudio(ref.Audio, audio)}
		switch {
		case streams.ShouldSkipSubtitleForCommentary(ref.Audio, audio):
			sel.KeepSub = true
		case ref.Subtitle == nil:
			// A choice of no subtitles means none, never a forced track in
			// the audio's language.
			sel.SubtitleOff = true
		default:
			sel.Subtitle = streams.MatchSubtitle(ref.Subtitle, streams.Subtitle(ep), floor)
			sel.KeepSub = sel.Subtitle == nil
		}
		return sel
	}
}

// pickForProfile returns what a learned language profile wants on an episode:
// the subtitle for subLang, none for an empty subLang, and the audio left alone.
func pickForProfile(subLang string, floor langtag.Tier) func(*streams.Episode) selection {
	return func(ep *streams.Episode) selection {
		if subLang == "" {
			return selection{SubtitleOff: true}
		}
		sub := streams.FindSubtitleByLanguage(streams.Subtitle(ep), subLang, floor)
		return selection{Subtitle: sub, KeepSub: sub == nil}
	}
}

// profileVersion is the basis version of a profile seed: FNV-1a 64 of the
// audio key, a NUL byte and the learned subtitle language, so it changes when
// the learned subtitle changes. state.json persists it; changing the hash
// re-applies every profile seed once.
func profileVersion(audioKey, subLang string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(audioKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(subLang))
	return int64(h.Sum64()) //nolint:gosec // the version is the hash's bit pattern; wrapping is intended
}
