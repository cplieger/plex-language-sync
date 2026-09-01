// Package streams holds the pure (I/O-free) stream-selection core for
// plex-language-sync along with the Plex value types it operates on.
//
// The JSON struct tags on Label, Episode, Media and Part are part of
// Plex's API contract and must not change during refactors. Stream
// embeds plexapi.Stream, so the library owns that half of the contract.
package streams

import (
	"fmt"
	"strings"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plexapi/v2"
	"github.com/cplieger/runesafe/v2"
)

// Label represents a label tag on a Plex metadata item.
type Label struct {
	Tag string `json:"tag"`
}

// Episode is a Plex metadata item of type="episode" (and, by extension,
// show or season metadata since /library/metadata/{key} is polymorphic).
// Only consumed fields are declared; the decoder is non-strict.
type Episode struct {
	RatingKey       string `json:"ratingKey"`
	ParentRatingKey string `json:"parentRatingKey"`
	// The two title fields come from the wild (agents, filenames), tagged
	// runesafe.Untrusted: raw bytes in, sanitized automatically at every
	// slog/fmt emit.
	GrandparentTitle     runesafe.Untrusted `json:"grandparentTitle"`
	LibraryTitle         runesafe.Untrusted `json:"librarySectionTitle"`
	Type                 string             `json:"type"`
	GrandparentRatingKey string             `json:"grandparentRatingKey"`
	Media                []Media            `json:"Media"`
	Index                FlexInt            `json:"index"`
	ParentIndex          FlexInt            `json:"parentIndex"`
}

// SeasonNum returns the parsed season index, or 0 when ParentIndex is
// absent.
func (e *Episode) SeasonNum() int {
	return int(e.ParentIndex)
}

// Num returns the parsed episode index, or 0 when Index is absent.
func (e *Episode) Num() int {
	return int(e.Index)
}

// ShortName returns a concise "'Show' (SxxEyy)" identifier for
// structured log lines.
func (e *Episode) ShortName() string {
	return fmt.Sprintf("'%s' (S%02dE%02d)", e.GrandparentTitle, e.SeasonNum(), e.Num())
}

// Media wraps a list of Parts for an Episode. Plex also sends a numeric
// media `id`, but selection is keyed on the PART id (see FirstPartID),
// so it is not decoded.
type Media struct {
	Part []Part `json:"Part"`
}

// Part wraps a list of Streams for a Media.
type Part struct {
	Stream []Stream `json:"Stream"`
	// ID stays a bare int where the embedded plexapi.Stream's is a
	// number-or-quoted-string FlexInt: /status/sessions is the only
	// endpoint that quotes these ids, and no session decodes into this
	// graph (plex.Session declares nothing below Player).
	ID int `json:"id"`
}

// StreamType identifies the kind of stream (video, audio, subtitle),
// aliased onto plexapi.StreamType, which types the promoted
// Stream.StreamType field.
type StreamType = plexapi.StreamType

// StreamTypeAudio and StreamTypeSubtitle enumerate the stream-type
// values the app acts on. Video is whatever answers no to both
// IsAudio and IsSubtitle, so it needs no constant of its own here.
const (
	StreamTypeAudio    = plexapi.StreamTypeAudio
	StreamTypeSubtitle = plexapi.StreamTypeSubtitle
)

// Stream is a single audio / subtitle / video stream on a Part.
//
// plexapi.Stream is EMBEDDED, not held as a named field, to promote its
// surface: the 14 wire fields and the IsAudio / IsSubtitle predicates.
// The outer type carries only what Go forbids declaring on a foreign
// type: Lang, languageRaw, HasNoLanguage, and TitleForMatch
// (describe.go).
type Stream struct {
	plexapi.Stream
}

// Lang returns the stream's canonical language, preferring Plex's BCP
// 47 languageTag over the coarser languageCode: LanguageCode cannot
// express a region, so a movie with both European and Latin American
// Spanish subtitles reports "spa" for both and the tag is what
// distinguishes them. LanguageCode still decides when the tag is
// absent or unparseable.
//
// Not memoized: Stream values are copied through slices throughout
// this package, and parsing costs under a microsecond.
func (s *Stream) Lang() langtag.Tag {
	if t, ok := langtag.Parse(s.LanguageTag); ok {
		return t
	}
	// Absent or unparseable tag: LanguageTag and LanguageCode derive
	// from different container metadata, so a malformed tag is treated
	// as "no tag" rather than "no language".
	t, _ := langtag.Parse(s.LanguageCode)
	return t
}

// languageRaw returns the raw identifier the matcher keys on
// (preferring the tag, falling back to the code), for comparing an
// unparseable identifier against another copy of itself.
func (s *Stream) languageRaw() string {
	if t := strings.TrimSpace(s.LanguageTag); t != "" {
		if _, ok := langtag.Parse(t); ok {
			return t
		}
	}
	return s.LanguageCode
}

// HasNoLanguage reports whether Plex supplied no language at all,
// distinct from supplying one this build cannot parse (see
// selectByLanguage).
//
// Two untagged tracks are treated as a match on the audio path
// (differs from langtag's rule that an unknown tag matches nothing),
// because propagating across an untagged library is behavior users
// rely on.
func (s *Stream) HasNoLanguage() bool {
	return strings.TrimSpace(s.LanguageTag) == "" && strings.TrimSpace(s.LanguageCode) == ""
}

// LanguageChoice is the pair a user's profile records: what they chose
// to listen to and read. Named fields prevent a transposition of the
// two bare language codes from learning the profile backwards.
type LanguageChoice struct {
	// Audio is the language the user selected for the audio track.
	Audio string
	// Subtitle is the language selected for subtitles, empty if none.
	Subtitle string
}

// Pair is a selected audio stream and its accompanying subtitle
// stream. Named fields prevent a transposition matching the audio
// track as the subtitle reference.
//
// Subtitle is nil when the user selected no subtitles; Audio nil
// means nothing was selected, which callers treat as "no reference".
type Pair struct {
	// Audio is the selected audio stream.
	Audio *Stream
	// Subtitle is the selected subtitle stream, nil when none is selected.
	Subtitle *Stream
}
