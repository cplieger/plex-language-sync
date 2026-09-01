package streams

import (
	"strings"

	"github.com/cplieger/langtag/v2"
)

// AudioFloor is the language distance the audio path accepts, fixed rather
// than configurable.
//
// It sits one tier looser than the subtitle default, and the two mean the
// same thing: script cannot matter for a spoken track, but langtag infers a
// script from the region, so two Mandarin tracks tagged zh-CN and zh-TW
// differ by "script" purely as an artifact of that inference. Both report
// languageCode="chi" and matched before this change, so flooring audio at
// TierSameLanguage would stop propagating regional audio that propagates now.
const AudioFloor = langtag.TierOtherScript

// languageMatch is how a reference's language relates to a candidate's when
// the reference has no language langtag can read.
type languageMatch uint8

const (
	// langGraded means the reference has a readable language, so the tier scale
	// decides.
	langGraded languageMatch = iota
	// langAbsent means Plex supplied no language for the reference. Candidates
	// with no language either are matches.
	langAbsent
	// langUnreadable means the reference's language string names nothing
	// langtag knows. Only a candidate carrying the same string matches; an
	// unrecognised code is not an absent one.
	langUnreadable
)

// classifyReference reports which of the three matching modes applies to a
// language string, and the tag when one is readable.
func classifyReference(raw string) (langtag.Tag, languageMatch) {
	if tag, ok := langtag.Parse(raw); ok {
		return tag, langGraded
	}
	if strings.TrimSpace(raw) == "" {
		return langtag.Tag{}, langAbsent
	}
	return langtag.Tag{}, langUnreadable
}

// selectByLanguage narrows candidates to those whose language is an acceptable
// stand-in for wantRaw, within floor. Every returned candidate sits at the same
// language distance.
func selectByLanguage(candidates []*Stream, wantRaw string, floor langtag.Tier) []*Stream {
	want, mode := classifyReference(wantRaw)
	if mode == langGraded {
		out, _, ok := langtag.Prefer(want).Best(candidates, (*Stream).Lang, floor)
		if !ok {
			return nil
		}
		return out
	}

	var out []*Stream
	for _, s := range candidates {
		switch mode {
		case langAbsent:
			if s.HasNoLanguage() {
				out = append(out, s)
			}
		case langUnreadable:
			// Compare the coarse code only: requiring the finer tag too
			// would refuse pairs that otherwise match.
			if s.LanguageCode == wantRaw {
				out = append(out, s)
			}
		case langGraded:
		}
	}
	return out
}

// languageDistance reports the tier at which a candidate relates to a
// reference, agreeing with what selectByLanguage would accept. An untagged
// pair reports TierIdentical when it matches at all, since nothing finer can
// be said; TierNone would contradict the match decision it describes.
func languageDistance(ref, candidate *Stream) langtag.Tier {
	if ref == nil || candidate == nil {
		return langtag.TierNone
	}
	want, mode := classifyReference(ref.languageRaw())
	switch mode {
	case langGraded:
		return langtag.Prefer(want).Compare(candidate.Lang())
	case langAbsent:
		if candidate.HasNoLanguage() {
			return langtag.TierIdentical
		}
	case langUnreadable:
		if candidate.LanguageCode == ref.languageRaw() {
			return langtag.TierIdentical
		}
	}
	return langtag.TierNone
}
