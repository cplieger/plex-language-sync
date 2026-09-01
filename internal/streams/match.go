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
// subtitle, accepting a language within floor. Returns nil when the reference
// had no subtitle or no candidate meets the derived criteria.
//
// Takes no reference-audio parameter: the "no subtitle means no subtitle"
// policy means audio language never derives subtitle criteria.
//
// The order of the three narrowing steps is load-bearing. Forced is a hard
// requirement and runs FIRST — grading first would collapse candidates to
// the closest language tier, and a hard filter afterwards could then empty
// that set even though a forced track existed one tier further out.
// Hearing-impaired is a preference and runs LAST — FilterByBoolPref falls
// back to the whole set only when nothing in it matches, so applying it
// before grading lets one HI track in an unrelated language capture the set.
func MatchSubtitle(ref *Stream, candidates []*Stream, floor langtag.Tier) *Stream {
	criteria, ok := SubtitleCriteria(ref)
	if !ok {
		return nil
	}
	// Untagged subtitle references match nothing (unlike untagged audio): an
	// untagged subtitle is as likely to be signs-and-songs or commentary, and
	// switching one on across a whole show is visible and unwanted.
	if _, mode := classifyReference(ref.languageRaw()); mode == langAbsent {
		return nil
	}

	streams := candidates
	if criteria.ForcedOnly {
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

// MatchTier reports the language distance at which a candidate was accepted
// for the reference, for log output so a surprising substitution can be
// diagnosed after the fact.
func MatchTier(ref, candidate *Stream) langtag.Tier {
	return languageDistance(ref, candidate)
}

// Criteria is the language and flag requirements derived from a reference
// subtitle for matching against a target episode's tracks.
type Criteria struct {
	// Lang is the zero Tag when Plex reported no usable language.
	Lang langtag.Tag
	// ForcedOnly requires a forced track; it is an exact requirement.
	ForcedOnly bool
	// HearingImpairedOnly prefers a hearing-impaired track.
	HearingImpairedOnly bool
}

// SubtitleCriteria extracts the language and flags used to match a subtitle
// stream on the target episode. ok is false when no subtitle should be
// matched at all — the "no subtitle means no subtitle" policy: when the
// reference has no subtitle selected, nothing searches for a forced sub in
// the audio language either. The caller's disable-subtitles guard then fires
// unconditionally when the target has subtitles selected.
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
// commentary/descriptive track but the target episode has no audio track in
// the reference's language at all — in which case subtitle changes should be
// skipped to avoid generalizing an atypical pairing.
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
