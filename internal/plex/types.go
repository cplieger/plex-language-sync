// Package plex holds the HTTP client and response types for the Plex Media
// Server API. Consumers get a typed *Client with library, history, identity,
// stream-selection, and shared-server methods. Value types (Episode, Stream,
// Media, Part, Label) live in plex-language-sync/internal/streams; this
// package owns the container + admin-facing types (Account, User, Section,
// Session, HistoryItem, Show, Season) and the MediaContainer wrapper.
package plex

import (
	"encoding/xml"
	"fmt"
	"strconv"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Plex wire-protocol constants. These strings/numbers appear in the
// Plex HTTP API and are inviolate item 9. Consumers (main, config,
// scheduler, notify, sync, this package's own library.go) import
// these rather than redeclaring them locally.
const (
	// TypeEpisode is the Plex metadata "type" string for episode items,
	// used when filtering MediaContainer responses by item type.
	TypeEpisode = "episode"

	// MetadataTypeEpisode is the numeric type ID for episode items used
	// in /library/sections/{id}/all?type=... URL parameters.
	MetadataTypeEpisode = 4

	// SectionTypeShow is the Plex library-section "type" string for TV
	// show sections, used when filtering library sections to TV shows
	// only.
	SectionTypeShow = "show"
)

// RatingKey is a typed Plex ratingKey — the opaque numeric string Plex
// uses to address episodes, seasons, shows, and other library items
// (runtime-types-p2). The on-disk and wire representation is always
// a string; this type exists to prevent ratingKey values from being
// conflated with other string keys (userID, sessionKey) at module
// boundaries and to give callers a single place to validate that a
// value is a well-formed numeric key.
//
// Methods on RatingKey intentionally avoid allocation: String returns
// the underlying string, and Validate parses without copying. The
// api.PlexReader interface and the *plex.Client methods (Episode,
// ShowEpisodes, SeasonEpisodes, ShowMetadata, RecentlyAdded) take
// RatingKey at the boundary so validation happens once via
// RatingKey.Validate rather than being repeated per-method with
// strconv.Atoi. Wire-origin strings (streams.Episode.RatingKey,
// plex.Section.Key, plex.HistoryItem.RatingKey) stay typed as string
// in their struct definitions — the plex.RatingKey wrap happens at
// the call site, which preserves the Plex JSON wire format (inviolate
// item 9).
type RatingKey string

// String returns the ratingKey as a plain string for APIs that accept
// strings (HTTP path interpolation, log values, cache keys).
func (r RatingKey) String() string { return string(r) }

// Validate reports whether the ratingKey is a non-empty numeric string.
// Plex ratingKeys are always numeric strings in practice; a malformed
// key indicates a programming error (e.g., a userID leaked into a
// ratingKey slot) that is worth surfacing rather than forwarding to the
// Plex API as a malformed URL path.
//
// The error format is deliberately `invalid rating key %q` — byte-for-
// byte identical to the five pre-extraction strconv.Atoi+fmt.Errorf
// stanzas it replaces in library.go (Episode, ShowEpisodes,
// SeasonEpisodes, ShowMetadata, RecentlyAdded). The inviolate contract
// item 5 (WARN/ERROR log-key stability) includes error messages that
// scrapers may grep for; keeping the exact text means no Loki query or
// dashboard breaks on the collapse.
func (r RatingKey) Validate() error {
	if r == "" {
		return fmt.Errorf("invalid rating key %q", string(r))
	}
	if _, err := strconv.Atoi(string(r)); err != nil {
		return fmt.Errorf("invalid rating key %q", string(r))
	}
	return nil
}

// mc is the MediaContainer envelope Plex wraps every JSON response in.
// Generic over T so callers can embed the specific Metadata/Directory/Account
// shape they expect.
type mc[T any] struct {
	MediaContainer T `json:"MediaContainer"`
}

// ServerIdentity is returned by GET /.
type ServerIdentity struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
}

// Account is a Plex system account returned by GET /accounts.
type Account struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

// User represents the resolved admin (or any) Plex user.
type User struct {
	ID   string
	Name string
}

// Section is a library section returned by GET /library/sections.
type Section struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// Show is the show-level metadata returned by GET /library/metadata/{key}
// when the key points to a TV show. Runtime-types-p1 split this off from
// Episode so callers asking "what are the show's labels?" don't receive an
// Episode-typed value.
type Show struct {
	RatingKey        string          `json:"ratingKey"`
	Title            string          `json:"title"`
	LibraryTitle     string          `json:"librarySectionTitle"`
	Label            []streams.Label `json:"Label"`
	LibrarySectionID streams.FlexInt `json:"librarySectionID"`
}

// Season is the season-level metadata returned by GET /library/metadata/{key}
// when the key points to a season. Runtime-types-p1 split this off from
// Episode for callers that only need the navigational spine (parent key,
// season index) without the whole media/part/stream graph.
type Season struct {
	RatingKey       string          `json:"ratingKey"`
	ParentRatingKey string          `json:"parentRatingKey"`
	Title           string          `json:"title"`
	Index           streams.FlexInt `json:"index"`
}

// Session represents a single active session from GET /status/sessions.
type Session struct {
	User struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"User"`
	Player struct {
		MachineIdentifier string `json:"machineIdentifier"`
	} `json:"Player"`
}

// SharedServersXML is the XML response from plex.tv shared_servers.
type SharedServersXML struct {
	XMLName      xml.Name          `xml:"MediaContainer"`
	SharedServer []SharedServerXML `xml:"SharedServer"`
}

// SharedServerXML is one <SharedServer> element from shared_servers.
type SharedServerXML struct {
	UserID      string `xml:"userID,attr"`
	Username    string `xml:"username,attr"`
	AccessToken string `xml:"accessToken,attr"`
}

// HistoryItem is one entry from GET /status/sessions/history/all.
type HistoryItem struct {
	RatingKey        string          `json:"ratingKey"`
	Type             string          `json:"type"`
	LibraryTitle     string          `json:"librarySectionTitle"`
	AccountID        streams.FlexInt `json:"accountID"`
	LibrarySectionID streams.FlexInt `json:"librarySectionID"`
}
