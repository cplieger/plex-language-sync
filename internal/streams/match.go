package streams

import (
	"strings"

	"github.com/cplieger/langtag"
)

// AudioFloor is the language distance the audio path accepts.
//
// It is fixed rather than configurable, and it sits one tier looser than a
// first reading suggests it should. Script cannot matter for audio: a spoken
// track has no script, and langtag infers one from the region, so two Mandarin
// tracks tagged zh-CN and zh-TW differ by "script" (Hans against Hant) purely
// as an artifact of that inference. Both report languageCode="chi" to this app
// today and match. Flooring audio at TierSameLanguage would therefore stop
// propagating regional audio that propagates now, so audio accepts a script
// difference and stops short of any cross-language substitution.
//
// The tiers above are subtitle-grade claims. Danish and Norwegian are close on
// the page and far apart aloud, which is exactly the substitution a viewer
// notices and resents coming out of the speakers.
const AudioFloor = langtag.TierOtherScript

// MatchAudio finds the best matching audio stream from candidates against a
// reference stream, accepting a language within AudioFloor.
func MatchAudio(ref *Stream, candidates []*Stream) *Stream {
	if ref == nil {
		return nil
	}
	streams := candidatesInLanguage(ref, candidates, AudioFloor)
	if len(streams) == 0 {
		return nil
	}
	if len(streams) == 1 {
		return streams[0]
	}

	streams = FilterByBoolPref(streams, ref.VisualImpaired,
		func(s *Stream) bool { return s.VisualImpaired })

	refTitle := strings.ToLower(ref.TitleForMatch())
	streams = FilterByBoolPref(streams, ContainsDescriptive(refTitle),
		func(s *Stream) bool { return ContainsDescriptive(strings.ToLower(s.TitleForMatch())) })

	if len(streams) == 1 {
		return streams[0]
	}
	return BestByScore(streams, func(s *Stream) int {
		return ScoreAudio(ref, s)
	})
}

// MatchSubtitle finds the best matching subtitle stream against the reference
// subtitle, accepting a language within floor. The refAudio parameter is
// accepted for call-site symmetry with the audio path but is NOT consulted:
// SubtitleCriteria discards it, because the "no subtitle means no subtitle"
// policy means the audio language is never used to derive subtitle criteria.
// Returns nil when no match applies, either because the reference had no
// subtitle or because no candidate meets the derived criteria.
//
// The order of the three narrowing steps is load-bearing, and the forced flag
// and the hearing-impaired flag sit on opposite sides of the language grading
// for opposite reasons.
//
// Forced is a hard requirement, so it runs FIRST. Grading first would collapse
// the candidates to the closest language tier reached, and a hard filter applied
// afterwards could then empty that set and return no subtitle even though a
// forced track existed one tier further out.
//
// Hearing-impaired is a preference, so it runs LAST. FilterByBoolPref falls back
// to the whole set only when NOTHING in it matches, so applying it before the
// grading lets a single hearing-impaired track in an unrelated language capture
// the candidate set; the grading then finds nothing acceptable in it and returns
// nil. That is a regression against plain string equality, which is what this
// ordering exists to avoid: the preference belongs inside the language the
// grading chose, not ahead of choosing it.
func MatchSubtitle(ref, refAudio *Stream, candidates []*Stream, floor langtag.Tier) *Stream {
	criteria, ok := SubtitleCriteria(ref, refAudio)
	if !ok {
		return nil
	}

	streams := candidates
	if criteria.ForcedOnly {
		// Forced is an exact requirement, not a preference: a non-forced track
		// is not an acceptable stand-in for a forced one.
		var forced []*Stream
		for _, s := range streams {
			if s.Forced {
				forced = append(forced, s)
			}
		}
		streams = forced
	}

	streams = candidatesInLanguage(ref, streams, floor)
	if len(streams) == 0 {
		return nil
	}

	if criteria.HearingImpairedOnly {
		// Prefer a hearing-impaired track, but fall back to a non-HI track in
		// the same language rather than returning no subtitle.
		streams = FilterByBoolPref(streams, true,
			func(s *Stream) bool { return s.HearingImpaired })
	}

	if len(streams) == 1 {
		return streams[0]
	}
	return BestByScore(streams, func(s *Stream) int {
		return ScoreSubtitle(ref, s)
	})
}

// candidatesInLanguage narrows candidates to those whose language is an
// acceptable stand-in for the reference's, within floor. Every returned
// candidate sits at the same language distance, so the caller's own scoring
// decides between them.
//
// Three cases, and keeping them apart matters. A reference with a language we
// can read is graded on the tier scale. A reference Plex gave no language for
// matches candidates Plex also gave no language for, preserving what this app
// has always done for untagged media. A reference carrying a language string
// that names nothing we know falls back to exact equality on the raw code,
// which is precisely the old behavior for that input: an unrecognised code is
// not the same thing as an absent one, and folding the two together would let
// an untagged track stand in for a track labelled with a private or misspelled
// code.
func candidatesInLanguage(ref *Stream, candidates []*Stream, floor langtag.Tier) []*Stream {
	if want := ref.Lang(); !want.IsZero() {
		out, _, ok := langtag.Best(want, candidates, (*Stream).Lang, floor)
		if !ok {
			return nil
		}
		return out
	}

	var out []*Stream
	if ref.HasNoLanguage() {
		for _, s := range candidates {
			if SameUnknownLanguage(ref, s) {
				out = append(out, s)
			}
		}
		return out
	}
	for _, s := range candidates {
		if s.LanguageCode == ref.LanguageCode && s.LanguageTag == ref.LanguageTag {
			out = append(out, s)
		}
	}
	return out
}

// MatchTier reports the language distance at which a candidate would be
// accepted for the reference, and whether it is within floor. Used for log
// output so that a surprising substitution can be diagnosed after the fact.
func MatchTier(ref, candidate *Stream) langtag.Tier {
	if ref == nil || candidate == nil {
		return langtag.TierNone
	}
	if SameUnknownLanguage(ref, candidate) {
		return langtag.TierIdentical
	}
	return langtag.Compare(ref.Lang(), candidate.Lang())
}

// Criteria is the language and flag requirements derived from a reference
// subtitle for matching against a target episode's tracks.
type Criteria struct {
	// Lang is the reference's language. It is the zero Tag when Plex reported
	// no usable language for the reference track.
	Lang langtag.Tag
	// ForcedOnly requires a forced track; it is an exact requirement.
	ForcedOnly bool
	// HearingImpairedOnly prefers a hearing-impaired track.
	HearingImpairedOnly bool
}

// SubtitleCriteria extracts the language and flags used to match a subtitle
// stream on the target episode. ok is false when no subtitle should be matched
// at all.
//
// Policy: "no subtitle means no subtitle". When the reference episode has no
// subtitle selected (ref == nil), we never search for forced subs based on the
// audio language. The user explicitly chose "no subtitles" and we respect that.
// The caller's disable-subtitles guard then fires unconditionally when the
// target has subtitles selected.
func SubtitleCriteria(ref, _ *Stream) (Criteria, bool) {
	if ref == nil {
		return Criteria{}, false
	}
	return Criteria{
		Lang:                ref.Lang(),
		ForcedOnly:          ref.Forced,
		HearingImpairedOnly: ref.HearingImpaired,
	}, true
}

// ShouldSkipSubtitleForCommentary returns true if the reference audio is a
// commentary/descriptive track but the target episode has no audio track in the
// reference's language at all (MatchAudio matches by language; commentary is
// only a soft preference, so a same-language non-commentary track still counts
// as a match) — in which case subtitle changes should be skipped to avoid
// generalizing an atypical pairing.
func ShouldSkipSubtitleForCommentary(refAudio *Stream, targetAudioStreams []*Stream) bool {
	if refAudio == nil {
		return false
	}
	if !ContainsDescriptive(strings.ToLower(refAudio.TitleForMatch())) {
		return false
	}
	matched := MatchAudio(refAudio, targetAudioStreams)
	return matched == nil
}
