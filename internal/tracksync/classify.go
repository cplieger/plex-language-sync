package tracksync

import (
	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// playVerdict is what one play-time selection means for the user's recorded choice.
type playVerdict uint8

// The verdicts classify returns.
const (
	verdictRepeat  playVerdict = iota + 1 // the anchor's own selection again
	verdictEcho                           // every kind is what the app left or would leave there
	verdictChoice                         // at least one kind is the user's pick
	verdictNoAudio                        // no audio selected on the play: nothing to classify
)

func (v playVerdict) String() string {
	switch v {
	case verdictRepeat:
		return "repeat"
	case verdictEcho:
		return "echo"
	case verdictChoice:
		return "choice"
	case verdictNoAudio:
		return "no_audio"
	default:
		return "unknown"
	}
}

// classifyInput is one play-time read and the user's records for its show
// and episode.
type classifyInput struct {
	Played     *streams.Episode // the user's play-time read: selection and inventory
	Intent     streams.Intent
	Applied    streams.Applied
	Floor      langtag.Tier
	HasIntent  bool
	HasApplied bool // false with no record, and for a stale one (PartID differs from Played's part)
}

// classify decides, per kind, whether the played selection is inherited or
// the user's pick. A kind is inherited when it equals the app's record for
// the part, the intent's anchor on the anchor episode, or, with no current
// record, what the app would leave there (no candidate counts as inherited).
// With no intent every kind is picked. The pair takes picked kinds from the
// play and inherited kinds from the intent; verdictNoAudio returns no pair.
func classify(in *classifyInput) (playVerdict, streams.Pair) {
	played := streams.Selected(in.Played)
	if played.Audio == nil {
		return verdictNoAudio, streams.Pair{}
	}
	if !in.HasIntent {
		return verdictChoice, played
	}
	ref := in.Intent.RefStreams()
	audioKept, subKept := inherited(in, played, ref)
	merged := ref
	if !audioKept {
		merged.Audio = played.Audio
	}
	if !subKept {
		merged.Subtitle = played.Subtitle
	}
	switch {
	case !audioKept || !subKept:
		return verdictChoice, merged
	case atAnchor(in):
		return verdictRepeat, merged
	default:
		return verdictEcho, merged
	}
}

// inherited reports, per kind, whether the record, the anchor or the match
// accounts for the played track. The match is consulted only without a
// current record, since a record says what the app actually left there.
func inherited(in *classifyInput, played, ref streams.Pair) (audio, subtitle bool) {
	audioID, subID := streams.ID(played.Audio), streams.ID(played.Subtitle)
	if in.HasApplied {
		audio = audioID == in.Applied.AudioID
		// A subtitle the app could not read back may be Plex's pick for
		// the app's audio write, so it is no evidence of the user's.
		subtitle = in.Applied.SubtitleUnknown || subID == in.Applied.SubtitleID
	}
	if atAnchor(in) {
		audio = audio || audioID == in.Intent.Anchor.AudioID
		subtitle = subtitle || subID == in.Intent.Anchor.SubtitleID
	}
	if !in.HasApplied {
		want := pickForIntent(ref, in.Floor)(in.Played)
		audio = audio || want.Audio == nil || audioID == streams.ID(want.Audio)
		subtitle = subtitle || leavesSubtitle(want, subID)
	}
	return audio, subtitle
}

// atAnchor reports a play of the episode the intent was observed on. A zero
// anchor matches nothing, though an empty rating key would equal it.
func atAnchor(in *classifyInput) bool {
	return !in.Intent.Anchor.IsZero() && in.Played.RatingKey == in.Intent.Anchor.RatingKey
}

// leavesSubtitle reports whether applying want leaves subtitle id subID
// selected (0 for none).
func leavesSubtitle(want selection, subID int) bool {
	switch {
	case want.KeepSub:
		return true
	case want.SubtitleOff:
		return subID == 0
	default:
		return subID == streams.ID(want.Subtitle)
	}
}
