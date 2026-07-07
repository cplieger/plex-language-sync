package sync

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// EpisodeRef bundles a shared reference episode and its selected
// streams. A nil *EpisodeRef means "no reference found, fall back to the
// learned language profile" for the caller.
type EpisodeRef struct {
	Episode  *streams.Episode
	Audio    *streams.Stream
	Subtitle *streams.Stream
}

// maxRefSearchDepth caps the number of per-episode metadata fetches the
// reference search will perform before giving up. Bounds latency on
// very long shows.
const maxRefSearchDepth = 50

// ProcessNewOrUpdatedEpisodeAllUsers processes a new/updated episode
// for every known user (admin + shared).
//
// The reference episode is searched ONCE per episode via the admin
// reader and shared across all users. Plex returns identical
// Stream.selected and metadata fields regardless of which user's token
// is used for reads (verified 2026-04-26 against live API + Tautulli
// playback history). Writes via UpdateEpisodeStreams /
// ApplyLanguageProfile still use per-user clients because PUTs set
// per-user playback state.
//
// Cost collapse: for an N-user household, this path previously ran
// ShowEpisodes + up to maxRefSearchDepth Episode calls × N times per
// episode (e.g. 15 users × ~10 calls = 150 admin-equivalent calls). Now
// ~10 calls total for the reference search plus N writes — the only
// per-user work required.
//
// Ignore contract: this entry point does NOT apply the ignore
// policy itself. Callers MUST gate on
// api.IgnoreChecker.ShouldSkipEpisode before invoking it (main.go
// handleTimeline and scheduler processRecentlyAddedEpisode both
// do). The gate is kept upstream deliberately to avoid a
// redundant ShowMetadata fetch here; a caller that skips it will
// silently apply language to an ignored show, breaking the
// documented "ignored show excluded from BOTH propagation AND
// learning" policy. (Contrast ChangeTracksForEpisode, which
// self-gates.)
func (s *Syncer) ProcessNewOrUpdatedEpisodeAllUsers(
	ctx context.Context,
	episode *streams.Episode,
	trigger string,
) {
	ref := s.FindEpisodeReference(ctx, episode)

	for _, u := range s.users.All() {
		if ctx.Err() != nil {
			return
		}
		userClient := s.userClient(u.ID)
		if userClient == nil {
			slog.Warn("sync: no per-user client, skipping user", "user_id", u.ID)
			continue
		}
		s.applyEpisodeForUser(ctx, userClient, u.ID, episode, ref, trigger)
	}
}

// FindEpisodeReference locates a reference episode for a new/updated
// episode: the most recent previously-seen episode in the show with a
// selected audio stream. Returns nil when the show has no reference yet
// (no prior episode with an active selection), which signals callers to
// fall back to the learned language profile.
//
// Uses the admin reader because stream-selection state is server-wide,
// not per-user (see ProcessNewOrUpdatedEpisodeAllUsers for full
// rationale). The caller shares the result across every user being
// processed for the same episode, so this runs ONCE per episode
// regardless of user count.
//
// When no reference is found, the branch emits a log line with a stable
// `reason` label so Loki can pinpoint why an episode fell through to the
// language-profile path. DEBUG reasons are benign ("no_grandparent_key",
// "no_candidate", "no_selected_audio"); WARN reasons signal a degraded
// fetch ("get_show_episodes_error", "candidate_fetch_errors") that may
// have masked an otherwise-usable reference.
func (s *Syncer) FindEpisodeReference(
	ctx context.Context,
	episode *streams.Episode,
) *EpisodeRef {
	showRatingKey := episode.GrandparentRatingKey
	if showRatingKey == "" {
		slog.Debug("reference search skipped",
			"episode", episode.ShortName(),
			"reason", "no_grandparent_key")
		return nil
	}

	episodes, err := s.plex.ShowEpisodes(ctx, plex.RatingKey(showRatingKey))
	if err != nil {
		slog.Warn("failed to fetch show episodes for reference",
			"show", episode.GrandparentTitle,
			"reason", "get_show_episodes_error",
			"error", err)
		return nil
	}

	ref, searched, fetchErrors := findReferenceEpisode(
		ctx, s.plex, episodes, episode.RatingKey, maxRefSearchDepth)

	if ref == nil {
		if fetchErrors > 0 && ctx.Err() == nil {
			slog.Warn("reference search failed: candidate fetches errored",
				"show", episode.GrandparentTitle,
				"searched", searched,
				"fetch_errors", fetchErrors,
				"reason", "candidate_fetch_errors")
			return nil
		}
		slog.Debug("reference search yielded no candidate",
			"show", episode.GrandparentTitle,
			"searched", searched,
			"fetch_errors", fetchErrors,
			"reason", "no_candidate")
		return nil
	}

	audio, sub := streams.Selected(ref)
	if audio == nil {
		// Reference found but has no selected audio — treat as
		// no-reference so callers fall through to language-profile
		// path.
		slog.Debug("reference found but has no selected audio",
			"episode", episode.ShortName(),
			"reference", ref.ShortName(),
			"reason", "no_selected_audio")
		return nil
	}

	slog.Debug("reference search completed",
		"show", episode.GrandparentTitle,
		"searched", searched,
		"reference", ref.ShortName())

	return &EpisodeRef{Episode: ref, Audio: audio, Subtitle: sub}
}

// applyEpisodeForUser applies a previously-found reference (or the
// learned language profile as a fallback) to a single episode for a
// single user. The reference search that produces `ref` is shared
// across all users for the same episode by the caller.
//
// A nil `ref` means no reference episode was found (new show, or no
// prior episode with selections) and the language-profile fallback
// should be tried if enabled.
//
// Writes via UpdateEpisodeStreams / ApplyLanguageProfile use the
// per-user client because PUTs set per-user playback state.
func (s *Syncer) applyEpisodeForUser(
	ctx context.Context,
	userClient api.PlexReadWriter,
	userID string,
	episode *streams.Episode,
	ref *EpisodeRef,
	trigger string,
) {
	username := s.users.Name(userID)

	if ref == nil {
		if s.cfg.LanguageProfiles {
			if s.ApplyLanguageProfile(ctx, userClient, userID, episode, trigger) {
				return
			}
		}
		slog.Debug("no reference episode found for new episode",
			"episode", episode.ShortName(), "user", username)
		return
	}

	changed := s.UpdateEpisodeStreams(ctx, userClient, username, episode.RatingKey, ref.Audio, ref.Subtitle)
	if changed {
		slog.Info("new/updated episode language set",
			"trigger", trigger,
			"user", username,
			"episode", episode.ShortName(),
			"reference", ref.Episode.ShortName(),
			"audio", streams.Desc(ref.Audio),
			"subtitle", streams.Desc(ref.Subtitle))
	}
}

// findReferenceEpisode walks episodes from newest to oldest and returns
// the first one (other than excludeKey) that has a selected audio
// stream. It fetches full metadata per candidate via the given reader
// and caps the search at maxDepth items to bound latency on very long
// shows.
//
// Package-private, exported only through FindEpisodeReference; retained
// as a standalone helper (rather than a method) because the test suite
// drives it directly with synthetic episode lists.
func findReferenceEpisode(
	ctx context.Context,
	reader api.PlexReader,
	episodes []streams.Episode,
	excludeKey string,
	maxDepth int,
) (reference *streams.Episode, searched, fetchErrors int) {
	for i := range slices.Backward(episodes) {
		if ctx.Err() != nil {
			return nil, searched, fetchErrors
		}
		if searched >= maxDepth {
			return nil, searched, fetchErrors
		}
		searched++
		ep := &episodes[i]
		if ep.RatingKey == excludeKey {
			continue
		}
		full, err := reader.Episode(ctx, plex.RatingKey(ep.RatingKey))
		if err != nil {
			// ErrNotFound = benign (candidate has no metadata); a context
			// error is shutdown, not degradation. Only real transport/server
			// errors signal a degraded Plex.
			if !errors.Is(err, plex.ErrNotFound) && ctx.Err() == nil {
				fetchErrors++
			}
			continue
		}
		if audio, _ := streams.Selected(full); audio != nil {
			return full, searched, fetchErrors
		}
	}
	return nil, searched, fetchErrors
}
