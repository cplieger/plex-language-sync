package streams

import "strings"

// MatchAudio finds the best matching audio stream from candidates
// against a reference stream. Matching logic inspired by
// Plex-Auto-Languages.
func MatchAudio(ref *Stream, candidates []*Stream) *Stream {
	if ref == nil {
		return nil
	}
	streams := FilterByLanguage(candidates, ref.LanguageCode)
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

// MatchSubtitle finds the best matching subtitle stream against the
// reference subtitle (and reference audio, for disambiguation). Returns
// nil when no match applies — either because the reference had no
// subtitle (respecting "no subtitle means no subtitle") or because no
// candidate meets the derived criteria.
func MatchSubtitle(ref, refAudio *Stream, candidates []*Stream) *Stream {
	langCode, matchForcedOnly, matchHIOnly := SubtitleCriteria(ref, refAudio)
	if langCode == "" {
		return nil
	}

	streams := FilterByLanguage(candidates, langCode)
	if matchForcedOnly {
		// For forced-only, we need exact match — not "prefer".
		var forced []*Stream
		for _, s := range streams {
			if s.Forced {
				forced = append(forced, s)
			}
		}
		streams = forced
	}
	if matchHIOnly {
		streams = FilterByBoolPref(streams, true,
			func(s *Stream) bool { return s.HearingImpaired })
	}

	if len(streams) == 0 {
		return nil
	}
	if len(streams) == 1 {
		return streams[0]
	}

	// Reaching here implies langCode != "", and SubtitleCriteria only
	// returns a non-empty langCode for a non-nil ref, so ref is
	// guaranteed non-nil here (ScoreSubtitle still nil-guards defensively).
	return BestByScore(streams, func(s *Stream) int {
		return ScoreSubtitle(ref, s)
	})
}

// SubtitleCriteria extracts the language/flags used to match a
// subtitle stream on the target episode. Policy: "no subtitle means no
// subtitle" — when the reference episode has no subtitle selected
// (ref == nil), we never search for forced subs based on the audio
// language. The user explicitly chose "no subtitle" and we respect
// that. The caller's disable-subtitles guard then fires unconditionally
// when the target has subtitles selected.
func SubtitleCriteria(ref, _ *Stream) (langCode string, forcedOnly, hiOnly bool) {
	if ref == nil {
		return "", false, false
	}
	return ref.LanguageCode, ref.Forced, ref.HearingImpaired
}

// ShouldSkipSubtitleForCommentary returns true if the reference audio
// is a commentary/descriptive track but the target episode has no
// matching commentary audio track — in which case subtitle changes
// should be skipped to avoid generalizing an atypical pairing.
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
