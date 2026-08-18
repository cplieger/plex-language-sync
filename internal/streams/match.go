package streams

import (
	"strings"

	"github.com/cplieger/langtag/v2"
)

// MatchAudio finds the best matching audio stream from candidates against a
// reference stream, accepting a language within AudioFloor.
func MatchAudio(ref *Stream, candidates []*Stream) *Stream {
	if ref == nil {
		return nil
	}
	streams := selectByLanguage(candidates, ref.languageRaw(), AudioFloor)
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
// subtitle, accepting a language within floor.
// Returns nil when no match applies, either because the reference had no
// subtitle or because no candidate meets the derived criteria.
//
// It takes no reference-AUDIO parameter, and deliberately so: the "no subtitle
// means no subtitle" policy means the audio language never derives subtitle
// criteria, so such a parameter could not affect the result. It used to be here
// for call-site symmetry with MatchAudio, which bought an adjacent
// *Stream pair that type-checks transposed — a swap would have matched the
// audio track as the subtitle reference, with the callers reading
// (ref.Subtitle, ref.Audio), the reverse of the Pair{Audio, Subtitle} order
// every other site uses.
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
func MatchSubtitle(ref *Stream, candidates []*Stream, floor langtag.Tier) *Stream {
	criteria, ok := SubtitleCriteria(ref)
	if !ok {
		return nil
	}
	// An untagged subtitle reference matches nothing, deliberately, and this is
	// the one place the audio and subtitle paths diverge on untagged media. An
	// untagged audio track is almost always THE audio track, so propagating it
	// serves a user with untagged files. An untagged subtitle is as likely to be
	// a signs-and-songs or commentary track, and switching one on across a whole
	// show is visible and unwanted. The previous implementation refused here too;
	// preserved rather than widened.
	if _, mode := classifyReference(ref.languageRaw()); mode == langAbsent {
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

	streams = selectByLanguage(streams, ref.languageRaw(), floor)
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

// MatchTier reports the language distance at which a candidate was accepted for
// the reference. Used for log output so that a surprising substitution can be
// diagnosed after the fact.
func MatchTier(ref, candidate *Stream) langtag.Tier {
	return languageDistance(ref, candidate)
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
// subtitle selected (ref == nil), no criteria are derived at all — in particular
// nothing searches for forced subs in the audio language, which this function
// could not do in any case since it takes no audio stream. The user explicitly
// chose "no subtitles" and we respect that.
// The caller's disable-subtitles guard then fires unconditionally when the
// target has subtitles selected.
func SubtitleCriteria(ref *Stream) (Criteria, bool) {
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
