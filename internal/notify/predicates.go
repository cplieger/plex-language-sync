package notify

import (
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// streamsKeyKind is the leading component of the stream dedup key,
// derived from cache.KeyPrefixStreams so the state.json prefix stays
// defined in one place.
var streamsKeyKind = strings.TrimSuffix(cache.KeyPrefixStreams, string(keyenc.Separator))

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

// BuildStreamCacheKey builds a deduplication key from user, episode and
// stream IDs, escaped with keyenc because userID and ratingKey are
// upstream strings that may contain a literal ':'. Under plain
// concatenation two different tuples could forge the same key (e.g.
// userID "42:1234" + ratingKey "5" collides with userID "42" +
// ratingKey "1234:5"), which silently drops the second selection for
// the dedup window. keyenc.Join escapes each component so distinct
// tuples always produce distinct keys, and is byte-identical to the
// previous fmt.Sprintf output for ordinary input free of ':' or '\'.
func BuildStreamCacheKey(sel StreamSelectionKey) string {
	return keyenc.Join(streamsKeyKind, sel.UserID, sel.RatingKey,
		strconv.Itoa(sel.AudioID), strconv.Itoa(sel.SubtitleID))
}

// StreamSelectionKey names the four values identifying one user's
// stream selection on one episode. Named fields prevent a transposition
// of the two adjacent strings or two adjacent ints from silently
// producing a well-formed key for the wrong stream.
type StreamSelectionKey struct {
	// UserID is the Plex account the selection belongs to.
	UserID string
	// RatingKey is the episode.
	RatingKey string
	// AudioID is the selected audio stream's id, 0 when none.
	AudioID int
	// SubtitleID is the selected subtitle stream's id, 0 when none.
	SubtitleID int
}

// BuildTimelineCacheKey builds the per-episode timeline dedup key.
//
// Deliberately NOT escaped like BuildStreamCacheKey: itemID is the
// whole remainder of the key, so the mapping is injective regardless
// of its contents.
func BuildTimelineCacheKey(itemID string) string {
	return cache.KeyPrefixTimeline + itemID
}
