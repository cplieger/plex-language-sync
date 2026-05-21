// Package api declares the cross-package interface spine.
//
// Concrete types in internal/{plex,cache,users,...} implement these
// interfaces; consumers (internal/{sync,scheduler,notify}) depend only
// on these interfaces. This keeps the composition root in main.go as
// the single wiring layer and lets tests substitute fakes without
// reaching into production packages.
package api

import (
	"context"

	"plex-language-sync/internal/plex"
	"plex-language-sync/internal/streams"
)

// PlexReader is the read side of the Plex HTTP client as consumed by the
// sync and scheduler packages. *plex.Client satisfies it by structural
// typing; tests supply a fake implementation.
//
// Rating keys are typed as plex.RatingKey at the interface boundary so
// the implementation validates once via RatingKey.Validate rather than
// per-method strconv.Atoi stanzas. Call sites wrap wire-origin strings
// (streams.Episode.RatingKey, plex.Section.Key, plex.HistoryItem.
// RatingKey) at the seam, e.g. reader.Episode(ctx,
// plex.RatingKey(ep.RatingKey)).
//
// sinceUnix is seconds since the Unix epoch (int64), matching
// *plex.Client's History / RecentlyAdded signatures which pass the
// value straight into Plex's `viewedAt>=` / `addedAt>=` query filters.
type PlexReader interface {
	Episode(ctx context.Context, ratingKey plex.RatingKey) (*streams.Episode, error)
	ShowEpisodes(ctx context.Context, showRatingKey plex.RatingKey) ([]streams.Episode, error)
	SeasonEpisodes(ctx context.Context, seasonRatingKey plex.RatingKey) ([]streams.Episode, error)
	ShowMetadata(ctx context.Context, showRatingKey plex.RatingKey) (*plex.Show, error)
	RecentlyAdded(ctx context.Context, sectionKey plex.RatingKey, sinceUnix int64) ([]streams.Episode, error)
	History(ctx context.Context, sinceUnix int64) ([]plex.HistoryItem, error)
	ShowSections(ctx context.Context) ([]plex.Section, error)
	UserFromSession(ctx context.Context, clientIdentifier string) (userID, username string, err error)
}

// PlexWriter is the write side of the Plex HTTP client (stream-selection
// PUTs). *plex.Client satisfies it by structural typing. The per-user
// write path runs through the user-scoped *plex.Client returned by
// users.Manager.ClientForUser, so the interface must be satisfied by
// both the admin and per-user clients.
type PlexWriter interface {
	SetAudioStream(ctx context.Context, partID, streamID int) error
	SetSubtitleStream(ctx context.Context, partID, streamID int) error
	DisableSubtitles(ctx context.Context, partID int) error
}

// PlexReadWriter is the union of PlexReader and PlexWriter. Per-user
// operations (track changes) need both read and write access through
// the same client value so the sync package can request a single typed
// parameter from its callers.
type PlexReadWriter interface {
	PlexReader
	PlexWriter
}

// UserClientFunc returns a per-user read+write Plex client for the
// given userID. Defined here so scheduler and sync share a single type
// rather than declaring identical local aliases.
type UserClientFunc func(userID string) PlexReadWriter

// IgnoreChecker is the cross-subsystem "should I skip this library /
// episode?" decision, shared by sync, scheduler, and the WebSocket
// notifyAdapter so the three paths honour identical ignore semantics.
// *ignore.Policy (see internal/ignore) is the only production
// implementation; tests substitute in-package fakes.
//
// ShouldSkipEpisode combines library-title + show-label checks into
// one call; IgnoreLibrary is the narrower library-only variant used
// by the scheduler's recently-added loop where only the Section.Title
// is available (no episode ref yet). Consumers should prefer
// ShouldSkipEpisode when they already hold an episode ref because it
// also checks the show-label ignore list via ShowMetadata.
type IgnoreChecker interface {
	IgnoreLibrary(title string) bool
	ShouldSkipEpisode(ctx context.Context, reader PlexReader, ref *streams.Episode) bool
}
