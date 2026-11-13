package notify

import (
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// Plex wire-format constants for the event predicates. Unexported:
// callers should use IsRelevantPlayEvent / IsRelevantTimelineEntry /
// TimelineAction rather than branch on these directly.
const (
	stateCreated = "created"
	stateUpdated = "updated"
	statePlaying = "playing"
	statePaused  = "paused"

	scanActionNew     = "scan_new"
	scanActionUpdated = "scan_updated"
)

// IsRelevantPlayEvent reports whether a play event should be processed
// (state playing/paused, non-empty rating key).
func IsRelevantPlayEvent(ev PlayEvent) bool {
	if ev.State != statePlaying && ev.State != statePaused {
		return false
	}
	return ev.RatingKey != ""
}

// IsPausedPlayEvent reports whether the event announces paused playback.
// A paused announcement carries no new selection to propagate, so it is
// still processed but must not be read as evidence about the resolver:
// Plex keeps a client's paused item flowing after it has removed the
// session, and a terminated-then-still-announcing client is the shape
// that made resolution look broken when nothing was.
func IsPausedPlayEvent(ev PlayEvent) bool {
	return ev.State == statePaused
}

// IsRelevantTimelineEntry reports whether a timeline entry should be
// processed (episode type, metadata/media created or updated,
// non-empty item ID).
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

// TimelineAction returns "scan_new" for a newly created item or
// "scan_updated" otherwise. The returned strings are frozen: dashboards
// consume them as log/metric values.
func TimelineAction(entry *TimelineEntry) string {
	if entry.MetadataState == stateCreated || entry.MediaState == stateCreated {
		return scanActionNew
	}
	return scanActionUpdated
}

// BuildTimelineCacheKey builds the per-episode timeline dedup key. It is
// not escaped: itemID is the whole remainder of the key, so the mapping is
// injective whatever its contents.
func BuildTimelineCacheKey(itemID string) string {
	return cache.KeyPrefixTimeline + itemID
}
