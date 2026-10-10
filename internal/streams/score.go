package streams

import (
	"strings"

	"github.com/cplieger/langtag/v2"
)

// scoreRule defines a single scoring criterion: a predicate that contributes
// weight points when it returns true for a (ref, candidate) pair.
type scoreRule struct {
	predicate func(ref, s *Stream) bool
	weight    int
}

// audioScoreRules is the declarative rule table for scoreAudio.
var audioScoreRules = []scoreRule{
	{
		weight: 5,
		predicate: func(ref, s *Stream) bool {
			return ref.Codec != "" && s.Codec != "" && ref.Codec == s.Codec
		},
	},
	{
		weight: 3,
		predicate: func(ref, s *Stream) bool {
			return ref.AudioChannelLayout != "" && s.AudioChannelLayout != "" &&
				ref.AudioChannelLayout == s.AudioChannelLayout
		},
	},
	{
		weight: 2,
		predicate: func(ref, s *Stream) bool {
			return ref.Channels > 0 && s.Channels > 0 &&
				ref.Channels < 3 && s.Channels > ref.Channels
		},
	},
}

// titleScoreRules is the declarative rule table for titleMatchScore.
var titleScoreRules = []scoreRule{
	{
		weight: 5,
		predicate: func(ref, s *Stream) bool {
			return ref.ExtendedDisplayTitle != "" && s.ExtendedDisplayTitle != "" &&
				ref.ExtendedDisplayTitle == s.ExtendedDisplayTitle
		},
	},
	{
		weight: 5,
		predicate: func(ref, s *Stream) bool {
			return ref.DisplayTitle != "" && s.DisplayTitle != "" &&
				ref.DisplayTitle == s.DisplayTitle
		},
	},
	{
		weight: 5,
		predicate: func(ref, s *Stream) bool {
			return ref.Title != "" && s.Title != "" && ref.Title == s.Title
		},
	},
}

// subtitleScoreRules is the declarative rule table for scoreSubtitle.
var subtitleScoreRules = []scoreRule{
	{
		weight: 3,
		predicate: func(ref, s *Stream) bool {
			return ref.Forced == s.Forced
		},
	},
	{
		weight: 3,
		predicate: func(ref, s *Stream) bool {
			return ref.HearingImpaired == s.HearingImpaired
		},
	},
	{
		weight: 1,
		predicate: func(ref, s *Stream) bool {
			return ref.Codec != "" && s.Codec != "" && ref.Codec == s.Codec
		},
	},
}

// sumRules evaluates rules and returns the total score.
func sumRules(rules []scoreRule, ref, s *Stream) int {
	score := 0
	for _, r := range rules {
		if r.predicate(ref, s) {
			score += r.weight
		}
	}
	return score
}

// scoreAudio ranks a candidate audio stream against a reference for
// the tie-break stage of MatchAudio. Higher is better. Codec match,
// channel layout match, and the same title fields each contribute.
func scoreAudio(ref, s *Stream) int {
	if ref == nil {
		return 0
	}
	return sumRules(audioScoreRules, ref, s) + titleMatchScore(ref, s)
}

// scoreSubtitle ranks a candidate subtitle stream against a reference.
// Higher is better. Returns 0 when ref is nil.
func scoreSubtitle(ref, s *Stream) int {
	if ref == nil {
		return 0
	}
	return sumRules(subtitleScoreRules, ref, s) + titleMatchScore(ref, s)
}

// titleMatchScore rewards exact equality on any of the three title
// fields. Each match adds 5. Empty fields never contribute.
func titleMatchScore(ref, s *Stream) int {
	return sumRules(titleScoreRules, ref, s)
}

// Subtitle codec identifiers (ffmpeg names) used as keys into
// subtitleCodecScores below.
const (
	codecASS             = "ass"
	codecSSA             = "ssa"
	codecPGS             = "pgs"
	codecVobsub          = "vobsub"
	codecDvdsub          = "dvdsub"
	codecDvbSubtitle     = "dvb_subtitle"
	codecHdmvPgsSubtitle = "hdmv_pgs_subtitle"
	codecSRT             = "srt"
	codecSubrip          = "subrip"
	codecMovText         = "mov_text"
	codecWebVTT          = "webvtt"
)

// subtitleCodecScores maps ffmpeg codec identifiers to quality tiers.
// Higher is better: styled text (3) > image-based (2) > plain text (1).
// Zero-value (not present) means unknown codec.
var subtitleCodecScores = map[string]int{
	codecASS:             3,
	codecSSA:             3,
	codecPGS:             2,
	codecVobsub:          2,
	codecDvdsub:          2,
	codecDvbSubtitle:     2,
	codecHdmvPgsSubtitle: 2,
	codecSRT:             1,
	codecSubrip:          1,
	codecMovText:         1,
	codecWebVTT:          1,
}

// subtitleCodecScore ranks subtitle codecs by quality/reliability.
// Higher is better: styled text > image-based (source) > plain text
// (Bazarr). Scores are defined in the subtitleCodecScores table above.
func subtitleCodecScore(codec string) int {
	return subtitleCodecScores[strings.ToLower(codec)]
}

// filterByBoolPref returns streams whose fn value matches desired. If
// no streams match, the original list is returned unchanged — callers
// treat the predicate as a preference, not a requirement.
func filterByBoolPref(streams []*Stream, desired bool, fn func(*Stream) bool) []*Stream {
	var matching []*Stream
	for _, s := range streams {
		if fn(s) == desired {
			matching = append(matching, s)
		}
	}
	if len(matching) > 0 {
		return matching
	}
	return streams
}

// bestByScore returns the stream with the highest scoreFn value. Ties
// go to the earlier entry. Returns nil for an empty list.
func bestByScore(streams []*Stream, scoreFn func(*Stream) int) *Stream {
	if len(streams) == 0 {
		return nil
	}
	// Seed from the first element so the general-purpose helper is correct
	// for any scoreFn, not only non-negative ones. Strict > keeps the
	// documented tie-goes-to-earliest behaviour.
	bestIdx, bestScore := 0, scoreFn(streams[0])
	for i := 1; i < len(streams); i++ {
		if score := scoreFn(streams[i]); score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return streams[bestIdx]
}

// FindSubtitleByLanguage returns the best subtitle stream whose language is an
// acceptable stand-in for langCode within floor, preferring higher-quality
// codecs (see subtitleCodecScore). Returns nil if none match.
//
// This is the learned-profile path: langCode comes from the profile map rather
// than from a track on the reference episode, so it is graded on the same scale
// as a propagated selection. Without that, a user whose profile learned "nob
// audio means nob subtitles" would get no subtitle on a new show carrying only
// nor, which is the reported bug in a second guise.
func FindSubtitleByLanguage(streams []*Stream, langCode string, floor langtag.Tier) *Stream {
	return bestByScore(selectByLanguage(streams, langCode, floor), func(s *Stream) int {
		return subtitleCodecScore(s.Codec)
	})
}
