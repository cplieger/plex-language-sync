// Package plex types: the app-facing container and admin types, plus
// aliases onto the shared plexapi library where the shapes are the
// library's own. Stream-selection domain types (Episode, Stream, Media,
// Part, Label) live in internal/streams.
package plex

import (
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v2"
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

// RatingKey is the library's validated Plex item identifier, aliased so
// this package's boundary vocabulary and error text (`invalid rating
// key %q`, which scrapers grep for) stay the library's.
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

// Show is the show-level metadata from GET /library/metadata/{key} when
// the key points to a TV show. Split off from Episode so a caller
// asking for show labels doesn't receive an Episode-typed value.
//
// Labels are the only field the one consumer (ignore.Policy) reads; the
// decoder is non-strict so the rest of Plex's fields are ignored.
type Show struct {
	Label []streams.Label `json:"Label"`
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
	RatingKey    string          `json:"ratingKey"`
	Type         string          `json:"type"`
	LibraryTitle string          `json:"librarySectionTitle"`
	AccountID    streams.FlexInt `json:"accountID"`
	// ViewedAt is the play's unix timestamp, the same field the History
	// fetch filters on server-side (viewedAt>=N). 0 when absent.
	ViewedAt streams.FlexInt `json:"viewedAt"`
}
