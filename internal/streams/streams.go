// Package streams holds the pure (I/O-free) stream-selection core for
// plex-language-sync along with the Plex value types it operates on.
//
// The types here mirror the JSON wire format returned by the Plex
// HTTP API; JSON struct tags are part of Plex's API contract
// (inviolate) and must not change during refactors.
//
// Callers (the internal/plex HTTP client, composition root, and tests)
// import this package; it has no dependencies on other internal
// packages so there are no circular-import risks.
package streams

import (
	"fmt"
	"strings"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/runesafe/v2"
)

// Label represents a label tag on a Plex metadata item.
type Label struct {
	Tag string `json:"tag"`
}

// Episode is a Plex metadata item of type="episode" (and, by extension,
// show or season metadata since /library/metadata/{key} is polymorphic).
// Only the fields the app consumes are declared: the plexapi decoder is
// non-strict, so Plex's other metadata fields are ignored on the wire
// rather than decoded into unread struct members.
type Episode struct {
	RatingKey       string `json:"ratingKey"`
	ParentRatingKey string `json:"parentRatingKey"`
	// The two title fields are Plex metadata sourced from the wild (agents,
	// filenames), tagged runesafe.Untrusted at this decode boundary: raw
	// bytes in (matching, e.g. the ignore policy, reads Raw()), sanitized
	// automatically at every slog/fmt emit.
	GrandparentTitle     runesafe.Untrusted `json:"grandparentTitle"`
	LibraryTitle         runesafe.Untrusted `json:"librarySectionTitle"`
	Type                 string             `json:"type"`
	GrandparentRatingKey string             `json:"grandparentRatingKey"`
	Media                []Media            `json:"Media"`
	Index                FlexInt            `json:"index"`
	ParentIndex          FlexInt            `json:"parentIndex"`
}

// SeasonNum returns the parsed season index, or 0 when the ParentIndex
// field is absent. FlexInt decodes both `14` and `"14"` JSON shapes
// directly to int, so this is now a trivial conversion — no strconv
// fallback needed.
func (e *Episode) SeasonNum() int {
	return int(e.ParentIndex)
}

// Num returns the parsed episode index, or 0 when the Index
// field is absent. See SeasonNum for the FlexInt rationale.
func (e *Episode) Num() int {
	return int(e.Index)
}

// ShortName returns a concise "'Show' (SxxEyy)" identifier useful for
// structured log lines.
func (e *Episode) ShortName() string {
	// ShortName is log/display vocabulary (every call site is a slog attr).
	// GrandparentTitle carries the runesafe.Untrusted tag, so %s renders its
	// sanitized form here — control/bidi runes are neutralized without a
	// per-site call.
	return fmt.Sprintf("'%s' (S%02dE%02d)", e.GrandparentTitle, e.SeasonNum(), e.Num())
}

// Media wraps a list of Parts for an Episode. Plex also sends a numeric
// media `id`, but the per-user stream-selection write is keyed on the
// PART id (see FirstPartID), so it is not decoded.
type Media struct {
	Part []Part `json:"Part"`
}

// Part wraps a list of Streams for a Media.
type Part struct {
	Stream []Stream `json:"Stream"`
	ID     int      `json:"id"`
}

// StreamType identifies the kind of stream (video, audio, subtitle).
// The underlying int values match the Plex API wire format and
// unmarshal directly from JSON integers without a custom decoder.
type StreamType int

// StreamTypeAudio and StreamTypeSubtitle enumerate the stream-type
// integer values the app acts on. Plex also uses 1 for video, but the
// app only ever asks "is this audio?" / "is this a subtitle?" (see
// IsAudio, IsSubtitle, Audio, Subtitle) — a video stream is simply
// whatever answers no to both, so nothing here needs to name it.
const (
	StreamTypeAudio    StreamType = 2
	StreamTypeSubtitle StreamType = 3
)

// Stream is a single audio / subtitle / video stream on a Part.
type Stream struct {
	// LanguageCode is Plex's ISO 639-2/B code ("nob", "spa"). Kept for log
	// output and for the persisted intent projection.
	LanguageCode string `json:"languageCode"`
	// LanguageTag is Plex's BCP 47 tag ("nb", "es-419"). Strictly more
	// informative than LanguageCode, which cannot express a region: a movie
	// carrying both a European and a Latin American Spanish subtitle reports
	// languageCode="spa" for both and distinguishes them only here.
	LanguageTag          string     `json:"languageTag"`
	DisplayTitle         string     `json:"displayTitle"`
	ExtendedDisplayTitle string     `json:"extendedDisplayTitle"`
	Title                string     `json:"title"`
	Codec                string     `json:"codec"`
	AudioChannelLayout   string     `json:"audioChannelLayout"`
	ID                   int        `json:"id"`
	StreamType           StreamType `json:"streamType"`
	Channels             int        `json:"channels"`
	Selected             bool       `json:"selected"`
	Forced               bool       `json:"forced"`
	HearingImpaired      bool       `json:"hearingImpaired"`
	VisualImpaired       bool       `json:"visualImpaired"`
}

// Lang returns the stream's canonical language, preferring Plex's BCP 47
// languageTag over the coarser languageCode. The zero Tag means Plex reported
// no usable language for the track, which is a real case: a track with no
// language metadata at all, and one Plex labels "unknown".
//
// Not memoized. Stream values are copied into and out of slices all over this
// package, so a cached field would either be silently stale after a copy or
// need a mutex on a type that has no other reason to hold one. Parsing costs
// under a microsecond and happens a handful of times per episode.
func (s *Stream) Lang() langtag.Tag {
	if t, ok := langtag.Parse(s.LanguageTag); ok {
		return t
	}
	// Fall back on an absent OR unparseable tag. Plex has been observed to
	// report a languageTag for every stream, but the two fields come from
	// different derivations of the same container metadata, so treating a
	// malformed tag as "no tag" rather than "no language" keeps the coarser
	// field useful.
	t, _ := langtag.Parse(s.LanguageCode)
	return t
}

// languageRaw returns the raw identifier the matcher keys on, preferring the
// BCP 47 tag and falling back to the coarser code. It is the un-parsed
// counterpart of Lang, needed because an identifier langtag cannot read still
// has to be comparable to another copy of itself.
func (s *Stream) languageRaw() string {
	if t := strings.TrimSpace(s.LanguageTag); t != "" {
		if _, ok := langtag.Parse(t); ok {
			return t
		}
	}
	return s.LanguageCode
}

// HasNoLanguage reports whether Plex supplied no language at all for the track,
// as opposed to supplying one this build cannot parse. The two cases are
// different and the app treats them differently: see selectByLanguage.
//
// Two tracks that both answer true here are treated as a match on the audio
// path, because the old comparison made an empty code equal an empty code and
// propagating across an untagged library is behavior a user relies on. That
// differs from langtag's rule that an unknown tag matches nothing, which is
// right for a library where two "undetermined" tracks prove nothing, and wrong
// here where the alternative is doing nothing for a whole class of library.
func (s *Stream) HasNoLanguage() bool {
	return strings.TrimSpace(s.LanguageTag) == "" && strings.TrimSpace(s.LanguageCode) == ""
}

// IsAudio reports whether the stream is an audio track.
func (s *Stream) IsAudio() bool { return s.StreamType == StreamTypeAudio }

// IsSubtitle reports whether the stream is a subtitle track.
func (s *Stream) IsSubtitle() bool { return s.StreamType == StreamTypeSubtitle }

// LanguageChoice is the pair a user's profile records: what they chose to
// listen to and what they chose to read. Two adjacent bare language codes
// type-checked in either order, and a swap learns the profile BACKWARDS and
// persists it, so every later episode gets the subtitle language as its audio
// preference. Named fields make the transposition visible at the call.
//
// Subtitle may be empty (the user chose no subtitles); Audio may not, and an
// empty Audio is ignored rather than recorded.
type LanguageChoice struct {
	// Audio is the language the user selected for the audio track.
	Audio string
	// Subtitle is the language the user selected for subtitles, empty when
	// they chose none.
	Subtitle string
}

// Pair is a selected audio stream and its accompanying subtitle stream — the
// two-stream unit this app propagates. The pair travels through six
// signatures (Selected returns it, NewIntent takes it, RefStreams returns it,
// and the sync plane threads it to the write), and as two adjacent
// *Stream parameters every one of those crossings type-checked in either
// order. A transposition writes the subtitle stream into the audio slot: the
// episode ends up with a valid selection nobody asked for, on the wrong track.
//
// Subtitle is nil when the user selected no subtitles; Audio nil means
// nothing was selected at all, which callers treat as "no reference".
type Pair struct {
	// Audio is the selected audio stream.
	Audio *Stream
	// Subtitle is the selected subtitle stream, nil when none is selected.
	Subtitle *Stream
}
