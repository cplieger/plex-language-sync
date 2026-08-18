package deepscan

import (
	"context"
	"time"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// The interfaces below are declared HERE, at the consumer, rather than in a
// shared contract package, and each names only what the deep scan calls. The
// reads are 4 of the Plex client's 8 methods; the ledger is 3 of the cache's 11,
// and those 3 do not overlap the 4 the propagation path uses or the 2 user
// management uses — one type had fused three unrelated stores.

// plexReader is the admin-scoped read surface the sweep needs: enumerate
// sections, replay recent history, list recently-added episodes, and resolve
// one episode.
type plexReader interface {
	Episode(ctx context.Context, ratingKey plex.RatingKey) (*streams.Episode, error)
	RecentlyAdded(ctx context.Context, sectionKey plex.RatingKey, sinceUnix int64) ([]streams.Episode, error)
	History(ctx context.Context, sinceUnix int64) ([]plex.HistoryItem, error)
	ShowSections(ctx context.Context) ([]plex.Section, error)
}

// EpisodeReader is the one thing the deep scan asks of a PER-USER client:
// resolve an episode under that user's token. Exported because it is the result
// type of UserClientFunc, which the composition root has to name.
//
// One method, because the propagation that follows no longer takes a client —
// tracksync derives its own from the userID, so the mismatch between "whose
// client" and "whose intent" cannot be constructed.
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

// skipChecker is the ignore decision. Two methods, because this pass needs both
// shapes: the library-only check for the recently-added loop (where only a
// section title is in hand) and the full episode check once a reference exists.
type skipChecker interface {
	IgnoreLibrary(title string) bool
	ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool
}

// Syncer is the propagation this pass drives, declared here so deepscan does
// not import internal/tracksync. *tracksync.Syncer satisfies it.
type Syncer interface {
	ReconcileWithIntent(ctx context.Context, userID string, episode *streams.Episode, viewedAt int64, trigger string)
	ProcessNewOrUpdatedEpisodeAllUsers(ctx context.Context, episode *streams.Episode, trigger string)
}

// CacheSaver flushes the cache to disk at the end of a tick. Deliberately
// separate from runLedger, which excludes file-system concerns, so the pass can
// trigger a flush without the ledger's consumers knowing about the persistence
// path. A trivial closure in the composition root supplies it.
type CacheSaver func() error
