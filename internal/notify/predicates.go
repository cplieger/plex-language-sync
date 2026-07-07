package notify

import (
	"fmt"

	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// Plex wire-format constants used by the event predicates. These mirror
// the Plex notification schema. Kept unexported here because they are
// implementation details of the notify package's classification logic;
// callers should use IsRelevantPlayEvent / IsRelevantTimelineEntry /
// TimelineAction rather than branching on these values directly. The
// episode metadata type is imported from internal/plex (single source of
// truth).
const (
	stateCreated = "created"
	stateUpdated = "updated"
	statePlaying = "playing"
	statePaused  = "paused"

	scanActionNew     = "scan_new"
	scanActionUpdated = "scan_updated"
)

// IsRelevantPlayEvent returns true if a play event should be processed
// (state is playing/paused and has a rating key).
func IsRelevantPlayEvent(ev PlayEvent) bool {
	if ev.State != statePlaying && ev.State != statePaused {
		return false
	}
	return ev.RatingKey != ""
}

// IsRelevantTimelineEntry returns true if a timeline entry should be
// processed (episode type, metadata/media created or updated, non-empty
// item ID).
func IsRelevantTimelineEntry(entry *TimelineEntry) bool {
	if entry.Type != plex.MetadataTypeEpisode {
		return false
	}
	if entry.MetadataState != stateCreated && entry.MetadataState != stateUpdated &&
		entry.MediaState != stateCreated && entry.MediaState != stateUpdated {
		return false
	}
	return entry.ItemID != ""
}

// TimelineAction returns "scan_new" if the entry represents a newly
// created item, or "scan_updated" otherwise. The returned strings are
// byte-for-byte frozen — they are emitted as log/metric values consumed
// by dashboards.
func TimelineAction(entry *TimelineEntry) string {
	if entry.MetadataState == stateCreated || entry.MediaState == stateCreated {
		return scanActionNew
	}
	return scanActionUpdated
}

// BuildStreamCacheKey builds a deduplication key from user, episode, and
// current stream IDs so we only process when the selection actually
// changes. The "streams:" prefix and colon-separated layout are part of
// the on-disk cache.json schema.
func BuildStreamCacheKey(userID, ratingKey string, audioID, subID int) string {
	return fmt.Sprintf("%s%s:%s:%d:%d", cache.KeyPrefixStreams, userID, ratingKey, audioID, subID)
}

// BuildTimelineCacheKey builds the per-episode timeline (library-scan)
// dedup key. The "timeline:" prefix is part of the on-disk cache.json
// schema.
func BuildTimelineCacheKey(itemID string) string {
	return cache.KeyPrefixTimeline + itemID
}
