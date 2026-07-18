// Package plex types: the app-facing container and admin types, plus
// aliases onto the shared plexapi library where the shapes are the
// library's own (RatingKey, Section, ServerIdentity, SharedServer). Value
// types for the stream-selection domain (Episode, Stream, Media, Part,
// Label) live in internal/streams; this package decodes into them via the
// generic fetch helpers.
package plex

import (
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi"
)

// Plex wire-protocol constants, re-exported from the library so consumers
// (main, config, scheduler, notify, sync, library.go) keep one import.
const (
	// TypeEpisode is the Plex metadata "type" string for episode items.
	TypeEpisode = plexapi.TypeEpisode
	// MetadataTypeEpisode is the numeric type ID for ?type= filters.
	MetadataTypeEpisode = plexapi.MetadataTypeEpisode
	// SectionTypeShow is the library-section "type" string for TV shows.
	SectionTypeShow = plexapi.SectionTypeShow
)

// RatingKey is the library's validated Plex item identifier. The alias
// preserves this package's boundary vocabulary (api.PlexReader and the
// *Client methods take plex.RatingKey); validation semantics — and the
// exact `invalid rating key %q` error text scrapers grep for — are the
// library's.
type RatingKey = plexapi.RatingKey

// ServerIdentity is the library's GET / identity payload (the app reads
// FriendlyName, MachineIdentifier, Version).
type ServerIdentity = plexapi.ServerIdentity

// Section is a library section returned by GET /library/sections.
type Section = plexapi.Section

// SharedServerXML is one shared-user entry from the plex.tv
// shared_servers endpoint (userID, username, user-scoped access token).
type SharedServerXML = plexapi.SharedServer

// User represents the resolved admin (or any) Plex user.
type User struct {
	ID   string
	Name string
}

// Show is the show-level metadata returned by GET /library/metadata/{key}
// when the key points to a TV show. Split off from Episode so callers
// asking "what are the show's labels?" don't receive an Episode-typed
// value.
type Show struct {
	RatingKey        string          `json:"ratingKey"`
	Title            string          `json:"title"`
	LibraryTitle     string          `json:"librarySectionTitle"`
	Label            []streams.Label `json:"Label"`
	LibrarySectionID streams.FlexInt `json:"librarySectionID"`
}

// Season is the season-level metadata returned by GET
// /library/metadata/{key} when the key points to a season: the
// navigational spine (parent key, season index) without the whole
// media/part/stream graph.
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

// HistoryItem is one entry from GET /status/sessions/history/all.
type HistoryItem struct {
	RatingKey        string          `json:"ratingKey"`
	Type             string          `json:"type"`
	LibraryTitle     string          `json:"librarySectionTitle"`
	AccountID        streams.FlexInt `json:"accountID"`
	LibrarySectionID streams.FlexInt `json:"librarySectionID"`
	// ViewedAt is the play's unix timestamp — the same field the History
	// fetch filters on server-side (viewedAt>=N). Consumed by the
	// reconcile plane's freshness guard; 0 when absent from the response.
	ViewedAt streams.FlexInt `json:"viewedAt"`
}
