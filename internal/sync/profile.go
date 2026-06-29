package sync

import (
	"context"
	"log/slog"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// ApplyLanguageProfile applies a learned language profile to a new
// episode when no reference episode exists in the show. This handles
// the case where a brand-new show is added and the user has
// established preferences (e.g., Japanese audio → English subtitles
// for anime).
//
// Historical note: this function used to Episode() via the per-user
// client on the theory that the user-scoped view might show different
// Stream.selected values than the caller's (possibly admin-fetched)
// episode. Verified 2026-04-26 against live data: Plex does NOT
// differentiate Stream.selected per user token — the same episode
// returns identical Stream[*].selected values whether fetched via
// admin token or a Plex Home user's token. The re-fetch was a
// per-user round-trip that added no information, just latency.
//
// The caller (applyEpisodeForUser) passes `episode` freshly fetched
// from either admin or user client upstream. The data we read from it
// here — selected streams and the first Part ID — is metadata-level
// and consistent across clients. When applyProfileSubtitle mutates
// state, it does so via `userClient`: per-user writes go through the
// user's own client when one is available. When the per-user client
// cannot be constructed, callers pass nothing through to here — the
// operation is SKIPPED upstream rather than written under the admin
// token, preserving per-user isolation (writing a shared user's
// selection under the admin token would corrupt the admin's per-user
// state and not apply the intended user's).
func (s *Syncer) ApplyLanguageProfile(
	ctx context.Context,
	userClient api.PlexWriter,
	userID string,
	episode *streams.Episode,
	trigger string,
) bool {
	target := episode

	// Get the default audio stream to determine the show's primary
	// language.
	curAudio, curSub := streams.Selected(target)
	if curAudio == nil || curAudio.LanguageCode == "" {
		return false
	}

	subLang, ok := s.cache.SubtitleLangForAudio(userID, curAudio.LanguageCode)
	if !ok {
		return false
	}

	partID := streams.FirstPartID(target)
	if partID == 0 {
		return false
	}

	username := s.users.Name(userID)
	changed := applyProfileSubtitle(ctx, userClient, target, partID, subLang, curSub, username)
	if changed {
		slog.Info("language profile applied to new show",
			"trigger", trigger,
			"user", username,
			"episode", target.ShortName(),
			"audio_lang", curAudio.LanguageCode,
			"subtitle_lang", subLang)
	}
	return changed
}

// applyProfileSubtitle sets or disables the subtitle stream based on
// the learned language profile. It returns true when the stream was
// changed.
//
// Unexported but documented: the tests exercise this through
// ApplyLanguageProfile rather than directly, keeping the package's
// public surface small.
func applyProfileSubtitle(
	ctx context.Context,
	userClient api.PlexWriter,
	target *streams.Episode,
	partID int,
	subLang string,
	curSub *streams.Stream,
	username string,
) bool {
	if subLang == "" {
		// Profile says no subtitles for this audio language.
		if curSub == nil {
			return false
		}
		if err := userClient.DisableSubtitles(ctx, partID); err != nil {
			slog.Warn("failed to disable subtitles via profile",
				"episode", target.ShortName(), "user", username, "error", err)
			return false
		}
		return true
	}

	bestSub := streams.FindSubtitleByLanguage(streams.Subtitle(target), subLang)
	if bestSub == nil || (curSub != nil && curSub.ID == bestSub.ID) {
		return false
	}
	if err := userClient.SetSubtitleStream(ctx, partID, bestSub.ID); err != nil {
		slog.Warn("failed to set subtitle via profile",
			"episode", target.ShortName(), "user", username, "error", err)
		return false
	}
	return true
}
