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
)

// Label represents a label tag on a Plex metadata item.
type Label struct {
	Tag string `json:"tag"`
}

// Episode is a Plex metadata item of type="episode" (and, by extension,
// show or season metadata since /library/metadata/{key} is polymorphic).
type Episode struct {
	RatingKey            string  `json:"ratingKey"`
	ParentRatingKey      string  `json:"parentRatingKey"`
	GrandparentKey       string  `json:"grandparentKey"`
	GrandparentTitle     string  `json:"grandparentTitle"`
	ParentTitle          string  `json:"parentTitle"`
	Title                string  `json:"title"`
	Type                 string  `json:"type"`
	LibraryTitle         string  `json:"librarySectionTitle"`
	GrandparentRatingKey string  `json:"grandparentRatingKey"`
	Label                []Label `json:"Label"`
	Media                []Media `json:"Media"`
	AddedAt              int64   `json:"addedAt"`
	Index                FlexInt `json:"index"`
	ParentIndex          FlexInt `json:"parentIndex"`
	LibrarySectionID     FlexInt `json:"librarySectionID"`
}

// SeasonNum returns the parsed season index, or 0 when the ParentIndex
// field is absent. FlexInt decodes both `14` and `"14"` JSON shapes
// directly to int, so this is now a trivial conversion — no strconv
// fallback needed.
func (e *Episode) SeasonNum() int {
	return int(e.ParentIndex)
}

// EpisodeNum returns the parsed episode index, or 0 when the Index
// field is absent. See SeasonNum for the FlexInt rationale.
func (e *Episode) EpisodeNum() int {
	return int(e.Index)
}

// ShortName returns a concise "'Show' (SxxEyy)" identifier useful for
// structured log lines.
func (e *Episode) ShortName() string {
	return fmt.Sprintf("'%s' (S%02dE%02d)", e.GrandparentTitle, e.SeasonNum(), e.EpisodeNum())
}

// Media wraps a list of Parts for an Episode.
type Media struct {
	Part []Part `json:"Part"`
	ID   int    `json:"id"`
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

// StreamTypeVideo, StreamTypeAudio, and StreamTypeSubtitle enumerate the
// stream-type integer values used in the Plex API wire format.
const (
	StreamTypeVideo    StreamType = 1
	StreamTypeAudio    StreamType = 2
	StreamTypeSubtitle StreamType = 3
)

// Stream is a single audio / subtitle / video stream on a Part.
type Stream struct {
	LanguageCode         string     `json:"languageCode"`
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

// IsAudio reports whether the stream is an audio track.
func (s *Stream) IsAudio() bool { return s.StreamType == StreamTypeAudio }

// IsSubtitle reports whether the stream is a subtitle track.
func (s *Stream) IsSubtitle() bool { return s.StreamType == StreamTypeSubtitle }
