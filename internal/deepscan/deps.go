package deepscan

import (
	"context"
	"time"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// plexReader is the admin-scoped read surface the sweep needs: enumerate
// sections, replay recent history, list recently-added episodes, and resolve
// one episode.
type plexReader interface {
	Episode(ctx context.Context, ratingKey plex.RatingKey) (*streams.Episode, error)
	RecentlyAdded(ctx context.Context, sectionKey plex.RatingKey, sinceUnix int64) ([]streams.Episode, error)
	History(ctx context.Context, sinceUnix int64) ([]plex.HistoryItem, error)
	ShowSections(ctx context.Context) ([]plex.Section, error)
}

// EpisodeReader is the one thing the deep scan asks of a per-user client:
// resolve an episode under that user's token. Exported because it is the
// result type of UserClientFunc.
//
// One method, because tracksync derives its own client from the userID
// rather than taking one — the mismatch between "whose client" and "whose
// intent" cannot be constructed.
type EpisodeReader interface {
	Episode(ctx context.Context, ratingKey plex.RatingKey) (*streams.Episode, error)
}

// UserClientFunc returns the per-user client for a userID, or nil when none can
// be built. Nil means skip the history item for that user.
type UserClientFunc func(userID string) EpisodeReader

// runLedger is the persistence the pass needs: the dedup gate plus the last-run
// watermark that keeps a cold restart from double-running the analysis.
type runLedger interface {
	CheckAndMark(key string) bool
	LastSchedulerRun() time.Time
	SetLastSchedulerRun(t time.Time)
}

// skipChecker is the ignore decision. Two methods: the library-only check for
// the recently-added loop (only a section title in hand) and the full
// episode check once a reference exists.
type skipChecker interface {
	IgnoreLibrary(title string) bool
	ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool
}

// Syncer is the propagation this pass drives, declared here so deepscan does
// not import internal/tracksync.
type Syncer interface {
	ReconcileWithIntent(ctx context.Context, userID string, episode *streams.Episode, viewedAt int64, trigger string)
	ProcessNewOrUpdatedEpisodeAllUsers(ctx context.Context, episode *streams.Episode, trigger string)
}

// CacheSaver flushes the cache to disk at the end of a tick. Separate from
// runLedger (which excludes file-system concerns), so the pass can trigger a
// flush without the ledger's consumers knowing about the persistence path.
type CacheSaver func() error
