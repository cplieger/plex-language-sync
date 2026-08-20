package streams

import (
	"strings"

	"github.com/cplieger/langtag/v2"
)

// AudioFloor is the language distance the audio path accepts. It is fixed
// rather than configurable.
//
// It sits one tier looser than the subtitle default, and the two mean the same
// thing. Script cannot matter for a spoken track, but langtag infers a script
// from the region, so two Mandarin tracks tagged zh-CN and zh-TW differ by
// "script" purely as an artifact of that inference. Both report
// languageCode="chi" to this app and matched before this change, so flooring
// audio at TierSameLanguage would stop propagating regional audio that
// propagates now. Because base-language equality is required either way, tier 2
// on the audio path admits nothing a listener would notice: it cannot reach
// another language, only another region of the same one.
//
// The tiers above are subtitle-grade claims. Danish and Norwegian are close on
// the page and far apart aloud, which is exactly the substitution a viewer
// notices and resents coming out of the speakers.
const AudioFloor = langtag.TierOtherScript

// languageMatch is how a reference's language relates to a candidate's when the
// reference has no language langtag can read. The three cases are genuinely
// different and folding them together loses behavior.
type languageMatch uint8

const (
	// langGraded means the reference has a readable language, so the tier scale
	// decides.
	langGraded languageMatch = iota
	// langAbsent means Plex supplied no language for the reference. Candidates
	// with no language either are matches, which is what this app has always
	// done for untagged media.
	langAbsent
	// langUnreadable means the reference carries a language string that names
	// nothing langtag knows: a private code, a misspelling, a convention local
	// to one library. Only a candidate carrying the same string matches, which
	// is exactly what the previous exact-string comparison did. An unrecognised
	// code is not an absent one, so an untagged track must not stand in for it.
	langUnreadable
)

// classifyReference reports which of the three matching modes applies to a
// language string, and the tag when one is readable. Shared by every call site
// so the rule has one implementation rather than one per caller.
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
// language distance, so the caller's own scoring decides between them.
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
			// Compare the coarse code only. The previous implementation
			// compared exactly this field, so requiring the finer tag to agree
			// as well would refuse pairs that used to match.
			if s.LanguageCode == wantRaw {
				out = append(out, s)
			}
		case langGraded:
			// Handled above; listed so the switch is exhaustive.
		}
	}
	return out
}

// languageDistance reports the tier at which a candidate relates to a
// reference, agreeing with what selectByLanguage would accept. The two cases
// that carry no readable tag report TierIdentical when they match at all,
// because the strings are the same and nothing finer can be said; reporting
// TierNone for a pair the matcher accepted would make the logged tier
// contradict the decision it describes.
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
