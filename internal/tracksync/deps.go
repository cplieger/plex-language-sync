package tracksync

import (
	"context"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/users"
	"github.com/cplieger/plexapi/v2"
)

// plexReader is the admin-scoped read surface: resolve one episode, and
// enumerate a show's or a season's episodes to propagate across.
type plexReader interface {
	Episode(ctx context.Context, ratingKey plex.RatingKey) (*streams.Episode, error)
	ShowEpisodes(ctx context.Context, showRatingKey plex.RatingKey) ([]streams.Episode, error)
	SeasonEpisodes(ctx context.Context, seasonRatingKey plex.RatingKey) ([]streams.Episode, error)
}

// plexWriter is the stream-selection write surface. Every write is user-scoped:
// Plex records a selection against the requesting token's user, not
// server-wide, so these must be called on a per-user client and never on the
// admin client as a fallback.
type plexWriter interface {
	SetAudioStream(ctx context.Context, sel plexapi.StreamSelection) error
	SetSubtitleStream(ctx context.Context, sel plexapi.StreamSelection) error
	DisableSubtitles(ctx context.Context, partID int) error
}

// PlexReadWriter is a per-user client: it reads an episode's current state and
// writes the new selection through the same token. Exported because it is the
// result type of UserClientFunc, which the composition root has to name.
type PlexReadWriter interface {
	plexReader
	plexWriter
}

// UserClientFunc returns the per-user read+write Plex client for a userID, or
// nil when none can be built. Nil means skip the user: falling back to the
// admin client would record the selection against the admin's account and
// silently drop the target user's preference.
type UserClientFunc func(userID string) PlexReadWriter

// cacheStore is the knowledge this package creates and re-reads: the learned
// audio->subtitle profiles and the per-(user, show) intent ledger.
type cacheStore interface {
	LearnLanguageProfile(userID string, choice streams.LanguageChoice)
	SubtitleLangForAudio(userID, audioLang string) (string, bool)
	// RecordIntent stores a user's observed track selection for a show
	// (event-plane only: callers record what they witnessed at a resolved
	// play session, never a reconstructed attribution).
	RecordIntent(userID, showKey string, intent *streams.Intent)
	IntentFor(userID, showKey string) (streams.Intent, bool)
}

// userLookup enumerates the users to fan out to and names them for logs.
type userLookup interface {
	All() []users.Account
	Name(userID string) string
}

// episodeSkipper is the ignore decision: should this episode be left alone.
type episodeSkipper interface {
	ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool
}
